// Package security 是流安全层「security → tlsSettings/realitySettings 段」的公共实现。
//
// 当前从 handler/admin 的出站 stream 组装中抽出（tls / reality）。行为逐字节不变
// （由 handler/admin 的出站 stream 特征测试守护）。
package security

import (
	"strings"

	"controlhub-server/internal/phpjson"
)

// ApplyTLS 组装 tlsSettings 并写入 stream。alpnH3 为真时追加 alpn:[h3]（hysteria 需要）。
func ApplyTLS(stream *phpjson.Object, serverName string, alpnH3 bool) {
	tls := phpjson.New()
	if sni := strings.TrimSpace(serverName); sni != "" {
		tls.Put("serverName", sni)
	}
	if alpnH3 {
		tls.Put("alpn", phpjson.Array{"h3"})
	}
	stream.Put("tlsSettings", tls)
}

// ApplyReality 组装 realitySettings 并写入 stream。fingerprint 空则默认 chrome（历史行为）。
func ApplyReality(stream *phpjson.Object, serverName, publicKey, shortID, fingerprint, spiderX string) {
	reality := phpjson.New()
	if v := strings.TrimSpace(serverName); v != "" {
		reality.Put("serverName", v)
	}
	if v := strings.TrimSpace(publicKey); v != "" {
		reality.Put("publicKey", v)
	}
	if v := strings.TrimSpace(shortID); v != "" {
		reality.Put("shortId", v)
	}
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		fp = "chrome"
	}
	reality.Put("fingerprint", fp)
	if v := strings.TrimSpace(spiderX); v != "" {
		reality.Put("spiderX", v)
	}
	stream.Put("realitySettings", reality)
}
