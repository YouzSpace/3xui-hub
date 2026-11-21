// Package crypto 是协议域「加密相关」的公共实现（uuid、随机凭据、口令摘要）。
//
// 说明：Shadowsocks-2022 的 PSK 派生/校验仍是唯一真相源 internal/ss2022（历史已独立成包），
// 本包不重复它，只归拢「协议包通用」的少量加密辅助。
package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// NewUUID 生成 RFC4122 v4 UUID（用于新建用户/凭据）。
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("protocol/crypto: 生成 UUID 失败: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := "0123456789abcdef"
	out := make([]byte, 36)
	idx := 0
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[idx] = '-'
			idx++
		}
		out[idx] = h[v>>4]
		out[idx+1] = h[v&0xf]
		idx += 2
	}
	return string(out), nil
}

// RandomPassword 生成 n 字节随机数的 base64 口令（n<=0 时按 16）。
func RandomPassword(n int) (string, error) {
	if n <= 0 {
		n = 16
	}
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("protocol/crypto: 生成随机口令失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// SHA256Base64 返回 sha256(data) 的 base64（与 ss2022.ClientKey 同口径的底层原语）。
func SHA256Base64(data string) string {
	sum := sha256.Sum256([]byte(data))
	return base64.StdEncoding.EncodeToString(sum[:])
}
