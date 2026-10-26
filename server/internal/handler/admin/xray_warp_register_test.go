package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"controlhub-server/internal/driver/xray"
)

// 首次注册时 existing 为 nil：账户必须带上路由里的 nodeID，否则落库撞外键约束
// （node_warp_accounts.node_id_foreign），面板只看到「服务器错误」。
func TestWarpRegisterCarriesNodeIDOnFirstRegister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reg" {
			t.Errorf("path = %q, want /reg", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "dev-1",
			"token": "tok-1",
			"config": {
				"client_id": "aGVsbG8=",
				"interface": {"addresses": {"v4": "172.16.0.2", "v6": "2606:4700:110::1"}},
				"peers": [{
					"public_key": "peer-pub",
					"endpoint": {"v4": "162.159.192.9:0", "host": "engage.cloudflareclient.com:2408", "ports": [2408]}
				}]
			},
			"account": {"license": "lic-lic-lic-lic-lic-lic-lic"}
		}`)
	}))
	defer srv.Close()

	ops := NewRealNodeOps(NodeConfig{}, nil, nil)
	ws := xray.NewWarpService()
	ws.APIBase = srv.URL
	ws.Now = func() time.Time { return time.Unix(0, 0).UTC() }
	ops.warp = ws

	acc, err := ops.WarpRegister(context.Background(), 40, "KR01", nil)
	if err != nil {
		t.Fatalf("WarpRegister() error = %v", err)
	}
	if acc.NodeID != 40 {
		t.Fatalf("NodeID = %d, want 40 (首次注册必须带回 node_id)", acc.NodeID)
	}
	if acc.DeviceID != "dev-1" || acc.PeerEndpoint != "162.159.192.9:2408" {
		t.Fatalf("账户字段不对: %+v", acc)
	}
}
