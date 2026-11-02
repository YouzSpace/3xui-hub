package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"
)

// 内核重启 / 配置校验入口。包级变量，测试可替换成计数桩，不必真起内核。
var (
	restartKernel  = restartXray
	validateConfig = xrayTest
)

// readAppliedVersion 已由 state.go 的 appliedVersion() 提供：文件缺失/非法都返回 0，
// 因此这里额外用 fileExists(p.Version) 区分「版本 0」与「从未应用过」。

// pullAndApply 全量配置拉取 + 校验 + 原子替换 + 必要时重启内核。
// 失败绝不替换旧配置（坏文件另存 config.bad.json 供排查）。
//
// 是否需要重启由 config_version 判定：
//   - 结构变更（入站/出站/路由/WARP/中转/订阅）面板会 bump 版本 → 重启；
//   - 用户级变更（封禁/解封/改额度/到期/绑套餐/增删用户）不 bump —— 这类变化
//     已经通过 adu/rmu 指令在内核里热应用过，全量配置落盘只为冷启动兜底。
//     此时重启内核只会白断该节点全部在线连接（线上实测单节点一天 20+ 次）。
//
// 首次落地（本地还没有 config.json）或版本文件丢失时一律重启，保证内核拿到配置。
func pullAndApply(p Paths, hub *HubClient, trigger string) error {
	etag := readFileString(p.ETag)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	resp, err := hub.FetchConfig(ctx, etag)
	if err != nil {
		return err
	}
	if resp.NotModified {
		return nil
	}

	// 判据必须在替换 config.json 之前算：fileExists(p.Config) 是「本次是否首次落地」的依据。
	// Version 文件缺失同样按「未应用过」处理（appliedVersion 会返回 0，可能与真实版本 0 混淆）。
	needRestart := !fileExists(p.Config) || !fileExists(p.Version) || resp.Version != int64(appliedVersion(p))

	// 落临时文件（.json 结尾：xray 按扩展名判断格式）
	if err := atomicWriteFile(p.ConfigNew, resp.Config, 0o644); err != nil {
		return fmt.Errorf("写临时配置失败: %w", err)
	}

	if err := validateConfig(p, p.ConfigNew); err != nil {
		logf("config 校验失败（保留旧配置，坏文件另存 config.bad.json 供排查）: %v", err)
		_ = os.Rename(p.ConfigNew, p.ConfigBad)
		return err
	}

	if fileExists(p.Config) {
		old, readErr := os.ReadFile(p.Config)
		if readErr == nil {
			_ = atomicWriteFile(p.ConfigBak, old, 0o644)
		}
	}
	if err := os.Rename(p.ConfigNew, p.Config); err != nil {
		return fmt.Errorf("替换配置失败: %w", err)
	}

	_ = atomicWriteFile(p.Version, []byte(strconv.FormatInt(resp.Version, 10)), 0o644)
	if resp.ETag != "" {
		_ = atomicWriteFile(p.ETag, []byte(resp.ETag), 0o644)
	}
	if resp.PullInterval >= 5 {
		_ = atomicWriteFile(p.Interval, []byte(strconv.Itoa(resp.PullInterval)), 0o644)
	}

	if needRestart {
		logf("全量配置已更新（version %d，触发=%s），重启内核", resp.Version, trigger)
		restartKernel(p)
		return nil
	}

	logf("全量配置已更新（version %d 未变，仅用户级变更，触发=%s），跳过内核重启", resp.Version, trigger)
	return nil
}

// configLoop 周期拉取循环（WS 的 config.changed 通知会提前唤醒并记录触发源）。
func configLoop(p Paths, hub *HubClient, trigger <-chan struct{}) {
	for {
		pi := pullIntervalCached(p, 30)
		reason := "周期"
		select {
		case <-trigger:
			reason = "变更通知"
			logf("收到配置变更通知，立即拉取")
		case <-time.After(time.Duration(pi) * time.Second):
		}

		if err := pullAndApply(p, hub, reason); err != nil {
			logf("配置拉取失败：%v", err)
		}
	}
}
