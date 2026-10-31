package threexui

// 3x-ui 建 client 的公共负载（新建路径专用）。
//
// 凭据字段一律不传，由 3x-ui 的 fillProtocolDefaults 按协议自动生成
// （client_crud.go:226-250，hysteria 缺 auth 时补随机串）。
func ClientPayload(email string, enable bool, totalGB, expiryMs int64) map[string]any {
	return map[string]any{
		"email":      email,
		"enable":     enable,
		"totalGB":    totalGB,
		"expiryTime": expiryMs,
		"limitIp":    0,
	}
}
