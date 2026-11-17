package metrics

// 本文件覆盖容量推荐的三档口径（#地区功能-23）：absolute / percent / default。
//
// 为什么单独一个文件：metrics_test.go 是 #地区功能-11 的验收用例（只校验 Suggest/PerUserKB），
// 这里补的是「Basis 判定 + 三种 Reason 文案」。两边的库名分开（ch_region11_* / ch_region23_*），
// 并行跑 go test 不会互相 DROP。
//
// 数据口径（这是本任务要修的核心）：
//   - absolute：mem_total > 100 且 mem_used > 0，单位真是 MB（驱动暴露字节数后才有）；
//   - percent：只有百分比 —— 新采集器写 mem_used=实测百分比 / mem_total=0，
//     旧采集器（升级前）写 mem_used=实测百分比 / mem_total=100，两者都按百分比口径算；
//   - default：样本不足 / 用户数跨度太小 / 内存没随用户数增长。

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// basisSchema 是本文件专用的库名（带进程号，避免并行 go test 互相覆盖）。
var basisSchema = fmt.Sprintf("ch_region23_basis_%d", os.Getpid())

// openBasisTestDB 建一个干净的同构库；连不上 MySQL 就 skip（要验的是真实 SQL 行为，不能打桩）。
func openBasisTestDB(t *testing.T) *sql.DB {
	t.Helper()

	admin, err := sql.Open("mysql", testDSN(""))
	if err != nil {
		t.Skipf("打不开 MySQL 连接（%v），跳过集成测试", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		t.Skipf("连不上 MySQL（%v），跳过集成测试", err)
	}
	mustExec(t, admin, "DROP DATABASE IF EXISTS "+basisSchema)
	mustExec(t, admin, "CREATE DATABASE "+basisSchema+" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
	t.Cleanup(func() {
		_, _ = admin.Exec("DROP DATABASE IF EXISTS " + basisSchema)
		admin.Close()
	})

	db, err := sql.Open("mysql", testDSN(basisSchema))
	if err != nil {
		t.Fatalf("连测试库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	mustExec(t, db, testSchemaDDL)
	return db
}

// logRec 统一打印实际结果，交付时贴的就是这行。
func logRec(t *testing.T, rec Recommendation) {
	t.Helper()
	t.Logf("实际结果：basis=%s perUserKB=%.4f confidence=%s samples=%d suggest=%d reason=%s",
		rec.Basis, rec.PerUserKB, rec.Confidence, rec.Samples, rec.Suggest, rec.Reason)
}

// TestRecommendBasisAbsolute 样本带真实 MB：24 条、用户 10 万→20 万、内存 800→1150MB（总量 2048）。
// 期望 Basis=absolute、每用户 ≈3.58 KB、Suggest ≈34 万。
func TestRecommendBasisAbsolute(t *testing.T) {
	db := openBasisTestDB(t)
	store := NewStore(db)
	const nodeID = 101

	base := time.Now().UTC().Add(-6 * 24 * time.Hour)
	for i := 0; i < 24; i++ {
		users := 100000 + 100000*i/23
		memUsed := 800 + 350*i/23
		insertSample(t, db, nodeID, base.Add(time.Duration(i)*6*time.Hour), users, memUsed, 2048)
	}

	rec, err := store.Recommend(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("Recommend 失败: %v", err)
	}
	logRec(t, rec)

	if rec.Basis != BasisAbsolute {
		t.Fatalf("带真实 MB 的样本应判为 absolute，实际 %q", rec.Basis)
	}
	if rec.Samples != 24 {
		t.Fatalf("样本数应为 24，实际 %d", rec.Samples)
	}
	if math.Abs(rec.PerUserKB-3.5) > 0.3 {
		t.Fatalf("每用户内存应 ≈3.5 KB，实际 %.4f KB", rec.PerUserKB)
	}
	if rec.Suggest < 300000 || rec.Suggest > 360000 {
		t.Fatalf("建议容量应在 30-36 万之间，实际 %d", rec.Suggest)
	}
	if !strings.Contains(rec.Reason, "每用户约") || !strings.Contains(rec.Reason, "KB") {
		t.Fatalf("absolute 的 Reason 应含「每用户约 … KB」，实际 %q", rec.Reason)
	}
	if !strings.Contains(rec.Reason, "2048") || !strings.Contains(rec.Reason, "80%") {
		t.Fatalf("absolute 的 Reason 应带内存总量与 80%% 上限，实际 %q", rec.Reason)
	}
}

// TestRecommendBasisPercent 3x-ui 节点：只有百分比（mem_total=0），20 条样本、
// 用户 10 万→20 万、占用 39%→56%。按百分比斜率推算 Suggest ≈34 万，PerUserKB 必须是 0。
func TestRecommendBasisPercent(t *testing.T) {
	db := openBasisTestDB(t)
	store := NewStore(db)
	const nodeID = 102

	base := time.Now().UTC().Add(-5 * 24 * time.Hour)
	for i := 0; i < 20; i++ {
		users := 100000 + 100000*i/19
		pct := 39 + 17*i/19 // 39% → 56%
		insertSample(t, db, nodeID, base.Add(time.Duration(i)*6*time.Hour), users, pct, 0)
	}

	rec, err := store.Recommend(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("Recommend 失败: %v", err)
	}
	logRec(t, rec)

	if rec.Basis != BasisPercent {
		t.Fatalf("只有百分比的样本应判为 percent，实际 %q", rec.Basis)
	}
	if rec.PerUserKB != 0 {
		t.Fatalf("percent 口径下 perUserKB 应为 0（不适用），实际 %.4f", rec.PerUserKB)
	}
	if rec.Suggest < 300000 || rec.Suggest > 400000 {
		t.Fatalf("建议容量应在 30-40 万之间，实际 %d", rec.Suggest)
	}
	if rec.Confidence != ConfidenceHigh {
		t.Fatalf("20 条样本、跨度 100%% 时置信度应为 high，实际 %s", rec.Confidence)
	}
	if !strings.Contains(rec.Reason, "每万用户约涨") {
		t.Fatalf("percent 的 Reason 应含「每万用户约涨」，实际 %q", rec.Reason)
	}
	if !strings.Contains(rec.Reason, "上限推算") {
		t.Fatalf("percent 的 Reason 应含「上限推算」，实际 %q", rec.Reason)
	}
}

// TestRecommendBasisPercentLegacyRows 升级前的旧数据是「百分比按总量 100 归一化写入」
// （mem_used=39→56, mem_total=100），必须同样识别成 percent —— 否则升级当天窗口里的
// 老数据会被当成 MB 去算，又回到 0.17 KB/用户 那种假数。
func TestRecommendBasisPercentLegacyRows(t *testing.T) {
	db := openBasisTestDB(t)
	store := NewStore(db)
	const nodeID = 103

	base := time.Now().UTC().Add(-5 * 24 * time.Hour)
	for i := 0; i < 20; i++ {
		users := 100000 + 100000*i/19
		pct := 39 + 17*i/19
		insertSample(t, db, nodeID, base.Add(time.Duration(i)*6*time.Hour), users, pct, 100)
	}

	rec, err := store.Recommend(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("Recommend 失败: %v", err)
	}
	logRec(t, rec)

	if rec.Basis != BasisPercent {
		t.Fatalf("mem_total=100 的旧数据应判为 percent，实际 %q", rec.Basis)
	}
	if rec.Suggest < 300000 || rec.Suggest > 400000 {
		t.Fatalf("建议容量应在 30-40 万之间，实际 %d", rec.Suggest)
	}
}

// TestRecommendBasisDefault 只有 3 条样本：Basis=default、PerUserKB=5.0、Reason 说明数据不足。
func TestRecommendBasisDefault(t *testing.T) {
	db := openBasisTestDB(t)
	store := NewStore(db)
	const nodeID = 104

	base := time.Now().UTC().Add(-3 * time.Hour)
	for i := 0; i < 3; i++ {
		insertSample(t, db, nodeID, base.Add(time.Duration(i)*time.Hour), 50000+i*10000, 700+i*50, 2048)
	}

	rec, err := store.Recommend(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("Recommend 失败: %v", err)
	}
	logRec(t, rec)

	if rec.Basis != BasisDefault {
		t.Fatalf("样本只有 3 条时应判为 default，实际 %q", rec.Basis)
	}
	if rec.PerUserKB != DefaultPerUserKB {
		t.Fatalf("默认口径下 perUserKB 应为 %.1f，实际 %.4f", DefaultPerUserKB, rec.PerUserKB)
	}
	if !strings.Contains(rec.Reason, "数据不足") {
		t.Fatalf("default 的 Reason 应含「数据不足」，实际 %q", rec.Reason)
	}
	if !strings.Contains(rec.Reason, "5.0 KB") {
		t.Fatalf("default 的 Reason 应含默认值「5.0 KB」，实际 %q", rec.Reason)
	}
	// 有绝对值时仍按「总内存 80% / 5KB」估：2048*80%-800 = 838.4MB → 70000 + 838.4*1024/5 ≈ 24.2 万
	if rec.Suggest < 200000 || rec.Suggest > 280000 {
		t.Fatalf("default 口径下建议容量应按总内存 80%% 估算（20-28 万），实际 %d", rec.Suggest)
	}
}

// TestRecommendBasisPercentNoTrend 百分比节点但内存没随用户数涨（斜率 0）：退回 default，
// 但百分比本身是实测值，按当前占用比例线性外推到 80% 上限，不能退化成「建议容量 = 当前用户数」。
func TestRecommendBasisPercentNoTrend(t *testing.T) {
	db := openBasisTestDB(t)
	store := NewStore(db)
	const nodeID = 105

	base := time.Now().UTC().Add(-4 * 24 * time.Hour)
	for i := 0; i < 15; i++ {
		users := 100000 + 100000*i/14
		insertSample(t, db, nodeID, base.Add(time.Duration(i)*6*time.Hour), users, 60, 0)
	}

	rec, err := store.Recommend(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("Recommend 失败: %v", err)
	}
	logRec(t, rec)

	if rec.Basis != BasisDefault {
		t.Fatalf("百分比没随用户数增长时应判为 default，实际 %q", rec.Basis)
	}
	// 200000 人占 60% → 80% 约 26.7 万人
	if rec.Suggest < 250000 || rec.Suggest > 280000 {
		t.Fatalf("应按当前占用比例外推到 80%%（25-28 万），实际 %d", rec.Suggest)
	}
	if !strings.Contains(rec.Reason, "数据不足") {
		t.Fatalf("Reason 应含「数据不足」，实际 %q", rec.Reason)
	}
	if rec.Suggest <= rec.Samples*15000 {
		t.Fatalf("不建议退化成「建议容量=当前用户数」，实际 %d", rec.Suggest)
	}
}
