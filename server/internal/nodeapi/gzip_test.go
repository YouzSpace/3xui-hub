package nodeapi

import "testing"

// 配置下发是最重的响应（百万用户约 2 MiB），gzip 后约 108 KiB。
// 这里只校验编码协商的判断，避免回归成「客户端明明支持却没压缩」。
func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"空头", "", false},
		{"只 gzip", "gzip", true},
		{"带其他编码", "gzip, deflate, br", true},
		{"gzip 在末尾", "deflate, br, gzip", true},
		{"大写 GZIP", "GZIP", true},
		{"混合大小写", "GZip", true},
		{"带 q 值", "gzip;q=0.5", true},
		{"只有 deflate", "deflate", false},
		{"只有 br", "br", false},
		{"identity", "identity", false},
		{"gzip 子串但非令牌", "x-gzip", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := acceptsGzip(c.header); got != c.want {
				t.Fatalf("acceptsGzip(%q) = %v，期望 %v", c.header, got, c.want)
			}
		})
	}
}
