package main

// 异步建号（ProvisionClientJob）：对齐 PHP UserAdminService::dispatchProvisionClient。
//
// 注册后为每个 enabled 节点派发一个 ProvisionClientJob，队列 worker 异步下发 client。
// 单节点逻辑：先 GetClient 查存在 → 不存在 AddClient，存在 UpdateClient。
// 仅处理 3x-ui 节点（xray 节点走 node_api_commands 指令通道，由 agent 侧消费）。

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	"controlhub-server/internal/collector"
	"controlhub-server/internal/crypto"
	"controlhub-server/internal/driver/threexui"
	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/handler/api"
	"controlhub-server/internal/task"
)

// queueProvisioner 实现 api.Provisioner：把建号任务派进 task.Queue。
type queueProvisioner struct {
	db     *sql.DB
	queue  *task.Queue
	appKey []byte
	logger *log.Logger
}

// NewQueueProvisioner 构造异步建号派发器。
func NewQueueProvisioner(db *sql.DB, queue *task.Queue, appKey []byte, logger *log.Logger) *queueProvisioner {
	if logger == nil {
		logger = log.Default()
	}
	return &queueProvisioner{db: db, queue: queue, appKey: appKey, logger: logger}
}

// Dispatch 实现 api.Provisioner：为每个 enabled 节点派发 ProvisionClientJob。
func (p *queueProvisioner) Dispatch(ctx context.Context, user *api.User) error {
	rows, err := p.db.QueryContext(ctx, "SELECT id FROM nodes WHERE enabled = 1")
	if err != nil {
		return fmt.Errorf("查询节点失败: %w", err)
	}
	defer rows.Close()

	uid := int64(user.ID)
	for rows.Next() {
		var nodeID int64
		if err := rows.Scan(&nodeID); err != nil {
			continue
		}
		p.queue.Enqueue(task.JobPayload{
			Kind:   task.JobProvisionClient,
			UserID: &uid,
			NodeID: nodeID,
		})
	}
	return rows.Err()
}

// registerProvisionHandler 注册 ProvisionClientJob 的执行体。
func registerProvisionHandler(q *task.Queue, db *sql.DB, appKey []byte, logger *log.Logger) {
	q.Register(task.JobProvisionClient, func(ctx context.Context, job task.JobPayload) error {
		return handleProvisionClient(ctx, db, appKey, logger, job)
	})
}

// handleProvisionClient 执行单节点建号（对齐 PHP ProvisionClientJob::handle）。
func handleProvisionClient(ctx context.Context, db *sql.DB, appKey []byte, logger *log.Logger, job task.JobPayload) error {
	if job.UserID == nil {
		return nil
	}
	userID := *job.UserID

	// 读用户（按当时的最新数据重算负载 —— 与 PHP 注释一致）
	var email, protocol, uuid string
	var planID sql.NullInt64
	var trafficLimit int64
	var expiredAt sql.NullTime
	var enabled bool
	err := db.QueryRowContext(ctx,
		`SELECT email, protocol, uuid, plan_id, traffic_limit, expired_at, enabled
		 FROM users WHERE id = ?`, userID).
		Scan(&email, &protocol, &uuid, &planID, &trafficLimit, &expiredAt, &enabled)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读用户 %d: %w", userID, err)
	}

	// 读节点
	var nodeHost, nodeScheme, nodeBasePath, nodeDriver string
	var nodePort int64
	var nodeUser string
	var nodePass, nodeAPIKey sql.NullString
	var nodeVerify, nodeEnabled bool
	err = db.QueryRowContext(ctx,
		`SELECT host, port, scheme, web_base_path, username, password, api_key,
		        verify_ssl, driver_type, enabled
		 FROM nodes WHERE id = ?`, job.NodeID).
		Scan(&nodeHost, &nodePort, &nodeScheme, &nodeBasePath, &nodeUser,
			&nodePass, &nodeAPIKey, &nodeVerify, &nodeDriver, &nodeEnabled)
	if err == sql.ErrNoRows || !nodeEnabled {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读节点 %d: %w", job.NodeID, err)
	}

	if nodeDriver != "3x-ui" {
		return nil
	}

	// 读入站 ID：该节点勾选的全部入站，不按协议过滤（用户全协议通用）
	inboundRows, err := db.QueryContext(ctx,
		`SELECT inbound_id FROM node_inbounds WHERE node_id = ?`,
		job.NodeID)
	if err != nil {
		return fmt.Errorf("读入站失败: %w", err)
	}
	var inboundIDs []int64
	for inboundRows.Next() {
		var id int64
		if err := inboundRows.Scan(&id); err == nil {
			inboundIDs = append(inboundIDs, id)
		}
	}
	inboundRows.Close()
	if len(inboundIDs) == 0 {
		return nil
	}

	// 构造 3x-ui client（解密密文凭据）
	cfg := threexui.Config{
		Scheme:      nodeScheme,
		Host:        nodeHost,
		Port:        int(nodePort),
		WebBasePath: nodeBasePath,
		Username:    nodeUser,
		VerifySSL:   nodeVerify,
	}
	if nodePass.Valid {
		cfg.Password = decryptOrRaw(nodePass.String, appKey)
	}
	if nodeAPIKey.Valid {
		cfg.APIKey = decryptOrRaw(nodeAPIKey.String, appKey)
	}
	client := threexui.New(cfg)

	// client 负载（对齐 PHP prepareClientData）
	enable := planID.Valid && enabled
	totalGB := int64(0)
	if trafficLimit > 0 {
		totalGB = trafficLimit
	}
	expiryMs := int64(0)
	if expiredAt.Valid {
		expiryMs = expiredAt.Time.UnixMilli()
	}

	clientEmail := fmt.Sprintf("ch_user_%d", userID)

	existing, getErr := client.GetClient(ctx, clientEmail)
	if getErr != nil || existing == nil {
		clientData := threexui.ClientPayload(clientEmail, enable, totalGB, expiryMs)
		// ⚠️ AddClient 内部已经按 {"client": …, "inboundIds": …} 包了一层；这里再包一次
		// 会让 3x-ui 解析出的 client.email 为空，节点直接回
		// `Something went wrong (client email is required…)` —— 线上 23:18 user=251
		// 在 node 25/33/36 三条就是这个：注册/续费派发的建号 Job 在全部 3x-ui 节点上白跑，
		// 用户付了钱但节点上根本没有他。
		if _, err := client.AddClient(ctx, clientData, inboundIDs); err != nil {
			logger.Printf("[provision] AddClient 失败 user=%d node=%d: %v", userID, job.NodeID, err)
			return nil
		}
	} else {
		// 更新（clients/update）：**刻意不传凭据**。3x-ui 的 update 是整条替换：只发
		// email/enable/totalGB/expiryTime/limitIp 会把 uuid/flow/subId 等按零值写掉。
		// 取回节点上的完整对象，归一化后只覆盖我们要改的字段 —— 凭据（id/password/auth）
		// 由 NormalizeForUpdate 原样带回，存量用户的 uuid 因此保持不变（订阅不会失效）。
		payload := collector.NormalizeForUpdate(existing)
		payload["email"] = clientEmail
		payload["enable"] = enable
		payload["totalGB"] = totalGB
		payload["expiryTime"] = expiryMs
		payload["limitIp"] = 0
		if err := client.UpdateClient(ctx, clientEmail, payload, nil); err != nil {
			logger.Printf("[provision] UpdateClient 失败 user=%d node=%d: %v", userID, job.NodeID, err)
			return nil
		}
	}

	return nil
}

// decryptOrRaw 解密失败返回原文（与 admin.nodeSQLStore.decryptOrRaw 一致）。
func decryptOrRaw(payload string, key []byte) string {
	if payload == "" || len(key) == 0 {
		return payload
	}
	plain, err := crypto.DecryptString(payload, key)
	if err != nil {
		return payload // 存量明文或解密失败，原样使用
	}
	return plain
}

var _ api.Provisioner = (*queueProvisioner)(nil)

// geodataAdapter 把 admin.GeodataService 适配到 task.GeodataUpdater 接口。
type geodataAdapter struct {
	inner admin.GeodataService
}

func (a *geodataAdapter) Update(ctx context.Context) (map[string]task.GeodataUpdateResult, error) {
	results, err := a.inner.Update(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]task.GeodataUpdateResult, len(results))
	for _, r := range results {
		out[r.Name] = task.GeodataUpdateResult{
			Name:    r.Name,
			Status:  r.Status,
			Size:    r.Size,
			Message: r.Message,
		}
	}
	return out, nil
}

func (a *geodataAdapter) DispatchToNodes(ctx context.Context) (queued, skipped int, missing bool, err error) {
	return a.inner.DispatchToNodes(ctx)
}
