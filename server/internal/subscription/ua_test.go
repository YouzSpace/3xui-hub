package subscription

import (
	"net/url"
	"testing"
)

func TestFormatFromUA(t *testing.T) {
	cases := []struct {
		ua   string
		want Format
		ok   bool
	}{
		{"Clash-Verge-Rev/2.5.2", FormatClash, true},
		{"clash.meta/1.19.9", FormatClash, true},
		{"FlClash/0.8.85 (Windows 10)", FormatClash, true},
		{"ClashMetaForAndroid/2.10.0", FormatClash, true},
		{"Mihomo/1.19.9", FormatClash, true},
		{"SFA/1.10.1 (Linux; Android 14)", FormatSingbox, true},
		{"sing-box 1.10.0", FormatSingbox, true},
		{"SFM/1.10.0 (macOS 15)", FormatSingbox, true},
		{"v2rayNG/1.9.0", "", false},
		{"Shadowrocket/2.2.40", "", false},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64)", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := formatFromUA(c.ua)
		if got != c.want || ok != c.ok {
			t.Errorf("formatFromUA(%q) = (%q,%v)，期望 (%q,%v)", c.ua, got, ok, c.want, c.ok)
		}
	}
}

func TestResolveFormat(t *testing.T) {
	always := func(Format) bool { return true }
	never := func(Format) bool { return false }

	// 显式参数优先于 UA（两个方向都测）。
	if got := resolveFormat(url.Values{"clash": {""}}, "SFA/1.10.1", always); got != FormatClash {
		t.Errorf("?clash 应优先于 sing-box UA，got=%q", got)
	}
	if got := resolveFormat(url.Values{"singbox": {"1"}}, "Clash-Verge-Rev/2.5.2", always); got != FormatSingbox {
		t.Errorf("?singbox 应优先于 Clash UA，got=%q", got)
	}
	// UA 识别且格式已启用。
	if got := resolveFormat(url.Values{}, "Clash-Verge-Rev/2.5.2", always); got != FormatClash {
		t.Errorf("Clash UA 应出 clash，got=%q", got)
	}
	if got := resolveFormat(url.Values{}, "SFA/1.10.1", always); got != FormatSingbox {
		t.Errorf("SFA UA 应出 singbox，got=%q", got)
	}
	// UA 识别但格式未启用 → 回落 base64（不给 400）。
	if got := resolveFormat(url.Values{}, "Clash-Verge-Rev/2.5.2", never); got != FormatBase64 {
		t.Errorf("clash 未启用时应回落 base64，got=%q", got)
	}
	// 认不出的 UA 保持原行为。
	if got := resolveFormat(url.Values{}, "Mozilla/5.0", always); got != FormatBase64 {
		t.Errorf("浏览器 UA 应 base64，got=%q", got)
	}
	// 参数名大小写敏感（与 PHP has() 口径一致）。
	if got := resolveFormat(url.Values{"Clash": {"1"}}, "", always); got != FormatBase64 {
		t.Errorf("?Clash 不应命中，got=%q", got)
	}
	// enabled 为 nil 时不做启用判断（内部调用方可能没有配置）。
	if got := resolveFormat(url.Values{}, "Clash-Verge-Rev/2.5.2", nil); got != FormatClash {
		t.Errorf("enabled=nil 时应直接采用 UA 推断，got=%q", got)
	}
}
