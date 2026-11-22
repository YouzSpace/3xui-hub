package subscription

// Clash 订阅模板：把 DNS 段、代理组、分流规则从代码里抽出来，管理员可在
// 「订阅配置」页编辑，不用改代码发版。照 Xboard 的 ClashMeta 模板思路。
//
// 占位符（保存时校验）：
//   {{proxies}}   —— 整段 "proxies:" 与节点列表，必须出现；
//   {{app_name}}  —— 站点标题（site_title，空则 ControlHub）。
//
// 代理组自动填充：组内写一行 `- "regex:<正则>"`，渲染时展开成匹配该正则的
// 节点名列表（保持该行缩进）。默认模板用 `regex:.*` 把所有节点塞进 Proxy 组。
// 正则非法时该行原样保留（不会让整份订阅崩掉）。

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"
)

// defaultClashGroups 是内置代理组段（默认模板用）。
const defaultClashGroups = "proxy-groups:\n" +
	"  - name: \"Proxy\"\n" +
	"    type: select\n" +
	"    proxies:\n" +
	"      - \"regex:.*\"\n"

// DefaultClashTemplate 是内置模板：与抽取前的硬编码输出逐字节一致
// （clashDNSConfig + proxies 段 + 默认组 + clashRules）。
var DefaultClashTemplate = clashDNSConfig +
	"{{proxies}}\n" +
	defaultClashGroups +
	"\nrules:\n" +
	clashRules

// maxClashTemplateBytes 是模板体积上限（site_configs.value 存整段文本，
// 防止有人粘一份几 MB 的规则集把库和每次订阅请求拖垮）。
const maxClashTemplateBytes = 512 << 10

// clashRegexLine 匹配代理组里的正则占位行：`      - "regex:香港|HK"`。
var clashRegexLine = regexp.MustCompile(`^(\s*)- "regex:(.*)"\s*$`)

// ValidateClashTemplate 在保存时校验模板：大小、必需占位符、YAML 语法。
// 校验前把占位符替换成合法 YAML（占位符本身不是合法值）。
func ValidateClashTemplate(tmpl string) error {
	if len(tmpl) > maxClashTemplateBytes {
		return fmt.Errorf("模板超过 %d KB", maxClashTemplateBytes>>10)
	}
	if !strings.Contains(tmpl, "{{proxies}}") {
		return fmt.Errorf("模板必须包含 {{proxies}} 占位符，否则节点不会出现在订阅里")
	}
	sample := strings.ReplaceAll(tmpl, "{{proxies}}", "proxies: []")
	sample = strings.ReplaceAll(sample, "{{app_name}}", "x")
	var v any
	if err := yaml.Unmarshal([]byte(sample), &v); err != nil {
		return fmt.Errorf("YAML 语法错误：%v", err)
	}
	return nil
}

// RenderClashTemplate 用模板生成正文：替换 proxies 段与品牌名，
// 再把组内的 regex 行展开成匹配的节点名。
func RenderClashTemplate(tmpl, proxiesBlock string, proxyNames []string, appName string) string {
	if appName == "" {
		appName = "ControlHub"
	}
	out := strings.ReplaceAll(tmpl, "{{proxies}}", proxiesBlock)
	out = strings.ReplaceAll(out, "{{app_name}}", appName)
	return expandClashGroupRegex(out, proxyNames)
}

// expandClashGroupRegex 展开 `- "regex:XXX"` 行；非法正则原样保留。
func expandClashGroupRegex(text string, names []string) string {
	if !strings.Contains(text, "regex:") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines)+len(names))
	for _, line := range lines {
		m := clashRegexLine.FindStringSubmatch(line)
		if m == nil {
			out = append(out, line)
			continue
		}
		re, err := regexp.Compile(m[2])
		if err != nil {
			out = append(out, line)
			continue
		}
		for _, n := range names {
			if re.MatchString(n) {
				out = append(out, m[1]+`- "`+escapeClashName(n)+`"`)
			}
		}
	}
	return strings.Join(out, "\n")
}

// escapeClashName 转义节点名里的双引号与反斜杠（YAML 双引号字符串）。
func escapeClashName(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}
