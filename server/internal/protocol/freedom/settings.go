// Package freedom 是 Freedom（直连）出站 settings。
package freedom

import "controlhub-server/internal/phpjson"

// BuildOutboundSettings 构造 Freedom 出站 settings（domainStrategy=AsIs）。
func BuildOutboundSettings() *phpjson.Object {
	return phpjson.New("domainStrategy", "AsIs")
}
