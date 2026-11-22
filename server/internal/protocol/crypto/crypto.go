// Package crypto 是代理协议加密原语（uuid / 随机口令 / sha256）。
package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

func RandomBase64(n int) (string, error) {
	if n <= 0 { n = 16 }
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil { return "", fmt.Errorf("protocol/crypto: %w", err) }
	return base64.StdEncoding.EncodeToString(raw), nil
}

func SHA256Base64(data string) string {
	sum := sha256.Sum256([]byte(data))
	return base64.StdEncoding.EncodeToString(sum[:])
}
