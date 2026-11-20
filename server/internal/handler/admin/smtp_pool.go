package admin

// 发信挑号 + 失败分类（多 SMTP 账号池的执行体）。
//
// 行为契约（对应任务书需求 2/3/4）：
//   - 池里有可用号 → 按 last_used_at 最久未用优先挑；发失败按类别换号，单封最多 3 个号；
//   - 池里没有可用号（空表 / 全部非 active / 全部到日限额）→ 回落站点配置的 smtp_*，
//     失败语义与上线前完全一致（不换号、不重试）；
//   - 认证失败 → 该号 disabled；限流/临时 → 该号 cooling 10 分钟；收件人永久拒收 → 不换号。

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// selectSMTPAccount 从候选里挑下一个该用的号（already=本次已试过的 id）。
//
// 挑号口径（需求 2）：
//  0. **默认账号优先**：is_default=1 且可用、且本次没试过 → 先用它；
//  1. 只考虑 usable 的（active / 冷却已到期的 cooling / 未到日限额）；
//  2. 按 last_used_at 最久未用优先，NULL（从未用过）排最前；
//  3. 同一时间并列时按 id 升序（候选本身已排好序，这里保持稳定）。
//
// 返回 nil 表示池里没有可用号 → 调用方回落站点配置。
func selectSMTPAccount(candidates []SMTPAccount, already map[uint64]struct{}, now time.Time) *SMTPAccount {
	// 0) 默认账号优先：它可用就直接用，不参与「最久未用」比较。
	for i := range candidates {
		acc := &candidates[i]
		if !acc.IsDefault {
			continue
		}
		if _, tried := already[acc.ID]; tried {
			continue
		}
		if acc.usable(now) {
			return acc
		}
	}

	var best *SMTPAccount
	for i := range candidates {
		acc := &candidates[i]
		if _, tried := already[acc.ID]; tried {
			continue
		}
		if !acc.usable(now) {
			continue
		}
		if best == nil || smtpLessRecentlyUsed(acc, best) {
			best = acc
		}
	}
	return best
}

// smtpLessRecentlyUsed 判断 a 是否比 b 更久没被用过（NULL 最久），并列时 id 小的优先。
func smtpLessRecentlyUsed(a, b *SMTPAccount) bool {
	switch {
	case a.LastUsedAt == nil && b.LastUsedAt == nil:
		return a.ID < b.ID
	case a.LastUsedAt == nil:
		return true
	case b.LastUsedAt == nil:
		return false
	}
	if !a.LastUsedAt.Equal(*b.LastUsedAt) {
		return a.LastUsedAt.Before(*b.LastUsedAt)
	}
	return a.ID < b.ID
}

// trySendViaPool 是「只走账号池」的发信实现（不做站点配置兜底）。
//
// 返回 sent=false 表示池里没有可用号（空表 / 全非 active / 全到日限额），
// 调用方据此决定是否回落到站点 smtp_* 配置 —— 与 deliverWithPool 的口径完全一致，
// 只是把「兜底」这一步交给调用方，好让 api 包的验证码发信复用同一套挑号逻辑。
//
// 轮换规则同 deliverWithPool：单封最多试 smtpMaxRotations 个号；
// 认证失败 → 该号 disabled；限流/临时 → 冷却 10 分钟；收件人永久拒收 → 立即返回不换号。
func (d *MailDomain) trySendViaPool(ctx context.Context, to, subject, htmlBody string) (sent bool, err error) {
	now := d.now().UTC()
	tried := map[uint64]struct{}{}

	for attempts := 0; attempts < smtpMaxRotations; attempts++ {
		candidates, cerr := d.store.SMTPAccountCandidates(ctx)
		if cerr != nil {
			d.logger.Printf("admin mail 挑号失败: %v", cerr)
			return false, cerr
		}
		acc := selectSMTPAccount(candidates, tried, now)
		if acc == nil {
			return false, nil
		}
		tried[acc.ID] = struct{}{}

		sendErr := d.transport.Send(acc.Settings(), to, subject, htmlBody)
		if sendErr == nil {
			if uerr := d.store.SMTPAccountUsed(ctx, acc.ID, now); uerr != nil {
				d.logger.Printf("admin mail 记录发信成功失败 (id=%d): %v", acc.ID, uerr)
			}
			return true, nil
		}

		kind := classifySMTPFailure(sendErr.Error())
		d.logger.Printf("admin mail 账号 %d 发信失败（%s）: %v", acc.ID, kind, sendErr)
		if !kind.rotatable() {
			return true, sendErr // 已用掉池号，失败不换号，交由调用方报错
		}
		if ferr := d.store.SMTPAccountFailed(ctx, acc.ID, kind, now); ferr != nil {
			d.logger.Printf("admin mail 记录账号失败态失败 (id=%d): %v", acc.ID, ferr)
		}
	}
	// 换满 3 个号仍未成功：按「已尝试池号」返回最后一次错误
	return true, fmt.Errorf("账号池已尝试 %d 个账号仍发送失败", smtpMaxRotations)
}

// SendViaPool 供 api 包（注册验证码 / 找回密码验证码）复用账号池发信。
//
// used=false → 池里没有可用号，调用方应回落站点 smtp_* 配置（保持上线前行为）。
// used=true   → 已经通过池发过（err 为 nil 表示成功）。
func (d *MailDomain) SendViaPool(ctx context.Context, to, subject, htmlBody string) (used bool, err error) {
	return d.trySendViaPool(ctx, to, subject, htmlBody)
}

// SendViaAccount 用**指定**的池账号发信（管理端「验证码用哪个号发」下拉选中具体号）。
//
// 不做轮换：管理员已明确指定，失败就直接返回错误 —— 让配置是否真的生效可判断。
func (d *MailDomain) SendViaAccount(ctx context.Context, accountID uint64, to, subject, htmlBody string) error {
	acc, err := d.store.SMTPAccountByID(ctx, accountID)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("SMTP 账号 %d 不存在", accountID)
		}
		return fmt.Errorf("查询 SMTP 账号 %d 失败: %w", accountID, err)
	}
	now := d.now().UTC()
	if !acc.usable(now) {
		return fmt.Errorf("SMTP 账号 %d 当前不可用（状态=%s）", accountID, acc.Status)
	}
	sendErr := d.transport.Send(acc.Settings(), to, subject, htmlBody)
	if sendErr == nil {
		if uerr := d.store.SMTPAccountUsed(ctx, acc.ID, now); uerr != nil {
			d.logger.Printf("admin mail 记录发信成功失败 (id=%d): %v", acc.ID, uerr)
		}
		return nil
	}
	// 失败也记账：认证失败 → disabled，限流/临时 → 冷却，便于后台看到号坏了。
	kind := classifySMTPFailure(sendErr.Error())
	if ferr := d.store.SMTPAccountFailed(ctx, acc.ID, kind, now); ferr != nil {
		d.logger.Printf("admin mail 记录账号失败态失败 (id=%d): %v", acc.ID, ferr)
	}
	return sendErr
}

// PoolAccountUsable 判断指定账号此刻可用（不存在/停用/冷却/超日限额都算不可用）。
// api 包据此决定「管理员指定的号坏了」时是否退回池挑号。
func (d *MailDomain) PoolAccountUsable(ctx context.Context, accountID uint64) bool {
	acc, err := d.store.SMTPAccountByID(ctx, accountID)
	if err != nil || acc == nil {
		return false
	}
	return acc.usable(d.now().UTC())
}

// HasUsableAccount 报告账号池此刻是否有可用账号（供 api 侧判断「能不能发信」）。
//
// 用它替代原来「站点 smtp_host 是否为空」的门槛：验证码发信已统一走池，
// 只要池里有可用号就视为「邮件服务已配置」，不该再因为站点 smtp_* 为空而报「SMTP未配置」。
func (d *MailDomain) HasUsableAccount(ctx context.Context) bool {
	candidates, err := d.store.SMTPAccountCandidates(ctx)
	if err != nil {
		d.logger.Printf("admin mail 查询账号池失败: %v", err)
		return false
	}
	now := d.now().UTC()
	for i := range candidates {
		if candidates[i].usable(now) {
			return true
		}
	}
	return false
}

// deliverAttempt 是一次发信的结果（供 deliver 写日志 + 打日志用）。
type deliverAttempt struct {
	sendErr  error
	account  *SMTPAccount // nil = 用的站点配置兜底
	attempts int          // 实际尝试次数（池号 + 兜底）
	failKind smtpFailure
}

// deliverWithPool 发一封信：先从账号池轮换挑号，池不可用则回落 smtp_* 配置。
func (d *MailDomain) deliverWithPool(ctx context.Context, job MailJob, cfg map[string]string) deliverAttempt {
	now := d.now().UTC()
	out := deliverAttempt{}
	tried := map[uint64]struct{}{}

	for out.attempts < smtpMaxRotations {
		candidates, err := d.store.SMTPAccountCandidates(ctx)
		if err != nil {
			// 挑号失败按「没有池」处理：回落兜底，别让一次 SQL 抖动把整封信丢掉。
			d.logger.Printf("admin mail 挑号失败，回落单账号配置: %v", err)
			break
		}
		acc := selectSMTPAccount(candidates, tried, now)
		if acc == nil {
			break
		}
		tried[acc.ID] = struct{}{}
		out.attempts++
		out.account = acc

		sendErr := d.transport.Send(acc.Settings(), job.ToEmail, job.Subject, job.HTMLBody)
		if sendErr == nil {
			out.sendErr = nil
			out.failKind = smtpFailureOther
			if err := d.store.SMTPAccountUsed(ctx, acc.ID, now); err != nil {
				d.logger.Printf("admin mail 记录发信成功失败 (id=%d): %v", acc.ID, err)
			}
			return out
		}

		out.sendErr = sendErr
		kind := classifySMTPFailure(sendErr.Error())
		out.failKind = kind
		d.logger.Printf("admin mail 账号 %d 发信失败（%s）: %v", acc.ID, kind, sendErr)

		if !kind.rotatable() {
			// 收件人永久拒收 / 未识别错误：换号也没用，直接记 failed。
			return out
		}
		if err := d.store.SMTPAccountFailed(ctx, acc.ID, kind, now); err != nil {
			d.logger.Printf("admin mail 记录账号失败态失败 (id=%d): %v", acc.ID, err)
		}
	}

	// 池里已经没有可用号（或本次已换满 3 个）：回落到站点配置的单套 smtp_*。
	// 兜底只发一次，失败即 failed —— 与多号池上线前的行为一致。
	settings := d.buildSMTPSettings(cfg)
	out.account = nil
	out.failKind = smtpFailureOther
	out.attempts++
	out.sendErr = d.transport.Send(settings, job.ToEmail, job.Subject, job.HTMLBody)
	return out
}
