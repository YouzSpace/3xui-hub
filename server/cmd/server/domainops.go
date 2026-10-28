package main

// domain 域真实实现：对齐 PHP DomainService + ApplyDomainJob。
//
//   - domainTaskSubmitter 实现 admin.DomainTaskSubmitter：建 async_tasks（domain_action）
//     并 dispatchAfterCommit，worker 异步执行（前端据 ssl_status=pending 显示「配置中」）。
//   - handleApplyDomain 是 JobApplyDomain 的执行体：exec 调 /usr/local/bin/3hub-domain
//     助手（sudoers 受限），解析 stdout JSON，回写 domains 表。
//
// 3hub-domain 助手未安装 / 签发失败时只回写 ssl_status=failed，不抛异常（对齐 PHP
// DomainService「不抛异常，标 failed」的语义，队列不重试、item 标 succeeded）。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/handler/admin"
	"controlhub-server/internal/task"
)

const domainHelperPath = "/usr/local/bin/3hub-domain"

// domainTaskSubmitter 实现 admin.DomainTaskSubmitter。
type domainTaskSubmitter struct {
	async *task.Service
}

// NewDomainTaskSubmitter 构造域名域的异步任务提交器。
func NewDomainTaskSubmitter(async *task.Service) admin.DomainTaskSubmitter {
	return &domainTaskSubmitter{async: async}
}

// Submit 对齐 DomainService::submit：建 domain_action 任务 + dispatchAfterCommit。
func (s *domainTaskSubmitter) Submit(ctx context.Context, action string, domainID uint64, domainName string) (*admin.DomainTask, error) {
	meta, err := json.Marshal(map[string]string{"action": action, "domain": domainName})
	if err != nil {
		return nil, err
	}
	subjectType := "domain"
	subjectID := int64(domainID)
	t, err := s.async.Create(ctx, task.CreateInput{
		Type:        task.TypeDomainAction,
		SubjectType: &subjectType,
		SubjectID:   &subjectID,
		Total:       1,
		Meta:        meta,
		MaxAttempts: 3,
		ItemKeys:    []string{"domain:" + strconv.FormatUint(domainID, 10)},
	})
	if err != nil {
		return nil, err
	}
	if err := s.async.DispatchAfterCommit(ctx, t); err != nil {
		return nil, err
	}
	return &admin.DomainTask{ID: uint64(t.ID)}, nil
}

// registerDomainHandler 注册 JobApplyDomain 的执行体。
func registerDomainHandler(q *task.Queue, svc *task.Service, db *sql.DB, logger *log.Logger) {
	q.Register(task.JobApplyDomain, func(ctx context.Context, job task.JobPayload) error {
		return handleApplyDomain(ctx, svc, db, logger, job)
	})
}

// handleApplyDomain 执行一次域名操作（对齐 ApplyDomainJob::handle）。
//
// 认领 → 执行 → 完成/失败，三步与 PHP 的 claimItem/completeItem/failItem 一致。
// 漏掉认领会让子项永远 pending，10 分钟后被 async-task-timeout 扫成「任务超时未完成」，
// 即使助手已经成功执行完（历史上就是这样误报的）。
func handleApplyDomain(ctx context.Context, svc *task.Service, db *sql.DB, logger *log.Logger, job task.JobPayload) error {
	if job.TaskID != 0 && job.ItemKey != "" {
		ok, err := svc.ClaimItem(ctx, job.TaskID, job.ItemKey)
		if err != nil {
			return err
		}
		if !ok {
			return nil // 已终态/已认领（幂等重放）
		}
	}

	action := job.Action
	if action == "" {
		action = "apply"
	}
	domainID := job.NodeID

	// 读域名行（拿 domain 名；remove 后行已删则跳过）
	var name string
	err := db.QueryRowContext(ctx, "SELECT domain FROM domains WHERE id = ?", domainID).Scan(&name)
	if err == sql.ErrNoRows {
		// 行已删（remove 场景）：用派发时携带的域名跑助手（按剩余 enabled 域名重建 conf）
		if job.Domain == nil || *job.Domain == "" {
			completeDomainItem(ctx, svc, job, logger)
			return nil
		}
		name = *job.Domain
	} else if err != nil {
		return fmt.Errorf("读域名 #%d: %w", domainID, err)
	} else if job.Domain != nil && *job.Domain != "" {
		name = *job.Domain
	}

	res := runDomainHelper(ctx, action, name)
	if err := writeDomainResult(ctx, db, domainID, res); err != nil {
		// 回写失败（如 datetime 格式）不吞掉：子项标失败并留错误，否则页面永远停在 pending。
		msg := fmt.Sprintf("回写域名 #%d 结果失败: %v", domainID, err)
		logger.Printf("[domain] %s", msg)
		failDomainItem(ctx, svc, job, msg, logger)
		return nil
	}
	if res.Error != "" {
		logger.Printf("[domain] 域名操作失败 domain=%s action=%s: %s", name, action, res.Error)
	}
	// 助手执行完毕即算 item 成功（对齐 PHP：签发失败只标 ssl_status=failed，不重试队列）。
	completeDomainItem(ctx, svc, job, logger)
	return nil
}

// completeDomainItem 标子项成功（无任务上下文时什么也不做）。
func completeDomainItem(ctx context.Context, svc *task.Service, job task.JobPayload, logger *log.Logger) {
	if svc == nil || job.TaskID == 0 || job.ItemKey == "" {
		return
	}
	if _, err := svc.CompleteItem(ctx, job.TaskID, job.ItemKey); err != nil {
		logger.Printf("[domain] 标记成功项出错 task=%d item=%s: %v", job.TaskID, job.ItemKey, err)
	}
}

// failDomainItem 标子项失败（无任务上下文时什么也不做）。
func failDomainItem(ctx context.Context, svc *task.Service, job task.JobPayload, reason string, logger *log.Logger) {
	if svc == nil || job.TaskID == 0 || job.ItemKey == "" {
		return
	}
	if _, err := svc.FailItem(ctx, job.TaskID, job.ItemKey, reason); err != nil {
		logger.Printf("[domain] 标记失败项出错 task=%d item=%s: %v", job.TaskID, job.ItemKey, err)
	}
}

// domainResult 是 3hub-domain 的输出解析结果。
type domainResult struct {
	OK            bool
	SSLStatus     string
	CertExpiresAt *time.Time
	Error         string
}

// runDomainHelper exec 调 3hub-domain 助手并解析 stdout JSON。
func runDomainHelper(ctx context.Context, action, domain string) domainResult {
	var out domainResult
	if _, err := exec.LookPath(domainHelperPath); err != nil {
		out.SSLStatus = "failed"
		out.Error = "3hub-domain 助手未安装（本地开发环境）"
		return out
	}

	cmd := exec.CommandContext(ctx, "sudo", "-n", domainHelperPath, action, domain)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	stderrTail := stderr.String()
	if len(stderrTail) > 800 {
		stderrTail = stderrTail[len(stderrTail)-800:]
	}

	var parsed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		out.SSLStatus = "failed"
		out.Error = fmt.Sprintf("3hub-domain 输出无法解析（exit=%v）", runErr)
		if stderrTail != "" {
			out.Error += "\n" + stderrTail
		}
		return out
	}

	ok, _ := parsed["ok"].(bool)
	ssl, _ := parsed["ssl_status"].(string)
	if ssl == "" {
		if ok {
			ssl = "ok"
		} else {
			ssl = "failed"
		}
	}
	switch ssl {
	case "none", "pending", "ok", "expiring", "failed":
	default:
		if ok {
			ssl = "ok"
		} else {
			ssl = "failed"
		}
	}
	out.OK = ok
	out.SSLStatus = ssl
	if v, isStr := parsed["cert_expires_at"].(string); isStr && v != "" {
		// 助手输出 RFC3339（如 2027-01-11T04:51:49Z）。必须转成 time.Time 再落库：
		// 原样把字符串当参数写 datetime 列会被 MariaDB 严格模式拒收（Error 1292），
		// 整条 UPDATE 回滚 → ssl_status/last_error 一起写不进去，页面永远停在「待配置」。
		// PHP 版用 Carbon::parse() 做同样的转换。
		if t, perr := parseCertExpiry(v); perr == nil {
			out.CertExpiresAt = &t
		} else {
			log.Printf("[domain] 无法解析证书到期时间 %q: %v（本次不回写该字段）", v, perr)
		}
	}
	if v, isStr := parsed["error"].(string); isStr && v != "" {
		out.Error = v
	}
	if !ok {
		if out.Error == "" {
			out.Error = "3hub-domain 执行失败"
		}
		if stderrTail != "" {
			out.Error += "\n" + stderrTail
		}
	}
	return out
}

// parseCertExpiry 把助手输出的到期时间转成 time.Time。
//
// 3hub-domain 的 cert_expiry() 固定输出 `date -u -d "${end}" +%Y-%m-%dT%H:%M:%SZ`，
// 即 RFC3339（UTC）。openssl 解析失败时会退回 openssl 原生格式
// `Jan 11 05:03:27 2027 GMT`，这里一并兜住，避免整条回写因为格式没认出来而失败。
func parseCertExpiry(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("空字符串")
	}
	for _, layout := range []string{
		time.RFC3339,                // 2027-01-11T04:51:49Z
		"2006-01-02T15:04:05Z07:00", // 同上（显式）
		"2006-01-02 15:04:05",       // 兜底：无时区（按 UTC，与 DSN loc=UTC 一致）
		"Jan _2 15:04:05 2006 MST",  // openssl -enddate 原生格式
		"Jan 2 15:04:05 2006 MST",
	} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("未知的时间格式")
}

// writeDomainResult 回写 domains 表（对齐 DomainService::finish）。
//
// cert_expires_at 是 datetime 列，DSN 为 parseTime=true&loc=UTC，传 time.Time 时驱动
// 会按 loc 格式化；这里显式 .UTC() 保证写进库的是 UTC，与读取口径一致。
func writeDomainResult(ctx context.Context, db *sql.DB, domainID int64, res domainResult) error {
	var expires any
	if res.CertExpiresAt != nil {
		expires = res.CertExpiresAt.UTC()
	}
	var lastErr any
	if res.Error != "" {
		lastErr = res.Error
	}
	_, err := db.ExecContext(ctx,
		"UPDATE domains SET ssl_status = ?, cert_expires_at = ?, last_error = ?, updated_at = NOW() WHERE id = ?",
		res.SSLStatus, expires, lastErr, domainID)
	return err
}
