package subscription

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml"
)

// TestGenerateClashRuleMode 锁定 Clash 输出的规则模式适配：
// 大陆 IP/域名/局域网/中国公共 DNS 全部 DIRECT，其余 MATCH→Proxy，
// 且带 fake-ip dns 段。只断言关键行与相对顺序，不锁字节（字节契约由
// 节点正文部分保证，这里本来就是有意扩展段）。
func TestGenerateClashRuleMode(t *testing.T) {
	s := &Service{Now: func() time.Time { return time.Unix(1700000000, 0) }}
	user := &User{ID: 1, Token: "t", Enabled: true}
	plan := &Plan{ID: 1, Type: "total"}

	cred := base64.RawURLEncoding.EncodeToString([]byte("aes-256-gcm:p@ss123"))
	links := []string{"ss://" + cred + "@1.2.3.4:8443#TestNode"}

	out := s.generateClash(user, plan, links, nil)
	if out == "" {
		t.Fatal("generateClash 返回空")
	}

	// 1) dns 段：fake-ip + 国内外双路解析 + 关键 filter（综合魔戒/jkun 口径）
	for _, want := range []string{
		"mode: rule",
		"enhanced-mode: fake-ip",
		"respect-rules: true",
		"proxy-server-nameserver:",
		"https://dns.alidns.com/dns-query",
		"https://doh.pub/dns-query",
		"https://cloudflare-dns.com/dns-query",
		"fallback-filter:",
		"geoip-code: CN",
		"'*.lan'",
		"'localhost.ptlogin2.qq.com'",
		"'+.msftconnecttest.com'",
		"unified-delay: true",
		"tcp-concurrent: true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dns 段缺少 %q", want)
		}
	}

	// 2) 规则段：关键规则齐全（直连只允许"确定大陆可达"的目标）
	for _, want := range []string{
		"  - GEOSITE,private,DIRECT\n",
		"  - GEOIP,private,DIRECT,no-resolve\n",
		"  - IP-CIDR,127.0.0.0/8,DIRECT\n",
		"  - IP-CIDR,100.64.0.0/10,DIRECT\n",
		"  - IP-CIDR6,fe80::/10,DIRECT\n",
		"  - DOMAIN-SUFFIX,alidns.com,DIRECT\n",
		"  - IP-CIDR,223.5.5.5/32,DIRECT,no-resolve\n",
		"  - IP-CIDR6,2400:3200::1/128,DIRECT,no-resolve\n",
		"  - DOMAIN-SUFFIX,dl.delivery.mp.microsoft.com,DIRECT\n",
		"  - DOMAIN-SUFFIX,steamchina.com,DIRECT\n",
		"  - DOMAIN-SUFFIX,mzstatic.com,DIRECT\n",
		"  - DOMAIN-SUFFIX,icloud.com.cn,DIRECT\n",
		"  - GEOSITE,cn,DIRECT\n",
		"  - DOMAIN-SUFFIX,cn,DIRECT\n",
		"  - GEOIP,CN,DIRECT,no-resolve\n",
		"  - MATCH,Proxy\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rules 段缺少 %q", want)
		}
	}

	// 2b) 反向断言（用户定稿：直连与远程冲突时远程优先）——
	// 以下"全球服务/海外 CDN"绝不允许出现在 DIRECT 里。
	// 必须整行精确匹配，避免 cdn-apple.com 这类合法直连被子串误伤。
	for _, banned := range []string{
		"  - DST-PORT,",                         // 邮件端口直连会打死 Gmail 等海外邮件
		"  - DOMAIN-SUFFIX,apple.com,DIRECT\n",  // 海外 Apple 服务（Intelligence 等）需代理
		"  - DOMAIN-SUFFIX,icloud.com,DIRECT\n", // 海外 iCloud 同步链路（仅 icloud.com.cn 可直连）
		"  - DOMAIN-SUFFIX,icloud-content.com,DIRECT\n",
		"  - DOMAIN-SUFFIX,apple-cloudkit.com,DIRECT\n",
		"  - DOMAIN-SUFFIX,me.com,DIRECT\n",
		"  - IP-CIDR,17.0.0.0/8",                // Apple 整段含海外服务
		"  - DOMAIN-KEYWORD,steampipe,DIRECT\n", // Steam 全球 CDN，大陆常不通
		"  - DOMAIN-SUFFIX,steamcontent.com,DIRECT\n",
		"  - DOMAIN-KEYWORD,epicgames-download,DIRECT\n",  // Epic 全球 CDN
		"  - DOMAIN,safebrowsing.googleapis.com,DIRECT\n", // Google 域名走代理才通
		"  - DOMAIN-KEYWORD,-cn,DIRECT\n",                 // 会误伤含 -cn 的海外域名
		"nintendo",                                        // 海外游戏平台不进 filter 也不直连
		"wargaming",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("违反远程优先原则，出现不该直连/放行的条目 %q", banned)
		}
	}

	// 3) 顺序：局域网 < 中国 DNS < 大陆域名 < 大陆 IP < MATCH；dns 在 proxies 前
	idx := func(sub string) int { return strings.Index(out, sub) }
	dnsAt, proxiesAt := idx("dns:"), idx("proxies:\n")
	lanAt := idx("GEOSITE,private")
	cnDnsAt := idx("IP-CIDR,223.5.5.5/32")
	cnSiteAt := idx("GEOSITE,cn,DIRECT")
	cnIPAt := idx("GEOIP,CN,DIRECT")
	matchAt := idx("MATCH,Proxy")
	if !(dnsAt >= 0 && dnsAt < proxiesAt) {
		t.Errorf("dns 段应在 proxies 之前 (dns=%d proxies=%d)", dnsAt, proxiesAt)
	}
	if !(lanAt < cnDnsAt && cnDnsAt < cnSiteAt && cnSiteAt < cnIPAt && cnIPAt < matchAt) {
		t.Errorf("规则顺序错误: lan=%d cnDns=%d cnSite=%d cnIP=%d match=%d",
			lanAt, cnDnsAt, cnSiteAt, cnIPAt, matchAt)
	}

	// 4) 不新增策略组：proxy-groups 里只有 Proxy
	if n := strings.Count(out, "proxy-groups:"); n != 1 {
		t.Errorf("proxy-groups 应恰好 1 个，实际 %d", n)
	}
	if !strings.Contains(out, "  - name: \"Proxy\"\n") {
		t.Error("缺少 Proxy 策略组")
	}
	if strings.Contains(out, "Gemini") || strings.Contains(out, "ChatGPT") {
		t.Error("不应出现额外分组")
	}

	// 5) 节点正文契约不变：proxies 段仍在 dns 段之后、组引用仍指向 Proxy
	if !strings.Contains(out, "      - \"TestNode\"\n") {
		t.Error("Proxy 组里应包含节点 TestNode")
	}

	// 6) 整份输出必须是合法 YAML（mihomo 直接加载的就是这份）
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("输出不是合法 YAML: %v", err)
	}
	for _, key := range []string{"dns", "proxies", "proxy-groups", "rules"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("YAML 顶层缺少 %q", key)
		}
	}
	wantRules := strings.Count(clashRules, "  - ")
	rulesList, ok := doc["rules"].([]any)
	if !ok || len(rulesList) != wantRules {
		t.Errorf("rules 条数应为 %d，实际 %#v", wantRules, doc["rules"])
	} else if last, _ := rulesList[len(rulesList)-1].(string); last != "MATCH,Proxy" {
		t.Errorf("最后一条规则应为 MATCH,Proxy，实际 %q", last)
	}

	// 可选：CLASH_SAMPLE_OUT 环境变量给路径时导出样例，供 mihomo -t 校验
	if p := os.Getenv("CLASH_SAMPLE_OUT"); p != "" {
		body := "#profile-title: dGVzdAo=\n" + out
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
