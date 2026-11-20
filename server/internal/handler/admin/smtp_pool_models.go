package admin

// 多 SMTP 账号池（发信挑号 + 失败分类 + 管理 API）的类型与接缝。
//
// 与站点配置里的单套 smtp_* 是「池 + 兜底」关系：
//   - 池里有可用账号（status=active 且未到日限额）→ 从池里挑；
//   - 池空/全不可用 → 回落到 smtp_*，行为与上线前完全一致（向后兼容）。
//
// 密码列 password_enc 与 smtp_password 同口径（internal/crypto 的
// Crypt::encryptString 兼容层），接口一律不回明文。

import (
	"context"
	"strings"
	"time"
)

// smtp_accounts.status 取值。
const (
	smtpAccountStatusActive   = "active"
	smtpAccountStatusCooling  = "cooling"
	smtpAccountStatusDisabled = "disabled"
)

// smtpCooldown 是「限流/临时失败」的冷却时长（cooldown_until = now + 10min）。
const smtpCooldown = 10 * time.Minute

// smtpMaxRotations 是单封信最多换几个号（首号失败后最多再换 2 个，共 3 次尝试）。
const smtpMaxRotations = 3

// SMTPAccount 是 smtp_accounts 一行（Password 已解密；接口输出不回）。
type SMTPAccount struct {
	ID            uint64
	Host          string
	Port          int
	Username      string
	Password      string
	Encryption    string // tls|ssl|none
	FromName      *string
	Weight        int
	DailyLimit    int
	SentToday     int
	LastUsedAt    *time.Time
	ResetAt       *time.Time
	Status        string
	CooldownUntil *time.Time
	ErrorCount    int
	// IsDefault 标记「默认优先使用的账号」（池里最多一个）。
	// 挑号时先试它；它不可用（停用/冷却/超日限额）才按最久未用轮换其它号。
	IsDefault bool
	CreatedAt *time.Time
	UpdatedAt *time.Time
}

// Settings 把账号转成一次发信用的参数（与单一 smtp_* 配置同构）。
func (a *SMTPAccount) Settings() SMTPSettings {
	return SMTPSettings{
		Host:       a.Host,
		Port:       a.Port,
		Username:   a.Username,
		Password:   a.Password,
		Encryption: a.Encryption,
		FromName:   derefString(a.FromName),
	}
}

// usable 判断这一刻能否挑出来发信（见 smtp_pool.go 的 selectSMTPAccount）：
//   - active 直接可用；
//   - cooling 在 cooldown_until 到期后恢复可用（发信成功后由 SMTPAccountUsed 落回 active）；
//   - disabled 永不可用；
//   - daily_limit>0 且今日已发达标时不可用（sent_today 跨天口径见 smtpSentToday）。
func (a *SMTPAccount) usable(now time.Time) bool {
	switch a.Status {
	case smtpAccountStatusActive:
	case smtpAccountStatusCooling:
		if a.CooldownUntil == nil || a.CooldownUntil.After(now) {
			return false
		}
	default:
		return false
	}
	if a.DailyLimit > 0 && smtpSentToday(a.SentToday, a.ResetAt, now) >= a.DailyLimit {
		return false
	}
	return true
}

// SMTPAccountInput 是新增/修改账号的入参（nil = 不修改；新增时按 default 落默认值）。
type SMTPAccountInput struct {
	Host       *string
	Port       *int
	Username   *string
	Password   *string // 明文；修改时留空 = 保持原密码
	Encryption *string
	FromName   *string
	Weight     *int
	DailyLimit *int
}

// SMTPAccountStore 是账号池对数据库的全部依赖（由 MailSQLStore 实现）。
type SMTPAccountStore interface {
	// SMTPAccountCandidates 返回所有「可能可用」的账号（active + 冷却已到期的 cooling），
	// 按 last_used_at 最久未用优先（NULL 最先）排序。日限额/冷却的最终判定由
	// selectSMTPAccount 在内存里做（口径见 smtp_pool.go）。
	SMTPAccountCandidates(ctx context.Context) ([]SMTPAccount, error)
	// SMTPAccountUsed 发信成功：sent_today+1（跨天先按 reset_at 清零）、last_used_at=now、
	// error_count 归零、冷却/冷却态恢复为 active。
	SMTPAccountUsed(ctx context.Context, id uint64, now time.Time) error
	// SMTPAccountFailed 发信失败：凭据错误 → disabled；限流/临时 → cooling + cooldown_until。
	// error_count 两条分支都 +1。
	SMTPAccountFailed(ctx context.Context, id uint64, kind smtpFailure, now time.Time) error

	// 管理端
	SMTPAccounts(ctx context.Context) ([]SMTPAccount, error)
	SMTPAccountByID(ctx context.Context, id uint64) (*SMTPAccount, error)
	CreateSMTPAccount(ctx context.Context, in SMTPAccountInput, now time.Time) (*SMTPAccount, error)
	UpdateSMTPAccount(ctx context.Context, id uint64, in SMTPAccountInput, now time.Time) (*SMTPAccount, error)
	DeleteSMTPAccount(ctx context.Context, id uint64) error
	SetSMTPAccountStatus(ctx context.Context, id uint64, status string, now time.Time) error
	// ResetSMTPAccountToday 手动把今日已发与连续失败清零（不动状态/冷却）：
	// sent_today 到 daily_limit 后不想等到跨天时的逃生口。
	ResetSMTPAccountToday(ctx context.Context, id uint64, now time.Time) error
	// SetSMTPAccountDefault 设置/清除「默认优先使用」的账号（id=0 表示清除默认）。
	// 池里默认账号唯一：实现里先把其它行 is_default 清零再置目标行。
	SetSMTPAccountDefault(ctx context.Context, id uint64, now time.Time) error
}

// smtpFailure 是一次失败的分类（决定「换号 / 冷却 / 直接失败」）。
type smtpFailure int

const (
	smtpFailureOther     smtpFailure = iota // 未识别：只记日志，不换号
	smtpFailureAuth                         // 535/501/Invalid credentials → 该号 disabled，换下一个
	smtpFailureTemp                         // 421/450/451/连接超时 → 该号 cooling，换下一个
	smtpFailureRecipient                    // 550/551/553 → 收件人永久拒收，不换号
)

// String 便于日志与测试断言。
func (f smtpFailure) String() string {
	switch f {
	case smtpFailureAuth:
		return "auth"
	case smtpFailureTemp:
		return "temp"
	case smtpFailureRecipient:
		return "recipient"
	}
	return "other"
}

// rotatable 表示失败后是否值得换号再试。
func (f smtpFailure) rotatable() bool {
	return f == smtpFailureAuth || f == smtpFailureTemp
}

// classifySMTPFailure 按错误文本判定失败类别。
//
// 判定顺序刻意是「先收件人、再认证、再临时、最后连接超时」：
//   - 550/551/553 只会出现在 RCPT 阶段，与发信账号无关，换号没用；
//   - 535/501/Invalid credentials 是账号问题；
//   - 421/450/451 与 connect timeout/i-o timeout 是临时问题。
//
// 无法识别的一律 smtpFailureOther（保持旧行为：记 failed，不换号）。
func classifySMTPFailure(errText string) smtpFailure {
	s := strings.ToLower(errText)
	for _, code := range []string{"550", "551", "553"} {
		if strings.Contains(s, code) {
			return smtpFailureRecipient
		}
	}
	for _, code := range []string{"535", "501"} {
		if strings.Contains(s, code) {
			return smtpFailureAuth
		}
	}
	if strings.Contains(s, "invalid credentials") || strings.Contains(s, "authentication failed") ||
		strings.Contains(s, "auth failed") {
		return smtpFailureAuth
	}
	for _, code := range []string{"421", "450", "451"} {
		if strings.Contains(s, code) {
			return smtpFailureTemp
		}
	}
	for _, word := range []string{"timeout", "timed out", "connection reset", "connection refused"} {
		if strings.Contains(s, word) {
			return smtpFailureTemp
		}
	}
	return smtpFailureOther
}

// smtpDayStart 返回 t 所在 UTC 日期的零点（reset_at 口径：跨天清零）。
func smtpDayStart(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// smtpSentToday 取「今天已发」计数：reset_at 不是今天（或为空）即视为 0。
func smtpSentToday(sentToday int, resetAt *time.Time, now time.Time) int {
	if resetAt == nil {
		return 0
	}
	if !smtpDayStart(*resetAt).Equal(smtpDayStart(now)) {
		return 0
	}
	return sentToday
}
