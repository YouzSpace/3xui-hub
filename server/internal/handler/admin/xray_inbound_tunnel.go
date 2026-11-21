package admin

// tunnel（dokodemo-door）与 wireguard 入站的专有字段解析。
// 从 xray_inbound.go 按职责拆出（issue #137）。

import (
	"encoding/base64"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// defaultTunnelNetwork 是 dokodemo-door 的缺省 L4 网络（3x-ui 表单同款）。
const defaultTunnelNetwork = "tcp,udp"

// tunnelNetwork 读 tunnel 入站的 L4 网络（落 settings.network 的那个值）。
// 第二个返回值 false = 值非法（合法值只有 tcp / udp / tcp,udp，缺省 tcp,udp）。
func tunnelNetwork(input map[string]any) (string, bool) {
	raw, _ := inputString(input["tunnel_network"])
	switch strings.TrimSpace(raw) {
	case "":
		return defaultTunnelNetwork, true
	case "tcp", "udp", "tcp,udp":
		return strings.TrimSpace(raw), true
	}
	return "", false
}

// wireGuardPeers 把「每行一条：公钥 [允许IP,...]」解析成内核的 peers 数组。
// 公钥必须是 32 字节 base64（WireGuard key）；允许IP 可省（服务端仍能握手，
// 只是没有回程路由，实际用不起来）。
func wireGuardPeers(raw string) (jsArr, bool) {
	peers := jsArr{}
	for _, line := range splitLines(raw) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !isWireGuardKey(fields[0]) {
			return nil, false
		}
		peer := jo("publicKey", fields[0])
		allowed := jsArr{}
		for _, chunk := range fields[1:] {
			for _, ip := range strings.Split(chunk, ",") {
				if ip = strings.TrimSpace(ip); ip != "" {
					allowed = append(allowed, ip)
				}
			}
		}
		if len(allowed) > 0 {
			peer.Put("allowedIPs", allowed)
		}
		peers = append(peers, peer)
	}
	return peers, true
}

// isWireGuardKey 判断是否为 32 字节的 base64 密钥（带/不带填充都收，
// 客户端导出的多为带填充的 44 字符形态）。
func isWireGuardKey(s string) bool {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if raw, err := enc.DecodeString(s); err == nil && len(raw) == 32 {
			return true
		}
	}
	return false
}

// wireGuardPublicKey 由 WireGuard 私钥推导公钥（同格式：标准 base64 带填充）。
// 服务端私钥只落库、读接口只回这个公钥 —— 管理员拿它配客户端。
func wireGuardPublicKey(privateKey string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil || len(raw) != 32 {
		if raw, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(privateKey)); err != nil || len(raw) != 32 {
			return ""
		}
	}
	pk, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(pk)
}
