package task

// 数据保留期清理（retention:purge）。
//
// 背景（issue #124）：日志 / 回执类表的保留策略原先散在四处 —— HTTP 中间件
// （operation_logs 500 条）、节点指令入队（node_api_commands 30 天）、流量同步
// （user_traffic_hourly 7 天）、监控汇总（monitor_samples 90 天）。新增表时很容易
// 漏掉，mail_logs 就是漏掉的那个：全仓库没有任何 DELETE，批量发信有多少封就留多少行。
//
// 实测一台跑 19 天的实例：整库 77.2 MB，其中 node_api_commands 一张表 68.7 MB（89%），
// 1290 条 client_list 回执占 42.5 MB（平均 34 KB/条）。库体积不是「日志表」撑起来的，
// 是「回执正文」撑起来的。
//
// 这里把「哪张表留多久」收敛成一张规则表 + 每天一次的定时任务：
//   - 阈值优先级：环境变量 > site_configs（可热改）> 代码默认值；
//   - 只删「终态且过期」的行，pending / running 一律不碰；
//   - 一律分批 DELETE（LIMIT），不做一次性大删除，避免长事务锁住业务表；
//   - 幂等且可预演：没有过期行时零写入，--dry-run 只统计不删。

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// 保留期默认值。任一阈值配成 0 或负数 = 关闭该项清理。
const (
	// defaultMailLogRetainDays 发信记录保留天数：排障够用，又不至于被批量发信撑爆。
	defaultMailLogRetainDays = 180
	// defaultNodeCommandRetainDays 与 nodeapi 的 commandRetainDays 同口径（终态回执 30 天）。
	defaultNodeCommandRetainDays = 30
	// defaultAsyncTaskRetainDays 异步任务（子项随任务级联删除）终态保留天数。
	defaultAsyncTaskRetainDays = 30
	// defaultAccessTokenRetainDays 已过期登录态保留天数；expires_at IS NULL 的视为
	// 永不过期，属于业务凭据，不在这里清。
	defaultAccessTokenRetainDays = 7
	// defaultOperationLogKeepRows 操作流水条数上限（不含 type=system 的系统审计）。
	defaultOperationLogKeepRows = 20000
	// defaultSystemAuditRetainDays 系统审计（对账漂移 / agent 升级 / 证书等）保留天数。
	defaultSystemAuditRetainDays = 30
	// defaultClientListKeepPerNode 每个节点保留的 client_list 回执条数。
	// 这类回执单条可达 40 KB（nodeapi.outputLimitClientList），历史全量没有读取价值 ——
	// 对账只关心最新一份。
	defaultClientListKeepPerNode = 50
	// purgeBatchSize 单条 DELETE 的行数上限，避免长事务锁表。
	purgeBatchSize = 2000
)

// retentionRule 是一条「按时间保留」的规则。
type retentionRule struct {
	table  string
	column string
	extra  string // 额外 WHERE（只能是本文件里的常量，不接受外部输入）
	key    string // site_configs 里的键（可热改，不改代码就生效）
	env    string // 环境变量（优先级最高）
	def    int    // 代码默认值（def<=0 表示默认关闭）
}

// runRetentionPurge 是 retention:purge 的命令体。
//
// args 支持 --dry-run=1：只统计「会删多少行」，一行都不删。
func (r *commandRunner) runRetentionPurge(ctx context.Context, args map[string]string) error {
	now := r.now().UTC()
	dryRun := args["--dry-run"] == "1" || args["--dry-run"] == "true"

	var firstErr error
	note := func(table string, n int64, err error) {
		switch {
		case err != nil:
			r.logf("[retention] %s 清理失败：%v", table, err)
			if firstErr == nil {
				firstErr = err
			}
		case n > 0 && dryRun:
			r.logf("[retention] %s 将删除 %d 行（dry-run，未执行）", table, n)
		case n > 0:
			r.logf("[retention] %s 已删除 %d 行", table, n)
		}
	}

	// 1) 按时间保留的流水表
	for _, rule := range []retentionRule{
		{"mail_logs", "created_at", "", "mail.retain_days", "MAIL_LOG_RETAIN_DAYS", defaultMailLogRetainDays},
		{"node_api_commands", "updated_at", "status IN ('success','failed')",
			"node_command.retain_days", "NODE_COMMAND_RETAIN_DAYS", defaultNodeCommandRetainDays},
		{"async_tasks", "updated_at", "status IN ('succeeded','partial','failed')",
			"async_task.retain_days", "ASYNC_TASK_RETAIN_DAYS", defaultAsyncTaskRetainDays},
	} {
		days := r.retainInt(ctx, rule.key, rule.env, rule.def)
		if days <= 0 {
			continue
		}
		cutoff := now.AddDate(0, 0, -days)
		n, err := r.purgeByTime(ctx, rule.table, rule.column, rule.extra, cutoff, dryRun)
		note(rule.table, n, err)
	}

	// 2) access_tokens：只清「已过期」的；expires_at IS NULL 是永不过期凭据，不动
	if days := r.retainInt(ctx, "access_token.retain_days", "ACCESS_TOKEN_RETAIN_DAYS", defaultAccessTokenRetainDays); days > 0 {
		n, err := r.purgeByTime(ctx, "access_tokens", "expires_at", "expires_at IS NOT NULL",
			now.AddDate(0, 0, -days), dryRun)
		note("access_tokens", n, err)
	}

	// 3) operation_logs 分两笔：
	//    - 系统审计（type=system）按时间保留，不能被管理员的日常操作挤掉；
	//    - 操作流水按条数保留最近 N 条。
	if days := r.retainInt(ctx, "system_audit.retain_days", "SYSTEM_AUDIT_RETAIN_DAYS", defaultSystemAuditRetainDays); days > 0 {
		n, err := r.purgeByTime(ctx, "operation_logs", "created_at", "type = 'system'",
			now.AddDate(0, 0, -days), dryRun)
		note("operation_logs(system)", n, err)
	}
	if keep := r.retainInt(ctx, "operation_log.keep_rows", "OPERATION_LOG_KEEP_ROWS", defaultOperationLogKeepRows); keep > 0 {
		n, err := r.purgeByRowCount(ctx, "operation_logs", "type <> 'system'", keep, dryRun)
		note("operation_logs", n, err)
	}

	// 4) client_list 回执：每个节点只留最近 N 条（体积最大的一类）
	if keep := r.retainInt(ctx, "node_command.client_list_keep", "NODE_COMMAND_CLIENT_LIST_KEEP", defaultClientListKeepPerNode); keep > 0 {
		n, err := r.purgeClientListReceipts(ctx, keep, dryRun)
		note("node_api_commands(client_list)", n, err)
	}

	return firstErr
}

// retainInt 取值优先级：环境变量 > site_configs（可热改）> 代码默认值。
func (r *commandRunner) retainInt(ctx context.Context, key, envKey string, def int) int {
	if v, ok := lookupEnvInt(envKey); ok {
		return v
	}
	return r.siteConfigInt(ctx, key, def)
}

// lookupEnvInt 读非负整数环境变量；空值 / 非法值一律视为「没配」。
func lookupEnvInt(name string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// purgeByTime 按时间列分批删到 cutoff 之前，返回删掉（或 dry-run 下将删）的行数。
// table / column / extra 全部来自本文件的常量，不做外部拼接。
func (r *commandRunner) purgeByTime(ctx context.Context, table, column, extra string, cutoff time.Time, dryRun bool) (int64, error) {
	where := column + " < ?"
	if extra != "" {
		where += " AND " + extra
	}
	arg := cutoff.UTC().Format("2006-01-02 15:04:05")

	if dryRun {
		var n int64
		if err := r.deps.DB.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE %s", table, where), arg).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	}

	var total int64
	for {
		res, err := r.deps.DB.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE %s LIMIT %d", table, where, purgeBatchSize), arg)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < purgeBatchSize {
			return total, nil
		}
	}
}

// purgeByRowCount 只保留最近 keep 条：先取第 keep+1 条的主键当水位，再按 id 分批删。
//
// 不用 `id NOT IN (SELECT ... LIMIT ...)`，那种写法在大表上要全表排序；按主键区间删
// 走的是主键索引，且天然可以分批。
func (r *commandRunner) purgeByRowCount(ctx context.Context, table, extra string, keep int, dryRun bool) (int64, error) {
	where := ""
	if extra != "" {
		where = " WHERE " + extra
	}

	var cutoffID int64
	err := r.deps.DB.QueryRowContext(ctx,
		fmt.Sprintf("SELECT id FROM %s%s ORDER BY id DESC LIMIT 1 OFFSET ?", table, where), keep).Scan(&cutoffID)
	if err == sql.ErrNoRows {
		return 0, nil // 行数还没超过 keep，无事可做
	}
	if err != nil {
		return 0, err
	}

	countWhere := where
	if countWhere == "" {
		countWhere = " WHERE id <= ?"
	} else {
		countWhere += " AND id <= ?"
	}

	if dryRun {
		var n int64
		if err := r.deps.DB.QueryRowContext(ctx,
			fmt.Sprintf("SELECT COUNT(*) FROM %s%s", table, countWhere), cutoffID).Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	}

	var total int64
	for {
		res, err := r.deps.DB.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE id <= ?%s LIMIT %d", table, andExtra(extra), purgeBatchSize), cutoffID)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < purgeBatchSize {
			return total, nil
		}
	}
}

// andExtra 把额外条件拼成 ` AND xxx`（空串返回空串）。
func andExtra(extra string) string {
	if extra == "" {
		return ""
	}
	return " AND " + extra
}

// purgeClientListReceipts 让每个节点的 client_list 回执只留最近 keep 条。
//
// 单条回执可达 40 KB（nodeapi.outputLimitClientList），是最占空间的一类；
// 对账只关心最新一份，历史全量排障时看日志即可。
func (r *commandRunner) purgeClientListReceipts(ctx context.Context, keep int, dryRun bool) (int64, error) {
	rows, err := r.deps.DB.QueryContext(ctx,
		"SELECT DISTINCT node_id FROM node_api_commands WHERE action = 'client_list'")
	if err != nil {
		return 0, err
	}
	var nodes []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		nodes = append(nodes, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var total int64
	for _, nodeID := range nodes {
		var cutoffID int64
		err := r.deps.DB.QueryRowContext(ctx,
			`SELECT id FROM node_api_commands
			  WHERE node_id = ? AND action = 'client_list'
			  ORDER BY id DESC LIMIT 1 OFFSET ?`, nodeID, keep).Scan(&cutoffID)
		if err == sql.ErrNoRows {
			continue // 这个节点还没攒到 keep 条
		}
		if err != nil {
			return total, err
		}

		if dryRun {
			var n int64
			if err := r.deps.DB.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM node_api_commands WHERE node_id = ? AND action = 'client_list' AND id <= ?",
				nodeID, cutoffID).Scan(&n); err != nil {
				return total, err
			}
			total += n
			continue
		}

		res, err := r.deps.DB.ExecContext(ctx,
			"DELETE FROM node_api_commands WHERE node_id = ? AND action = 'client_list' AND id <= ?",
			nodeID, cutoffID)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
