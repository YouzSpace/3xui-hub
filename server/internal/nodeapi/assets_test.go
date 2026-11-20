package nodeapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

// manifestVersionFloor 是 issue #139 的内核下限：HY2(hysteria) 入站流量要 ≥26.4.15
// 才会计入用户级统计，低于它用户的 HY2 流量就白跑了。
const manifestVersionFloor = "26.4.15"

// sha256HexPattern 是 manifest 里 sha256 的合法形状（小写十六进制 64 位）。
var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// repoManifest 是仓库里真实 assets/manifest.json 的测试视图（只取内核版本段）。
type repoManifest struct {
	Default  string `json:"default"`
	Versions map[string]struct {
		Assets []manifestAsset `json:"assets"`
	} `json:"versions"`
}

// loadRepoManifest 读仓库真实的 assets/manifest.json（包目录往上三级是仓库根），
// 同时返回按段解析的原始 JSON，便于直接喂给生产代码里的 findAsset。
func loadRepoManifest(t *testing.T) (repoManifest, map[string]json.RawMessage) {
	t.Helper()
	p := filepath.Join("..", "..", "..", "assets", "manifest.json")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", p, err)
	}
	var m repoManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析 %s 失败: %v", p, err)
	}
	if len(m.Versions) == 0 {
		t.Fatalf("%s 的 versions 为空", p)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sections); err != nil {
		t.Fatalf("按段解析 %s 失败: %v", p, err)
	}
	return m, sections
}

// parseVersion 把 "26.4.15" 拆成三段数字。
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(strings.TrimSpace(v), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// atLeastVersion 判断 v >= want；任一侧不是三段数字版本号都按「不满足」处理。
func atLeastVersion(v, want string) bool {
	a, okA := parseVersion(v)
	b, okB := parseVersion(want)
	if !okA || !okB {
		return false
	}
	for i := range b {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// sortedVersions 返回 manifest 里的版本号（排序后测试输出稳定）。
func sortedVersions(m repoManifest) []string {
	out := make([]string, 0, len(m.Versions))
	for v := range m.Versions {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// TestManifestKernelVersions 校验仓库真实 manifest 的内核版本段（issue #139）：
// default 必须 ≥26.4.15、必须有对应条目且字段齐全、旧版本保留供回滚。
func TestManifestKernelVersions(t *testing.T) {
	repo, sections := loadRepoManifest(t)

	if _, ok := repo.Versions[repo.Default]; !ok {
		t.Fatalf("default = %q 在 versions 里没有条目", repo.Default)
	}
	if !atLeastVersion(repo.Default, manifestVersionFloor) {
		t.Fatalf("default = %q，低于 issue #139 要求的内核下限 %s：HY2 入站流量不会计入用户",
			repo.Default, manifestVersionFloor)
	}
	// 回滚目标：26.3.27 必须留着（新内核出问题时能切回去）。
	if _, ok := repo.Versions["26.3.27"]; !ok {
		t.Fatalf("versions 里缺少 26.3.27 回滚条目")
	}

	for _, version := range sortedVersions(repo) {
		entry := repo.Versions[version]
		if len(entry.Assets) == 0 {
			t.Fatalf("%s: assets 为空", version)
		}
		for _, a := range entry.Assets {
			if a.Arch == "" || a.File == "" || a.SHA256 == "" || a.Size <= 0 || a.URL == "" {
				t.Fatalf("%s/%s: 字段不全（file=%q sha256=%q size=%d url=%q）",
					version, a.Arch, a.File, a.SHA256, a.Size, a.URL)
			}
			if !sha256HexPattern.MatchString(a.SHA256) {
				t.Fatalf("%s/%s: sha256 形状非法: %q", version, a.Arch, a.SHA256)
			}
			// 地址必须是这个版本自己的 Release 资产：抄错版本号等于把旧内核发给新节点。
			if !strings.Contains(a.URL, "/v"+version+"/") {
				t.Fatalf("%s/%s: url 与版本号不符: %q", version, a.Arch, a.URL)
			}
		}
		// 生产代码按「版本 + arch」查表（findAsset）：查不到就是节点安装 404。
		if findAsset(sections, "versions", version, "linux-64") == nil {
			t.Fatalf("%s: findAsset 查不到 linux-64 资产（节点安装会 404）", version)
		}
	}

	// 26.4.15 的 sha256 / size 必须与上游 Release 资产逐字一致（已用 GitHub API 的
	// assets[].digest + size 核对）：面板据此校验下载内容，抄错一个字符就等于所有节点装不上内核。
	pinned := findAsset(sections, "versions", manifestVersionFloor, "linux-64")
	if pinned == nil {
		t.Fatalf("%s 缺少 linux-64 条目", manifestVersionFloor)
	}
	const (
		wantSHA  = "8626549737eb4fdb9d6a3c2131b046977c6279fbfbbaa676ec148ebccb71ecfa"
		wantSize = int64(20976808)
	)
	if pinned.SHA256 != wantSHA || pinned.Size != wantSize {
		t.Fatalf("%s 资产与上游不一致: sha256=%q size=%d，期望 sha256=%q size=%d",
			manifestVersionFloor, pinned.SHA256, pinned.Size, wantSHA, wantSize)
	}
}

// 内核 zip / agent 二进制都不进 git（#138 / #116）：面板本地缺文件时按 manifest 的 url
// 下载 → sha256 复验 → 落盘缓存。版本表直接取仓库真实的 manifest，所以 default 升到哪个
// 内核（#139 → 26.4.15），这条下载路径就跟着哪个版本跑。这里用 httptest 顶替上游 Release，
// 验证三条路径：
//  1. 本地已有一致的文件 → 直接下发，不联网（离线/内网部署照旧）；
//  2. 本地缺失 → 自动下载 + 缓存，第二次请求命中本地不再重复下载；
//  3. 上游内容与 manifest 的 sha256 不符 → 拒发（500）且不留半截缓存。
func TestBinaryDownloadOnDemand(t *testing.T) {
	gin.SetMode(gin.TestMode)

	payload := []byte("fake-xray-zip-payload")
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	repo, _ := loadRepoManifest(t)

	for _, version := range sortedVersions(repo) {
		version := version
		// file / arch 用真实清单里的命名（node-install.sh 按 manifest 的 file 拼 URL，改错就 404）。
		asset := repo.Versions[version].Assets[0]

		t.Run(version, func(t *testing.T) {
			// setup 造一个面板资产目录 + manifest，返回根目录和挂好路由的引擎。
			// sha 传空则用真实 payload 的 sha256（模拟正常上游）；传别的值模拟被投毒的上游。
			// url 指向本地 httptest，不碰外网。
			setup := func(t *testing.T, sha string) (string, *gin.Engine) {
				t.Helper()
				if sha == "" {
					sha = sumHex
				}
				root := t.TempDir()
				manifest := fmt.Sprintf(
					`{"default":%q,"versions":{%q:{"assets":[{"arch":%q,"file":%q,"sha256":%q,"size":%d,"url":%q}]}}}`,
					version, version, asset.Arch, asset.File, sha, len(payload), upstream.URL)
				if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
					t.Fatalf("写 manifest 失败: %v", err)
				}
				r := gin.New()
				NewAssetHandler(root, "").RegisterRoutes(r)
				return root, r
			}

			get := func(r *gin.Engine) *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/node-bin/"+version+"/"+asset.File, nil))
				return w
			}

			t.Run("本地已有一致的文件不联网", func(t *testing.T) {
				root, r := setup(t, "")
				local := filepath.Join(root, "node-bin", version, asset.File)
				if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
					t.Fatalf("建目录失败: %v", err)
				}
				if err := os.WriteFile(local, payload, 0o644); err != nil {
					t.Fatalf("预置本地文件失败: %v", err)
				}
				atomic.StoreInt32(&hits, 0)

				w := get(r)
				if w.Code != http.StatusOK {
					t.Fatalf("状态码 = %d，期望 200，body=%s", w.Code, w.Body.String())
				}
				if got := w.Body.Bytes(); string(got) != string(payload) {
					t.Fatalf("响应体 = %q，期望 %q", got, payload)
				}
				if n := atomic.LoadInt32(&hits); n != 0 {
					t.Fatalf("本地命中却访问了上游 %d 次", n)
				}
			})

			t.Run("本地缺失自动下载并缓存", func(t *testing.T) {
				root, r := setup(t, "")
				atomic.StoreInt32(&hits, 0)

				w := get(r)
				if w.Code != http.StatusOK {
					t.Fatalf("状态码 = %d，期望 200，body=%s", w.Code, w.Body.String())
				}
				if got := w.Body.Bytes(); string(got) != string(payload) {
					t.Fatalf("响应体 = %q，期望 %q", got, payload)
				}
				if n := atomic.LoadInt32(&hits); n != 1 {
					t.Fatalf("上游被访问 %d 次，期望 1 次", n)
				}

				local := filepath.Join(root, "node-bin", version, asset.File)
				cached, err := os.ReadFile(local)
				if err != nil {
					t.Fatalf("下载后未落盘缓存: %v", err)
				}
				if string(cached) != string(payload) {
					t.Fatalf("缓存内容 = %q，期望 %q", cached, payload)
				}

				// 第二次请求必须命中本地缓存，不再重复下载。
				if w := get(r); w.Code != http.StatusOK {
					t.Fatalf("第二次请求状态码 = %d，期望 200", w.Code)
				}
				if n := atomic.LoadInt32(&hits); n != 1 {
					t.Fatalf("第二次请求又下载了一次，上游累计 %d 次", n)
				}
			})

			t.Run("上游内容与 sha256 不符拒绝下发", func(t *testing.T) {
				root, r := setup(t, strings.Repeat("0", 64))
				atomic.StoreInt32(&hits, 0)

				w := get(r)
				if w.Code != http.StatusInternalServerError {
					t.Fatalf("状态码 = %d，期望 500，body=%s", w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "checksum mismatch") {
					t.Fatalf("响应未提示校验失败: %s", w.Body.String())
				}

				local := filepath.Join(root, "node-bin", version, asset.File)
				if _, err := os.Stat(local); !os.IsNotExist(err) {
					t.Fatalf("校验失败却落了盘: stat err = %v", err)
				}
				// 临时文件也不能留在资产目录里。
				entries, err := os.ReadDir(filepath.Dir(local))
				if err != nil && !os.IsNotExist(err) {
					t.Fatalf("读目录失败: %v", err)
				}
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".asset-download-") {
						t.Fatalf("残留临时文件: %s", e.Name())
					}
				}
			})
		})
	}
}

// TestAgentBinaryDownloadOnDemand 确认 agent 二进制与内核 zip 共用同一套下载逻辑
// （helpers 改动时两处一起坏、一起好）。
func TestAgentBinaryDownloadOnDemand(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const fileName = "controlhub-agent-linux-64"
	payload := []byte("fake-agent-binary")
	sum := sha256.Sum256(payload)

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	root := t.TempDir()
	manifest := fmt.Sprintf(
		`{"agent":{"version":"1.2.3","assets":[{"arch":"linux-64","file":%q,"sha256":%q,"size":%d,"url":%q}]}}`,
		fileName, hex.EncodeToString(sum[:]), len(payload), upstream.URL)
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("写 manifest 失败: %v", err)
	}

	r := gin.New()
	NewAssetHandler(root, "").RegisterRoutes(r)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/node-bin/agent/linux-64", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != string(payload) {
		t.Fatalf("响应体 = %q，期望 %q", w.Body.String(), payload)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("上游被访问 %d 次，期望 1 次", n)
	}
	if _, err := os.Stat(filepath.Join(root, "node-bin", "agent", fileName)); err != nil {
		t.Fatalf("下载后未落盘缓存: %v", err)
	}
}
