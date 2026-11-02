package main

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
	"time"
)

// Metrics 实时监控指标（随 report/心跳上报，面板节点页展示）。
//
// NetTotalUp/Down 用**指针**：读不到 /proc/net/dev（非 Linux）时保持 nil，面板按「无数据」
// 展示；而 0 是合法读数（机器刚重启），两者必须能区分 —— 与 cpu/mem 同一口径。
type Metrics struct {
	CPU          *float64 `json:"cpu"`
	Mem          *float64 `json:"mem"`
	Uptime       int64    `json:"uptime"`
	Conns        int      `json:"conns"`
	ActiveUsers  int      `json:"active_users"`
	UpSpeed      float64  `json:"up_speed"`        // bytes/s（网卡差分）
	DownSpeed    float64  `json:"down_speed"`      // bytes/s（网卡差分）
	NetTotalUp   *uint64  `json:"net_total_up"`    // 机器网卡累计发送字节
	NetTotalDown *uint64  `json:"net_total_down"`  // 机器网卡累计接收字节
	At           int64    `json:"at"`
}

type cpuSnapshot struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (c cpuSnapshot) busy() uint64 {
	return c.user + c.nice + c.system + c.irq + c.softirq + c.steal
}

func (c cpuSnapshot) total() uint64 {
	return c.busy() + c.idle + c.iowait
}

func readCPUSnapshot() (cpuSnapshot, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSnapshot{}, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuSnapshot{}, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 8 || fields[0] != "cpu" {
		return cpuSnapshot{}, false
	}
	var vals [8]uint64
	for i := 1; i <= 7 && i < len(fields); i++ {
		vals[i-1], _ = strconv.ParseUint(fields[i], 10, 64)
	}
	return cpuSnapshot{
		user: vals[0], nice: vals[1], system: vals[2], idle: vals[3],
		iowait: vals[4], irq: vals[5], softirq: vals[6], steal: vals[7],
	}, true
}

func readMemPercent() (float64, bool) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	defer f.Close()

	var total, avail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseMeminfoValue(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = parseMeminfoValue(line)
		}
	}
	if total == 0 {
		return 0, false
	}
	if avail > total {
		avail = total
	}
	return float64(total-avail) * 100 / float64(total), true
}

func parseMeminfoValue(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, _ := strconv.ParseUint(fields[1], 10, 64)
	return v // kB
}

func readUptime() int64 {
	f, err := os.Open("/proc/uptime")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0
	}
	fields := strings.Fields(sc.Text())
	if len(fields) == 0 {
		return 0
	}
	if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
		return int64(v)
	}
	return 0
}

// countEstablishedIn 统计 /proc/net/tcp 格式文件中本地端口 ∈ ports 的 ESTABLISHED 连接数。
func countEstablishedIn(path string, ports map[int]bool) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	count := 0
	sc := bufio.NewScanner(f)
	sc.Scan() // 跳过表头
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != "01" { // 01 = ESTABLISHED
			continue
		}
		// local_address 形如 "0100007F:1F90"（hex 端口）
		idx := strings.LastIndex(fields[1], ":")
		if idx < 0 {
			continue
		}
		port, err := strconv.ParseInt(fields[1][idx+1:], 16, 32)
		if err != nil {
			continue
		}
		if ports[int(port)] {
			count++
		}
	}
	return count
}

// MetricsCollector 采集器：跨轮保存差分状态（CPU / 速度 / 活跃用户）。
type MetricsCollector struct {
	hasCPU  bool
	prevCPU cpuSnapshot

	// 网卡累计差分：两次 readNetTotals 的差 / 间隔秒数。
	hasNet        bool
	prevNetAt     time.Time
	prevNetSent   uint64
	prevNetRecv   uint64

	growthAt map[string]time.Time // email → 最近一次有流量增量的时刻
	prevUser map[string]int64     // email → 上轮 up+down 总量
}

func newMetricsCollector() *MetricsCollector {
	return &MetricsCollector{
		growthAt: map[string]time.Time{},
		prevUser: map[string]int64{},
	}
}

// collect 生成一轮指标（stats 可为 nil —— 内核未就绪时只报系统指标）。
func (m *MetricsCollector) collect(systemConns int, stats map[string]StatsPair) Metrics {
	metrics := Metrics{At: time.Now().Unix()}

	// CPU：与上轮差分；首轮即时采样（200ms 窗口）
	if snap, ok := readCPUSnapshot(); ok {
		if m.hasCPU {
			busyDiff := snap.busy() - m.prevCPU.busy()
			totalDiff := snap.total() - m.prevCPU.total()
			if totalDiff > 0 && busyDiff <= totalDiff {
				v := float64(busyDiff) * 100 / float64(totalDiff)
				metrics.CPU = &v
			}
		} else {
			time.Sleep(200 * time.Millisecond)
			if snap2, ok2 := readCPUSnapshot(); ok2 {
				busyDiff := snap2.busy() - snap.busy()
				totalDiff := snap2.total() - snap.total()
				if totalDiff > 0 {
					v := float64(busyDiff) * 100 / float64(totalDiff)
					metrics.CPU = &v
				}
				m.prevCPU = snap2
				m.hasCPU = true
			}
		}
		if metrics.CPU != nil {
			m.prevCPU = snap
			m.hasCPU = true
		}
	}

	if mem, ok := readMemPercent(); ok {
		metrics.Mem = &mem
	}
	metrics.Uptime = readUptime()
	metrics.Conns = systemConns

	// 网卡累计收发 + 实时速率：机器级读数，与内核 stats 无关。
	// 之前 speed 来自 stats 差分，xray 内核拿不到 stats 时（statsErr≠nil）永远为 0，
	// 节点卡片上的实时上/下行于是长期是 0 —— 换成网卡 delta 后不再依赖内核。
	now := time.Now()
	if sent, recv, ok := readNetTotals(); ok {
		metrics.NetTotalUp = &sent
		metrics.NetTotalDown = &recv

		if m.hasNet {
			elapsed := now.Sub(m.prevNetAt).Seconds()
			metrics.UpSpeed = netSpeed(sent, m.prevNetSent, elapsed)
			metrics.DownSpeed = netSpeed(recv, m.prevNetRecv, elapsed)
		}
		m.prevNetSent = sent
		m.prevNetRecv = recv
		m.prevNetAt = now
		m.hasNet = true
	}

	// 活跃用户：仍基于内核 stats 差分（不受上面的网卡口径影响）
	if stats != nil {
		for email, pair := range stats {
			total := pair.Up + pair.Down
			if prev, ok := m.prevUser[email]; ok && total > prev {
				m.growthAt[email] = now
			}
			m.prevUser[email] = total
		}

		// 活跃 = 最近 60s 内有增量的 email
		active := 0
		for email, at := range m.growthAt {
			if now.Sub(at) <= 60*time.Second {
				active++
			} else {
				delete(m.growthAt, email)
			}
		}
		metrics.ActiveUsers = active
	}

	return metrics
}

// netSpeed 由两次网卡累计值算速率（bytes/s）。
// 计数器重置（机器重启 / 接口重置）时 cur < prev → 0，不能落到负速率；
// 间隔不足 1s 时差分噪声太大，同样返回 0。
func netSpeed(cur, prev uint64, elapsedSeconds float64) float64 {
	if elapsedSeconds < 1 || cur <= prev {
		return 0
	}
	return float64(cur-prev) / elapsedSeconds
}

// readNetTotals 读取本机网卡累计收发字节（排除虚拟接口）。
// 非 Linux（无 /proc/net/dev）→ ok=false，调用方按「无数据」处理。
func readNetTotals() (sent, recv uint64, ok bool) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	sent, recv = parseNetDev(data)
	return sent, recv, true
}

// virtualInterfacePrefixes 参与流量统计时需排除的虚拟/隧道接口前缀。
// lo 单独精确匹配（"lo" 也是前缀，但 "lo" 只该匹配回环自身，不能误伤 lo 开头的物理口）。
var virtualInterfacePrefixes = []string{
	"docker", "veth", "br-", "virbr", "vnet", "tun", "tap", "wg",
}

func isVirtualInterface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range virtualInterfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// parseNetDev 解析 /proc/net/dev，累加非虚拟接口的 发送/接收 字节数。
//
// 行格式（接口名与冒号之间可能有空格，字段以空白分隔）：
//
//	eth0: 12345 7 0 0 0 0 0 0 67890 8 0 0 0 0 0 0
//	       ↑ 收发各 8 列，第 1 列是字节数
//
// 畸形行（字段不足、数值非法）跳过，不 panic。
func parseNetDev(data []byte) (sent, recv uint64) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		idx := strings.IndexByte(line, ':')
		if idx < 0 {
			continue // 表头两行没有冒号
		}
		name := strings.TrimSpace(line[:idx])
		if name == "" || isVirtualInterface(name) {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 9 {
			continue // 收(8) + 发(8)，至少要有到发送字节位
		}
		// fields[0..7] = 接收(rx) 8 列，fields[8..15] = 发送(tx) 8 列
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			continue
		}
		recv += rx
		sent += tx
	}
	return sent, recv
}
