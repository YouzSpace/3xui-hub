// Package httpupgrade 是 httpupgrade 传输的 streamSettings 段构造。
package httpupgrade

import (
	p "controlhub-server/internal/protocol"
	tu "controlhub-server/internal/protocol/transport/util"
)

// Build 返回 httpupgradeSettings 段 JSON。
func Build(input map[string]any) []byte {
	hu := p.New("path", tu.OrSlash(tu.Str(input, "httpupgrade_path")))
	if h := tu.Str(input, "httpupgrade_host"); h != "" {
		hu.Put("host", h)
	}
	return p.Encode(p.New("httpupgradeSettings", hu))
}
