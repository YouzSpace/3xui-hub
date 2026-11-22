// Package security 是安全层公共组装：tls / reality 段（map/值 进、JSON 出）。
package security

import (
	"strings"

	p "controlhub-server/internal/protocol"
)

// TLS 返回 tlsSettings 段 JSON。
func TLS(serverName string, alpnH3 bool) []byte {
	t := p.New()
	if v := strings.TrimSpace(serverName); v != "" { t.Put("serverName", v) }
	if alpnH3 { t.Put("alpn", p.Array{"h3"}) }
	return p.Encode(p.New("tlsSettings", t))
}

// Reality 返回 realitySettings 段 JSON。
func Reality(serverName, publicKey, shortID, fingerprint, spiderX string) []byte {
	r := p.New()
	if v := strings.TrimSpace(serverName); v != "" { r.Put("serverName", v) }
	if v := strings.TrimSpace(publicKey); v != "" { r.Put("publicKey", v) }
	if v := strings.TrimSpace(shortID); v != "" { r.Put("shortId", v) }
	fp := strings.TrimSpace(fingerprint); if fp == "" { fp = "chrome" }
	r.Put("fingerprint", fp)
	if v := strings.TrimSpace(spiderX); v != "" { r.Put("spiderX", v) }
	return p.Encode(p.New("realitySettings", r))
}
