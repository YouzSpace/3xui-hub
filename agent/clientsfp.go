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

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// clientsFingerprintBody 是心跳里的 clients_fp 字段（面板契约：n / sum，别改名）。
type clientsFingerprintBody struct {
	// N 是去重后的 client 数（与 client_list 回执的 total 同口径）。
	N int `json:"n"`
	// Sum 是名单的 md5 十六进制串。
	Sum string `json:"sum"`
}

// clientsFingerprint 计算配置里的名单指纹，返回 (md5 十六进制串, 去重后客户端数)。
//
// 排序后按 "email|enable|totalGB|expiryTime\n" 逐行喂 md5：与入站顺序、client 对象的
// 键序无关，同一份名单永远得到同一个串。
//
// 解析失败返回 ("", 0)：调用方据此**不上报** clients_fp（面板不会短路，照旧每轮全量读）。
func clientsFingerprint(config []byte) (sum string, n int) {
	res, err := parseClientList(config)
	if err != nil {
		return "", 0
	}

	lines := make([]string, 0, len(res.Clients))
	for _, c := range res.Clients {
		lines = append(lines, strings.Join([]string{
			c.Email,
			fpBool(c.Enable),
			fpValue(c.TotalGB),
			fpValue(c.ExpiryTime),
		}, "|"))
	}
	sort.Strings(lines)

	h := md5.New()
	for _, line := range lines {
		_, _ = h.Write([]byte(line))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), res.Total
}

// readClientsFingerprint 读节点 config.json 并算指纹；读不到/解析失败返回 nil
// （心跳里就不带 clients_fp，面板完全走现状）。
func readClientsFingerprint(p Paths) *clientsFingerprintBody {
	raw, err := os.ReadFile(p.Config)
	if err != nil {
		return nil
	}
	sum, n := clientsFingerprint(raw)
	if sum == "" {
		return nil
	}
	return &clientsFingerprintBody{N: n, Sum: sum}
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
