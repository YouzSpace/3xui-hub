package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// hubCandidateURLs 返回面板地址候选（issue #88）：v4（hub_url）优先，v6 兜底。
//
// 顺序即优先级：newHubClientMulti 会去重、跳过空串，所以没配 hub_url_v6 的老节点
// 拿到的仍是单元素列表，行为与改动前一致。
func hubCandidateURLs(st *State) []string {
	return []string{strings.TrimRight(st.HubURL, "/"), strings.TrimRight(st.HubURLV6, "/")}
}

// isVersionQuery 判断命令行是否只是查版本（agent --version）。
func isVersionQuery(args []string) bool {
	for _, a := range args[1:] {
		switch a {
		case "-version", "--version", "version":
			return true
		}
	}

	return false
}

// wsRuntime 当前活跃 WS 会话（nil = WS 未连接，走 HTTP 降级通道）。
type wsRuntime struct {
	client *WSClient
}

func main() {
	// -version / --version：只打印版本就退出，绝不落进常驻逻辑。
	// 运维手工查版本时若把 binary 当常驻进程跑起来，会和 systemd 里的实例共用同一个
	// node secret 去连 WS，服务端「新连接踢旧连接」让两边互相踢 —— 表现为 WS 每秒
	// 断开重连（实测复现）。安装/巡检脚本一律用 --version 查版本。
	if isVersionQuery(os.Args) {
		fmt.Println(agentVersion)
		return
	}

	p := resolvePaths()
	_ = os.MkdirAll(p.Dir, 0o755)
	_ = os.MkdirAll(p.Tmp, 0o755)

	st, err := loadState(p.State)
	if err != nil {
		logf("缺少 state.json（请用面板生成的安装命令重装）: %v", err)
		os.Exit(1)
	}
	if st.HubURL == "" || st.NodeSecret == "" {
		logf("state.json 缺 hub_url / node_secret")
		os.Exit(1)
	}
	if !fileExists(p.XrayBin) {
		logf("缺少 %s 内核（请重新执行安装命令）", p.XrayBin)
		os.Exit(1)
	}

	hub := newHubClientMulti(hubCandidateURLs(st), st.NodeSecret)
	logf("启动 agent v%s（%s → %s）", agentVersion, p.Dir, st.HubURL)
	if st.HubURLV6 != "" {
		logf("面板备用地址（IPv6 兜底）：%s", st.HubURLV6)
	}

	// 1) 注册：只负责"连上"（业务材料由面板侧管理）
	var reg *RegisterInfo
	for {
		reg, err = hub.Register(context.Background(), RegisterBody{
			XrayVersion:  st.XrayVersion,
			AgentVersion: agentVersion,
			KernelArch:   st.KernelArch,
			Hostname:     hostname(),
		})
		if err == nil {
			break
		}
		logf("注册未成功：%v，60s 后重试（检查节点密钥/面板可达性）", err)
		time.Sleep(60 * time.Second)
	}
	logf("注册成功（%s，config_version=%d，pull_interval=%ds）", hub.Base(), reg.ConfigVersion, reg.PullInterval)
	_ = atomicWriteFile(p.Interval, []byte(strconv.Itoa(reg.PullInterval)), 0o644)

	// 2) 首次拉取全量配置（失败 15s 重试）
	for {
		if err = pullAndApply(p, hub, "首次"); err == nil {
			break
		}
		logf("首次拉取配置未成功：%v，15s 后重试", err)
		time.Sleep(15 * time.Second)
	}

	// 3) 启动内核（起不来由看护循环持续重试）
	if err := startXray(p); err != nil {
		logf("xray 启动失败（看护循环会持续重试）: %v", err)
	}

	logf("进入常驻循环：ws / report / config / execute / supervise")

	configCh := make(chan struct{}, 1)
	var wsState atomic.Pointer[wsRuntime]

	go superviseLoop(p)
	go configLoop(p, hub, configCh)
	go reportLoop(p, hub, &wsState, st)
	go executeFallbackLoop(p, hub, &wsState)

	wsMainLoop(p, st, reg, &wsState, configCh) // 阻塞
}

// wsMainLoop WS 主通道：连接-鉴权-收消息；断开自动指数退避重连。
func wsMainLoop(p Paths, st *State, reg *RegisterInfo, wsState *atomic.Pointer[wsRuntime], configCh chan struct{}) {
	if !reg.Ws.Enabled || reg.Ws.URL == "" {
		logf("面板未启用 WS（agent 以 HTTP 降级模式运行）")
		select {}
	}

	backoff := time.Second
	for {
		client := newWSClient(reg.Ws.URL, st.NodeSecret)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := client.connect(ctx, map[string]any{
			"agent_version":  agentVersion,
			"xray_version":   st.XrayVersion,
			"kernel_arch":    st.KernelArch,
			"hostname":       hostname(),
			"config_version": appliedVersion(p),
			"at":             time.Now().Unix(),
		})
		cancel()

		if err != nil {
			logf("WS 连接失败：%v（%v 后重试）", err, backoff)
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
			continue
		}

		backoff = time.Second
		wsState.Store(&wsRuntime{client: client})
		logf("WS 已连接：%s", reg.Ws.URL)

		err = client.readLoop(func(event string, data json.RawMessage) {
			switch event {
			case "hello.ok":
				logf("WS 鉴权成功")
			case "ping":
				_ = client.Send("pong", nil)
			case "config.changed":
				select {
				case configCh <- struct{}{}:
				default:
				}
			case "commands":
				handleCommandsPush(p, client, data)
			}
		})

		wsState.Store(nil)
		client.Close()
		logf("WS 断开：%v，1s 后重连", err)
		time.Sleep(time.Second)
	}
}

// handleCommandsPush WS 推送的指令：立即执行 + 回执。
func handleCommandsPush(p Paths, client *WSClient, data json.RawMessage) {
	var payload struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || len(payload.Commands) == 0 {
		return
	}

	results := make([]AckResult, 0, len(payload.Commands))
	for _, cmd := range payload.Commands {
		r := runCommand(p, cmd)
		results = append(results, r)
		if r.Success {
			logf("指令 #%d 执行成功（%s）", cmd.ID, cmd.Action)
		} else {
			logf("指令 #%d 执行失败（%s）：%s", cmd.ID, cmd.Action, truncate(r.Output, 300))
		}
	}

	if err := client.Send("ack", map[string]any{"results": results}); err != nil {
		logf("指令回执发送失败：%v", err)
	}
	// 升级指令：回执已尽力发出，退出让 systemd 拉起新版（无条件，不看回执是否成功）
	exitIfUpgradeRequested()
	cleanTmpAduFiles(p)
}

// executeFallbackLoop HTTP 降级：WS 未连接时用长轮询取指令。
func executeFallbackLoop(p Paths, hub *HubClient, wsState *atomic.Pointer[wsRuntime]) {
	for {
		if wsState.Load() != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		runOneCycleHTTP(p, hub)
	}
}

func runOneCycleHTTP(p Paths, hub *HubClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	cmds, err := hub.Execute(ctx)
	cancel()
	if err != nil {
		logf("execute 失败：%v", err)
		time.Sleep(5 * time.Second)
		return
	}
	if len(cmds) == 0 {
		return
	}

	results := make([]AckResult, 0, len(cmds))
	for _, cmd := range cmds {
		r := runCommand(p, cmd)
		results = append(results, r)
		if r.Success {
			logf("指令 #%d 执行成功（%s）", cmd.ID, cmd.Action)
		} else {
			logf("指令 #%d 执行失败（%s）：%s", cmd.ID, cmd.Action, truncate(r.Output, 300))
		}
	}

	actx, acancel := context.WithTimeout(context.Background(), 20*time.Second)
	err = hub.Ack(actx, results)
	acancel()
	if err != nil {
		logf("指令回执失败：%v", err)
	}
	// 升级指令：回执已尽力发出，退出让 systemd 拉起新版
	exitIfUpgradeRequested()
	cleanTmpAduFiles(p)
}

// buildReportPayloads 把一轮采集内容装成待发的一条或多条 report 消息。
//
// 面板单条消息只接受 maxStatsPerPush 条流量（超过整包丢弃、该批用户不落账），
// 而首连/重连与 HTTP 降级都会整份重推，单节点活跃用户破 2000 就会撞线，所以
// 这里按 email 字典序切片：
//   - metrics / config_version / bbr 每条都带（每轮相同，重复写无害）
//   - clients_fp 也每条都带（面板据它决定本轮要不要发 client_list；
//     分片时挂在每条上只是因为每条都可能被面板当成一次心跳，重复写同值无害）
//   - outbounds 只挂第一条（避免同一批出站流量被重复写）
//   - withStats=false（本轮统计采集失败）时只发指标那一条，不带 stats/outbounds
func buildReportPayloads(metrics Metrics, configVersion int, bbr bbrStatus, stats, outbounds map[string]StatsPair, withStats bool, clientsFP *clientsFingerprintBody) []map[string]any {
	base := func() map[string]any {
		payload := map[string]any{
			"metrics":        metrics,
			"config_version": configVersion,
			"bbr":            bbr,
		}
		// 读不到 config.json（或解析失败）时 fp=nil → 不带该键：面板照旧每轮全量读，
		// 绝不拿「没算出来」当成「名单没变」。
		if clientsFP != nil {
			payload["clients_fp"] = *clientsFP
		}
		return payload
	}

	if !withStats {
		return []map[string]any{base()}
	}

	chunks := chunkStats(stats, maxStatsPerPush)
	if len(chunks) == 0 {
		// 本轮没有要推的流量（增量全 0），但仍要带出站流量。
		payload := base()
		payload["outbounds"] = outbounds
		return []map[string]any{payload}
	}

	out := make([]map[string]any, 0, len(chunks))
	for i, chunk := range chunks {
		payload := base()
		payload["stats"] = chunk
		if i == 0 {
			payload["outbounds"] = outbounds
		}
		out = append(out, payload)
	}
	return out
}

// sendReportPayloads 逐条发送 report 分片，任一条失败立即返回（调用方据此不推进
// 已推送基线，下轮整份重推；推的是绝对值，重复推幂等）。
func sendReportPayloads(client *WSClient, payloads []map[string]any) error {
	for _, payload := range payloads {
		if err := client.Send("report", payload); err != nil {
			return err
		}
	}
	return nil
}

// pushStatsChunked 按面板单条上限分片推送整张流量表（HTTP 降级通道）。
// 成功推完返回批数；任一批失败立即返回错误（已推的批次幂等，下轮整份重推即可）。
func pushStatsChunked(hub *HubClient, stats map[string]StatsPair) (int, error) {
	chunks := chunkStats(stats, maxStatsPerPush)
	for i, chunk := range chunks {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := hub.Push(ctx, chunk)
		cancel()
		if err != nil {
			return i, err
		}
	}
	return len(chunks), nil
}

// reportLoop 每 5s 采集：流量 + 出站流量 + 监控指标。
// WS 模式随 report 消息推送；HTTP 降级模式走 push（有变化才推）+ alive（30s）。
func reportLoop(p Paths, hub *HubClient, wsState *atomic.Pointer[wsRuntime], st *State) {
	collector := newMetricsCollector()
	var lastAlive time.Time
	lastPushHash := ""
	// BBR 运行态读 /proc/sys，30s 一次足够；每 5s 重读只是白烧 IO。
	var bbr bbrStatus
	var bbrAt time.Time

	// WS 主通道「有变化才带」的基线：上轮已推给 hub 的绝对值。
	// lastWsRuntime 用来识别重连 —— 换了连接就把基线作废，下一轮整份重推（hub 断线期间
	// 的变化只能靠全量重推拉平）。
	var lastWsStats map[string]StatsPair
	var lastWsRuntime *wsRuntime

	for {
		ports := listenPortsFromConfig(p)
		conns := listenPortConns(ports)

		// 名单指纹（issue #115 3.5）：与 client_list 一样每轮读一次 config.json，
		// 面板据它短路掉「名单没变」时的全量读回。读不到就 nil（心跳不带该键）。
		clientsFP := readClientsFingerprint(p)

		stats, statsErr := queryStats(p, "user")
		var outbounds map[string]StatsPair
		if statsErr == nil {
			outbounds, _ = queryStats(p, "outbound")
		}

		var statsForMetrics map[string]StatsPair
		if statsErr == nil {
			statsForMetrics = stats
		}
		metrics := collector.collect(conns, statsForMetrics)

		if time.Since(bbrAt) >= 30*time.Second {
			bbr = collectBBRStatus()
			bbrAt = time.Now()
		}

		if wr := wsState.Load(); wr != nil {
			if wr != lastWsRuntime {
				lastWsRuntime = wr
				lastWsStats = nil
			}

			// 全量推（首连/重连）会一次带上所有用户，超过面板单条上限就整批丢账；
			// buildReportPayloads 按 maxStatsPerPush 切片成多条 report 逐条发。
			payloads := buildReportPayloads(metrics, appliedVersion(p), bbr, changedStats(lastWsStats, stats), outbounds, statsErr == nil, clientsFP)
			if err := sendReportPayloads(wr.client, payloads); err != nil {
				logf("report 发送失败：%v", err)
			} else if statsErr == nil {
				lastWsStats = stats // 只在全部片发送成功后推进基线，失败下轮重推
			}
		} else {
			if statsErr == nil && len(stats) > 0 {
				h := hashStats(stats)
				if h != lastPushHash {
					// 同 WS：全量推可能超过面板 /push 的单条上限（超了 422 整批不落账），
					// 按 email 字典序切片逐批推；绝对值重复推幂等。
					if _, err := pushStatsChunked(hub, stats); err == nil {
						lastPushHash = h
					} else {
						logf("流量推送失败：%v，下轮重试", err)
					}
				}
			}
			if time.Since(lastAlive) >= 30*time.Second {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, err := hub.Alive(ctx, AliveBody{
					XrayVersion:   st.XrayVersion,
					ConfigVersion: appliedVersion(p),
					CPU:           metrics.CPU,
					Mem:           metrics.Mem,
					Uptime:        metrics.Uptime,
					Conns:         metrics.Conns,
					ActiveUsers:   metrics.ActiveUsers,
					UpSpeed:       metrics.UpSpeed,
					DownSpeed:     metrics.DownSpeed,
					NetTotalUp:    metrics.NetTotalUp,
					NetTotalDown:  metrics.NetTotalDown,
					BBR:           &bbr,
					ClientsFP:     clientsFP,
				})
				cancel()
				if err == nil {
					lastAlive = time.Now()
				} else {
					logf("心跳失败：%v", err)
				}
			}
		}

		time.Sleep(5 * time.Second)
	}
}
