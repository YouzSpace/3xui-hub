package httpx

// 操作日志中间件：对齐 backend/app/Http/Middleware/RecordOperationLog.php。
//
// 记录条件（与 PHP 逐字一致）：
//   - 写操作（POST/PUT/DELETE）无论成败都记
//   - 5xx 无论读写都记
//   - 成功/失败的 GET（含登录轮询）不记
//
// 路径过滤：只处理 admin-api/* 和 api/*，跳过 api/ping 与 api/node-api/*。

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"reflect"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// OpLogErrorKey 是 gin.Context 的键：failServer 把「具体错误」（what + err）存到这里，
// 中间件优先读它写入 operation_logs.error（比响应 body 的「服务器错误」更具体，
// 让操作审计页面能查到真实报错，而不是前端 toast 消失后无从追溯）。
const OpLogErrorKey = "admin.oplog_error"

// operationLogActionMap 是 describeAction 的前缀映射表（逐字复制 PHP 的 $map）。
var operationLogActionMap = []struct{ prefix, desc string }{
	{"admin-api/login", "登录"},
	{"admin-api/logout", "退出"},
	{"admin-api/change-password", "修改密码"},
	{"admin-api/update-username", "修改用户名"},
	{"admin-api/users", "管理用户"},
	{"admin-api/nodes", "管理节点"},
	{"admin-api/plans", "管理套餐"},
	{"admin-api/orders", "查看订单"},
	{"admin-api/sync-traffic", "同步流量"},
	{"admin-api/backup/export", "导出备份"},
	{"admin-api/backup/import", "导入备份"},
	{"admin-api/backup/preview", "预览备份"},
	{"admin-api/payments", "管理支付"},
	{"admin-api/email", "配置邮箱"},
	{"admin-api/site-settings", "修改站点设置"},
	{"admin-api/subscription-settings", "修改订阅设置"},
	{"admin-api/tutorials", "管理教程"},
	{"admin-api/announcement", "发布公告"},
	{"admin-api/async-tasks", "异步任务"},
	{"admin-api/google2fa", "2FA设置"},
	{"admin-api/system/status", "查看系统状态"},
	{"api/login", "用户登录"},
	{"api/register", "用户注册"},
	{"api/sync-traffic", "用户同步流量"},
	{"api/payment/create", "用户创建订单"},
	{"api/payment/notify", "支付回调"},
}

// describeOperationAction 对齐 PHP describeAction()：前缀映射 + 兜底 "METHOD /path"。
func describeOperationAction(method, path string) string {
	for _, m := range operationLogActionMap {
		if strings.HasPrefix(path, m.prefix) {
			return m.desc
		}
	}
	return strings.ToUpper(method) + " /" + path
}

// responseRecorder 包装 gin.ResponseWriter，捕获状态码与 body。
type responseRecorder struct {
	gin.ResponseWriter
	body   *bytes.Buffer
	status int
}

func (r *responseRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = 200
	}
	r.body.Write(data)
	return r.ResponseWriter.Write(data)
}

// OperationLogMiddleware 对齐 RecordOperationLog 中间件。
type OperationLogMiddleware struct {
	db     *sql.DB
	logger *log.Logger
}

// NewOperationLogMiddleware 构造操作日志中间件。
func NewOperationLogMiddleware(db *sql.DB, logger *log.Logger) *OperationLogMiddleware {
	if logger == nil {
		logger = log.Default()
	}
	return &OperationLogMiddleware{db: db, logger: logger}
}

// Handler 返回 gin.HandlerFunc。
func (m *OperationLogMiddleware) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		trimPath := strings.TrimPrefix(path, "/")

		isAPI := strings.HasPrefix(trimPath, "admin-api/") || strings.HasPrefix(trimPath, "api/")
		if !isAPI || trimPath == "api/ping" || strings.HasPrefix(trimPath, "api/node-api/") {
			c.Next()
			return
		}

		rec := &responseRecorder{ResponseWriter: c.Writer, body: &bytes.Buffer{}}
		c.Writer = rec

		c.Next()

		status := rec.status
		if status == 0 {
			status = 200
		}
		method := c.Request.Method
		isWrite := method == "POST" || method == "PUT" || method == "DELETE"

		// 业务错误码（Go writeJSON 恒返 HTTP 200，错误码在 body 里）
		bodyCode := 0
		if rec.body.Len() > 0 {
			var tmp struct {
				Code int `json:"code"`
			}
			if json.Unmarshal(rec.body.Bytes(), &tmp) == nil {
				bodyCode = tmp.Code
			}
		}
		isServerError := status >= 500 || bodyCode >= 500

		if !isWrite && !isServerError {
			return
		}

		// 优先用 handler 通过 failServer 存的「具体错误」（what + err），
		// 比响应 body 里的「服务器错误」笼统文案更有用——操作审计要能看到具体报错。
		var errMsg *string
		if v, ok := c.Get(OpLogErrorKey); ok {
			if s, ok2 := v.(string); ok2 && s != "" {
				r := []rune(s)
				if len(r) > 500 {
					s = string(r[:500])
				}
				errMsg = &s
			}
		} else if rec.body.Len() > 0 {
			var parsed struct {
				Code    int    `json:"code"`
				Msg     string `json:"msg"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(rec.body.Bytes(), &parsed); err == nil {
				if status >= 400 || parsed.Code >= 400 {
					msg := parsed.Msg
					if msg == "" {
						msg = parsed.Message
					}
					if msg != "" {
						r := []rune(msg)
						if len(r) > 500 {
							msg = string(r[:500])
						}
						errMsg = &msg
					}
				}
			}
		}

		// type / actor
		var logType string
		var actorID *uint64
		var actorName *string

		if strings.HasPrefix(trimPath, "admin-api/") {
			logType = "admin"
			if admin := AdminFromContext(c); admin != nil {
				id := admin.ID
				actorID = &id
				name := admin.Username
				if name == "" {
					name = "unknown"
				}
				actorName = &name
			} else {
				unknown := "unknown"
				actorName = &unknown
			}
		} else {
			logType = "user"
			if v, ok := c.Get("api.user"); ok {
				actorID, actorName = extractUserFields(v)
			}
		}

		action := describeOperationAction(method, trimPath)
		ip := c.ClientIP()
		now := time.Now().UTC()

		if _, err := m.db.ExecContext(context.Background(),
			`INSERT INTO operation_logs (type, actor_id, actor_name, action, method, path, status, error, ip, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			logType, actorID, actorName, action, method, trimPath, status, errMsg, ip, now,
		); err != nil {
			m.logger.Printf("记录操作日志失败: %v", err)
		}

		// 保留期不在这里做：原先的「500 条硬上限」改由 retention:purge 定时任务统一处理
		// （见 internal/task/retention.go）。放在请求路径里有三个问题 —— 每个写请求多
		// 两次库操作、500 这个值运维改不了、系统审计（type=system）会被一起挤掉。
	}
}

// extractUserFields 用反射从 context 里的 User 对象提取 ID 与 Email。
// 不依赖 handler/api 包类型（避免循环 import），只按字段名取。
func extractUserFields(v any) (*uint64, *string) {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		if rv.IsNil() {
			return nil, nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, nil
	}

	var actorID *uint64
	var actorName *string

	if f := rv.FieldByName("ID"); f.IsValid() && f.Kind() == reflect.Uint64 {
		id := f.Uint()
		actorID = &id
	}

	if f := rv.FieldByName("Email"); f.IsValid() {
		// sql.NullString{String string; Valid bool}
		if f.Kind() == reflect.Struct {
			if sf := f.FieldByName("Valid"); sf.IsValid() && sf.Kind() == reflect.Bool && sf.Bool() {
				if ss := f.FieldByName("String"); ss.IsValid() && ss.Kind() == reflect.String {
					e := ss.String()
					if e != "" {
						actorName = &e
					}
				}
			}
		}
	}

	return actorID, actorName
}
