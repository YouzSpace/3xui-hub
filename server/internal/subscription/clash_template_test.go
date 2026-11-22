package subscription

import (
	"strings"
	"testing"
)

// TestClashTemplateDefaultMatchesLegacy 内置模板必须与抽取前的硬编码输出逐字节一致。
func TestClashTemplateDefaultMatchesLegacy(t *testing.T) {
	names := []string{"日本 01", "香港 02"}
	proxiesBlock := "proxies:\n" +
		"  - name: \"日本 01\"\n" +
		"  - name: \"香港 02\"\n"

	got := RenderClashTemplate(DefaultClashTemplate, proxiesBlock, names, "MySite")

	var want strings.Builder
	want.WriteString(clashDNSConfig)
	want.WriteString(proxiesBlock)
	want.WriteString("\nproxy-groups:\n")
	want.WriteString("  - name: \"Proxy\"\n")
	want.WriteString("    type: select\n")
	want.WriteString("    proxies:\n")
	for _, n := range names {
		want.WriteString("      - \"" + n + "\"\n")
	}
	want.WriteString("\nrules:\n")
	want.WriteString(clashRules)

	if got != want.String() {
		t.Fatalf("默认模板输出与旧拼法不一致\n--- got ---\n%s\n--- want ---\n%s", got, want.String())
	}
}

// TestClashTemplateCustom 自定义模板只输出模板内容，regex 行按名字展开。
func TestClashTemplateCustom(t *testing.T) {
	tmpl := "proxies:\n{{proxies}}\n" +
		"proxy-groups:\n" +
		"  - name: \"香港组\"\n" +
		"    type: select\n" +
		"    proxies:\n" +
		"      - \"regex:香港\"\n" +
		"rules:\n" +
		"  - MATCH,{{app_name}}\n"

	proxiesBlock := "proxies:\n" +
		"  - name: \"香港 01\"\n" +
		"  - name: \"日本 01\"\n"
	got := RenderClashTemplate(tmpl, proxiesBlock, []string{"香港 01", "日本 01"}, "柚子云")

	if !strings.Contains(got, `- "香港 01"`) {
		t.Errorf("正则应展开出香港节点:\n%s", got)
	}
	if strings.Contains(got, "日本 01\",\n") && strings.Contains(got, "proxies:\n  - name: \"日本 01\"\n    - \"日本 01\"") {
		t.Errorf("日本节点不应进香港组")
	}
	if !strings.Contains(got, "MATCH,柚子云") {
		t.Errorf("{{app_name}} 未替换:\n%s", got)
	}
	if strings.Contains(got, "regex:") {
		t.Errorf("regex 行应被展开而不是原样输出:\n%s", got)
	}
	if strings.Contains(got, "clashRules") || strings.Contains(got, "GEOIP") {
		t.Errorf("自定义模板不应混入内置规则:\n%s", got)
	}
}

// TestValidateClashTemplate 保存前的三道闸。
func TestValidateClashTemplate(t *testing.T) {
	if err := ValidateClashTemplate("dns:\n  enable: true\n{{proxies}}\nrules: []\n"); err != nil {
		t.Errorf("合法模板被判失败: %v", err)
	}
	if err := ValidateClashTemplate("proxies: []\n"); err == nil {
		t.Error("缺 {{proxies}} 应报错")
	}
	if err := ValidateClashTemplate("{{proxies}}\nbad_indent: [\n"); err == nil {
		t.Error("YAML 语法错误应报错")
	}
	big := strings.Repeat("a", maxClashTemplateBytes+1)
	if err := ValidateClashTemplate(big); err == nil {
		t.Error("超限模板应报错")
	}
}

// TestExpandClashGroupRegexInvalid 非法正则原样保留，不炸整份订阅。
func TestExpandClashGroupRegexInvalid(t *testing.T) {
	in := "proxies:\nproxy-groups:\n  - name: X\n    proxies:\n      - \"regex:[unclosed\"\n"
	got := expandClashGroupRegex(in, []string{"a"})
	if !strings.Contains(got, `- "regex:[unclosed"`) {
		t.Fatalf("非法正则应原样保留:\n%s", got)
	}
}
