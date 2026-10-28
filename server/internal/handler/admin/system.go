package admin

// 管理端系统状态（/admin-api/system/status、/admin-api/audit-logs）。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/SystemStatusController.php。

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// Register 把系统域路由挂到给定分组。
func (d *SystemDomain) Register(r gin.IRouter) {
	r.GET("/system/status", d.systemStatus)
	r.GET("/audit-logs", d.auditLogs)
}

// RegisterGeodata 把 geo 数据路由挂到给定分组（geodata 服务未注入时整组 500）。
func (d *SystemDomain) RegisterGeodata(r gin.IRouter) {
	r.GET("/geodata", d.geodataIndex)
	r.POST("/geodata/update", d.geodataUpdate)
	r.POST("/geodata/dispatch", d.geodataDispatch)
}

// ---- 响应封装 ----

func (d *SystemDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

func (d *SystemDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *SystemDomain) failValidation(c *gin.Context, summary string) { d.fail(c, summary, 500) }

func (d *SystemDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

// ---- 系统状态 ----

// systemStatus 对齐 GET /admin-api/system/status。
func (d *SystemDomain) systemStatus(c *gin.Context) {
	ctx := ctxOf(c)

	logs, err := d.store.OperationLogsRecent(ctx, 100)
	if err != nil {
		d.failServer(c, "读取操作日志", err)
		return
	}
	logsOut := make(jsArr, 0, len(logs))
	for i := range logs {
		// status() 的 get([...]) 限定了列，不含 ip
		logsOut = append(logsOut, presentOperationLog(&logs[i], false))
	}

	tasks, err := d.taskStatuses(ctx)
	if err != nil {
		d.failServer(c, "读取定时任务状态", err)
		return
	}

	worker := d.workerStatus(ctx)
	php := d.php.Audit()

	d.success(c, jo(
		"logs", logsOut,
		"tasks", tasks,
		"worker", worker,
		"php", presentPHPStatus(php),
	))
}

// taskStatuses 对齐 SystemStatusController::taskStatuses。
func (d *SystemDomain) taskStatuses(ctx context.Context) (*jsObj, error) {
	events := make(jsArr, 0, len(d.schedule))
	for _, ev := range d.schedule {
		last, err := d.store.ScheduledTaskRunLast(ctx, ev.Command)
		if err != nil {
			return nil, err
		}
		item := jo(
			"command", ev.Command,
			"expression", ev.Expression,
			"next_run", nextRunISO(ev.Expression, d.now().UTC()),
			"last_run", nil,
			"last_status", nil,
			"last_error", nil,
			"last_dur_ms", nil,
			"has_mutex", ev.HasMutex,
		)
		if last != nil {
			item.put("last_run", nullableEloq(last.RanAt))
			item.put("last_status", last.Status)
			item.put("last_error", nullableStr(last.Error))
			if last.DurationMS != nil {
				item.put("last_dur_ms", *last.DurationMS)
			} else {
				item.put("last_dur_ms", nil)
			}
		}
		events = append(events, item)
	}

	recent, err := d.store.ScheduledTaskRunsRecent(ctx, 10)
	if err != nil {
		return nil, err
	}
	recentOut := make(jsArr, 0, len(recent))
	for i := range recent {
		r := &recent[i]
		var dur any
		if r.DurationMS != nil {
			dur = *r.DurationMS
		}
		recentOut = append(recentOut, jo(
			"command", r.Command,
			"status", r.Status,
			"error", nullableStr(r.Error),
			"duration_ms", dur,
			"ran_at", nullableEloq(r.RanAt),
		))
	}
	return jo("events", events, "recent", recentOut), nil
}

// workerStatus 对齐 SystemStatusController::workerStatus（心跳超 15 分钟视为不在线）。
func (d *SystemDomain) workerStatus(ctx context.Context) *jsObj {
	heartbeat, err := d.store.WorkerHeartbeat(ctx)
	if err != nil {
		d.logger.Printf("admin 读取 worker 心跳失败: %v", err)
	}
	alive := false
	if heartbeat != nil {
		alive = math.Abs(d.now().UTC().Sub(*heartbeat).Minutes()) < 15
	}

	pending, running, failed, qerr := d.store.QueueCounts(ctx)
	if qerr != nil {
		// 队列表未就绪时保持 0 / 空数组的容错语义，但必须留痕
		d.logger.Printf("系统状态：读取队列数据失败: %v", qerr)
	}
	// Go 侧队列走进程内 goroutine，Laravel 的 jobs/failed_jobs 表在纯 Go 部署下恒为 0 行
	// （历史表现：面板永远显示 0/0/0，用户以为没在排队）。有内存队列时以它为准。
	queueSource := "jobs_table"
	if d.mailQueue != nil {
		if st, ok := d.mailQueue.QueueStats(); ok {
			pending, running, failed = st.Pending, st.Running, st.Failed
			queueSource = "memory_queue"
		}
	}
	recent, ferr := d.store.FailedJobsRecent(ctx, 5)
	if ferr != nil {
		d.logger.Printf("系统状态：读取失败任务失败: %v", ferr)
	}
	recentOut := make(jsArr, 0, len(recent))
	for i := range recent {
		f := &recent[i]
		var attempts any
		if f.Attempts != nil {
			attempts = *f.Attempts
		}
		recentOut = append(recentOut, jo(
			"queue", f.Queue,
			"name", nullableStr(f.Name),
			"attempts", attempts,
			"error_summary", nullableStr(f.ErrorSummary),
			"failed_at", nullableEloq(f.FailedAt),
		))
	}

	var lastSeen any
	if heartbeat != nil {
		lastSeen = eloqTime(*heartbeat)
	}
	return jo(
		"alive", alive,
		"last_seen", lastSeen,
		"pending", pending,
		"running", running,
		"failed_count", failed,
		"queue_source", queueSource,
		"recent_failed", recentOut,
	)
}

func presentPHPStatus(p PHPStatus) *jsObj {
	if !p.Checked {
		return jo("checked", false, "error", p.Error)
	}
	fixedIn := any(nil)
	if p.FixedIn != nil {
		fixedIn = *p.FixedIn
	}
	return jo(
		"checked", true,
		"version", p.Version,
		"sapi", p.SAPI,
		"patched", p.Patched,
		"fixed_in", fixedIn,
		"mitigated", p.Mitigated,
		"ini_file", p.IniFile,
	)
}

// ---- 操作审计 ----

// auditLogs 对齐 GET /admin-api/audit-logs（独立分页接口）。
//
// 与「系统监控」分开：监控 30 秒轮询看当前状态；审计按需翻页筛选。
// per_page 限 10–100，默认 50（与 successPage 的固定 50 不同）。
func (d *SystemDomain) auditLogs(c *gin.Context) {
	f := AuditLogFilter{
		Type:   queryStringOr(c, "type"),
		Status: queryStringOr(c, "status"),
		Search: strings.TrimSpace(queryStringOr(c, "search")),
	}
	if f.Type != "" && !containsString([]string{"admin", "user", "system"}, f.Type) {
		f.Type = ""
	}
	if f.Status != "" && f.Status != "ok" && f.Status != "error" {
		f.Status = ""
	}

	perPage := int64(50)
	if s := queryStringOr(c, "per_page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			perPage = int64(n)
		}
	}
	if perPage < 10 {
		perPage = 10
	}
	if perPage > 100 {
		perPage = 100
	}
	page := int64(1)
	if s := queryStringOr(c, "page"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 1 {
			page = int64(n)
		}
	}

	rows, total, err := d.store.OperationLogPage(ctxOf(c), f, page, perPage)
	if err != nil {
		d.failServer(c, "查询审计日志", err)
		return
	}
	items := make(jsArr, 0, len(rows))
	for i := range rows {
		items = append(items, presentOperationLog(&rows[i], true))
	}
	d.successPage(c, items, page, perPage, total)
}

// successPage 对齐 ApiResponse::successPage（页码越界夹回最后一页）。
func (d *SystemDomain) successPage(c *gin.Context, items []any, page, perPage, total int64) {
	lastPage := (total + perPage - 1) / perPage
	if lastPage < 1 {
		lastPage = 1
	}
	if page < 1 {
		page = 1
	}
	if page > lastPage {
		page = lastPage
	}
	writeJSON(c, phpEncode(jo(
		"code", 0,
		"msg", "ok",
		"data", jsArr(items),
		"pagination", jo(
			"current_page", page,
			"last_page", lastPage,
			"total", total,
			"per_page", perPage,
		),
	)))
}

// presentOperationLog 对齐 status() / auditLogs() 的 map。
// withIP=true 时带 ip（auditLogs 有，status() 的 logs 列表没有 ——
// status() 用 get([...]) 限定了列）。
func presentOperationLog(l *OperationLogRow, withIP bool) *jsObj {
	var status any
	if l.Status != nil {
		status = *l.Status
	}
	out := jo(
		"id", l.ID,
		"type", l.Type,
		"actor_name", nullableStr(l.ActorName),
		"action", l.Action,
		"method", nullableStr(l.Method),
		"path", nullableStr(l.Path),
		"status", status,
		"error", nullableStr(l.Error),
	)
	if withIP {
		out.put("ip", nullableStr(l.IP))
	}
	// auditLogs 用 toISOString()（Eloquent 口径 .000000Z）；status() 的 logs 是 Eloquent 模型输出同款
	out.put("created_at", nullableEloq(l.CreatedAt))
	return out
}

// queryStringOr 取查询串参数，缺失回空串。
func queryStringOr(c *gin.Context, key string) string {
	if s := queryString(c, key); s != nil {
		return *s
	}
	return ""
}

// ---- next_run 估算 ----

// nextRunISO 按 cron 五段式估算下一次运行（UTC，对齐 Laravel nextRunDate 的日期语义）。
// 只覆盖本项目用到的几种写法：`*/N * * * *`、`M H * * *`、`0 */H * * *`、`M H * * *`。
// 解析不了返回 null（前端按占位符处理）。
func nextRunISO(expr string, from time.Time) any {
	t := nextRunTime(expr, from)
	if t == nil {
		return nil
	}
	return eloqTime(*t)
}

func nextRunTime(expr string, from time.Time) *time.Time {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil
	}
	minute, hour := fields[0], fields[1]

	// 逐分钟往前找，最多找 4 年（覆盖 2/29 类边界；本项目只用到简单表达式，几步就到）
	t := from.Truncate(time.Minute).Add(time.Minute)
	limit := from.AddDate(4, 0, 0)
	for t.Before(limit) {
		if cronFieldMatches(minute, t.Minute(), 60) && cronFieldMatches(hour, t.Hour(), 24) {
			// 日/月/周字段本项目都是 `*`，不再展开
			cp := t
			return &cp
		}
		t = t.Add(time.Minute)
	}
	return nil
}

// cronFieldMatches 支持 `*`、`*/N`、纯数字、`N`（小时段还有 `*/H`）。
func cronFieldMatches(field string, value, mod int) bool {
	if field == "*" {
		return true
	}
	if strings.HasPrefix(field, "*/") {
		n, err := strconv.Atoi(field[2:])
		if err != nil || n <= 0 {
			return false
		}
		return value%n == 0
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return false
	}
	return n == value%mod
}

// errorSummary 对齐 errorSummary（去掉尾部 in /path:line）。
func errorSummary(exception string) *string {
	firstLine := strings.TrimSpace(strings.SplitN(exception, "\n", 2)[0])
	if firstLine == "" {
		return nil
	}
	// 去掉尾部的 " in /path:123"
	if i := strings.LastIndex(firstLine, " in "); i >= 0 {
		rest := firstLine[i+4:]
		if strings.Contains(rest, ":") && !strings.Contains(rest, " ") {
			firstLine = strings.TrimSpace(firstLine[:i])
		}
	}
	if len([]rune(firstLine)) > 300 {
		firstLine = string([]rune(firstLine)[:297]) + "..."
	}
	return &firstLine
}

// jobNameFromPayload 对齐 jobNameFromPayload（坏 JSON 只能兜底 null）。
func jobNameFromPayload(payload string) *string {
	if payload == "" {
		return nil
	}
	dec := newJSONDecoder(payload)
	var generic map[string]any
	if err := dec.Decode(&generic); err != nil {
		return nil
	}
	if name, ok := generic["displayName"].(string); ok && name != "" {
		return &name
	}
	return nil
}
