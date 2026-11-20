package api

import (
	"context"
	"fmt"
	"strconv"

	"controlhub-server/internal/config"
	"controlhub-server/internal/crypto"
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/smtpmail"
)

// SMTPMailer 是 SimpleMailerService 的 Go 复刻：
//
//   - 每次发信都从 site_configs 现读 SMTP 配置（后台改完立即生效，不缓存）；
//   - 发件地址 = smtp_username（两者必须一致，与 PHP 的类注释一致）；
//   - smtp_password 是 Crypt::encryptString 密文，解密失败按空密码继续（只记日志）；
//   - smtp_encryption：'none' → 明文；'ssl' → 隐式 TLS（465）；'tls' → STARTTLS。
//
// ⭐ 验证码发信统一走 SMTP 账号池：
//
//	注册验证码 / 找回密码验证码与「后台批量发信」共用同一个账号池（smtp_accounts），
//	挑号口径完全一致：最久未用优先、认证失败换号并停用、限流冷却 10 分钟、单封最多试 3 个号。
//	池里**没有任何可用号**时才回落到站点 smtp_* 单套配置 —— 这是账号池既有的兜底契约，
//	保证「配了池但池全挂了」不会把验证码彻底发不出去。
//
//	为什么必须统一：验证码是「新用户注册 / 找回密码」的唯一入口，单账号额度一耗尽
//	就整站注册瘫痪（实测 SMTP2GO 报 552 monthly allowance exhausted 即为此）。
//
// 与 PHP 的差异：PHP 走 Laravel Mail + Symfony Mailer（有连接复用与更全的 MIME 能力），
// 这里只发「纯 HTML、单收件人」的验证码邮件 —— 足够覆盖 P2 的两个发码端点。
type SMTPMailer struct {
	store  Store
	appKey []byte
	logger interface{ Printf(string, ...any) }
	// dial 供测试注入假连接（默认 net.Dialer）。
	dial smtpmail.DialFunc
	// pool 是账号池发信接缝；nil 表示尚未注入（此时等价于改造前：只用站点 smtp_*）。
	// 由 SetPoolMailer 在路由装配时注入，避免 NewSMTPMailer 签名变化影响现有调用方/测试。
	pool PoolMailer
}

// PoolMailer 是账号池发信接缝（由 admin.MailDomain 实现）。
//
// used=false 表示池里没有可用号，调用方应回落站点 smtp_* 配置。
type PoolMailer interface {
	// SendViaPool 从池里挑号轮换发信（最久未用优先 / 失败换号 / 冷却）。
	// used=false → 池里没有可用号，调用方回落站点配置。
	SendViaPool(ctx context.Context, to, subject, htmlBody string) (used bool, err error)
	// SendViaAccount 用**指定的**池账号发信（管理端「验证码用哪个号发」下拉选中具体号时用）。
	// 不做轮换（用户已经明确指定），失败即返回错误。
	SendViaAccount(ctx context.Context, accountID uint64, to, subject, htmlBody string) error
	// PoolAccountUsable 判断指定账号此刻是否可用（不存在/停用/冷却/超日限额都算不可用）。
	PoolAccountUsable(ctx context.Context, accountID uint64) bool
	// HasUsableAccount 报告池里此刻有没有可用账号 —— 作为「邮件服务是否可用」的判据
	// （替代过去只看站点 smtp_host 的旧口径）。
	HasUsableAccount(ctx context.Context) bool
}

// 验证码发信账号来源（site_configs.verify_mail_sender）：
//
//	pool  → 走账号池（默认）：池里挑号轮换，池空回落站点 smtp_*；
//	<数字> → 用账号池里 id 等于该数字的那一个号发；
//	空/其它 → 等同 pool（向后兼容，没配过也能用）。
//
// 设置入口：管理端「邮箱配置 → 验证码发送」页（GET/PUT 随 email 配置一起读写）。
const (
	cfgVerifyMailSender = "verify_mail_sender"
	verifySenderPool    = "pool"
)

// NewSMTPMailer 构造 SMTP 发信器。cfg 用于取 APP_KEY（解密 smtp_password）。
func NewSMTPMailer(store Store, cfg *config.Config, logger interface{ Printf(string, ...any) }) *SMTPMailer {
	m := &SMTPMailer{store: store, logger: logger}
	if cfg != nil {
		m.appKey = cfg.AppKey
	}
	return m
}

// SetPoolMailer 注入账号池发信实现（路由装配时调用一次）。
func (m *SMTPMailer) SetPoolMailer(p PoolMailer) { m.pool = p }

// senderChoice 读站点配置，返回 (是否走池, 指定账号 id)。
//
//	id>0  → 用该号发（不走轮换）；
//	id==0 且 usePool → 走池轮换；
//	id==0 且 !usePool → 只用站点 smtp_*（历史行为；当前默认不会是这条）。
func (m *SMTPMailer) senderChoice(ctx context.Context) (usePool bool, accountID uint64) {
	if m.pool == nil {
		return false, 0
	}
	values, err := m.store.SiteConfigMany(ctx, []string{cfgVerifyMailSender})
	if err != nil {
		if m.logger != nil {
			m.logger.Printf("读取 verify_mail_sender 失败，按默认走账号池: %v", err)
		}
		return true, 0
	}
	raw := phpTrimSpaceOnly(values[cfgVerifyMailSender])
	if raw == "" || raw == verifySenderPool {
		return true, 0
	}
	if id, convErr := strconv.ParseUint(raw, 10, 64); convErr == nil && id > 0 {
		// 指定的号此刻不可用（停用/冷却/超限额/已删）→ 退回走池，
		// 不让「管理员选了一个坏号」把整站验证码卡死。
		if m.pool.PoolAccountUsable(ctx, id) {
			return true, id
		}
		if m.logger != nil {
			m.logger.Printf("验证码发信：指定的 SMTP 账号 %d 当前不可用，退回账号池挑号", id)
		}
		return true, 0
	}
	return true, 0
}

// smtpSettings 是一次发信用的配置快照。
type smtpSettings struct {
	Host       string
	Port       int
	Username   string
	Password   string
	Encryption string // "ssl" | "tls" | "none" | ""
	FromName   string
}

// load 读取并解密 SMTP 配置。
func (m *SMTPMailer) load(ctx context.Context) (smtpSettings, error) {
	values, err := m.store.SiteConfigMany(ctx, smtpConfigKeys)
	if err != nil {
		return smtpSettings{}, err
	}

	s := smtpSettings{
		Host:       values["smtp_host"],
		Username:   values["smtp_username"],
		Encryption: values["smtp_encryption"],
		FromName:   values["smtp_from_name"],
	}
	if rawPort := values["smtp_port"]; rawPort != "" {
		p, convErr := strconv.Atoi(rawPort)
		if convErr != nil || p <= 0 {
			p = 587
		}
		s.Port = p
	} else {
		s.Port = 587
	}

	// 解密密码：失败只记日志、按空密码继续（与原实现一致）。
	if enc := values["smtp_password"]; enc != "" {
		plain, decErr := crypto.DecryptString(enc, m.appKey)
		if decErr != nil {
			if m.logger != nil {
				m.logger.Printf("SMTP password decrypt failed: %v", decErr)
			}
		} else {
			s.Password = plain
		}
	}
	return s, nil
}

// Send 发一封 HTML 邮件（单收件人、单主题）。
//
// 顺序：指定的池账号 → 池轮换 → 站点 smtp_* 兜底。
func (m *SMTPMailer) Send(ctx context.Context, to, subject, htmlBody string) error {
	if m.pool != nil {
		usePool, accountID := m.senderChoice(ctx)
		if usePool {
			if accountID > 0 {
				// 管理员指定了具体账号：只用它发，失败即报错（不偷偷换号，
				// 否则管理员无法判断配置是否真的生效）。
				if err := m.pool.SendViaAccount(ctx, accountID, to, subject, htmlBody); err != nil {
					m.logf("验证码发信：指定账号 %d 发送失败: %v", accountID, err)
					return err
				}
				return nil
			}
			used, err := m.pool.SendViaPool(ctx, to, subject, htmlBody)
			if used {
				return err
			}
			m.logf("验证码发信：账号池无可用号，回落站点 SMTP 配置")
		}
	}

	s, err := m.load(ctx)
	if err != nil {
		return fmt.Errorf("读取 SMTP 配置失败: %w", err)
	}
	return smtpmail.Send(ctx, smtpmail.Settings{
		Host:       s.Host,
		Port:       s.Port,
		Username:   s.Username,
		Password:   s.Password,
		Encryption: s.Encryption,
		FromName:   s.FromName,
	}, to, subject, htmlBody, m.dial)
}

// Configured 报告「验证码邮件通道是否可用」。
//
// 判据：账号池里有可用账号 → 可用（验证码统一走池）；
// 池不可用时再看站点 smtp_host 是否有兜底配置。
// 用它替代调用方原来直接读 configs["smtp_host"] == "" 的旧判断 ——
// 否则「池里配了号但站点 smtp_* 空着」会被误判成「SMTP未配置」而拒绝发码。
func (m *SMTPMailer) Configured(ctx context.Context) bool {
	if m.pool != nil && m.pool.HasUsableAccount(ctx) {
		return true
	}
	values, err := m.store.SiteConfigMany(ctx, []string{"smtp_host"})
	if err != nil {
		return false
	}
	return phpTrimSpaceOnly(values["smtp_host"]) != ""
}

func (m *SMTPMailer) logf(format string, args ...any) {
	if m.logger != nil {
		m.logger.Printf(format, args...)
	}
}

// 编译期确认 admin.MailDomain 满足 PoolMailer（防止上游改签名后这里静默失效）。
var _ PoolMailer = (*admin.MailDomain)(nil)
