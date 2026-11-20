package admin

// SMTP 账号池的单测 + 一条真库用例。
//
// 这里钉住的是挑号口径（smtp_pool.go 文件头那段契约）：
//   - 默认账号可用就先试它，试过/不可用才回到「最久未用」；
//   - 单封最多换 smtpMaxRotations 个号，认证/临时失败换号，收件人拒收不换号；
//   - 池里没有可用号 → sent=false（调用方回落站点 smtp_*）。
//
// 真库用例只覆盖「SQL 口径与 Go 口径一致」这件事：日限额到量不选、reset_at 跨天恢复。
// 其余边界（冷却到期、并列排序）全部在注入时钟的纯函数用例里，不依赖数据库时钟。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// ---- 测试替身 ----

// fakeMailStore 只实现挑号链路用到的方法；其余 MailStore 方法不实现（本文件用不到）。
type fakeMailStore struct {
	MailStore // 其余方法留空：一旦被调用会 panic，等于「这条用例不该用到它」

	candidates    []SMTPAccount
	candidateErr  error
	candidateCall int
	used          []uint64
	failed        []fakeFailure
	usedErr       error
	failedErr     error
}

type fakeFailure struct {
	id   uint64
	kind smtpFailure
}

func (s *fakeMailStore) SMTPAccountCandidates(context.Context) ([]SMTPAccount, error) {
	s.candidateCall++
	if s.candidateErr != nil {
		return nil, s.candidateErr
	}
	out := make([]SMTPAccount, len(s.candidates))
	copy(out, s.candidates)
	return out, nil
}

func (s *fakeMailStore) SMTPAccountUsed(_ context.Context, id uint64, _ time.Time) error {
	s.used = append(s.used, id)
	return s.usedErr
}

func (s *fakeMailStore) SMTPAccountFailed(_ context.Context, id uint64, kind smtpFailure, _ time.Time) error {
	s.failed = append(s.failed, fakeFailure{id: id, kind: kind})
	return s.failedErr
}

// scriptedTransport 按 host 决定发送结果（host 就是测试里区分账号的标号）。
type scriptedTransport struct {
	errByHost map[string]error
	sentHosts []string
}

func (t *scriptedTransport) Send(cfg SMTPSettings, _, _, _ string) error {
	t.sentHosts = append(t.sentHosts, cfg.Host)
	if err, ok := t.errByHost[cfg.Host]; ok {
		return err
	}
	return nil
}

// testNow 是固定时钟：挑号只看注入的 now，不读系统时间。
func testNow() time.Time { return time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) }

func ptrTime(t time.Time) *time.Time { return &t }

// ---- 1. selectSMTPAccount：表驱动 ----

func TestSMTPSelectAccount(t *testing.T) {
	now := testNow()
	recent := now.Add(-time.Hour)
	older := now.Add(-2 * time.Hour)

	acc := func(id uint64, mutate func(*SMTPAccount)) SMTPAccount {
		a := SMTPAccount{ID: id, Host: fmt.Sprintf("smtp-%d.example.com", id), Status: smtpAccountStatusActive}
		if mutate != nil {
			mutate(&a)
		}
		return a
	}

	cases := []struct {
		name       string
		candidates []SMTPAccount
		already    []uint64
		wantID     uint64 // 0 = 期望 nil
	}{
		{
			name: "默认账号可用就用它，即使别人更久未用",
			candidates: []SMTPAccount{
				acc(1, func(a *SMTPAccount) { a.LastUsedAt = nil }), // 从未用过，最久
				acc(2, func(a *SMTPAccount) { a.IsDefault = true; a.LastUsedAt = ptrTime(recent) }),
			},
			wantID: 2,
		},
		{
			name: "默认账号本次已试过 → 回退到最久未用",
			candidates: []SMTPAccount{
				acc(1, func(a *SMTPAccount) { a.IsDefault = true }),
				acc(2, func(a *SMTPAccount) { a.LastUsedAt = ptrTime(recent) }),
				acc(3, func(a *SMTPAccount) { a.LastUsedAt = ptrTime(older) }),
			},
			already: []uint64{1},
			wantID:  3,
		},
		{
			name: "默认账号不可用（停用）→ 回退",
			candidates: []SMTPAccount{
				acc(1, func(a *SMTPAccount) { a.IsDefault = true; a.Status = smtpAccountStatusDisabled }),
				acc(2, nil),
			},
			wantID: 2,
		},
		{
			name: "LRU：最久未用优先，从未用过排最前",
			candidates: []SMTPAccount{
				acc(1, func(a *SMTPAccount) { a.LastUsedAt = ptrTime(recent) }),
				acc(2, func(a *SMTPAccount) { a.LastUsedAt = ptrTime(older) }),
				acc(3, nil),
			},
			wantID: 3,
		},
		{
			name: "LRU 并列时 id 小的优先",
			candidates: []SMTPAccount{
				acc(7, func(a *SMTPAccount) { a.LastUsedAt = ptrTime(older) }),
				acc(4, func(a *SMTPAccount) { a.LastUsedAt = ptrTime(older) }),
			},
			wantID: 4,
		},
		{
			name: "跳过冷却未到期的号",
			candidates: []SMTPAccount{
				acc(1, func(a *SMTPAccount) {
					a.Status = smtpAccountStatusCooling
					a.CooldownUntil = ptrTime(now.Add(time.Minute))
				}),
				acc(2, nil),
			},
			wantID: 2,
		},
		{
			name: "冷却已到期可恢复使用",
			candidates: []SMTPAccount{
				acc(1, func(a *SMTPAccount) {
					a.Status = smtpAccountStatusCooling
					a.CooldownUntil = ptrTime(now)
				}),
			},
			wantID: 1,
		},
		{
			name: "today 到日限额的号被跳过",
			candidates: []SMTPAccount{
				acc(1, func(a *SMTPAccount) {
					a.DailyLimit = 2
					a.SentToday = 2
					a.ResetAt = ptrTime(now)
				}),
				acc(2, nil),
			},
			wantID: 2,
		},
		{
			name: "全部不可用 → nil",
			candidates: []SMTPAccount{
				acc(1, func(a *SMTPAccount) { a.Status = smtpAccountStatusDisabled }),
				acc(2, func(a *SMTPAccount) {
					a.DailyLimit = 1
					a.SentToday = 1
					a.ResetAt = ptrTime(now)
				}),
			},
			wantID: 0,
		},
		{
			name:       "空候选 → nil",
			candidates: nil,
			wantID:     0,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			already := map[uint64]struct{}{}
			for _, id := range c.already {
				already[id] = struct{}{}
			}
			got := selectSMTPAccount(c.candidates, already, now)
			switch {
			case c.wantID == 0 && got != nil:
				t.Fatalf("期望 nil，得到 id=%d", got.ID)
			case c.wantID != 0 && got == nil:
				t.Fatalf("期望 id=%d，得到 nil", c.wantID)
			case got != nil && got.ID != c.wantID:
				t.Fatalf("挑到 id=%d，期望 id=%d", got.ID, c.wantID)
			}
		})
	}
}

// ---- 2. trySendViaPool：假 store + 假 transport ----

// newPoolDomain 组装一个只有挑号依赖的 MailDomain。
func newPoolDomain(store MailStore, transport MailTransport) *MailDomain {
	return NewMailDomain(MailDeps{
		Store:     store,
		Transport: transport,
		Now:       testNow,
		Logger:    discardLogger{},
		Queue:     nopMailQueue{},
	})
}

type discardLogger struct{}

func (discardLogger) Printf(string, ...any) {}

type nopMailQueue struct{}

func (nopMailQueue) Enqueue(MailJob) error { return nil }

func TestSMTPTrySendViaPool(t *testing.T) {
	ctx := context.Background()
	authErr := errors.New("535 Authentication failed")
	tempErr := errors.New("451 4.7.1 try again later")
	recipientErr := errors.New("550 5.1.1 mailbox unavailable")

	t.Run("第一个号认证失败 → 换下一个号成功", func(t *testing.T) {
		store := &fakeMailStore{candidates: []SMTPAccount{
			{ID: 1, Host: "bad.example.com", Status: smtpAccountStatusActive},
			{ID: 2, Host: "good.example.com", Status: smtpAccountStatusActive},
		}}
		tr := &scriptedTransport{errByHost: map[string]error{"bad.example.com": authErr}}
		d := newPoolDomain(store, tr)

		sent, err := d.trySendViaPool(ctx, "u@example.com", "s", "<p>x</p>")
		if !sent || err != nil {
			t.Fatalf("sent=%v err=%v，期望 sent=true err=nil", sent, err)
		}
		if len(tr.sentHosts) != 2 || tr.sentHosts[0] != "bad.example.com" || tr.sentHosts[1] != "good.example.com" {
			t.Fatalf("发信顺序不对: %v", tr.sentHosts)
		}
		if len(store.failed) != 1 || store.failed[0].id != 1 || store.failed[0].kind != smtpFailureAuth {
			t.Fatalf("失败记账不对: %+v", store.failed)
		}
		if len(store.used) != 1 || store.used[0] != 2 {
			t.Fatalf("成功记账不对: %v", store.used)
		}
	})

	t.Run("全部号都失败 → 换满 3 个后返回错误", func(t *testing.T) {
		store := &fakeMailStore{candidates: []SMTPAccount{
			{ID: 1, Host: "a.example.com", Status: smtpAccountStatusActive},
			{ID: 2, Host: "b.example.com", Status: smtpAccountStatusActive},
			{ID: 3, Host: "c.example.com", Status: smtpAccountStatusActive},
		}}
		tr := &scriptedTransport{errByHost: map[string]error{
			"a.example.com": tempErr, "b.example.com": tempErr, "c.example.com": tempErr,
		}}
		d := newPoolDomain(store, tr)

		sent, err := d.trySendViaPool(ctx, "u@example.com", "s", "<p>x</p>")
		if !sent || err == nil {
			t.Fatalf("sent=%v err=%v，期望 sent=true 且 err!=nil", sent, err)
		}
		if !strings.Contains(err.Error(), "3") {
			t.Fatalf("错误信息应带上尝试次数: %v", err)
		}
		if len(tr.sentHosts) != smtpMaxRotations {
			t.Fatalf("应尝试 %d 个号，实际 %d 个: %v", smtpMaxRotations, len(tr.sentHosts), tr.sentHosts)
		}
		if len(store.failed) != smtpMaxRotations {
			t.Fatalf("应记 %d 次失败，实际 %+v", smtpMaxRotations, store.failed)
		}
	})

	t.Run("收件人永久拒收 → 不换号，立即返回该错误", func(t *testing.T) {
		store := &fakeMailStore{candidates: []SMTPAccount{
			{ID: 1, Host: "a.example.com", Status: smtpAccountStatusActive},
			{ID: 2, Host: "b.example.com", Status: smtpAccountStatusActive},
		}}
		tr := &scriptedTransport{errByHost: map[string]error{"a.example.com": recipientErr}}
		d := newPoolDomain(store, tr)

		sent, err := d.trySendViaPool(ctx, "u@example.com", "s", "<p>x</p>")
		if !sent || !errors.Is(err, recipientErr) {
			t.Fatalf("sent=%v err=%v，期望 sent=true 且原样返回拒收错误", sent, err)
		}
		if len(tr.sentHosts) != 1 {
			t.Fatalf("拒收不该换号，实际发信 %v", tr.sentHosts)
		}
		if len(store.failed) != 0 {
			t.Fatalf("不换号的分支不该记账号失败: %+v", store.failed)
		}
	})

	t.Run("池里没有可用号 → sent=false 且无错误", func(t *testing.T) {
		store := &fakeMailStore{}
		tr := &scriptedTransport{}
		d := newPoolDomain(store, tr)

		sent, err := d.trySendViaPool(ctx, "u@example.com", "s", "<p>x</p>")
		if sent || err != nil {
			t.Fatalf("sent=%v err=%v，期望 sent=false err=nil（调用方回落站点配置）", sent, err)
		}
		if len(tr.sentHosts) != 0 {
			t.Fatalf("无可用号时不该发信: %v", tr.sentHosts)
		}
	})

	t.Run("挑号查询失败 → 透传错误", func(t *testing.T) {
		boom := errors.New("sql down")
		store := &fakeMailStore{candidateErr: boom}
		d := newPoolDomain(store, &scriptedTransport{})

		sent, err := d.trySendViaPool(ctx, "u@example.com", "s", "<p>x</p>")
		if sent || !errors.Is(err, boom) {
			t.Fatalf("sent=%v err=%v，期望 sent=false 且透传挑号错误", sent, err)
		}
	})
}

// ---- 3. 真库：日限额 / 跨天恢复 ----
//
// 只在 TEST_DSN 可连时跑（默认 127.0.0.1:3307，root 空密码）；连不上就 Skip，
// 与仓库里其它集成测试口径一致。

const smtpPoolDDL = "CREATE TABLE smtp_accounts (\n" +
	"  id bigint unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  host varchar(255) NOT NULL,\n" +
	"  port int NOT NULL DEFAULT 587,\n" +
	"  username varchar(255) NOT NULL DEFAULT '',\n" +
	"  password_enc text,\n" +
	"  encryption varchar(8) NOT NULL DEFAULT 'tls',\n" +
	"  from_name varchar(100) DEFAULT NULL,\n" +
	"  weight int NOT NULL DEFAULT 1,\n" +
	"  daily_limit int NOT NULL DEFAULT 0,\n" +
	"  sent_today int NOT NULL DEFAULT 0,\n" +
	"  last_used_at timestamp NULL DEFAULT NULL,\n" +
	"  reset_at timestamp NULL DEFAULT NULL,\n" +
	"  status varchar(16) NOT NULL DEFAULT 'active',\n" +
	"  cooldown_until timestamp NULL DEFAULT NULL,\n" +
	"  error_count int NOT NULL DEFAULT 0,\n" +
	"  is_default tinyint(1) NOT NULL DEFAULT 0,\n" +
	"  created_at timestamp NULL DEFAULT NULL,\n" +
	"  updated_at timestamp NULL DEFAULT NULL,\n" +
	"  PRIMARY KEY (id),\n" +
	"  KEY smtp_accounts_status_index (status)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci"

// smtpPoolDSN 返回管理连接 DSN；TEST_DSN 覆盖，本机默认 3307（任务书口径）。
func smtpPoolDSN() string {
	if dsn := os.Getenv("TEST_DSN"); dsn != "" {
		return dsn
	}
	return "root:@tcp(127.0.0.1:3307)/?timeout=3s"
}

// smtpPoolDSNWithSchema 把 DSN 的默认库换成 schema，并确保带 parseTime（时间列才能扫进 time.Time）。
func smtpPoolDSNWithSchema(dsn, schema string) string {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return dsn
	}
	head := dsn[:slash+1]
	tail := ""
	if q := strings.Index(dsn[slash:], "?"); q >= 0 {
		tail = dsn[slash+q:]
	}
	if !strings.Contains(tail, "parseTime") {
		if tail == "" {
			tail = "?"
		} else {
			tail += "&"
		}
		tail += "parseTime=true&loc=UTC"
	}
	return head + schema + tail
}

// newSMTPPoolDB 建一个干净的测试库（只用 smtp_accounts 这一张表），跑完自动 DROP。
func newSMTPPoolDB(t *testing.T) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", smtpPoolDSN())
	if err != nil {
		t.Skipf("打不开 MySQL（%v），跳过 SMTP 账号池集成测试", err)
	}
	if err := admin.Ping(); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过 SMTP 账号池集成测试", err)
	}
	t.Cleanup(func() { admin.Close() })

	schema := fmt.Sprintf("ch_smtp_pool_%d", os.Getpid())
	if _, err := admin.Exec("DROP DATABASE IF EXISTS `" + schema + "`"); err != nil {
		t.Fatalf("清理旧测试库失败: %v", err)
	}
	if _, err := admin.Exec("CREATE DATABASE `" + schema + "` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		t.Fatalf("建测试库失败: %v", err)
	}
	t.Cleanup(func() {
		again, err := sql.Open("mysql", smtpPoolDSN())
		if err != nil {
			return
		}
		defer again.Close()
		_, _ = again.Exec("DROP DATABASE IF EXISTS `" + schema + "`")
	})

	db, err := sql.Open("mysql", smtpPoolDSNWithSchema(smtpPoolDSN(), schema))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })

	if _, err := db.Exec(smtpPoolDDL); err != nil {
		t.Fatalf("建表失败: %v\nSQL: %s", err, smtpPoolDDL)
	}
	return db
}

// TestSMTPPoolStoreQuotaAndCrossDay 用真库验「SQL 取候选 + Go 判可用」的口径一致：
//   - reset_at 是今天、sent_today 已到 daily_limit → 不选；
//   - reset_at 是昨天（跨天）→ 即便 sent_today 也是限值，今天视为 0，可选。
func TestSMTPPoolStoreQuotaAndCrossDay(t *testing.T) {
	db := newSMTPPoolDB(t)
	store := NewMailSQLStore(db)
	ctx := context.Background()

	now := testNow()
	yesterday := now.AddDate(0, 0, -1)
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("准备数据失败: %v\nSQL: %s", err, query)
		}
	}

	// 1 号：今天已到限额；2 号：reset_at 还停在昨天（跨天恢复）；3 号：没有限额兜底。
	mustExec(`INSERT INTO smtp_accounts (id, host, daily_limit, sent_today, reset_at, last_used_at, status, created_at, updated_at)
		VALUES (1, 'used-up.example.com', 1, 1, ?, ?, 'active', ?, ?)`, now, now, now, now)
	mustExec(`INSERT INTO smtp_accounts (id, host, daily_limit, sent_today, reset_at, last_used_at, status, created_at, updated_at)
		VALUES (2, 'new-day.example.com', 1, 1, ?, ?, 'active', ?, ?)`, yesterday, now.Add(-time.Hour), now, now)
	mustExec(`INSERT INTO smtp_accounts (id, host, daily_limit, sent_today, reset_at, last_used_at, status, created_at, updated_at)
		VALUES (3, 'unlimited.example.com', 0, 0, NULL, NULL, 'disabled', ?, ?)`, now, now)

	candidates, err := store.SMTPAccountCandidates(ctx)
	if err != nil {
		t.Fatalf("取候选失败: %v", err)
	}
	// disabled 的 3 号不该出现在候选里（状态过滤在 SQL）。
	if len(candidates) != 2 {
		t.Fatalf("候选应只剩 2 个 active 号，实际 %d 个: %+v", len(candidates), candidates)
	}

	got := selectSMTPAccount(candidates, map[uint64]struct{}{}, now)
	if got == nil {
		t.Fatal("期望挑到跨天恢复的 2 号，得到 nil")
	}
	if got.ID != 2 {
		t.Fatalf("期望挑到跨天恢复的 2 号，实际 id=%d（host=%s）", got.ID, got.Host)
	}

	// 发信成功后 SMTPAccountUsed 要把 sent_today 记在本日窗口：今日已发从 1 → 2（reset_at 改成今天）。
	if err := store.SMTPAccountUsed(ctx, 2, now); err != nil {
		t.Fatalf("记发信成功失败: %v", err)
	}
	var sentToday, limit int
	var resetAt time.Time
	if err := db.QueryRow("SELECT sent_today, daily_limit, reset_at FROM smtp_accounts WHERE id = 2").
		Scan(&sentToday, &limit, &resetAt); err != nil {
		t.Fatalf("回查 2 号失败: %v", err)
	}
	if sentToday != 1 {
		t.Fatalf("跨天后的第一次成功应把 sent_today 记成 1（旧计数清零），实际 %d", sentToday)
	}
	// 实现把 reset_at 记成 now；跨天口径按「日期相同」判，所以断言落在今天这个窗口。
	if !smtpDayStart(resetAt).Equal(smtpDayStart(now)) {
		t.Fatalf("reset_at 应落到今天的计数窗口，实际 %v", resetAt.UTC())
	}

	// 此刻再挑：2 号已到限额（1/1），1 号仍到限额 → 无可选号。
	candidates, err = store.SMTPAccountCandidates(ctx)
	if err != nil {
		t.Fatalf("再次取候选失败: %v", err)
	}
	if again := selectSMTPAccount(candidates, map[uint64]struct{}{}, now); again != nil {
		t.Fatalf("两个号都到日限额时应返回 nil，实际挑到 id=%d", again.ID)
	}
}
