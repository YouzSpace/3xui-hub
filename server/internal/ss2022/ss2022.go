// Package ss2022 是 Shadowsocks-2022（SIP022）多用户口径的唯一来源：
// method 判定、每用户 PSK 派生、分享链接 userinfo。
//
// 为什么必须只有一个来源：渲染（nodeapi/render.go、driver/xray/config.go）与
// 订阅（driver/xray/subscription.go、subscription/xrayuri.go）各算一次 key。
// 两边一旦算出不同的 key，用户拿到的链接连不上自己那个 client，内核也不报错，
// 只是握不上手 —— 排查起来毫无线索。
//
// 实测（xray 26.3.27，本机真跑）：
//   - 入站 settings = {method, password: 服务端 PSK, clients: [{password: 用户 PSK, email}]}
//     才能按 email 统计流量；clients 的 key 长度不对时内核直接
//     "proxy/shadowsocks_2022: create service > bad key" 拒载。
//   - 客户端凭据必须是 "服务端PSK:用户PSK"（只给用户 PSK 连不上 —— 服务端要先用
//     主密钥解出身份头）。这也决定了链接 userinfo 的形态，见 UserInfo。
package ss2022

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

const methodPrefix = "2022-blake3-"

// IsMultiUser 判断该 method 是否按用户维度发凭据（每用户独立 PSK）。
//
// 只有 2022-blake3-aes-128-gcm / 2022-blake3-aes-256-gcm 支持多用户：内核的
// 多用户实现只认 blake3-aes-*-gcm（chacha20-poly1305 进去会报
// "shadowsocks 2022 (multi-user): only blake3-aes-*-gcm methods are supported"，
// 整份节点配置被拒）。所以 chacha 即便以 2022- 开头也按单凭据处理，保持现状。
func IsMultiUser(method string) bool {
	switch strings.TrimSpace(method) {
	case "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm":
		return true
	}
	return false
}

// Is2022 判断 method 是否属于 SS-2022 家族（含不支持多用户的 chacha）。
// 单凭据 2022 的链接写法与传统 AEAD 不同（明文百分号编码），见 UserInfo。
func Is2022(method string) bool {
	return strings.HasPrefix(strings.TrimSpace(method), methodPrefix)
}

// KeyLen 返回该方法 PSK 的字节数：aes-128 要 16 字节，aes-256 要 32 字节
// （SIP022 §2.1）。非 2022 方法返回 0。
func KeyLen(method string) int {
	switch strings.TrimSpace(method) {
	case "2022-blake3-aes-128-gcm":
		return 16
	case "2022-blake3-aes-256-gcm", "2022-blake3-chacha20-poly1305":
		return 32
	}
	return 0
}

// ValidKey 判断服务端 PSK 是不是该 method 的**合法**密钥：base64 解码后字节数
// 必须正好等于 KeyLen(method)（SIP022 §2.1；内核拿到长度不对的 key 会
// "proxy/shadowsocks_2022: create service > bad key" 拒载整份节点配置）。
//
// 非 2022 方法没有「密钥格式」这回事（传统 AEAD 的 password 是自由文本），
// 一律返回 false —— 调用方据此**不校验也不自动生成**，保持历史口径。
func ValidKey(method, key string) bool {
	n := KeyLen(method)
	if n == 0 {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
	if err != nil {
		return false
	}
	return len(raw) == n
}

// GenerateKey 生成一把该 method 的随机服务端 PSK：crypto/rand 取 KeyLen 字节再 base64。
func GenerateKey(method string) (string, error) {
	n := KeyLen(method)
	if n == 0 {
		return "", fmt.Errorf("ss2022: %q 没有固定密钥长度（不是 2022 方法）", method)
	}
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("ss2022: 生成随机密钥失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// ClientKey 派生用户的 PSK：base64(sha256(uuid)) 取前 KeyLen(method) 字节。
//
// 确定性是刻意的 —— 面板没有「每用户 ss key」表，同一 uuid 在任何一次渲染、
// 任何一次订阅里都必须得到同一把 key，否则改一次套餐就把存量用户全踢下线。
// 32 字节的 sha256 直接用于 aes-256；aes-128 截前 16 字节（长度不对内核会拒载）。
//
// method 不是多用户方法 / uuid 为空时返回空串（调用方据此退回单凭据形态）。
func ClientKey(method, uuid string) string {
	n := KeyLen(method)
	if n == 0 || !IsMultiUser(method) || uuid == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(uuid))
	return base64.StdEncoding.EncodeToString(sum[:n])
}

// UserInfo 拼 ss 链接的 userinfo（"ss://" 与 "@" 之间那一段）。
//
// 两种形态：
//   - 多用户（2022-blake3-aes-*-gcm）：method:服务端PSK:用户PSK，三段**明文**
//     百分号编码。SIP022 明确 2022 链接不走 base64；少一段服务端 PSK 客户端
//     就握不上手（本机实测：只给用户 PSK → curl 8 秒超时，零字节）。
//   - 单凭据（传统 AEAD / 单用户 2022）：base64(method:password)，历史口径不动。
func UserInfo(method, password, clientPSK string) string {
	if Is2022(method) {
		// 2022 必须明文百分号编码（3x-ui / SIP022 同口径），base64 会让客户端解不出密钥。
		if clientPSK == "" {
			return escape(method) + ":" + escape(password)
		}
		return escape(method) + ":" + escape(password) + ":" + escape(clientPSK)
	}
	return base64.StdEncoding.EncodeToString([]byte(method + ":" + password))
}

// escape 等价于 PHP rawurlencode / RFC 3986：只保留 unreserved 字符。
// base64 里的 '+' '/' '=' 必须编码，否则 '+' 会被客户端 URL 解析成空格。
func escape(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0F])
		}
	}
	return b.String()
}
