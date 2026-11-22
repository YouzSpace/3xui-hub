package wireguard

import (
	"encoding/base64"
	"strings"

	p "controlhub-server/internal/protocol"
)

// ParsePeers 把「每行一条：公钥 [允许IP,...]」解析成内核的 peers 数组。
// 公钥必须是 32 字节 base64（WireGuard key）；允许IP 可省（服务端仍能握手，
// 只是没有回程路由，实际用不起来）。第二个返回值 false = 公钥格式非法。
func ParsePeers(raw string) (p.Array, bool) {
	peers := p.Array{}
	for _, line := range splitLines(raw) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !isKey(fields[0]) {
			return nil, false
		}
		peer := p.New("publicKey", fields[0])
		allowed := p.Array{}
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

// isKey 判断是否为 32 字节的 base64 密钥（带/不带填充都收）。
func isKey(s string) bool {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if raw, err := enc.DecodeString(s); err == nil && len(raw) == 32 {
			return true
		}
	}
	return false
}

// splitLines 复刻 preg_split('/\r\n|\r|\n/', $text)。
func splitLines(text string) []string {
	if text == "" {
		return []string{""}
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.Split(text, "\n")
}
