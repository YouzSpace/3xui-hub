package task

// std02_reset_ban_test.go 是规范化-02 场景 5/6 的**真库真代码**验证。
//
// 这两条逻辑的真实实现不在本包（流量重置 = collector.MonthlyReset，
// 到期/超量处理 = collector.ScanBans），本包的调度项只是接线。
// 但产品铁律落在这两段 SQL 上，接线对了、SQL 写歪了照样伤用户，所以这里
// 直接构造真实 collector（不 mock DB）跑一遍，断言「库里的行」而不是「函数被调过」：
//
//	场景 5：只清 monthly_traffic_used，next_traffic_reset_at += 30 天，
//	        **不动**套餐时长 / 额度 / 到期时间（用户自购重置同理）。
//	场景 6：到期用户只关流量，enabled 永远保持 true（产品铁律：永不封禁）。

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"controlhub-server/internal/collector"
)

// newStd02Collector 用同一个测试库构造真实采集器（AppKey 给 32 字节零值 --
// 本用例只走不需要解密节点凭据的分支）。
func newStd02Collector(t *testing.T, db *sql.DB, now time.Time) *collector.Collector {
	t.Helper()
	c, err := collector.New(collector.Options{
		DB:     db,
		AppKey: make([]byte, 32),
		Now:    func() time.Time { return now },
		Logf:   func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("构造 collector 失败: %v", err)
	}
	return c
}

// TestStd02MonthlyResetOnlyClearsUsedTraffic：周期套餐到期重置只清已用流量。
func TestStd02MonthlyResetOnlyClearsUsedTraffic(t *testing.T) {
	db := newStd02TaskDB(t, "reset")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	c := newStd02Collector(t, db, now)

	// 套餐：周期 1 个月、月流量 100G。
	mustExecTask(t, db, `INSERT INTO plans (id, name, price, reset_price, type, months, monthly_traffic, period_traffic, total_traffic, is_active, include_local, created_at, updated_at)
		VALUES (1,'周期100G',50.00,5.00,'period',1,107374182400,0,0,1,1,NOW(),NOW())`)
	mustExecTask(t, db, `INSERT INTO plans (id, name, price, reset_price, type, months, monthly_traffic, period_traffic, total_traffic, is_active, include_local, created_at, updated_at)
		VALUES (2,'总量500G',100.00,0.00,'total',0,0,536870912000,536870912000,1,1,NOW(),NOW())`)

	oldNext := now.AddDate(0, 0, -2) // 已到期该重置
	expired := now.AddDate(0, 0, 30)
	mustExecTask(t, db, `INSERT INTO users (id, email, token, uuid, protocol, plan_id, traffic_limit, traffic_used,
		monthly_traffic_used, monthly_traffic_limit, next_traffic_reset_at, expired_at, enabled, created_at, updated_at)
		VALUES (1,'u1@example.com','t1','11111111-1111-1111-1111-111111111111','vless',1,107374182400,53687091200,
		53687091200,107374182400,?,?,1,NOW(),NOW())`, oldNext, expired)
	// 总量套餐（不该被重置）。
	mustExecTask(t, db, `INSERT INTO users (id, email, token, uuid, protocol, plan_id, traffic_limit, traffic_used,
		monthly_traffic_used, monthly_traffic_limit, next_traffic_reset_at, expired_at, enabled, created_at, updated_at)
		VALUES (2,'u2@example.com','t2','22222222-2222-2222-2222-222222222222','vless',2,536870912000,1073741824,
		1073741824,0,?,?,1,NOW(),NOW())`, oldNext, expired)
	// 已过期用户（不该被重置 —— 到期用户由封禁扫描处理，续费后才有下一周期）。
	mustExecTask(t, db, `INSERT INTO users (id, email, token, uuid, protocol, plan_id, traffic_limit, traffic_used,
		monthly_traffic_used, monthly_traffic_limit, next_traffic_reset_at, expired_at, enabled, created_at, updated_at)
		VALUES (3,'u3@example.com','t3','33333333-3333-3333-3333-333333333333','vless',1,107374182400,1024,
		1024,107374182400,?,?,1,NOW(),NOW())`, oldNext, now.AddDate(0, 0, -1))

	res, err := c.MonthlyReset(ctx)
	if err != nil {
		t.Fatalf("MonthlyReset 失败: %v", err)
	}
	if res.Reset != 1 {
		t.Fatalf("重置用户数 = %d，期望只重置用户 1（周期套餐且未过期）", res.Reset)
	}

	var monthlyUsed, trafficUsed, trafficLimit, monthlyLimit int64
	var planID sql.NullInt64
	var nextReset, expiredAt sql.NullTime
	var enabled int
	if err := db.QueryRow(`SELECT monthly_traffic_used, traffic_used, traffic_limit, monthly_traffic_limit,
		plan_id, next_traffic_reset_at, expired_at, enabled FROM users WHERE id = 1`).
		Scan(&monthlyUsed, &trafficUsed, &trafficLimit, &monthlyLimit, &planID, &nextReset, &expiredAt, &enabled); err != nil {
		t.Fatal(err)
	}
	if monthlyUsed != 0 {
		t.Errorf("月已用流量 = %d，必须清零", monthlyUsed)
	}
	// 铁律：只清「已用」，其余一律不动。
	if trafficUsed != 53687091200 {
		t.Errorf("累计已用 traffic_used = %d，被重置误改（只该清 monthly_traffic_used）", trafficUsed)
	}
	if trafficLimit != 107374182400 {
		t.Errorf("流量额度被改了: %d", trafficLimit)
	}
	if monthlyLimit != 107374182400 {
		t.Errorf("月额度被改了: %d", monthlyLimit)
	}
	if !planID.Valid || planID.Int64 != 1 {
		t.Errorf("套餐被改了: %v", planID)
	}
	if !expiredAt.Valid || !expiredAt.Time.Equal(expired) {
		t.Errorf("套餐到期时间被改了: %v（重置不动套餐时长）", expiredAt)
	}
	if enabled != 1 {
		t.Errorf("enabled = %d，重置不许碰账号开关", enabled)
	}
	// next_traffic_reset_at 必须是「原值 + 30 天」（不是 now + 30 天）。
	if !nextReset.Valid {
		t.Fatal("next_traffic_reset_at 被清空了")
	}
	if want := oldNext.AddDate(0, 0, 30); !nextReset.Time.Equal(want) {
		t.Errorf("next_traffic_reset_at = %v，期望原值 +30 天 = %v", nextReset.Time, want)
	}

	// 总量套餐与已过期用户：一行都不能动。
	var used2, used3 int64
	if err := db.QueryRow("SELECT monthly_traffic_used FROM users WHERE id = 2").Scan(&used2); err != nil {
		t.Fatal(err)
	}
	if used2 != 1073741824 {
		t.Errorf("总量套餐用户的月已用被清了（%d），它没有周期概念", used2)
	}
	if err := db.QueryRow("SELECT monthly_traffic_used FROM users WHERE id = 3").Scan(&used3); err != nil {
		t.Fatal(err)
	}
	if used3 != 1024 {
		t.Errorf("已过期用户的流量被重置了（%d）", used3)
	}

	// 幂等：再跑一次不会把日期 +60 天。
	if _, err := c.MonthlyReset(ctx); err != nil {
		t.Fatalf("第二次 MonthlyReset 失败: %v", err)
	}
	if err := db.QueryRow("SELECT next_traffic_reset_at FROM users WHERE id = 1").Scan(&nextReset); err != nil {
		t.Fatal(err)
	}
	if want := oldNext.AddDate(0, 0, 30); !nextReset.Time.Equal(want) {
		t.Errorf("重复执行后 next_traffic_reset_at = %v，期望仍为 %v（并发/补跑必须幂等）", nextReset.Time, want)
	}
}

// TestStd02BanScanNeverDisablesAccount：到期/超量用户只被关流量，账号与套餐原样保留。
//
// 产品铁律（用户定稿）：**永不封禁账号**。到期只影响「流量能不能用」，
// users.enabled 与 users.plan_id 不能被定时任务改写。
func TestStd02BanScanNeverDisablesAccount(t *testing.T) {
	db := newStd02TaskDB(t, "ban")
	ctx := context.Background()
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	c := newStd02Collector(t, db, now)

	mustExecTask(t, db, `INSERT INTO plans (id, name, price, reset_price, type, months, monthly_traffic, period_traffic, total_traffic, is_active, include_local, created_at, updated_at)
		VALUES (1,'周期10G',10.00,0.00,'period',1,10737418240,0,0,1,1,NOW(),NOW())`)

	// 1 号：已过期。2 号：当月超量。3 号：完全正常。
	mustExecTask(t, db, `INSERT INTO users (id, email, token, uuid, protocol, plan_id, traffic_limit, traffic_used,
		monthly_traffic_used, monthly_traffic_limit, next_traffic_reset_at, expired_at, enabled, created_at, updated_at)
		VALUES (1,'u1@example.com','t1','11111111-1111-1111-1111-111111111111','vless',1,0,0,
		0,10737418240,?,?,1,NOW(),NOW())`, now.AddDate(0, 0, 5), now.AddDate(0, 0, -1))
	mustExecTask(t, db, `INSERT INTO users (id, email, token, uuid, protocol, plan_id, traffic_limit, traffic_used,
		monthly_traffic_used, monthly_traffic_limit, next_traffic_reset_at, expired_at, enabled, created_at, updated_at)
		VALUES (2,'u2@example.com','t2','22222222-2222-2222-2222-222222222222','vless',1,0,0,
		10737418240,10737418240,?,?,1,NOW(),NOW())`, now.AddDate(0, 0, 5), now.AddDate(0, 0, 20))
	mustExecTask(t, db, `INSERT INTO users (id, email, token, uuid, protocol, plan_id, traffic_limit, traffic_used,
		monthly_traffic_used, monthly_traffic_limit, next_traffic_reset_at, expired_at, enabled, created_at, updated_at)
		VALUES (3,'u3@example.com','t3','33333333-3333-3333-3333-333333333333','vless',1,0,0,
		1024,10737418240,?,?,1,NOW(),NOW())`, now.AddDate(0, 0, 5), now.AddDate(0, 0, 20))

	res, err := c.ScanBans(ctx)
	if err != nil {
		t.Fatalf("ScanBans 失败: %v", err)
	}
	_ = res

	// 铁律断言：三个用户的 enabled 全部保持 1，plan_id 全部保持 1。
	for _, id := range []int64{1, 2, 3} {
		var enabled int
		var planID sql.NullInt64
		if err := db.QueryRow("SELECT enabled, plan_id FROM users WHERE id = ?", id).Scan(&enabled, &planID); err != nil {
			t.Fatal(err)
		}
		if enabled != 1 {
			t.Errorf("用户 %d 的 enabled 被定时任务改成了 %d —— 违反「永不封禁」铁律", id, enabled)
		}
		if !planID.Valid || planID.Int64 != 1 {
			t.Errorf("用户 %d 的套餐被定时任务清掉了（plan_id=%v）—— 到期不等于退套餐", id, planID)
		}
	}

	// 本机没有可用节点时，即使判定该关流量也不会写标记（「全部节点确认才写」的正确口径）。
	var disabled int
	if err := db.QueryRow("SELECT COUNT(*) FROM users WHERE traffic_disabled_at IS NOT NULL").Scan(&disabled); err != nil {
		t.Fatal(err)
	}
	if disabled != 0 {
		t.Errorf("无节点可操作时不该写 traffic_disabled_at，实际 %d 个用户被标记", disabled)
	}
	// 扫描范围必须覆盖三个启用用户（否则「没封禁」可能只是因为没扫到人）。
	if res.Scanned != 3 {
		t.Errorf("扫描用户数 = %d，期望 3（enabled=1 的全部用户）", res.Scanned)
	}
	// Disabled 是「判定该关流量的人数」：到期 + 当月超量 = 2 人命中，健康用户不命中。
	if res.Disabled != 2 {
		t.Errorf("判定需关流量的人数 = %d，期望 2（到期 1 人 + 超量 1 人）", res.Disabled)
	}
	if res.Errors != 0 {
		t.Errorf("单用户失败 %d 条，无节点时不该有错误", res.Errors)
	}
}
