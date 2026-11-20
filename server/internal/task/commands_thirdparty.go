package task

// 第三方公告/链接抓取（third-party:fetch）。
// 从 commands.go 按职责拆出（issue #137）。

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// runThirdPartyFetch 对齐 FetchThirdPartySubsCommand::handle()。
// 抓取所有启用的第三方订阅 → 解析 → 缓存节点链接。
func (r *commandRunner) runThirdPartyFetch(ctx context.Context) error {
	// 查启用的订阅
	rows, err := r.deps.DB.QueryContext(ctx,
		`SELECT id, name, url FROM third_party_subs WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("查询第三方订阅失败: %w", err)
	}
	type subRow struct {
		ID   int64
		Name string
		URL  string
	}
	var subs []subRow
	for rows.Next() {
		var s subRow
		if err := rows.Scan(&s.ID, &s.Name, &s.URL); err != nil {
			rows.Close()
			return err
		}
		subs = append(subs, s)
	}
	rows.Close()

	if len(subs) == 0 {
		r.logf("没有启用的第三方订阅，跳过")
		return nil
	}

	r.logf("%s 开始拉取 %d 条第三方订阅...", r.now().Format("2006-01-02 15:04:05"), len(subs))

	for _, sub := range subs {
		// 拉取订阅（HTTP fetch + 解析）
		links, fetchErr := r.fetchThirdPartyLinks(ctx, sub.URL)
		now := r.now().UTC()
		if fetchErr != nil || len(links) == 0 {
			msg := ""
			if fetchErr != nil {
				msg = fetchErr.Error()
			} else {
				msg = "拉取成功但未解析到任何节点链接"
			}
			// 保留上次成功缓存，只记错误
			_, _ = r.deps.DB.ExecContext(ctx,
				`UPDATE third_party_subs SET last_status = 'failed', last_error = ?, last_fetched_at = ?, updated_at = ?
				 WHERE id = ?`, truncateStr(msg, 500), now, now, sub.ID)
			r.logf("  %s: 失败（%s）", sub.Name, msg)
			continue
		}

		cached := strings.Join(links, "\n")
		_, err := r.deps.DB.ExecContext(ctx,
			`UPDATE third_party_subs SET last_status = 'success', last_error = NULL,
			 node_count = ?, cached_links = ?, last_fetched_at = ?, updated_at = ?
			 WHERE id = ?`, len(links), cached, now, now, sub.ID)
		if err != nil {
			r.logf("  %s: 保存失败（%v）", sub.Name, err)
			continue
		}
		r.logf("  %s: 成功（%d 节点）", sub.Name, len(links))
	}

	r.logf("拉取完成")
	return nil
}

// fetchThirdPartyLinks 拉取第三方订阅并解析节点链接。
func (r *commandRunner) fetchThirdPartyLinks(ctx context.Context, url string) ([]string, error) {
	// 订阅内容通常是 base64 编码的 URI 列表
	body, err := httpGet(ctx, url, 15*time.Second)
	if err != nil {
		return nil, err
	}

	// 尝试 base64 解码
	decoded := tryBase64Decode(string(body))
	if decoded == "" {
		decoded = string(body)
	}

	var links []string
	for _, line := range strings.Split(decoded, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "vmess://") || strings.HasPrefix(line, "vless://") ||
			strings.HasPrefix(line, "trojan://") || strings.HasPrefix(line, "ss://") ||
			strings.HasPrefix(line, "ssr://") || strings.HasPrefix(line, "hysteria2://") ||
			strings.HasPrefix(line, "hy2://") {
			links = append(links, line)
		}
	}
	return links, nil
}

// ── 9. xray:warp-rotate ────────────────────────────────────────────
