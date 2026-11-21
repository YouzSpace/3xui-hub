// Package blackhole 是 Blackhole（丢包）出站 settings。
package blackhole

import "controlhub-server/internal/phpjson"

// BuildOutboundSettings 构造 Blackhole 出站 settings（空对象）。
func BuildOutboundSettings() *phpjson.Object {
	return phpjson.New()
}
