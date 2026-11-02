package admin

// GeodataService 真实实现：对齐 backend/app/Services/GeodataService.php。
// 面板从白名单源下载 geoip.dat/geosite.dat → 落盘 ../assets/geo/
// → 分发到 enabled xray 节点（node_api_commands 指令通道）。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// geoFileSources 是白名单下载源（逐字对齐 PHP GeodataService::FILES）。
var geoFileSources = map[string]string{
	"geoip.dat":   "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geoip.dat",
	"geosite.dat": "https://github.com/Loyalsoldier/v2ray-rules-dat/releases/latest/download/geosite.dat",
}

// geoMinBytes 是单文件大小下限（100KB）。
const geoMinBytes = 100 * 1024

// geodataServiceImpl 实现 GeodataService 接口。
type geodataServiceImpl struct {
	db      *sql.DB
	dir     string
	httpCli *http.Client
	now     func() time.Time
}

// NewGeodataService 构造 Geo 数据服务。geoDir 空则用 ../assets/geo。
func NewGeodataService(db *sql.DB, geoDir string, now func() time.Time) GeodataService {
	if geoDir == "" {
		geoDir = filepath.Join("..", "assets", "geo")
	}
	if now == nil {
		now = time.Now
	}
	return &geodataServiceImpl{
		db:      db,
		dir:     geoDir,
		httpCli: &http.Client{Timeout: 300 * time.Second},
		now:     now,
	}
}

func (g *geodataServiceImpl) filePath(name string) string {
	return filepath.Join(g.dir, name)
}

func (g *geodataServiceImpl) manifestPath() string {
	return filepath.Join(g.dir, "manifest.json")
}

// geoManifestEntry 是 manifest.json 里单文件的记录。
type geoManifestEntry struct {
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	UpdatedAt string `json:"updated_at"`
	CheckedAt string `json:"checked_at"`
}

func (g *geodataServiceImpl) readManifest() map[string]geoManifestEntry {
	data, err := os.ReadFile(g.manifestPath())
	if err != nil {
		return map[string]geoManifestEntry{}
	}
	var m map[string]geoManifestEntry
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]geoManifestEntry{}
	}
	return m
}

func (g *geodataServiceImpl) writeManifest(m map[string]geoManifestEntry) {
	data, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(g.manifestPath(), data, 0644)
}

// Status 对齐 PHP status()。
func (g *geodataServiceImpl) Status(ctx context.Context) ([]GeodataFile, error) {
	manifest := g.readManifest()
	names := []string{"geoip.dat", "geosite.dat"}
	files := make([]GeodataFile, 0, len(names))

	for _, name := range names {
		path := g.filePath(name)
		meta := manifest[name]
		fi, err := os.Stat(path)
		exists := err == nil

		f := GeodataFile{Name: name, Exists: exists}
		if exists {
			f.Size = fi.Size()
			f.SHA256 = fileSHA256(path)
		}

		if meta.UpdatedAt != "" {
			f.UpdatedAt = &meta.UpdatedAt
		} else if exists {
			t := fi.ModTime().Format(time.RFC3339)
			f.UpdatedAt = &t
		}
		if meta.CheckedAt != "" {
			f.CheckedAt = &meta.CheckedAt
		}
		files = append(files, f)
	}
	return files, nil
}

// Update 对齐 PHP update() + updateOne()。
func (g *geodataServiceImpl) Update(ctx context.Context) ([]GeodataUpdateResult, error) {
	if err := os.MkdirAll(g.dir, 0755); err != nil {
		return []GeodataUpdateResult{
			{Name: "geoip.dat", Status: "failed", Message: "创建目录失败: " + g.dir},
			{Name: "geosite.dat", Status: "failed", Message: "创建目录失败: " + g.dir},
		}, nil
	}

	names := []string{"geoip.dat", "geosite.dat"}
	results := make([]GeodataUpdateResult, 0, len(names))
	for _, name := range names {
		results = append(results, g.updateOne(ctx, name, geoFileSources[name]))
	}
	return results, nil
}

func (g *geodataServiceImpl) updateOne(ctx context.Context, name, url string) GeodataUpdateResult {
	target := g.filePath(name)
	tmp := target + ".tmp"

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "ControlHub-Geodata/1.0")
	if fi, err := os.Stat(target); err == nil {
		req.Header.Set("If-Modified-Since", fi.ModTime().UTC().Format("Mon, 02 Jan 2006 15:04:05")+" GMT")
	}

	resp, err := g.httpCli.Do(req)
	if err != nil {
		g.touchChecked(name)
		return GeodataUpdateResult{Name: name, Status: "failed", Message: "下载失败: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode == 304 {
		g.touchChecked(name)
		if fi, err := os.Stat(target); err == nil {
			return GeodataUpdateResult{Name: name, Status: "unchanged", Size: fi.Size(), SHA256: fileSHA256(target)}
		}
		return GeodataUpdateResult{Name: name, Status: "unchanged"}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return GeodataUpdateResult{Name: name, Status: "failed", Message: fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return GeodataUpdateResult{Name: name, Status: "failed", Message: "下载失败: " + err.Error()}
	}

	if len(body) < geoMinBytes {
		return GeodataUpdateResult{Name: name, Status: "failed", Message: fmt.Sprintf("内容过小（%d 字节），疑似错误响应，已放弃", len(body))}
	}

	h := sha256.Sum256(body)
	sha := hex.EncodeToString(h[:])

	// 已是同一份 → unchanged（保持 mtime）
	if fi, err := os.Stat(target); err == nil {
		if fileSHA256(target) == sha {
			g.touchChecked(name)
			return GeodataUpdateResult{Name: name, Status: "unchanged", Size: fi.Size(), SHA256: sha}
		}
	}

	if err := os.WriteFile(tmp, body, 0644); err != nil {
		return GeodataUpdateResult{Name: name, Status: "failed", Message: "写入临时文件失败"}
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return GeodataUpdateResult{Name: name, Status: "failed", Message: "替换文件失败"}
	}

	g.recordUpdated(name, int64(len(body)), sha)
	return GeodataUpdateResult{Name: name, Status: "updated", Size: int64(len(body)), SHA256: sha}
}

// DispatchToNodes 对齐 PHP dispatchToNodes()。
func (g *geodataServiceImpl) DispatchToNodes(ctx context.Context) (queued, skipped int, missing bool, err error) {
	type fileEntry struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
		URL    string `json:"url"`
	}
	var files []fileEntry
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		path := g.filePath(name)
		if fi, err := os.Stat(path); err == nil {
			files = append(files, fileEntry{
				Name:   name,
				Size:   fi.Size(),
				SHA256: fileSHA256(path),
				URL:    "/node-bin/geo/" + name,
			})
		}
	}
	if len(files) == 0 {
		return 0, 0, true, nil
	}

	payload, _ := json.Marshal(map[string]any{"files": files})

	rows, err := g.db.QueryContext(ctx,
		`SELECT id FROM nodes WHERE enabled = 1 AND driver_type = 'xray' ORDER BY id`)
	if err != nil {
		return 0, 0, false, err
	}
	defer rows.Close()

	var nodeIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			nodeIDs = append(nodeIDs, id)
		}
	}

	for _, nodeID := range nodeIDs {
		var count int64
		g.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM node_api_commands
			 WHERE node_id = ? AND action = 'geodata_sync'
			   AND status IN ('pending', 'acknowledged')`, nodeID).Scan(&count)
		if count > 0 {
			skipped++
			continue
		}
		_, err := g.db.ExecContext(ctx,
			`INSERT INTO node_api_commands (node_id, user_id, action, payload, status, created_at, updated_at)
			 VALUES (?, NULL, 'geodata_sync', ?, 'pending', ?, ?)`,
			nodeID, string(payload), g.now().UTC(), g.now().UTC())
		if err == nil {
			queued++
		}
	}
	return queued, skipped, false, nil
}

// NodeStatuses 对齐 PHP nodeStatuses()。
func (g *geodataServiceImpl) NodeStatuses(ctx context.Context) ([]GeodataNodeStatus, error) {
	rows, err := g.db.QueryContext(ctx,
		`SELECT id, name, enabled FROM nodes WHERE driver_type = 'xray' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type nodeRow struct {
		ID      uint64
		Name    string
		Enabled bool
	}
	var nodes []nodeRow
	for rows.Next() {
		var n nodeRow
		if err := rows.Scan(&n.ID, &n.Name, &n.Enabled); err == nil {
			nodes = append(nodes, n)
		}
	}

	result := make([]GeodataNodeStatus, 0, len(nodes))
	for _, n := range nodes {
		var status, resultStr sql.NullString
		var finishedAt sql.NullTime
		g.db.QueryRowContext(ctx,
			`SELECT status, result, updated_at FROM node_api_commands
			 WHERE node_id = ? AND action = 'geodata_sync'
			 ORDER BY id DESC LIMIT 1`, n.ID).
			Scan(&status, &resultStr, &finishedAt)

		st := GeodataNodeStatus{
			NodeID:   n.ID,
			NodeName: n.Name,
			Enabled:  n.Enabled,
		}
		if status.Valid {
			s := status.String
			st.Status = &s
		}
		if resultStr.Valid {
			r := resultStr.String
			st.Result = &r
		}
		if finishedAt.Valid {
			t := finishedAt.Time
			st.FinishedAt = &t
		}
		result = append(result, st)
	}
	return result, nil
}

// recordUpdated 更新 manifest。
func (g *geodataServiceImpl) recordUpdated(name string, size int64, sha string) {
	m := g.readManifest()
	nowStr := g.now().UTC().Format(time.RFC3339)
	m[name] = geoManifestEntry{Size: size, SHA256: sha, UpdatedAt: nowStr, CheckedAt: nowStr}
	g.writeManifest(m)
}

// touchChecked 只更新 checked_at。
func (g *geodataServiceImpl) touchChecked(name string) {
	m := g.readManifest()
	nowStr := g.now().UTC().Format(time.RFC3339)
	entry := m[name]
	entry.CheckedAt = nowStr
	m[name] = entry
	g.writeManifest(m)
}

// fileSHA256 计算文件 SHA-256（失败返回空串）。
func fileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
