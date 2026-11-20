package task

// REALITY 巡检（xray:reality-audit）：目标探测、serverNames 缺失比对、告警落库。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// runXrayRealityAudit 对齐 RealityAuditCommand::handle()。
// REALITY 入站巡检：节点侧探测 dest，告警 SNI 不匹配与证书链超限。
func (r *commandRunner) runXrayRealityAudit(ctx context.Context, args map[string]string) error {
	nodeFilter := int64(0)
	if v, ok := args["--node"]; ok {
		fmt.Sscanf(v, "%d", &nodeFilter)
	}
	waitSec := 20.0
	if v, ok := args["--wait"]; ok {
		fmt.Sscanf(v, "%g", &waitSec)
	}

	// 查启用的 xray 节点
	nodeRows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, name FROM nodes WHERE enabled = 1 AND driver_type = 'xray' ORDER BY id`)
	if err != nil {
		return err
	}
	type xrayNode struct {
		ID   int64
		Name string
	}
	var xrayNodes []xrayNode
	for nodeRows.Next() {
		var n xrayNode
		if err := nodeRows.Scan(&n.ID, &n.Name); err != nil {
			nodeRows.Close()
			return err
		}
		if nodeFilter > 0 && n.ID != nodeFilter {
			continue
		}
		xrayNodes = append(xrayNodes, n)
	}
	nodeRows.Close()

	if len(xrayNodes) == 0 {
		r.logf("没有启用中的 REALITY 入站")
		return nil
	}

	// 查 REALITY 入站
	alerts := 0
	for _, node := range xrayNodes {
		ibRows, err := r.deps.DB.QueryContext(ctx,
			`SELECT id, tag, port, stream_settings FROM xray_inbounds
			 WHERE node_id = ? AND enabled = 1 ORDER BY sort, id`, node.ID)
		if err != nil {
			continue
		}

		type inbound struct {
			ID             int64
			Tag            string
			Port           int64
			StreamSettings string
		}
		var inbounds []inbound
		for ibRows.Next() {
			var ib inbound
			if err := ibRows.Scan(&ib.ID, &ib.Tag, &ib.Port, &ib.StreamSettings); err != nil {
				continue
			}
			// 检查 security == "reality"
			var stream map[string]any
			if json.Unmarshal([]byte(ib.StreamSettings), &stream) == nil {
				if sec, _ := stream["security"].(string); sec == "reality" {
					inbounds = append(inbounds, ib)
				}
			}
		}
		ibRows.Close()

		if len(inbounds) == 0 {
			continue
		}

		// 收集 dest 目标
		targets := map[string]bool{}
		for _, ib := range inbounds {
			var stream map[string]any
			if json.Unmarshal([]byte(ib.StreamSettings), &stream) != nil {
				continue
			}
			realitySettings, _ := stream["realitySettings"].(map[string]any)
			if realitySettings == nil {
				continue
			}
			dest, _ := realitySettings["dest"].(string)
			dest = strings.TrimSpace(dest)
			if dest != "" {
				host := xrayHostOf(dest)
				if host != "" {
					targets[host] = true
				}
			}
		}

		if len(targets) == 0 {
			continue
		}

		// 下发 reality_scan 指令并等回执
		targetList := make([]string, 0, len(targets))
		for t := range targets {
			targetList = append(targetList, t)
		}
		sort.Strings(targetList)

		results := r.probeRealityScan(ctx, node.ID, targetList, waitSec)
		if results == nil {
			r.logf("%s：节点未回执（离线 / agent 过旧），本轮跳过", node.Name)
			continue
		}

		for _, ib := range inbounds {
			for _, issue := range r.realityIssuesFor(ib, results) {
				alerts++
				msg := fmt.Sprintf("%s（节点 %s，入站 %s，端口 %d）",
					issue, node.Name, ib.Tag, ib.Port)
				r.logf("[WARN] %s", msg)
				r.recordRealityAlert(ctx, msg)
			}
		}
	}

	if alerts == 0 {
		r.logf("REALITY 巡检通过，未发现问题")
	} else {
		r.logf("REALITY 巡检完成，%d 项告警已记入操作日志", alerts)
	}
	return nil
}

// realityIssuesFor 检查单条入站的问题清单。
func (r *commandRunner) realityIssuesFor(ib struct {
	ID             int64
	Tag            string
	Port           int64
	StreamSettings string
}, results map[string]map[string]any) []string {
	var stream map[string]any
	if json.Unmarshal([]byte(ib.StreamSettings), &stream) != nil {
		return nil
	}
	realitySettings, _ := stream["realitySettings"].(map[string]any)
	if realitySettings == nil {
		return nil
	}

	dest, _ := realitySettings["dest"].(string)
	dest = strings.TrimSpace(dest)
	host := xrayHostOf(dest)
	row := results[host]

	var issues []string
	if row == nil {
		return []string{fmt.Sprintf("dest 探测未返回结果：%s", dest)}
	}

	if feasible, _ := row["feasible"].(bool); !feasible {
		reason, _ := row["reason"].(string)
		msg := "dest 无法完成 REALITY 所需的 TLS 握手"
		if reason = strings.TrimSpace(reason); reason != "" {
			msg += "：" + reason
		}
		issues = append(issues, msg)
	}

	if chainWarn, _ := row["chain_warn"].(bool); chainWarn {
		chainBytes := toInt64(row["chain_bytes"])
		issues = append(issues, fmt.Sprintf(
			"dest 证书链 %d 字节超过 REALITY 的 8192 上限，握手会被截断（Xray-core #6356）", chainBytes))
	}

	// SAN 检查
	var san []string
	if sanArr, ok := row["server_names"].([]any); ok {
		for _, name := range sanArr {
			if s, ok := name.(string); ok {
				s = strings.ToLower(strings.TrimSpace(s))
				if s != "" {
					san = append(san, s)
				}
			}
		}
	}

	if len(san) > 0 {
		// 回执里的 server_names 可能被 agent 截断（条目上限 / 回执体积上限）。截断时列表
		// 里没有某个名字 ≠ 证书里没有它，这时做「不在覆盖范围」的硬判定必然误报——
		// 以前把 24 条 SAN 的 www.amazon.com 截成 8 条，正确配置就是这样被判成故障的。
		// san_total 是截断前的真实条数；0 = 旧版 agent 不回带，总数未知，同样不能硬判。
		sanTotal := int(toInt64Value(row["san_total"]))
		incomplete := sanTotal == 0 || sanTotal > len(san)
		if incomplete {
			// 只剩「列表里有的名字一定没问题」这一半结论；列表外的名字给一条事实性
			// 提示（不是故障），说明本轮没比全，免得把校验盲区藏起来。
			var missed []string
			if snArr, ok := realitySettings["serverNames"].([]any); ok {
				missed = missingServerNames(snArr, san)
			}
			if len(missed) > 0 {
				if sanTotal == 0 {
					issues = append(issues, fmt.Sprintf(
						"dest 回执未带 SAN 总数（agent 过旧），巡检只比对了 %d 条，未比对 serverName %s",
						len(san), strings.Join(missed, "、")))
				} else {
					issues = append(issues, fmt.Sprintf(
						"dest 证书有 %d 条 SAN，巡检只拿到 %d 条（agent 回执截断），未比对 serverName %s",
						sanTotal, len(san), strings.Join(missed, "、")))
				}
			}
		} else if snArr, ok := realitySettings["serverNames"].([]any); ok {
			// 列表完整（len(server_names) >= san_total），缺失即证书真没覆盖，报故障
			for _, s := range missingServerNames(snArr, san) {
				issues = append(issues, fmt.Sprintf(
					"serverName %s 不在 dest 证书覆盖范围内（被分到它的用户会握手失败）", s))
			}
		}
	}

	return issues
}

// missingServerNames 返回入站 serverNames 里在 san（已小写去空白）中找不到的条目，
// 按入站声明顺序、去重。空串与重复项跳过。
func missingServerNames(snArr []any, san []string) []string {
	seen := make(map[string]bool, len(snArr))
	var missing []string
	for _, sn := range snArr {
		s, ok := sn.(string)
		if !ok {
			continue
		}
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		found := false
		for _, name := range san {
			if name == s {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, s)
		}
	}

	return missing
}

// probeRealityScan 下发 reality_scan 指令并等回执。
func (r *commandRunner) probeRealityScan(ctx context.Context, nodeID int64, targets []string, waitSec float64) map[string]map[string]any {
	// 创建指令
	payload, _ := json.Marshal(map[string]any{"targets": targets})
	var cmdID int64
	err := r.deps.DB.QueryRowContext(ctx,
		`INSERT INTO node_api_commands (node_id, user_id, action, payload, status, created_at, updated_at)
		 VALUES (?, NULL, 'reality_scan', ?, 'pending', ?, ?)`,
		nodeID, string(payload), r.now().UTC(), r.now().UTC()).Scan(&cmdID)
	if err != nil {
		// MySQL 不支持 RETURNING，用 LastInsertId
		res, ierr := r.deps.DB.ExecContext(ctx,
			`INSERT INTO node_api_commands (node_id, user_id, action, payload, status, created_at, updated_at)
			 VALUES (?, NULL, 'reality_scan', ?, 'pending', ?, ?)`,
			nodeID, string(payload), r.now().UTC(), r.now().UTC())
		if ierr != nil {
			return nil
		}
		cmdID, _ = res.LastInsertId()
	}

	deadline := time.Now().Add(time.Duration(waitSec * float64(time.Second)))
	for time.Now().Before(deadline) {
		var status, result string
		err := r.deps.DB.QueryRowContext(ctx,
			"SELECT status, COALESCE(result, '') FROM node_api_commands WHERE id = ?", cmdID).
			Scan(&status, &result)
		if err != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}

		if status == "failed" {
			if !strings.Contains(result, "unknown action") {
				r.logf("扫描失败 %s", result)
			}
			return nil
		}

		if status == "success" {
			var rows []map[string]any
			if json.Unmarshal([]byte(result), &rows) != nil {
				return nil
			}
			resultMap := map[string]map[string]any{}
			for _, row := range rows {
				target, _ := row["target"].(string)
				key := xrayHostOf(target)
				if key != "" {
					resultMap[key] = row
				}
			}
			return resultMap
		}

		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

// recordRealityAlert 写操作日志（同一问题 24h 内只留一条）。
func (r *commandRunner) recordRealityAlert(ctx context.Context, message string) {
	var n int
	err := r.deps.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM operation_logs
		 WHERE type = 'system' AND action = 'REALITY 巡检' AND error = ?
		   AND created_at >= ?`,
		message, r.now().UTC().AddDate(0, 0, -1)).Scan(&n)
	if err != nil || n > 0 {
		return
	}

	_, _ = r.deps.DB.ExecContext(ctx,
		`INSERT INTO operation_logs (type, actor_name, action, method, path, status, error, created_at)
		 VALUES ('system', 'reality-audit', 'REALITY 巡检', 'CLI', 'xray:reality-audit', 500, ?, ?)`,
		message, r.now().UTC())
}

// ── 辅助工具 ───────────────────────────────────────────────────────

// xrayHostOf 提取 dest 中的主机名。
func xrayHostOf(dest string) string {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return ""
	}
	// 去掉端口
	if idx := strings.LastIndex(dest, ":"); idx > 0 {
		host := dest[:idx]
		// 确保不是 IPv6
		if !strings.Contains(host, "]") {
			return strings.ToLower(host)
		}
	}
	return strings.ToLower(dest)
}
