package task

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"controlhub-server/internal/collector"
	"controlhub-server/internal/driver/threexui"
)

// 队列执行体：对齐 app/Jobs/*.php 的 handle()。
//
// 本切片只接 P6 采集已有能力（traffic_sync）；其余 Job（入站同步/节点初始化/
// 域名操作/客户端清理）的服务在 P8/P10，队列里留好 Register 接口与派发路由，
// 上层接上真实实现即可（不接则按「不支持的任务类型」标失败，不会永久 running）。

// TrafficSyncer 是 SyncNodeTrafficJob 的落账依赖。
//
// *collector.Collector 不直接满足本接口（SyncNode 需要取数回调），
// 用 NewCollectorSyncer 包一层；测试可 fake。
type TrafficSyncer interface {
	// SyncWholeNode 落账整节点（meta.user_id 为空的 item：node:{id}）。
	// 返回 acquired=false 表示节点锁被占（PHP release(30) 重入队场景）。
	SyncWholeNode(ctx context.Context, nodeID int64) (acquired bool, err error)
	// SyncUserNode 落账单用户在节点上的流量（meta.user_id 有值）。
	SyncUserNode(ctx context.Context, userID, nodeID int64) (acquired bool, err error)
}

// NodeGate 是 SyncNodeTrafficJob 的前置检查：节点存在且 enabled。
// 返回 (exists, enabled)；PHP：节点删除 → failItem('节点已删除，跳过同步')、
// 节点禁用 → failItem('节点已禁用，跳过同步')。
type NodeGate interface {
	NodeUsable(ctx context.Context, nodeID int64) (exists bool, enabled bool, err error)
}

// RegisterTrafficSync 把 SyncNodeTrafficJob 挂进队列（claim → 同步 → complete/fail）。
func RegisterTrafficSync(q *Queue, svc *Service, syncer TrafficSyncer, gate NodeGate) {
	q.Register(JobSyncNodeTraffic, func(ctx context.Context, job JobPayload) error {
		return runTrafficSyncJob(ctx, svc, syncer, gate, job)
	})
}

// runTrafficSyncJob 对齐 SyncNodeTrafficJob::handle() 的主干。
//
// 与 PHP 的两处偏差（在交付说明里点名）：
//   - 增量封禁检查（banService->checkAfterSync）由 5 分钟 ban-check（collector.ScanBans）
//     兜底 —— P6 未导出单用户检查，定时封禁扫描是 console.php 注释里的「兜底」路径；
//   - 漂移修复（repairDrift）同理：只在定时流量同步里由 RunTrafficSync 记录 DriftUserIDs，
//     不在异步 item 上自动修（避免与 3x-ui 自动禁用拉锯）。
func runTrafficSyncJob(ctx context.Context, svc *Service, syncer TrafficSyncer, gate NodeGate, job JobPayload) error {
	if job.TaskID != 0 && job.ItemKey != "" {
		ok, err := svc.ClaimItem(ctx, job.TaskID, job.ItemKey)
		if err != nil {
			return err
		}
		if !ok {
			return nil // 已被认领/已终态（P2-3）
		}
	}

	if gate != nil {
		exists, enabled, err := gate.NodeUsable(ctx, job.NodeID)
		if err != nil {
			return err
		}
		if !exists || !enabled {
			reason := "节点已禁用，跳过同步"
			if !exists {
				reason = "节点已删除，跳过同步"
			}
			if job.TaskID != 0 && job.ItemKey != "" {
				_, err := svc.FailItem(ctx, job.TaskID, job.ItemKey, reason)
				return err
			}
			return nil
		}
	}

	var acquired bool
	var err error
	if job.UserID != nil {
		acquired, err = syncer.SyncUserNode(ctx, *job.UserID, job.NodeID)
	} else {
		acquired, err = syncer.SyncWholeNode(ctx, job.NodeID)
	}
	if err != nil {
		return err // 交给队列重试/failed()
	}

	if !acquired {
		// 节点锁竞争：PHP release(30) 重入队，最多 max_lock_releases 次后 failItem。
		if job.ReleaseCount < svc.MaxLockReleases {
			job.ReleaseCount++
			qRequeue(svc, job)
			return nil
		}
		if job.TaskID != 0 && job.ItemKey != "" {
			_, err := svc.FailItem(ctx, job.TaskID, job.ItemKey, "节点锁长时间竞争，同步终止")
			return err
		}
		return nil
	}

	if job.TaskID != 0 && job.ItemKey != "" {
		_, err := svc.CompleteItem(ctx, job.TaskID, job.ItemKey)
		return err
	}
	return nil
}

// qRequeue 是 PHP $this->release(30) 的等价物：30s 后重投递同一单元。
func qRequeue(svc *Service, job JobPayload) {
	if svc == nil || svc.Queue == nil {
		return
	}
	// 不在 handler 里 sleep（会占住 worker）：定时器到点后重投。
	time.AfterFunc(lockRequeueDelay, func() {
		defer func() { _ = recover() }()
		svc.Queue.Enqueue(job)
	})
}

// lockRequeueDelay 对齐 PHP SyncNodeTrafficJob::release(30)。
const lockRequeueDelay = 30 * time.Second

// SQLNodeGate 是 NodeGate 的 MySQL 实现（nodes 表）。
type SQLNodeGate struct {
	DB *sql.DB
}

// NodeUsable 查节点存在性与 enabled。
func (g *SQLNodeGate) NodeUsable(ctx context.Context, nodeID int64) (bool, bool, error) {
	var enabled int64
	err := g.DB.QueryRowContext(ctx, "SELECT enabled FROM nodes WHERE id = ?", nodeID).Scan(&enabled)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, enabled != 0, nil
}

// CollectorSyncer 是 TrafficSyncer 的 P6 collector 实现。
//
// 对齐 SyncNodeTrafficJob 的两条取数路径：
//   - 单用户：$sync->syncUserNodeFromSource(...) → collector.SyncSingleUserNode；
//   - 整节点：$driver->getClientStatsByEmail() → collector.SyncNode + 3x-ui 取数回调。
type CollectorSyncer struct {
	C   *collector.Collector
	DB  *sql.DB
	Key []byte // APP_KEY 解出的 32 字节 AES 密钥（节点 password/api_key 解密用）
}

// NewCollectorSyncer 构造适配器。
func NewCollectorSyncer(c *collector.Collector, db *sql.DB, appKey []byte) *CollectorSyncer {
	return &CollectorSyncer{C: c, DB: db, Key: appKey}
}

// SyncWholeNode 取一次节点全量 client 流量并落账。
//
// 裸内核（driver=xray）节点没有面板侧轮询接口，流量由 agent 主动推送
// （/node-api/push）—— 这里必须**跳过而不是报错**，与同一份代码的另外两条路径同口径：
// collector.RunTrafficSync 的 xray 分支（continue）、SyncSingleUserNode 的 xray 分支
// （返回空 map）。此前直接返回错误，导致管理员/用户在后台点「同步流量」时，
// 4 台裸内核节点的子项必定失败、整条任务被标红（日志原文：
// 「节点 #37 驱动类型 xray 不支持面板侧轮询」）。
func (s *CollectorSyncer) SyncWholeNode(ctx context.Context, nodeID int64) (bool, error) {
	pollable, err := s.panelPollable(ctx, nodeID)
	if err != nil {
		return false, err
	}
	if !pollable {
		// 无可轮询数据不等于同步失败：视为已完成，让子项正常收尾。
		return true, nil
	}

	res, err := s.C.SyncNode(ctx, nodeID, func(ctx context.Context) (map[string]collector.ClientStat, error) {
		client, err := s.clientFor(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		// 数据层已按 email 去重（同 email 跨入站只取首值，避免 N 倍计费）。
		stats, err := client.GetClientStatsByEmail(ctx)
		if err != nil {
			return nil, err
		}
		out := make(map[string]collector.ClientStat, len(stats))
		for email, st := range stats {
			// panel.ClientTraffic 在 internal/driver/internal/panel 下，外部包不可具名，
			// 但字段可读（与 collector.RunTrafficSync 的转换同构）。
			out[email] = collector.ClientStat{Up: st.Up, Down: st.Down, Enable: st.Enable}
		}
		return out, nil
	})
	if err != nil {
		return false, err
	}
	return res.Acquired, nil
}

// SyncUserNode 落账单用户（collector.SyncSingleUserNode 内部同口径取数）。
func (s *CollectorSyncer) SyncUserNode(ctx context.Context, userID, nodeID int64) (bool, error) {
	res, err := s.C.SyncSingleUserNode(ctx, userID, nodeID)
	if err != nil {
		return false, err
	}
	return res.Acquired, nil
}

// panelPollable 判断节点能否走面板侧轮询：只有 3x-ui 驱动有 client_traffics 可取。
//
// 与 collector.RunTrafficSync 的判据一致（driver_type 为空按 3x-ui 处理）。
func (s *CollectorSyncer) panelPollable(ctx context.Context, nodeID int64) (bool, error) {
	var driverType sql.NullString
	err := s.DB.QueryRowContext(ctx, "SELECT driver_type FROM nodes WHERE id = ?", nodeID).Scan(&driverType)
	if err == sql.ErrNoRows {
		return false, fmt.Errorf("节点 #%d 不存在", nodeID)
	}
	if err != nil {
		return false, fmt.Errorf("读取节点 #%d 失败: %w", nodeID, err)
	}
	return !driverType.Valid || driverType.String == "" || driverType.String == "3x-ui", nil
}

// clientFor 读节点行并构造 3x-ui 客户端（password/api_key 走 crypto 严格解密）。
func (s *CollectorSyncer) clientFor(ctx context.Context, nodeID int64) (*threexui.Client, error) {
	var (
		scheme, host, basePath, username string
		port                             int64
		password, apiKey                 sql.NullString
		verify                           int64
		driverType                       sql.NullString
	)
	err := s.DB.QueryRowContext(ctx,
		"SELECT scheme, host, port, web_base_path, username, password, api_key, verify_ssl, driver_type "+
			"FROM nodes WHERE id = ?", nodeID).
		Scan(&scheme, &host, &port, &basePath, &username, &password, &apiKey, &verify, &driverType)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("节点 #%d 不存在", nodeID)
	}
	if err != nil {
		return nil, fmt.Errorf("读取节点 #%d 失败: %w", nodeID, err)
	}
	if driverType.Valid && driverType.String != "" && driverType.String != "3x-ui" {
		return nil, fmt.Errorf("节点 #%d 驱动类型 %s 不支持面板侧轮询", nodeID, driverType.String)
	}

	row := threexui.NodeRow{
		Scheme:      scheme,
		Host:        host,
		Port:        int(port),
		WebBasePath: basePath,
		Username:    username,
		VerifySSL:   verify != 0,
	}
	if password.Valid {
		v := password.String
		row.PasswordCipher = &v
	}
	if apiKey.Valid {
		v := apiKey.String
		row.APIKeyCipher = &v
	}
	return threexui.NewForNode(row, s.Key)
}
