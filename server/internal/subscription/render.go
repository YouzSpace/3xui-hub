package subscription

import (
	"strings"
)

// 逐条复刻 SubscriptionService.php 的三个渲染器：
//
//	generateClash() / generateSingbox() / generateBase64()（后者在 service.go）
//
// Clash 是**手写字符串拼接**（不是 yaml 库）：字段顺序、引号、缩进、空行都由
// PHP 的拼接顺序决定；这里逐行照抄，连"哪些行可能重复"都保持不变。
//
// 在 PHP 原版基础上有意扩展了 Clash 输出的两处（节点正文的字节契约不变）：
//   - 头部设置与 dns 段：unified-delay / tcp-concurrent + fake-ip + 国内 DoH +
//     fake-ip-filter（对应 clashDNSConfig），解决规则模式下国内软件解析异常导致的卡顿；
//   - rules 段：大陆域名/大陆 IP/局域网/中国公共 DNS 全部 DIRECT，其余走
//     既有的 Proxy 组（对应 clashRules），不新增任何策略组，
//     等价于 FlClash「路由设置」里全部开关打开。

// clashDNSConfig 是订阅正文最前面的全局设置 + dns 段。
//
// 设计口径（综合三家成熟订阅的最优部分）：
//   - mode: rule —— 新导入配置默认就是规则模式（FlClash 面板仍可随时切换）；
//   - fake-ip 模式：规则匹配走域名，不受节点侧解析污染影响；filter 清单
//     参考魔戒（局域网、系统探测、NTP、QQ/微信登录、国内音乐、游戏主机、
//     STUN 等一律真实 IP——这些是"规则模式下软件卡"的高发区）；
//   - respect-rules + proxy-server-nameserver + fallback/fallback-filter
//     参考 jkun：DNS 请求本身也按规则分流，海外域名由境外 DNS 应答
//     （geoip 非 CN / geosite:gfw 触发），国内 DNS 污染结果不会漏进来；
//   - nameserver 用国内 DoH（比 jkun 的明文 UDP 防劫持），DoH 服务器地址
//     已在 rules 里钉死直连。
const clashDNSConfig = "" +
	"mode: rule\n" +
	"unified-delay: true\n" +
	"tcp-concurrent: true\n" +
	"\n" +
	"dns:\n" +
	"  enable: true\n" +
	"  ipv6: false\n" +
	"  enhanced-mode: fake-ip\n" +
	"  fake-ip-range: 198.18.0.1/16\n" +
	"  respect-rules: true\n" +
	"  default-nameserver:\n" +
	"    - 223.5.5.5\n" +
	"    - 223.6.6.6\n" +
	"    - 119.29.29.29\n" +
	"  proxy-server-nameserver:\n" +
	"    - 223.5.5.5\n" +
	"    - 119.29.29.29\n" +
	"  nameserver:\n" +
	"    - https://dns.alidns.com/dns-query\n" +
	"    - https://doh.pub/dns-query\n" +
	"  fallback:\n" +
	"    - https://cloudflare-dns.com/dns-query\n" +
	"    - https://dns.google/dns-query\n" +
	"  fallback-filter:\n" +
	"    geoip: true\n" +
	"    geoip-code: CN\n" +
	"    geosite:\n" +
	"      - gfw\n" +
	"    ipcidr:\n" +
	"      - 240.0.0.0/4\n" +
	"  fake-ip-filter:\n" +
	"    - '*.lan'\n" +
	"    - '*.local'\n" +
	"    - 'localhost.ptlogin2.qq.com'\n" +
	"    - 'localhost.sec.qq.com'\n" +
	"    - '+.msftconnecttest.com'\n" +
	"    - '+.msftncsi.com'\n" +
	"    - 'msftconnecttest.com'\n" +
	"    - 'msftncsi.com'\n" +
	"    - 'time.*.com'\n" +
	"    - 'time.*.gov'\n" +
	"    - 'time.*.edu.cn'\n" +
	"    - 'time.*.apple.com'\n" +
	"    - 'ntp.*.com'\n" +
	"    - '+.pool.ntp.org'\n" +
	"    - 'time1.cloud.tencent.com'\n" +
	"    - '*.ntp.org.cn'\n" +
	"    - 'music.163.com'\n" +
	"    - '*.music.163.com'\n" +
	"    - '*.126.net'\n" +
	"    - 'y.qq.com'\n" +
	"    - '*.y.qq.com'\n" +
	"    - '*.kuwo.cn'\n" +
	"    - '*.xiami.com'\n" +
	"    - '*.music.migu.cn'\n" +
	"    - '+.battlenet.com.cn'\n" +
	"    - 'stun.*.*'\n" +
	"    - 'stun.*.*.*'\n" +
	"    - '+.stun.*.*'\n" +
	"    - '+.miwifi.com'\n" +
	"    - 'dns.alidns.com'\n" +
	"    - 'doh.pub'\n" +
	"    - 'dot.pub'\n" +
	"\n"

// clashRules 是分流规则：只指向 DIRECT 与既有的 Proxy 组，不新增分组。
//
// 总原则（用户拍板）：**远程优先、直连次要**——只有"确定大陆可达/大陆专属"
// 的目标才钉 DIRECT，一切拿不准的（含海外 CDN、全球服务）留给 MATCH,Proxy，
// 优先保证远程能通。等价于 FlClash「路由设置」全部打开，外加两类必要补充：
//   - 中国公共 DNS（阿里/腾讯/DNSPod 的域名与 IP）：解析链路自身必须直连，
//     否则 DoH 请求绕代理造成循环/卡顿；
//   - 大陆可达的下载 CDN（Windows Update、Steam 国服/国内镜像、苹果国内
//     CDN 域名）：大流量走代理既卡又耗订阅流量；这些域名在大陆可直连，
//     不存在"本来该走代理"的问题。
//
// 刻意**不做**的（参考订阅里有、但会误伤远程）：邮件端口 DST-PORT 直连
// （打死 Gmail 客户端）、steampipe/steamcontent/Epic 全球 CDN 直连（大陆
// 常不通）、apple.com 全后缀与 17/8 整段直连（Apple Intelligence 等海外
// 服务需要代理）、DOMAIN-KEYWORD,-cn（误伤含 -cn 的海外域名）、
// safebrowsing.googleapis.com（走代理才通）。
const clashRules = "" +
	"  # 局域网与私有地址（geodata + 显式网段双保险，参考 jkun 尾部）\n" +
	"  - GEOSITE,private,DIRECT\n" +
	"  - GEOIP,private,DIRECT,no-resolve\n" +
	"  - DOMAIN-SUFFIX,local,DIRECT\n" +
	"  - IP-CIDR,127.0.0.0/8,DIRECT\n" +
	"  - IP-CIDR,100.64.0.0/10,DIRECT\n" +
	"  - IP-CIDR,224.0.0.0/4,DIRECT\n" +
	"  - IP-CIDR6,fe80::/10,DIRECT\n" +
	"  # 中国公共 DNS（域名 + IP 钉直连，解析链路防绕圈）\n" +
	"  - DOMAIN-SUFFIX,alidns.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,doh.pub,DIRECT\n" +
	"  - DOMAIN-SUFFIX,dot.pub,DIRECT\n" +
	"  - IP-CIDR,223.5.5.5/32,DIRECT,no-resolve\n" +
	"  - IP-CIDR,223.6.6.6/32,DIRECT,no-resolve\n" +
	"  - IP-CIDR,119.29.29.29/32,DIRECT,no-resolve\n" +
	"  - IP-CIDR,1.12.12.12/32,DIRECT,no-resolve\n" +
	"  - IP-CIDR6,2400:3200::1/128,DIRECT,no-resolve\n" +
	"  # Windows Update / 交付优化（微软在华有节点，直连不伤远程）\n" +
	"  - DOMAIN-SUFFIX,dl.delivery.mp.microsoft.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,download.windowsupdate.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,dl.ws.microsoft.com,DIRECT\n" +
	"  # Steam 国服与国产下载镜像（完美世界等国服专属域名；全球 CDN 留给代理）\n" +
	"  - DOMAIN-SUFFIX,steamchina.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,csgo.wmsj.cn,DIRECT\n" +
	"  - DOMAIN-SUFFIX,dota2.wmsj.cn,DIRECT\n" +
	"  - DOMAIN-SUFFIX,wmsjsteam.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,dl.steam.clngaa.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,dl.steam.ksyna.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,st.dl.bscstorage.net,DIRECT\n" +
	"  - DOMAIN-SUFFIX,st.dl.eccdnx.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,st.dl.pinyuncloud.com,DIRECT\n" +
	"  # 苹果在华可达的 CDN 域名（App Store/系统更新内容分发；\n" +
	"  #  icloud.com / icloud-content / cloudkit 属海外账户同步链路，直连有争议，\n" +
	"  #  按「直连与远程冲突时远程优先」留给代理；仅大陆专属的 icloud.com.cn 直连）\n" +
	"  - DOMAIN-SUFFIX,mzstatic.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,cdn-apple.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,aaplimg.com,DIRECT\n" +
	"  - DOMAIN-SUFFIX,icloud.com.cn,DIRECT\n" +
	"  # 大陆域名与大陆 IP（geosite:cn 覆盖 QQ/微信/网易/阿里/字节等主流域名）\n" +
	"  - GEOSITE,cn,DIRECT\n" +
	"  - DOMAIN-SUFFIX,cn,DIRECT\n" +
	"  - GEOIP,CN,DIRECT,no-resolve\n" +
	"  # 其余一律走代理（远程优先兜底）\n" +
	"  - MATCH,Proxy\n"

// clashInfoProxyKeys 是 generateClash 里输出单个 proxy 的固定字段顺序。
//
// 与 PHP 的 if 序列一一对应，不要按「更合理」的顺序重排：订阅正文是字节级契约。
func (s *Service) generateClash(user *User, plan *Plan, links []string, cfg *siteConfigReader) string {
	proxies := make([]phpMap, 0, len(links)+3)
	proxyNames := make([]string, 0, len(links))
	regionCounters := map[string]int{}

	// 流量信息节点排在真实节点之前；它们不进 proxyNames（不加入代理组）
	proxies = append(proxies, s.createInfoProxies(user, plan, cfg)...)

	for _, link := range links {
		if proxy := s.parseLinkToClashProxy(link, regionCounters, cfg); proxy != nil {
			proxies = append(proxies, proxy)
			proxyNames = append(proxyNames, phpString(field(proxy, "name")))
		}
	}

	if len(proxies) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(clashDNSConfig)
	b.WriteString("proxies:\n")
	for _, proxy := range proxies {
		b.WriteString("  - name: \"" + phpString(field(proxy, "name")) + "\"\n")
		b.WriteString("    type: " + phpString(field(proxy, "type")) + "\n")
		b.WriteString("    server: " + phpString(field(proxy, "server")) + "\n")
		b.WriteString("    port: " + phpString(field(proxy, "port")) + "\n")

		writeClashScalar(&b, proxy, "uuid", "uuid")
		writeClashScalar(&b, proxy, "cipher", "cipher")
		writeClashScalar(&b, proxy, "password", "password")
		writeClashScalar(&b, proxy, "encryption", "encryption")
		writeClashScalar(&b, proxy, "alter_id", "alter_id")
		writeClashScalar(&b, proxy, "flow", "flow")
		writeClashBool(&b, proxy, "udp", "udp")
		writeClashBool(&b, proxy, "sniffing", "sniffing")
		writeClashBool(&b, proxy, "tls", "tls")

		// PHP: `if (isset($proxy['sni'])) $yaml .= "    servername: {$proxy['sni']}\n";`
		writeClashScalar(&b, proxy, "sni", "servername")
		writeClashScalar(&b, proxy, "servername", "servername")
		writeClashScalar(&b, proxy, "client-fingerprint", "client-fingerprint")

		if reality, ok := field(proxy, "reality-opts").(phpMap); ok {
			b.WriteString("    reality-opts:\n")
			// 必须给 public-key / short-id 加引号：066559 这类纯数字前导零的 sid
			// 不加引号会被 YAML 当数值解析，前导零丢失后 mihomo 校验失败。
			if v, has := reality.get("public-key"); has {
				b.WriteString("      public-key: \"" + phpString(v) + "\"\n")
			}
			if v, has := reality.get("short-id"); has {
				b.WriteString("      short-id: \"" + phpString(v) + "\"\n")
			}
		}

		writeClashScalar(&b, proxy, "network", "network")

		if wsOpts, ok := field(proxy, "ws-opts").(phpMap); ok {
			b.WriteString("    ws-opts:\n")
			b.WriteString("      path: \"" + phpString(field(wsOpts, "path")) + "\"\n")
			if headers, okHeaders := field(wsOpts, "headers").(phpMap); okHeaders {
				b.WriteString("      headers:\n")
				for _, kv := range headers {
					b.WriteString("        " + kv.Key + ": \"" + phpString(kv.Value) + "\"\n")
				}
			}
		}

		if xhttpOpts, ok := field(proxy, "xhttp-opts").(phpMap); ok {
			b.WriteString("    xhttp-opts:\n")
			b.WriteString("      path: \"" + phpString(field(xhttpOpts, "path")) + "\"\n")
			b.WriteString("      mode: \"" + phpString(field(xhttpOpts, "mode")) + "\"\n")
			if v, has := xhttpOpts.get("x-padding-bytes"); has {
				b.WriteString("      x-padding-bytes: " + phpString(v) + "\n")
			}
		}

		// 注意：PHP 的 generateClash 没有输出 http-upgrade-opts 的分支
		// （httpupgrade 只影响 network 值），此处保持一致 —— 不是遗漏。
	}

	b.WriteString("\nproxy-groups:\n")
	b.WriteString("  - name: \"Proxy\"\n")
	b.WriteString("    type: select\n")
	b.WriteString("    proxies:\n")
	for _, name := range proxyNames {
		b.WriteString("      - \"" + name + "\"\n")
	}

	b.WriteString("\nrules:\n")
	b.WriteString(clashRules)

	return b.String()
}

// writeClashScalar 输出 `    {label}: {value}`（键存在即输出，空串也输出）。
func writeClashScalar(b *strings.Builder, proxy phpMap, key, label string) {
	if v, has := proxy.get(key); has {
		b.WriteString("    " + label + ": " + phpString(v) + "\n")
	}
}

// writeClashBool 输出 `    {label}: true|false`（PHP 真值语义：空串/"0" 为 false）。
func writeClashBool(b *strings.Builder, proxy phpMap, key, label string) {
	if v, has := proxy.get(key); has {
		b.WriteString("    " + label + ": " + phpBoolText(v) + "\n")
	}
}

func phpBoolText(v any) string {
	if phpBool(v) {
		return "true"
	}
	return "false"
}

// field 取 phpMap 的键值（缺失返回 nil，与 PHP 的 $arr[$k] ?? null 一致）。
func field(m phpMap, key string) any {
	if m == nil {
		return nil
	}
	v, _ := m.get(key)
	return v
}

// generateSingbox 对应 generateSingbox()：
// direct 固定第一，其次（可选提示条目）→ 信息出站 → 真实出站。
func (s *Service) generateSingbox(user *User, plan *Plan, links []string, cfg *siteConfigReader) string {
	regionCounters := map[string]int{}

	infoOutbounds := s.createInfoOutbounds(user, plan, cfg)

	realOutbounds := make([]phpMap, 0, len(links))
	for _, link := range links {
		if outbound := s.parseLinkToSingboxOutbound(link, regionCounters, cfg); outbound != nil {
			realOutbounds = append(realOutbounds, outbound)
		}
	}

	// 有链接但全部被跳过（无真实节点出站）时注入提示条目，排 direct 之后第一位；
	// $links 本身为空保持现状（返回空串），不加提示。
	var hintOutbounds []phpMap
	if len(links) > 0 && len(realOutbounds) == 0 {
		hintOutbounds = append(hintOutbounds, obj(
			"type", "shadowsocks",
			"tag", "不支持Sing-box请用Clash或Base64",
			"server", "127.0.0.1",
			"server_port", jsonNumber("1"),
			"method", "aes-256-gcm",
			"password", "info",
		))
	}

	outbounds := make([]any, 0, len(hintOutbounds)+len(infoOutbounds)+len(realOutbounds))
	for _, o := range hintOutbounds {
		outbounds = append(outbounds, o)
	}
	for _, o := range infoOutbounds {
		outbounds = append(outbounds, o)
	}
	for _, o := range realOutbounds {
		outbounds = append(outbounds, o)
	}

	if len(outbounds) == 0 {
		return ""
	}

	allOutbounds := make([]any, 0, len(outbounds)+1)
	allOutbounds = append(allOutbounds, obj("type", "direct", "tag", "direct"))
	allOutbounds = append(allOutbounds, outbounds...)

	config := obj("outbounds", allOutbounds)
	return jsonPretty(config)
}
