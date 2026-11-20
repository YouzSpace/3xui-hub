package task

// 邀请码刷新（invite:refresh）：重算邀请码的计数/失效条件。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
)

// runInviteRefresh 对齐 RefreshInviteCodes::handle()。
// 刷新到期的邀请码：旧码立即失效（status=refreshed），给同一用户发新码。
func (r *commandRunner) runInviteRefresh(ctx context.Context) error {
	r.logf("[%s] 开始刷新邀请码...", r.now().Format("2006-01-02 15:04:05"))

	// 查到期的 active 邀请码
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, user_id, code FROM discount_codes
		 WHERE source = 'invite' AND status = 'active'
		   AND next_refresh_at IS NOT NULL AND next_refresh_at <= ?`,
		r.now().UTC())
	if err != nil {
		return fmt.Errorf("查询到期邀请码失败: %w", err)
	}
	defer rows.Close()

	type inviteRow struct {
		ID     int64
		UserID int64
		Code   string
	}
	var expired []inviteRow
	for rows.Next() {
		var row inviteRow
		if err := rows.Scan(&row.ID, &row.UserID, &row.Code); err != nil {
			return err
		}
		expired = append(expired, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	refreshed := 0
	for _, code := range expired {
		done, err := r.refreshOneInvite(ctx, code.ID, code.UserID)
		if err != nil {
			r.logf("刷新邀请码 %s 失败: %v", code.Code, err)
			continue
		}
		if done {
			refreshed++
			r.logf("  刷新用户 #%d 的邀请码 %s", code.UserID, code.Code)
		}
	}

	r.logf("邀请码刷新完成：%d 个", refreshed)
	return nil
}

// refreshOneInvite 在事务内刷新一张邀请码（锁行重读，重复执行只刷一次）。
//
// 刷新语义（2026-11-06 用户定稿）：**只在原行上换码文本，绝不新增行**。
//   - 锁定关系（users.used_invite_code_id）与返佣记录存的都是码的 **id**，
//     原行 UPDATE 码文本 id 不变 → 用过/锁定的返佣链自动保持，无需留「已刷新」旧行；
//   - used_count 清零（码文本已换，旧码的使用次数不能算到新码头上）。
//
// 用户已注销（users 行不存在）时不发新码：否则每次刷新都会给幽灵用户
// 再插一张新孤儿码，孤儿自我复制永不收敛。孤儿码只把状态置 refreshed（留证据），
// 下轮刷新直接跳过（status != active），不会反复产生新行。
func (r *commandRunner) refreshOneInvite(ctx context.Context, codeID, userID int64) (bool, error) {
	tx, err := r.deps.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// 锁行重读
	var status string
	var nextRefresh sql.NullTime
	err = tx.QueryRowContext(ctx,
		"SELECT status, next_refresh_at FROM discount_codes WHERE id = ? FOR UPDATE", codeID).
		Scan(&status, &nextRefresh)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if status != "active" {
		return false, nil
	}
	if nextRefresh.Valid && nextRefresh.Time.After(r.now().UTC()) {
		return false, nil
	}

	now := r.now().UTC()

	// 锁用户行，保证「同一用户最多一张 active 邀请码」；顺带判用户是否还在。
	var liveUserID int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", userID).Scan(&liveUserID)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if err == sql.ErrNoRows {
		// 用户已注销：不发新码、不删行（锁定/返佣可能还引用），置 refreshed 后下轮自动跳过。
		if _, err := tx.ExecContext(ctx,
			"UPDATE discount_codes SET status = 'refreshed', refreshed_at = ?, updated_at = ? WHERE id = ?",
			now, now, codeID); err != nil {
			return false, err
		}
		r.logf("  用户 #%d 已注销，邀请码 %d 置为 refreshed，不再刷新", userID, codeID)
		return tx.Commit() == nil, nil
	}

	// 原行刷新：换码文本 + 重置使用计数，id 不变 → 返佣链不断。
	// 折扣沿用配置（invite_discount）：刷新即按当前配置重定折扣。
	refreshDays := r.siteConfigInt(ctx, "invite_refresh_days", 30)
	if refreshDays < 1 {
		refreshDays = 30
	}
	newCode, err := r.generateUniqueCode(ctx, "INV")
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE discount_codes SET code = ?, used_count = 0, status = 'active',
		 next_refresh_at = ?, refreshed_at = ?, updated_at = ? WHERE id = ?`,
		newCode, now.AddDate(0, 0, refreshDays), now, now, codeID); err != nil {
		return false, err
	}
	r.logf("  刷新用户 #%d 的邀请码（原行）→ %s", userID, newCode)

	// 把该用户可能残留的其他 active 邀请码也收拢到本行口径（历史数据兼容，幂等）。
	if _, err := tx.ExecContext(ctx,
		"UPDATE discount_codes SET status = 'refreshed', refreshed_at = ?, updated_at = ? WHERE source = 'invite' AND user_id = ? AND status = 'active' AND id <> ?",
		now, now, userID, codeID); err != nil {
		return false, err
	}

	return tx.Commit() == nil, nil
}

// generateUniqueCode 对齐 DiscountCode::generateCode()。
func (r *commandRunner) generateUniqueCode(ctx context.Context, prefix string) (string, error) {
	const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	const codeLen = 8
	for attempt := 0; attempt < 32; attempt++ {
		code := strings.ToUpper(prefix) + randomCodeBody(alphabet, codeLen)
		var n int
		err := r.deps.DB.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM discount_codes WHERE code = ?", code).Scan(&n)
		if err != nil {
			return "", err
		}
		if n == 0 {
			return code, nil
		}
	}
	return "", fmt.Errorf("生成唯一码失败（多次冲突）")
}

// randomCodeBody 生成随机码体（逐字节拒绝采样保证等概率）。
func randomCodeBody(alphabet string, length int) string {
	size := len(alphabet)
	limit := (256 / size) * size
	out := make([]byte, 0, length)
	buf := make([]byte, 1)
	for len(out) < length {
		if _, err := rand.Read(buf); err != nil {
			panic("task: 生成随机数失败: " + err.Error())
		}
		if int(buf[0]) >= limit {
			continue
		}
		out = append(out, alphabet[int(buf[0])%size])
	}
	return string(out)
}

// ── 2. backup:run ──────────────────────────────────────────────────
