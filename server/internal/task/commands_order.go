package task

// 订单过期（order:expire-pending）命令体。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"fmt"
	"time"

	"controlhub-server/internal/driver/payment"
)

// runOrderExpirePending 作废「支付窗口已过」的订单。
//
// ⚠️ 过期时间**不由这个任务决定**：判定是 orders.pay_expires_at（下单时按全局支付有效期
// 算好的绝对时刻，site_configs.payment_ttl_minutes），用户端倒计时与 /api/payment/retry
// 都按它实时判，到点即过期、不等任务。本任务只做**兜底落库**：把没人访问、状态还停在
// pending 的废单扫成 expired、清掉缓存链接，保证库与管理端不长期漂移。
// 因此它可以被停掉而不影响用户侧的过期表现（stop 后只是库里 pending 会多留一会儿）。
func (r *commandRunner) runOrderExpirePending(ctx context.Context, args map[string]string) error {
	dryRun := args["--dry-run"] == "1" || args["--dry-run"] == "true"

	// 全局支付有效期（分钟）：老单没有 pay_expires_at，用 created_at + 它兜底。
	ttlMinutes := r.siteConfigInt(ctx, payment.TTLMinutesKey, payment.DefaultTTLMinutes)
	if ttlMinutes < 1 {
		ttlMinutes = payment.DefaultTTLMinutes
	}
	now := r.now().UTC()
	legacyCutoff := now.Add(-time.Duration(ttlMinutes) * time.Minute)

	// 兜底清扫：把所有「非待支付」订单上残留的缓存支付链接清掉。
	// 正常路径在订单变 expired/paid 时已经清过（UpdateOrderStatus / MarkOrderPaid / 充值到账）；
	// 这里兜住历史遗留、以及「本次上线前就已过期/支付」老单上从来没有过的 pay_url。
	// 幂等、且只动非 pending 行（只看 status，不会误伤索引）。
	if !dryRun {
		if _, err := r.deps.DB.ExecContext(ctx,
			"UPDATE orders SET pay_url = NULL WHERE status <> 'pending' AND pay_url IS NOT NULL"); err != nil {
			r.logf("清扫非待支付订单的缓存支付链接失败: %v", err)
		}
	}

	// 查询支付窗口已过的 pending 订单：优先用 pay_expires_at（新单），老单按 created_at + TTL 兜底。
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, order_no, user_id, amount, created_at
		 FROM orders WHERE status = 'pending'
		   AND (
		     (pay_expires_at IS NOT NULL AND pay_expires_at <= ?)
		     OR (pay_expires_at IS NULL AND created_at < ?)
		   )
		 ORDER BY id LIMIT 10000`, now, legacyCutoff)
	if err != nil {
		return fmt.Errorf("查询超时订单失败: %w", err)
	}
	type orderRow struct {
		ID        int64
		OrderNo   string
		UserID    int64
		Amount    float64
		CreatedAt time.Time
	}
	var orders []orderRow
	for rows.Next() {
		var o orderRow
		if err := rows.Scan(&o.ID, &o.OrderNo, &o.UserID, &o.Amount, &o.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		orders = append(orders, o)
	}
	rows.Close()

	if len(orders) == 0 {
		r.logf("没有超时未支付订单，无需处理。")
		return nil
	}

	expired := 0
	for _, o := range orders {
		if dryRun {
			r.logf("[DRY-RUN] #%d %s（用户 %d，%.2f 元，创建于 %s）",
				o.ID, o.OrderNo, o.UserID, o.Amount, o.CreatedAt.Format("2006-01-02 15:04:05"))
			expired++
			continue
		}

		// 更新状态（同时清空缓存的支付链接：过期订单不再保留续付链接）
		_, err := r.deps.DB.ExecContext(ctx,
			"UPDATE orders SET status = 'expired', pay_url = NULL, updated_at = ? WHERE id = ? AND status = 'pending'",
			now, o.ID)
		if err != nil {
			r.logf("作废订单 #%d 失败: %v", o.ID, err)
			continue
		}
		expired++
	}

	r.logf("共 %d 笔超时未支付订单%s。", expired, map[bool]string{true: "（dry-run 未写库）", false: " 已作废"}[dryRun])
	return nil
}

// ── 6. mail:notify-scan ────────────────────────────────────────────
