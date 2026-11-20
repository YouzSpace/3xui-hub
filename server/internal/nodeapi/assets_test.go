package nodeapi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

// 内核 zip / agent 二进制都不进 git（#138 / #116）：面板本地缺文件时按 manifest 的 url
// 下载 → sha256 复验 → 落盘缓存。这里用 httptest 顶替上游 Release，验证三条路径：
//  1. 本地已有一致的文件 → 直接下发，不联网（离线/内网部署照旧）；
//  2. 本地缺失 → 自动下载 + 缓存，第二次请求命中本地不再重复下载；
//  3. 上游内容与 manifest 的 sha256 不符 → 拒发（500）且不留半截缓存。
func TestBinaryDownloadOnDemand(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const version = "26.3.27"
	const fileName = "Xray-linux-64.zip"
	payload := []byte("fake-xray-zip-payload")
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])

	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write(payload)
	}))
	defer upstream.Close()

	// setup 造一个面板资产目录 + manifest，返回根目录和挂好路由的引擎。
	// sha 传空则用真实 payload 的 sha256（模拟正常上游）；传别的值模拟被投毒的上游。
	setup := func(t *testing.T, sha string) (string, *gin.Engine) {
		t.Helper()
		if sha == "" {
			sha = sumHex
		}
		root := t.TempDir()
		manifest := fmt.Sprintf(
			`{"default":%q,"versions":{%q:{"assets":[{"arch":"linux-64","file":%q,"sha256":%q,"size":%d,"url":%q}]}}}`,
			version, version, fileName, sha, len(payload), upstream.URL)
		if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o644); err != nil {
			t.Fatalf("写 manifest 失败: %v", err)
		}
		r := gin.New()
		NewAssetHandler(root, "").RegisterRoutes(r)
		return root, r
	}

	get := func(r *gin.Engine) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/node-bin/"+version+"/"+fileName, nil))
		return w
	}

	t.Run("本地已有一致的文件不联网", func(t *testing.T) {
		root, r := setup(t, "")
		local := filepath.Join(root, "node-bin", version, fileName)
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

		local := filepath.Join(root, "node-bin", version, fileName)
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

		local := filepath.Join(root, "node-bin", version, fileName)
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
