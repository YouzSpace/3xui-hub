package main

// client 名单指纹（issue #115 3.5）：心跳顺带报「节点配置里的 client 集合」的 md5，
// 面板平时只比指纹，指纹没变就跳过 client_list 全量读回（对齐 Xboard 的 ETag 思路）。
//
// 口径刻意复用 parseClientList（与 client_list 回执同一个解析）：
//   - 按 email 去重（同一个人挂在多个入站只算一条）；
//   - enable 取「任一处停用即停用」，totalGB / expiryTime 取第一次出现的值。
//
// 于是指纹正好是「面板对账能看到的那份集合」的函数：指纹相同 ⇔ 对账输入一字不差。
// 入站 tag 不参与指纹 —— 面板对账只认 email / enable / totalGB / expiryTime。
//
// 分桶指纹（issue #128 B）：整体指纹一变（哪怕只增删 1 人）面板就得全量回读 —— 万级
// 用户是 50 页。所以同时上报 32 个桶的 hash（桶 = fnv32a(email) % 32，桶 hash = 桶内
// email 排序后 join "\n" 的 md5）。面板逐桶比对，只回差异桶（1 桶 ≈ 2 页）。
//
// ⚠️ 分桶算法是 agent/面板两侧必须一字不差的协议（面板侧实现在
// server/internal/collector/reconcile_bare.go 的 bareBucketID / bareBucketSum）。
// 改这里必须同步改面板，否则每轮都判「全部 32 桶都变了」→ 退化成全量读（不至于错，
// 但优化彻底失效）。

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"sort"
	"strconv"
	"strings"
)

// clientBucketCount 是名单分桶数（issue #128 B）。面板 bareBucketCount 与之相同。
const clientBucketCount = 32

// clientsFingerprintBody 是心跳里的 clients_fp 字段（面板契约：n / sum，别改名）。
type clientsFingerprintBody struct {
	// N 是去重后的 client 数（与 client_list 回执的 total 同口径）。
	N int `json:"n"`
	// Sum 是名单的 md5 十六进制串。
	Sum string `json:"sum"`
	// Buckets 是 32 个桶的 hash（bucket_id = fnv32a(email) % 32，桶内 email 排序后
	// join "\n" 取 md5；空桶 = md5("")）。面板逐桶比对后只回差异桶。
	//
	// 老面板不认这个键（忽略），老 agent 不上报（面板按「没有」处理，走全量读）——
	// 双向都向后兼容。
	//
	// 用切片而不是 [32]string：encoding/json 的 omitempty 对定长数组无效，每页回执都会
	// 白背 32 个 hash（约 1.1KB，离 39000 上限更近）。切片才能「心跳带上、回执不带」。
	// 面板判「可用」看的是「恰好 32 个非空 hash」，不是键在不在。
	Buckets []string `json:"buckets,omitempty"`
}

// clientsFingerprint 计算配置里的整体指纹 + 32 桶指纹。
//
// 解析失败返回 nil：调用方据此**不上报** clients_fp（面板不会短路，照旧每轮全量读）。
func clientsFingerprint(config []byte) *clientsFingerprintBody {
	res, err := parseClientList(config)
	if err != nil {
		return nil
	}
	return fingerprintOf(res.Clients, res.Total)
}

// fingerprintOf 从「已解析的名单」算整体指纹 + 分桶指纹（纯函数）。
//
// 整体指纹：排序后按 "email|enable|totalGB|expiryTime\n" 逐行喂 md5 —— 与入站顺序、
// client 对象的键序无关，同一份名单永远得到同一个串。
//
// 分桶指纹只看 email（成员集合），不看 enable/totalGB —— 面板拿它判「这个桶的成员
// 有没有变」，字段漂移由读回的实际值判。
func fingerprintOf(clients []clientListClient, total int) *clientsFingerprintBody {
	lines := make([]string, 0, len(clients))
	bucketEmails := make([][]string, clientBucketCount)
	for _, c := range clients {
		lines = append(lines, strings.Join([]string{
			c.Email,
			fpBool(c.Enable),
			fpValue(c.TotalGB),
			fpValue(c.ExpiryTime),
		}, "|"))
		// 没有 email 的 client（中转凭据这类）没有身份，不进任何桶 —— 分桶哈希
		// 定义在 email 上，它们本来也没法参与逐桶比对（面板没有对应条目）。
		if c.Email != "" {
			b := clientBucketID(c.Email)
			bucketEmails[b] = append(bucketEmails[b], c.Email)
		}
	}
	sort.Strings(lines)

	h := md5.New()
	for _, line := range lines {
		_, _ = h.Write([]byte(line))
		_, _ = h.Write([]byte{'\n'})
	}
	body := &clientsFingerprintBody{
		N:       total,
		Sum:     hex.EncodeToString(h.Sum(nil)),
		Buckets: make([]string, clientBucketCount),
	}
	for i, emails := range bucketEmails {
		body.Buckets[i] = bucketSum(emails)
	}
	return body
}

// clientBucketID 把 email 映射到桶号：fnv32a(email) % 32。
//
// ⚠️ 面板侧必须用同一算法（reconcile_bare.go 的 bareBucketID）。FNV-1a 是标准库实现，
// 两侧天然一致；不能换 hash（换了要同时升级两侧，否则优化失效）。
func clientBucketID(email string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(email))
	return int(h.Sum32() % clientBucketCount)
}

// bucketSum 是桶内 email 的聚合 hash：排序后 join "\n" 再 md5。
//
// 空桶 = md5("") 的十六进制（join 空列表就是 ""，同一个公式自然得出）—— 两侧都不许
// 给空桶另立常量，否则一边改一边忘就又对不上了。
func bucketSum(emails []string) string {
	sort.Strings(emails)
	h := md5.New()
	_, _ = h.Write([]byte(strings.Join(emails, "\n")))
	return hex.EncodeToString(h.Sum(nil))
}

// readClientsFingerprint 读节点 config.json 并算指纹；读不到/解析失败返回 nil
// （心跳里就不带 clients_fp，面板完全走现状）。
func readClientsFingerprint(p Paths) *clientsFingerprintBody {
	raw, err := os.ReadFile(p.Config)
	if err != nil {
		return nil
	}
	return clientsFingerprint(raw)
}

// fpBool 把 enable 归一成 "1"/"0"。
func fpBool(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// fpValue 把 totalGB / expiryTime 归一成稳定文本。
//
// 数字按十进制原文（json.Number 保精度，不经 float64）；缺省按 0 —— 与 client_list
// 回执里「缺字段给 0」同口径（内核里 0 = 不限流量 / 不过期）。字符串原样（expiryTime
// 可能是 ISO8601），其它类型用默认格式，保证同一份配置永远得到同一个串。
func fpValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "0"
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
		return t.String()
	case bool:
		return fpBool(t)
	case string:
		return strings.TrimSpace(t)
	default:
		return fmt.Sprint(t)
	}
}
