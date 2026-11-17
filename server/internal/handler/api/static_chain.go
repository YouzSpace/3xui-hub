package api

// 静态 IP 套餐（plans.exit_type='static_chain'）支付成功后的开通链路（#静态IP-02）。
//
// 分工：
//   - 本文件：读 exit_profiles 模板 → 先分配前置节点 → 再开/续 webshare 子账号 → 收敛多余绑定；
//   - admin 包：真正打上游开号与落 proxy_accounts（OpenProfileAccount / RenewProfileAccount）；
//   - scheduler 包：在分类的前置节点里按「已绑 / capacity」选一台（AssignProfileNode）。
//
// 三条口径（下发方拍板）：
//  1. static_chain 用户**不跑**地区分配：入口必须是分类里的前置节点，按地区再分配会给他绑直连节点；
//  2. static_socks 与本任务无关，按 direct 走（见 afterPaid 的注释）；
//  3. exit_type 为空/未知 → 按 direct 处理（安全降级），不打断支付。
//
// 失败只返回错误给 afterPaid，由它记日志：钱已经收了，订单必须保持 paid。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"controlhub-server/internal/handler/admin"
)

// StaticChainProfile 是开通要用到的出口分类模板（exit_profiles 一行）。
type StaticChainProfile struct {
	ID             uint64
	ProviderID     uint64
	TrafficGB      int64
	DurationDays   int64
	MaxThreadCount int64
	Enabled        bool
}

// StaticChainAccountStore 是 webshare 子账号的开/续能力（生产实现：*admin.Handler）。
//
// 拆成窄接口而不是直接依赖 *admin.Handler：支付域的单测可以注入假实现，
// 不需要连上游也不需要构造完整的 admin Handler。
type StaticChainAccountStore interface {
	FindProfileAccount(ctx context.Context, userID uint64, spec admin.ExitProfileSpec) (*admin.Account, error)
	OpenProfileAccount(ctx context.Context, userID, orderID uint64, spec admin.ExitProfileSpec, now time.Time) (*admin.Account, error)
	RenewProfileAccount(ctx context.Context, acc *admin.Account, spec admin.ExitProfileSpec, now time.Time) (*admin.Account, error)
}

// StaticChainNodeBinder 是前置节点分配能力（生产实现：*scheduler.Scheduler）。
type StaticChainNodeBinder interface {
	AssignProfileNode(ctx context.Context, userID uint64, profileID uint64) (int64, error)
	PruneNonProfileNodes(ctx context.Context, userID uint64, profileID uint64) (int64, error)
}

// StaticChainProvisioner 是「买了 static_chain 套餐 → 开号 + 绑前置节点」的接缝。
//
// nil 表示未接线（纯 PHP 对齐期 / 单测环境）：afterPaid 记一条日志后跳过，
// 订单照常完成 —— 与 AssignUserNodes 的容错口径一致。
type StaticChainProvisioner interface {
	Provision(ctx context.Context, userID, orderID, profileID uint64) error
}

// staticChainProvisioner 是 StaticChainProvisioner 的真实实现。
type staticChainProvisioner struct {
	db       *sql.DB
	accounts StaticChainAccountStore
	nodes    StaticChainNodeBinder
	now      func() time.Time
	logger   loggerLike
}

// NewStaticChainProvisioner 组装静态 IP 开通链路。
//
// db 用于读 exit_profiles（模板值只在这里用一次，不必进 PaymentStore 接口）；
// accounts 为空时整体跳过（返回 nil，afterPaid 不会拿到半吊子实现）。
func NewStaticChainProvisioner(
	db *sql.DB, accounts StaticChainAccountStore, nodes StaticChainNodeBinder,
	now func() time.Time, logger loggerLike,
) StaticChainProvisioner {
	if db == nil || accounts == nil || nodes == nil {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &staticChainProvisioner{db: db, accounts: accounts, nodes: nodes, now: now, logger: logger}
}

// Provision 执行一次静态 IP 套餐开通。
//
// 顺序是刻意排的：
//  1. 读分类模板（分类不存在/停用 → 失败，不碰上游）；
//  2. 先分配前置节点 —— 没有可用节点时直接失败，不会白开一个上游子账号；
//  3. 已有账号 → 续费（只改额度/到期），没有 → 开号；
//  4. 开号/续费成功后，清掉该用户不属于该分类的绑定的旧节点。
func (p *staticChainProvisioner) Provision(ctx context.Context, userID, orderID, profileID uint64) error {
	profile, err := p.loadProfile(ctx, profileID)
	if err != nil {
		return err
	}
	if profile == nil {
		return fmt.Errorf("出口分类不存在（id=%d）", profileID)
	}
	if !profile.Enabled {
		return fmt.Errorf("出口分类已停用（id=%d）", profileID)
	}

	spec := admin.ExitProfileSpec{
		ID:             profile.ID,
		ProviderID:     profile.ProviderID,
		TrafficGB:      profile.TrafficGB,
		DurationDays:   profile.DurationDays,
		MaxThreadCount: profile.MaxThreadCount,
	}
	now := p.now().UTC().Truncate(time.Second)

	// 先绑节点：无可用节点时直接失败，订单保持 paid，等管理员补节点后再重试，
	// 此时还没有任何上游子账号被占用。
	if _, err := p.nodes.AssignProfileNode(ctx, userID, profileID); err != nil {
		return fmt.Errorf("分配前置节点失败 profile_id=%d: %w", profileID, err)
	}

	existing, err := p.accounts.FindProfileAccount(ctx, userID, spec)
	if err != nil {
		return fmt.Errorf("查询已有子账号失败 profile_id=%d: %w", profileID, err)
	}
	if existing != nil {
		if _, err := p.accounts.RenewProfileAccount(ctx, existing, spec, now); err != nil {
			return fmt.Errorf("续费子账号失败 account_id=%d: %w", existing.ID, err)
		}
	} else {
		if _, err := p.accounts.OpenProfileAccount(ctx, userID, orderID, spec, now); err != nil {
			if errors.Is(err, admin.ErrExitProfileAccountExists) {
				// 并发支付同一订单时另一条回调已经开过号：不是错误，续费语义等价。
				p.logf("静态 IP 开号并发命中已存在账号 user=%d profile=%d（视为已完成）", userID, profileID)
			} else {
				return fmt.Errorf("开 webshare 子账号失败 profile_id=%d: %w", profileID, err)
			}
		}
	}

	// 节点已绑好、账号也有了：清掉不属于该分类的旧绑定（从 direct 套餐换过来的用户
	// 会带着地区节点绑定，不清掉订阅里仍会下发直连节点）。
	if pruned, perr := p.nodes.PruneNonProfileNodes(ctx, userID, profileID); perr != nil {
		p.logf("静态 IP 清理非分类绑定失败 user=%d profile=%d（可事后重试）: %v", userID, profileID, perr)
	} else if pruned > 0 {
		p.logf("静态 IP 解除 %d 条非分类节点绑定 user=%d profile=%d", pruned, userID, profileID)
	}
	return nil
}

// loadProfile 读 exit_profiles 一行；不存在返回 (nil, nil)（由调用方给文案）。
func (p *staticChainProvisioner) loadProfile(ctx context.Context, profileID uint64) (*StaticChainProfile, error) {
	var (
		pr      StaticChainProfile
		enabled int
	)
	err := p.db.QueryRowContext(ctx, `
		SELECT id, provider_id, traffic_gb, duration_days, max_thread_count, enabled
		FROM exit_profiles WHERE id = ?`, profileID).
		Scan(&pr.ID, &pr.ProviderID, &pr.TrafficGB, &pr.DurationDays, &pr.MaxThreadCount, &enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取出口分类失败 profile_id=%d: %w", profileID, err)
	}
	pr.Enabled = enabled != 0
	return &pr, nil
}

func (p *staticChainProvisioner) logf(format string, args ...any) {
	if p != nil && p.logger != nil {
		p.logger.Printf(format, args...)
	}
}
