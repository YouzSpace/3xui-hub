package subscription

import (
	"regexp"
	"strings"
)

// 逐字复刻 app/Support/NodeRegion.php。
//
// 判定规则（顺序有意义，不要重排）：
//   - 按声明顺序取**第一个**命中的关键词；
//   - 两位字母代码必须以「自成一段」的形式出现（前后不紧挨字母），否则满屏误判
//     （local-verify→CA、Russia→US、China→IN 等修复案例）；
//   - 中文与多字母关键词走子串匹配（大小写不敏感的 ASCII 折叠）。
//
// 关键字 → 地区代码。顺序即优先级。
var regionKeywords = []struct {
	Keyword string
	Code    string
}{
	{"香港", "HK"}, {"HK", "HK"}, {"Hong Kong", "HK"},
	{"台湾", "TW"}, {"TW", "TW"}, {"Taiwan", "TW"},
	{"日本", "JP"}, {"JP", "JP"}, {"Japan", "JP"},
	{"韩国", "KR"}, {"KR", "KR"}, {"Korea", "KR"},
	{"新加坡", "SG"}, {"SG", "SG"}, {"Singapore", "SG"},
	{"美国", "US"}, {"US", "US"}, {"USA", "US"}, {"America", "US"},
	{"英国", "UK"}, {"UK", "UK"}, {"Britain", "UK"},
	{"德国", "DE"}, {"DE", "DE"}, {"Germany", "DE"},
	{"法国", "FR"}, {"FR", "FR"}, {"France", "FR"},
	{"加拿大", "CA"}, {"CA", "CA"}, {"Canada", "CA"},
	{"澳大利亚", "AU"}, {"AU", "AU"}, {"Australia", "AU"},
	{"印度", "IN"}, {"IN", "IN"}, {"India", "IN"},
	{"泰国", "TH"}, {"TH", "TH"}, {"Thailand", "TH"},
	{"马来西亚", "MY"}, {"MY", "MY"}, {"Malaysia", "MY"},
	{"越南", "VN"}, {"VN", "VN"}, {"Vietnam", "VN"},
	{"俄罗斯", "RU"}, {"RU", "RU"}, {"Russia", "RU"},
	{"巴西", "BR"}, {"BR", "BR"}, {"Brazil", "BR"},
	{"土耳其", "TR"}, {"TR", "TR"}, {"Turkey", "TR"},
	{"荷兰", "NL"}, {"NL", "NL"}, {"Netherlands", "NL"},
	{"中国", "CN"}, {"CN", "CN"}, {"China", "CN"},
}

// 地区代码 → 旗帜 emoji。
var regionFlags = map[string]string{
	"HK": "🇭🇰", "TW": "🇹🇼", "JP": "🇯🇵", "KR": "🇰🇷",
	"SG": "🇸🇬", "US": "🇺🇸", "UK": "🇬🇧", "DE": "🇩🇪",
	"FR": "🇫🇷", "CA": "🇨🇦", "AU": "🇦🇺", "IN": "🇮🇳",
	"TH": "🇹🇭", "MY": "🇲🇾", "VN": "🇻🇳", "RU": "🇷🇺",
	"BR": "🇧🇷", "TR": "🇹🇷", "NL": "🇳🇱", "CN": "🇨🇳",
}

// twoLetterCode 匹配「两位字母代码自成一段」的形态（对应 PHP 的正则）。
var twoLetterCode = regexp.MustCompile(`(?i)^[A-Za-z]{2}$`)

// flagEmojiPair 匹配任意两个连续的「区域指示符」（旗帜字符），用于剥掉已有旗帜。
var flagEmojiPair = regexp.MustCompile(`[\x{1F1E0}-\x{1F1FF}]{2}\s*`)

// regionCode 对应 NodeRegion::code()：识别不出返回 ""。
func regionCode(name string) string {
	if phpTrim(name) == "" {
		return ""
	}
	for _, kw := range regionKeywords {
		if regionMatches(name, kw.Keyword) {
			return kw.Code
		}
	}
	return ""
}

// regionMatches 对应 NodeRegion::matches()。
//
// 两位字母代码要求「前后不紧挨字母」；其余（中文、多字母）走子串匹配。
// PHP 用 stripos(strtoupper(...)) 做大小写不敏感匹配，而 strtoupper 是多字节感知的
// （只折叠 ASCII 与小写拉丁扩展），这里用 asciiUpper 折叠 ASCII 后再做子串判断，
// 对中文关键词无影响（汉字字节不变）。
func regionMatches(name, keyword string) bool {
	if twoLetterCode.MatchString(keyword) {
		lowered := asciiLower(name)
		kw := asciiLower(keyword)
		idx := 0
		for {
			pos := strings.Index(lowered[idx:], kw)
			if pos < 0 {
				return false
			}
			abs := idx + pos
			before := abs == 0 || !isASCIILetter(lowered[abs-1])
			after := abs+len(kw) == len(lowered) || !isASCIILetter(lowered[abs+len(kw)])
			if before && after {
				return true
			}
			idx = abs + 1
			if idx >= len(lowered) {
				return false
			}
		}
	}
	return strings.Contains(asciiUpper(name), asciiUpper(keyword))
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// regionFlag 对应 NodeRegion::flag()：无对应返回空串。
func regionFlag(code string) string { return regionFlags[code] }

// stripFlagEmoji 剥掉名称里已存在的旗帜（含其后空白），对应 PHP 的
// preg_replace('/[\x{1F1E0}-\x{1F1FF}]{2}\s*/u', ”, $name)。
//
// 注意 PHP 的 \s 在非 UCP 模式下是 ASCII 空白集合（含 \f、\v，不含 \x00）。
func stripFlagEmoji(name string) string { return flagEmojiPair.ReplaceAllString(name, "") }

// 以下是 PHP preg 的等价实现，供订阅链路复用。

var (
	// ch_user_\d+
	userIDPattern = regexp.MustCompile(`ch_user_[0-9]+`)
	// \[\s*\]（ASCII 空白集）
	emptyBracketPattern = regexp.MustCompile(`\[[ \t\n\r\f\v]*\]`)
	// \s{2,}
	multiSpacePattern = regexp.MustCompile(`[ \t\n\r\f\v]{2,}`)
	// (?:\s*-\s*){2,}
	multiDashPattern = regexp.MustCompile(`(?:[ \t\n\r\f\v]*-[ \t\n\r\f\v]*){2,}`)
	// 首尾孤立分隔符：`/^[\s\-\[\]()_]+|[\s\-\[\]()_]+$/u`，\s 取 PCRE 非 UCP 的
	// [ \t\n\r\f\v]（**不含** \x00 —— 这一点与 PHP trim() 的默认字符集不同）。
	edgeSeparatorPattern = regexp.MustCompile(`^[ \t\n\r\f\v\-\[\]()_]+|[ \t\n\r\f\v\-\[\]()_]+$`)
	// 倍率后缀：-1.25x 挂在名称末尾
	multiplierSuffixPattern = regexp.MustCompile(`-([0-9]+(?:\.[0-9]+)?)x$`)
	// 链接协议 + userinfo + host + port（Clash/Sing-box 解析器共用）。
	//
	// 对应 PHP 的 `/^(vless|vmess|trojan|ss|hysteria2|hysteria):\/\/([^@]+)@([^:\/\]]+):(\d+)/`，
	// 另外接受 `hy2://` 别名（hysteria 官方文档与多数客户端的惯用简写，落库前归一成
	// hysteria2，后续分支不用再关心写法）。
	// host 段排除 ':'、'/'、']'（Go 的字符类里写 `]` 需要转义，反斜杠本身不排除 ——
	// 写成 `\\\]` 会把 '\\' 也排除掉，与 PHP 不一致）。
	//
	// 比 PHP 多认一处：带方括号的 IPv6 字面量 `[2001:db8::1]`。旧写法 `[^:/\]]+`
	// 一遇 ':' 就停（`@[2001:db8::1]:443` 只吃到 `2001`）→ 整条失配 → 调用方静默丢掉该节点。
	// 方括号分支里额外允许 '.'，覆盖 `[::ffff:1.2.3.4]` 这类 v4-mapped 写法。
	linkPattern = regexp.MustCompile(`^(vless|vmess|trojan|ss|hysteria2|hy2|hysteria)://([^@]+)@(\[[0-9a-fA-F:.]+\]|[^:/\]]+):([0-9]+)`)
	// hysteria 密码的 aead 后缀
	hysteriaAeadPattern = regexp.MustCompile(`(?i):(\/gch|\/aead)$`)
	// 自定义信息行切分
	lineSplitPattern = regexp.MustCompile(`\r\n|\r|\n`)
)

// phpPregReplaceAll 用 $replacement 里 $1..$9 的语义替换匹配（用于正则改名）。
//
// 复刻 PHP preg_replace($regex, $replacement, $name) 的行为子集：支持 $n / \n 反向引用。
// 返回 (新串, 是否可用)；正则不可用（编译失败）时由调用方决定降级。
func phpPregReplaceAll(pat *regexp.Regexp, replacement, subject string) string {
	if pat == nil {
		return subject
	}
	buf := make([]byte, 0, len(subject))
	last := 0
	for _, loc := range pat.FindAllStringSubmatchIndex(subject, -1) {
		buf = append(buf, subject[last:loc[0]]...)
		buf = appendExpanded(buf, replacement, subject, loc)
		last = loc[1]
	}
	buf = append(buf, subject[last:]...)
	return string(buf)
}

// appendExpanded 展开 $n / \n 反向引用（PHP preg_replace 语义）。
func appendExpanded(dst []byte, replacement, subject string, loc []int) []byte {
	for i := 0; i < len(replacement); i++ {
		c := replacement[i]
		if (c == '$' || c == '\\') && i+1 < len(replacement) {
			next := replacement[i+1]
			if next >= '0' && next <= '9' {
				group := int(next - '0')
				if i+2 < len(replacement) && replacement[i+2] >= '0' && replacement[i+2] <= '9' {
					group = group*10 + int(replacement[i+2]-'0')
					i++
				}
				if 2*group+1 < len(loc) && loc[2*group] >= 0 {
					dst = append(dst, subject[loc[2*group]:loc[2*group+1]]...)
				}
				i++
				continue
			}
			if c == '\\' && (next == '\\' || next == '$') {
				dst = append(dst, next)
				i++
				continue
			}
		}
		dst = append(dst, c)
	}
	return dst
}

// isAllDigitsASCII 对应 PHP ctype_digit()（空串返回 false）。
func isAllDigitsASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

