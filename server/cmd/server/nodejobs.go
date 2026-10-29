package main

// 节点类异步 Job 的执行体，对齐 backend/app/Jobs 下的四个 PHP Job：
//
//	NodeInboundSyncJob     入站变更后把已有用户挂到新入站（item = user:{id}）
//	NodeInboundScanJob     入站同步的扫描入口（一台节点一个 Job，不占 item）
//	NodeInitUserJob        节点接入时按用户初始化 client（item = user:{id}）
//	NodeClientsCleanupJob  删节点后清理远端托管 client（item = node:{id}）
//
// 为什么放在 cmd/server 而不是 internal/task：这四个 Job 都要「按 nodeID 构造 3x-ui
// 客户端」，而节点在初始化阶段 enabled=0（collector.NodeDrivers 只返回 enabled 节点），
// 所以和 provisioner.go 一样直接读 nodes 行 + 解密凭据最省事，internal/task 也继续保持
// 对驱动层零依赖（只认小接口，见 jobs.go 的 TrafficSyncer）。
//
// 失败口径与 PHP 的差异（有意为之）：
//   - PHP 靠 Laravel 的 tries/backoff 在队列层重试，并在 failed() 钩子里把整批 item 标失败；
//     Go 的队列没有 failed 钩子，批量 Job 的 ItemKey 为空，failItem 会静默 return ——
//     重试一旦用尽，任务就会永远停在 running。
//   - 因此这里每个 item 自己落终态：单用户失败立刻 FailItem（文案带原因），handler 返回 nil
//     不触发队列重放。日志页看得到「哪个人失败、为什么」，管理员点重试会重新走扫描
//     （task.Service.Retry 对 sync_inbounds 派发 NodeInboundScanJob，与 PHP 同路）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/driver/threexui"
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/task"
)

// registerNodeJobs 把四个节点类 Job 挂进队列。
func registerNodeJobs(q *task.Queue, db *sql.DB, appKey []byte, svc *task.Service, logger *log.Logger) {
	q.Register(task.JobNodeInboundSync, func(ctx context.Context, job task.JobPayload) error {
		return runNodeInboundSync(ctx, db, appKey, svc, logger, job)
	})
	q.Register(task.JobNodeInboundScan, func(ctx context.Context, job task.JobPayload) error {
		return runNodeInboundScan(ctx, db, appKey, svc, logger, job)
	})
	q.Register(task.JobNodeInitUser, func(ctx context.Context, job task.JobPayload) error {
		return runNodeInitUser(ctx, db, appKey, svc, logger, job)
	})
	q.Register(task.JobNodeClientsCleanup, func(ctx context.Context, job task.JobPayload) error {
		return runNodeClientsCleanup(ctx, db, appKey, svc, logger, job)
	})
}

// ── 取数小工具 ───────────────────────────────────────────────

// jobNode 是 Job 用到的节点行（不看 enabled：初始化阶段节点本来就还没启用）。
type jobNode struct {
	ID          int64
	Name        string
	Host        string
	Port        int64
	Scheme      string
	WebBasePath string
	Username    string
	Password    sql.NullString
	APIKey      sql.NullString
	VerifySSL   bool
	DriverType  string
	Enabled     bool
}

// loadJobNode 读节点行（不存在返回 (nil, nil)）。
func loadJobNode(ctx context.Context, db *sql.DB, nodeID int64) (*jobNode, error) {
	var n jobNode
	err := db.QueryRowContext(ctx,
		`SELECT id, name, host, port, scheme, web_base_path, username, password, api_key,
		        verify_ssl, driver_type, enabled FROM nodes WHERE id = ?`, nodeID).
		Scan(&n.ID, &n.Name, &n.Host, &n.Port, &n.Scheme, &n.WebBasePath, &n.Username,
			&n.Password, &n.APIKey, &n.VerifySSL, &n.DriverType, &n.Enabled)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// threeXUiFor 按节点行构造 3x-ui 客户端（凭据走正常解密路径）。
func threeXUiFor(n *jobNode, appKey []byte) *threexui.Client {
	cfg := threexui.Config{
		Scheme:      n.Scheme,
		Host:        n.Host,
		Port:        int(n.Port),
		WebBasePath: n.WebBasePath,
		Username:    n.Username,
		VerifySSL:   n.VerifySSL,
	}
	if n.Password.Valid {
		cfg.Password = decryptOrRaw(n.Password.String, appKey)
	}
	if n.APIKey.Valid {
		cfg.APIKey = decryptOrRaw(n.APIKey.String, appKey)
	}
	return threexui.New(cfg)
}

// isXrayNode 判定裸核节点（这些 Job 只处理面板式 3x-ui 节点）。
func isXrayNode(n *jobNode) bool {
	return strings.EqualFold(strings.TrimSpace(n.DriverType), "xray")
}

// jobUser 是 Job 用到的用户行。
//
// ⚠️ 这里的 Email 是**用户登录用的邮箱**，只用于日志与通知；
// **绝不能当 3x-ui 的 client 主键**（那是 clientEmailOf(ID) 的活）。见 clientEmailOf。
type jobUser struct {
	ID                 int64
	Email              string
	Protocol           string
	PlanID             sql.NullInt64
	Enabled            bool
	TrafficLimit       int64
	TrafficUsed        int64
	MonthlyTrafficUsed int64
	MonthlyLimit       int64
	ExpiredAt          sql.NullTime
}

// clientEmailOf 是 3x-ui client 主键的唯一口径（`ch_user_{id}`）。
//
// 全仓同口径：collector.clientEmail / xray.ClientEmail / threexui.ClientEmail /
// UserRow.ClientEmail / provisioner 建号 / 订阅生成 / 流量归属 全都用它。
// 历史 bug：node-init 曾用 users.email 当主键建号（有真实邮箱就不回落），
// 结果这批 client 与其余链路对不上 —— 流量永远 0、对账当孤儿跳过、清理也删不掉。
func clientEmailOf(userID int64) string { return "ch_user_" + strconv.FormatInt(userID, 10) }

// loadJobUser 读用户行（不存在返回 (nil, nil)）。
func loadJobUser(ctx context.Context, db *sql.DB, userID int64) (*jobUser, error) {
	var u jobUser
	err := db.QueryRowContext(ctx,
		`SELECT id, email, protocol, plan_id, enabled, traffic_limit, traffic_used,
		        monthly_traffic_used, monthly_traffic_limit, expired_at
		 FROM users WHERE id = ?`, userID).
		Scan(&u.ID, &u.Email, &u.Protocol, &u.PlanID, &u.Enabled, &u.TrafficLimit,
			&u.TrafficUsed, &u.MonthlyTrafficUsed, &u.MonthlyLimit, &u.ExpiredAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// usableNow 判定「套餐现在还能用」：有套餐 + 未过期 + 总量未超 + 当月未超。
//
// 判据与封禁扫描器 collector.banReasonAt **逐条一致**（那边是私有的，无法直接复用）；
// 三处口径必须同源，改一处要同步另一处。不看用户的登录邮箱，也不看 enabled ——
// enabled 只决定 client 建出来是开还是关（建号时仍按 u.Enabled 下发）。
func (u *jobUser) usableNow(now time.Time) bool {
	if u == nil || !u.PlanID.Valid {
		return false
	}
	if u.ExpiredAt.Valid && u.ExpiredAt.Time.Before(now) {
		return false
	}
	if u.MonthlyLimit > 0 && u.MonthlyTrafficUsed >= u.MonthlyLimit {
		return false
	}
	if u.TrafficLimit > 0 && u.TrafficUsed >= u.TrafficLimit {
		return false
	}
	return true
}

// inboundIDsForNode 取该节点上协议匹配的入站 id（对齐 Node::inboundIdsFor）。
func inboundIDsForNode(ctx context.Context, db *sql.DB, nodeID int64, protocol string) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT inbound_id FROM node_inbounds WHERE node_id = ? AND protocol = ?", nodeID, protocol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// itemKeyAt 取与第 i 个用户对应的 item_key（batch 形态里两者一一对应）。
func itemKeyAt(job task.JobPayload, i int) string {
	if i >= 0 && i < len(job.ItemKeys) {
		return job.ItemKeys[i]
	}
	return ""
}

// completeJobItem 标成功（无任务上下文时什么也不做）。
func completeJobItem(ctx context.Context, svc *task.Service, taskID int64, itemKey string) {
	if svc == nil || taskID == 0 || itemKey == "" {
		return
	}
	if _, err := svc.CompleteItem(ctx, taskID, itemKey); err != nil {
		log.Printf("[nodejob] 标记成功项出错 task=%d item=%s: %v", taskID, itemKey, err)
	}
}

// failJobItem 标失败（无任务上下文时什么也不做）。
func failJobItem(ctx context.Context, svc *task.Service, taskID int64, itemKey, reason string) {
	if svc == nil || taskID == 0 || itemKey == "" {
		return
	}
	if _, err := svc.FailItem(ctx, taskID, itemKey, reason); err != nil {
		log.Printf("[nodejob] 标记失败项出错 task=%d item=%s: %v", taskID, itemKey, err)
	}
}

// claimJobItem 认领子项：已终态/已认领返回 false（幂等重放要跳过）。
func claimJobItem(ctx context.Context, svc *task.Service, taskID int64, itemKey string) bool {
	if svc == nil || taskID == 0 || itemKey == "" {
		return true
	}
	ok, err := svc.ClaimItem(ctx, taskID, itemKey)
	if err != nil {
		log.Printf("[nodejob] 认领子项失败 task=%d item=%s: %v", taskID, itemKey, err)
		return false
	}
	return ok
}

// ── NodeInboundSyncJob ─────────────────────────────────────

// runNodeInboundSync 把一批用户挂到该节点协议下的全部入站。
//
// 前置条件（PHP 同）：该 email 在面板上已存在 —— 扫描阶段用一次清单确认过；走重试时会先
// 重跑扫描。AttachClient 本身幂等（同一批 inboundIds 重复挂结果不变），重放无副作用。
func runNodeInboundSync(ctx context.Context, db *sql.DB, appKey []byte, svc *task.Service,
	logger *log.Logger, job task.JobPayload) error {

	node, err := loadJobNode(ctx, db, job.NodeID)
	if err != nil {
		return fmt.Errorf("读节点 %d: %w", job.NodeID, err)
	}
	if node == nil {
		// 节点已删：每项无事可做，判成功（与 PHP complete 同义）。
		for i, uid := range job.UserIDs {
			key := itemKeyAt(job, i)
			if !claimJobItem(ctx, svc, job.TaskID, key) {
				continue
			}
			logger.Printf("[inbound-sync] 节点已删除，跳过 user=%d", uid)
			completeJobItem(ctx, svc, job.TaskID, key)
		}
		return nil
	}
	if isXrayNode(node) {
		// 裸核节点没有面板式「挂入站」动作（客户端由 agent 按配置渲染），无事可做。
		for i := range job.UserIDs {
			key := itemKeyAt(job, i)
			if !claimJobItem(ctx, svc, job.TaskID, key) {
				continue
			}
			completeJobItem(ctx, svc, job.TaskID, key)
		}
		return nil
	}

	client := threeXUiFor(node, appKey)

	for i, uid := range job.UserIDs {
		key := itemKeyAt(job, i)
		if !claimJobItem(ctx, svc, job.TaskID, key) {
			continue
		}

		u, err := loadJobUser(ctx, db, uid)
		if err != nil {
			failJobItem(ctx, svc, job.TaskID, key, "读取用户失败："+err.Error())
			continue
		}
		// 用户已删 / 已失去套餐：无事可做（面板上本就不该有他的 client）。
		if u == nil || !u.PlanID.Valid {
			completeJobItem(ctx, svc, job.TaskID, key)
			continue
		}

		ids, err := inboundIDsForNode(ctx, db, node.ID, u.Protocol)
		if err != nil {
			failJobItem(ctx, svc, job.TaskID, key, "读取节点入站失败："+err.Error())
			continue
		}
		if len(ids) == 0 {
			completeJobItem(ctx, svc, job.TaskID, key)
			continue
		}

		email := clientEmailOf(u.ID)
		if err := client.AttachClient(ctx, email, ids); err != nil {
			failJobItem(ctx, svc, job.TaskID, key,
				fmt.Sprintf("挂载入站失败（user=%d email=%s inbounds=%v）：%v", uid, email, ids, err))
			continue
		}
		completeJobItem(ctx, svc, job.TaskID, key)
	}
	return nil
}

// ── NodeInboundScanJob ─────────────────────────────────────

// runNodeInboundScan 拉一次节点客户端清单，与任务里待处理的 item 做 diff：
// 已经挂好的直接判完成，只有缺挂载的才交给执行 Job（100 用户省掉 100 次往返）。
func runNodeInboundScan(ctx context.Context, db *sql.DB, appKey []byte, svc *task.Service,
	logger *log.Logger, job task.JobPayload) error {

	if job.TaskID == 0 || svc == nil {
		return nil
	}
	t, err := svc.Get(ctx, job.TaskID)
	if err != nil {
		return fmt.Errorf("读任务 %d: %w", job.TaskID, err)
	}
	if t == nil || t.Status == task.StatusSucceeded || t.Status == task.StatusFailed {
		return nil // 已判终态（超时兜底 / 上一次已完成），不再扫描
	}

	node, err := loadJobNode(ctx, db, job.NodeID)
	if err != nil {
		return fmt.Errorf("读节点 %d: %w", job.NodeID, err)
	}
	if node == nil {
		_, err := svc.FailPendingItems(ctx, job.TaskID, "节点已删除，入站同步中止")
		return err
	}
	if isXrayNode(node) {
		// 裸核节点没有面板清单可拉：直接按「无需挂载」把待处理项判完成。
		pending, err := svc.PendingItemKeys(ctx, job.TaskID)
		if err != nil {
			return err
		}
		for _, key := range pending {
			completeJobItem(ctx, svc, job.TaskID, key)
		}
		return nil
	}

	client := threeXUiFor(node, appKey)
	snap, snapErr := client.Snapshot(ctx)
	if snapErr != nil {
		// 清单拉不到（不可达 / 鉴权失败 / 响应非法）：把待处理项全部标失败并写明原因。
		// 不靠队列重试 —— 3 次重试窗口大概率等不到节点恢复，反而会让任务挂在 running 上
		// 先被 async-task-timeout 判失败（与 PHP scanNode 同口径）。
		_, err := svc.FailPendingItems(ctx, job.TaskID,
			"拉取节点客户端清单失败，未能同步入站："+snapErr.Error())
		return err
	}

	pending, err := svc.PendingItemKeys(ctx, job.TaskID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	var needsWork []string
	for _, key := range pending {
		uid, ok := parseJobUserKey(key)
		if !ok {
			completeJobItem(ctx, svc, job.TaskID, key)
			continue
		}
		u, err := loadJobUser(ctx, db, uid)
		if err != nil {
			failJobItem(ctx, svc, job.TaskID, key, "读取用户失败："+err.Error())
			continue
		}
		if u == nil || !u.PlanID.Valid {
			completeJobItem(ctx, svc, job.TaskID, key)
			continue
		}
		ids, err := inboundIDsForNode(ctx, db, node.ID, u.Protocol)
		if err != nil {
			failJobItem(ctx, svc, job.TaskID, key, "读取节点入站失败："+err.Error())
			continue
		}
		if len(ids) == 0 {
			completeJobItem(ctx, svc, job.TaskID, key)
			continue
		}

		st, exists := snap.Clients[clientEmailOf(u.ID)]
		switch {
		case !exists && snap.Complete:
			// 面板上根本没有这个 client：老语义就是跳过（入站同步不负责建号）。
			completeJobItem(ctx, svc, job.TaskID, key)
		case !exists:
			// 清单不完整（退化探测路径）→ 无法判定，宁可多发一次幂等 attach。
			needsWork = append(needsWork, key)
		case len(st.Inbounds) == 0:
			// 面板没给挂载信息 → 一律当「没挂全」交给执行 Job。
			needsWork = append(needsWork, key)
		case subsetOf(ids, st.Inbounds):
			completeJobItem(ctx, svc, job.TaskID, key)
		default:
			needsWork = append(needsWork, key)
		}
	}

	logger.Printf("[inbound-scan] node=%s 待处理 %d，需要挂载 %d", node.Name, len(pending), len(needsWork))
	if len(needsWork) > 0 {
		// 只把需要动手的 item 派出去执行（任务已由上面的 completeJobItem 判过部分终态）。
		fresh, err := svc.Get(ctx, job.TaskID)
		if err != nil {
			return err
		}
		return svc.Dispatch(ctx, fresh, needsWork)
	}
	return nil
}

// parseJobUserKey 解析 item_key = "user:{id}"。
func parseJobUserKey(key string) (int64, bool) {
	trimmed := strings.TrimSpace(key)
	if !strings.HasPrefix(trimmed, "user:") {
		return 0, false
	}
	id, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(trimmed, "user:")), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// subsetOf 判定 want 是否全部包含在 have 里（两边都是入站 id）。
func subsetOf(want, have []int64) bool {
	if len(want) == 0 {
		return true
	}
	set := make(map[int64]bool, len(have))
	for _, id := range have {
		set[id] = true
	}
	for _, id := range want {
		if !set[id] {
			return false
		}
	}
	return true
}

// ── NodeInitUserJob ────────────────────────────────────────

// runNodeInitUser 节点接入时按用户初始化 client：
// 幂等清理（已有残留先删）→ 建号 → 批末定节点终态（全部成功才启用）。
func runNodeInitUser(ctx context.Context, db *sql.DB, appKey []byte, svc *task.Service,
	logger *log.Logger, job task.JobPayload) error {

	node, err := loadJobNode(ctx, db, job.NodeID)
	if err != nil {
		return fmt.Errorf("读节点 %d: %w", job.NodeID, err)
	}

	defer func() {
		// 本批末尾定一次节点终态（PHP finally 里的 maybeFinalize）。
		if svc != nil && job.TaskID != 0 {
			store := admin.NewNodeSQLStore(db, appKey)
			if err := store.FinalizeInitTask(ctx, uint64(job.NodeID), uint64(job.TaskID)); err != nil {
				logger.Printf("[node-init] 定节点终态失败 node=%d task=%d: %v", job.NodeID, job.TaskID, err)
			}
		}
	}()

	if node == nil {
		for i := range job.UserIDs {
			key := itemKeyAt(job, i)
			if !claimJobItem(ctx, svc, job.TaskID, key) {
				continue
			}
			failJobItem(ctx, svc, job.TaskID, key, "节点已删除，初始化中止")
		}
		return nil
	}
	if isXrayNode(node) {
		// 裸核节点不走面板初始化（agent 按配置渲染）；判成功不拖住节点启用。
		for i := range job.UserIDs {
			key := itemKeyAt(job, i)
			if !claimJobItem(ctx, svc, job.TaskID, key) {
				continue
			}
			completeJobItem(ctx, svc, job.TaskID, key)
		}
		return nil
	}

	client := threeXUiFor(node, appKey)
	now := time.Now().UTC()

	for i, uid := range job.UserIDs {
		key := itemKeyAt(job, i)
		if !claimJobItem(ctx, svc, job.TaskID, key) {
			continue
		}

		u, err := loadJobUser(ctx, db, uid)
		if err != nil {
			failJobItem(ctx, svc, job.TaskID, key, "读取用户失败："+err.Error())
			continue
		}
		if u == nil {
			// 用户已删：该项无事可做，判成功（不影响节点定状态）。
			completeJobItem(ctx, svc, job.TaskID, key)
			continue
		}

		// client 主键固定是 ch_user_{id}（与订阅/流量归属/对账同一口径）。
		email := clientEmailOf(u.ID)

		// 幂等清理：该用户在此节点已有残留则先删，再重建（避免重接后重复计流量）。
		existing, getErr := client.GetClient(ctx, email)
		if getErr != nil {
			failJobItem(ctx, svc, job.TaskID, key, "查询残留 client 失败："+getErr.Error())
			continue
		}
		if existing != nil {
			if err := client.DeleteClient(ctx, email, false); err != nil {
				failJobItem(ctx, svc, job.TaskID, key, "清理残留 client 失败："+err.Error())
				continue
			}
		}

		ids, err := inboundIDsForNode(ctx, db, node.ID, u.Protocol)
		if err != nil {
			failJobItem(ctx, svc, job.TaskID, key, "读取节点入站失败："+err.Error())
			continue
		}
		// 只给「套餐现在还能用」的人建号：无套餐 / 已过期 / 总量超 / 当月超 一律不建。
		// 与封禁扫描器同一判据；用户续费或重置流量后，下一轮建号任务会重新建回来。
		if !u.usableNow(now) || len(ids) == 0 {
			completeJobItem(ctx, svc, job.TaskID, key)
			continue
		}

		totalGB := int64(0)
		if u.TrafficLimit > 0 {
			totalGB = u.TrafficLimit
		}
		expiryMs := int64(0)
		if u.ExpiredAt.Valid {
			expiryMs = u.ExpiredAt.Time.UnixMilli()
		}
		payload := map[string]any{
			"email":      email,
			"enable":     u.Enabled,
			"totalGB":    totalGB,
			"expiryTime": expiryMs,
			"limitIp":    0,
		}
		if _, err := client.AddClient(ctx, payload, ids); err != nil {
			failJobItem(ctx, svc, job.TaskID, key, "创建 client 失败："+err.Error())
			continue
		}

		// 该路径以 enable=enabled 建 client（等价于把用户重新打开）：撤销「已关闭」标记，
		// 否则扫描器会以为用户仍关闭而跳过校验。
		if u.Enabled {
			if _, err := db.ExecContext(ctx,
				"UPDATE users SET traffic_disabled_at = NULL WHERE id = ? AND traffic_disabled_at IS NOT NULL",
				uid); err != nil {
				logger.Printf("[node-init] 清除关闭标记失败 user=%d: %v", uid, err)
			}
		}
		completeJobItem(ctx, svc, job.TaskID, key)
	}
	return nil
}

// ── NodeClientsCleanupJob ──────────────────────────────────

// cleanupSnapshot 是删节点时落进 meta.node_snapshot 的连接信息（password/api_key 仍是密文）。
type cleanupSnapshot struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        int64  `json:"port"`
	Scheme      string `json:"scheme"`
	WebBasePath string `json:"web_base_path"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	APIKey      string `json:"api_key"`
	VerifySSL   bool   `json:"verify_ssl"`
	DriverType  string `json:"driver_type"`
}

// runNodeClientsCleanup 在单台节点上清理全部托管 client（一台节点一个 Job，item=node:{id}）。
func runNodeClientsCleanup(ctx context.Context, db *sql.DB, appKey []byte, svc *task.Service,
	logger *log.Logger, job task.JobPayload) error {

	key := job.ItemKey
	if !claimJobItem(ctx, svc, job.TaskID, key) {
		return nil
	}

	snap, err := decodeCleanupSnapshot(job.NodeSnapshot)
	if err != nil {
		failJobItem(ctx, svc, job.TaskID, key, "节点连接信息无法解析，远端客户端未清理："+err.Error())
		return nil
	}
	if snap.Host == "" {
		failJobItem(ctx, svc, job.TaskID, key, "缺少节点连接信息，无法清理远端客户端")
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(snap.DriverType), "xray") {
		// xray 节点：client 只在内核里，删节点后 agent 密钥失效、无下发通道 → 无需清理。
		logger.Printf("[cleanup] xray 节点 %s 无面板式远端客户端，跳过", snap.Name)
		completeJobItem(ctx, svc, job.TaskID, key)
		return nil
	}

	n := &jobNode{
		ID: snap.ID, Name: snap.Name, Host: snap.Host, Port: snap.Port, Scheme: snap.Scheme,
		WebBasePath: snap.WebBasePath, Username: snap.Username,
		VerifySSL: snap.VerifySSL, DriverType: snap.DriverType,
	}
	if snap.Password != "" {
		n.Password = sql.NullString{String: snap.Password, Valid: true}
	}
	if snap.APIKey != "" {
		n.APIKey = sql.NullString{String: snap.APIKey, Valid: true}
	}
	client := threeXUiFor(n, appKey)

	health, err := client.HealthCheck(ctx)
	if err != nil || !health.OK {
		reason := "节点不可达，远端客户端未清理"
		if err != nil {
			reason += "：" + err.Error()
		} else if health.Error != "" {
			reason += "：" + health.Error
		}
		failJobItem(ctx, svc, job.TaskID, key, reason)
		return nil
	}

	list, err := client.ListClients(ctx)
	if err != nil {
		failJobItem(ctx, svc, job.TaskID, key, "拉取远端客户端列表失败，未做清理："+err.Error())
		return nil
	}
	onPanel := make(map[string]bool, len(list))
	for _, c := range list {
		if email, ok := c["email"].(string); ok && email != "" {
			onPanel[email] = true
		}
	}

	// 只删「本地有记录 且 远端确实存在」的 client：不存在的逐条 delete 会因
	// record not found 报错，把一次干净清理染成失败。
	targets, err := ourEmailsOnPanel(ctx, db, onPanel)
	if err != nil {
		failJobItem(ctx, svc, job.TaskID, key, "查询本地用户失败，未做清理："+err.Error())
		return nil
	}

	var failed []string
	for _, email := range targets {
		if err := client.DeleteClient(ctx, email, false); err != nil {
			failed = append(failed, email+": "+err.Error())
		}
	}
	if len(failed) > 0 {
		failJobItem(ctx, svc, job.TaskID, key,
			fmt.Sprintf("清理 %d 个客户端失败（成功 %d）：%s",
				len(failed), len(targets)-len(failed), strings.Join(failed, "; ")))
		return nil
	}
	logger.Printf("[cleanup] 节点 %s 清理完成，共删除 %d 个 client", snap.Name, len(targets))
	completeJobItem(ctx, svc, job.TaskID, key)
	return nil
}

// decodeCleanupSnapshot 解析 meta.node_snapshot（可能是 map[string]any 或原始 JSON）。
func decodeCleanupSnapshot(v any) (cleanupSnapshot, error) {
	var snap cleanupSnapshot
	switch t := v.(type) {
	case nil:
		return snap, fmt.Errorf("快照为空")
	case cleanupSnapshot:
		return t, nil
	case map[string]any:
		raw, err := json.Marshal(t)
		if err != nil {
			return snap, err
		}
		if err := json.Unmarshal(raw, &snap); err != nil {
			return snap, err
		}
		return snap, nil
	case []byte:
		if err := json.Unmarshal(t, &snap); err != nil {
			return snap, err
		}
		return snap, nil
	case string:
		if err := json.Unmarshal([]byte(t), &snap); err != nil {
			return snap, err
		}
		return snap, nil
	default:
		return snap, fmt.Errorf("不支持的快照类型 %T", v)
	}
}

// ourEmailsOnPanel 返回「本地用户 email ∩ 面板上确实存在」的清单。
func ourEmailsOnPanel(ctx context.Context, db *sql.DB, onPanel map[string]bool) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT id FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		email := clientEmailOf(id)
		if onPanel[email] {
			out = append(out, email)
		}
	}
	return out, rows.Err()
}
