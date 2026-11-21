package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// 内核版本探测（真机缺陷 #2）：state.json 的 xray_version 是安装时写死的，
// 运维手工替换 xray 二进制后它不会变，面板就永远显示旧版本。agent 启动时
// 实测一次 `xray version`，与 state 不同则更新并写回 state.json，
// 之后的注册 / 心跳（WS hello、HTTP alive）上报的都是真实版本。

// xrayVersionTimeout 是 `xray version` 的探测时限：正常毫秒级返回，
// 卡住（磁盘损坏 / 架构不匹配）也不能拖住 agent 启动。
const xrayVersionTimeout = 10 * time.Second

// probeXrayVersionOutput 跑 `xray version` 并返回原始输出；测试可替换。
var probeXrayVersionOutput = func(p Paths) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), xrayVersionTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.XrayBin, "version")
	cmd.Dir = p.Dir // 与其他内核调用一致：工作目录在节点目录
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("执行 xray version 失败: %w", err)
	}
	return string(out), nil
}

// parseXrayVersion 从 `xray version` 输出里取版本号：首行形如
// `Xray 26.7.28 (Xray, Penetrates Everything.) ...`，取第二个字段，
// 与安装脚本（awk '{print $2}'）口径一致；带 v 前缀时去掉。
// 解析不出来返回空串，调用方据此保留 state 里的旧值（宁可显示旧的，也不写坏值）。
func parseXrayVersion(out string) string {
	line := out
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	if len(fields) < 2 || !strings.EqualFold(fields[0], "xray") {
		return ""
	}
	return strings.TrimPrefix(fields[1], "v")
}

// syncXrayVersion 用实测版本校准 state，不同则写回 state.json。
// 探测 / 解析失败只记日志、保留旧值，绝不阻断 agent 启动。
func syncXrayVersion(p Paths, st *State) {
	out, err := probeXrayVersionOutput(p)
	if err != nil {
		logf("探测内核版本失败（沿用 state 里的 %q）：%v", st.XrayVersion, err)
		return
	}
	version := parseXrayVersion(out)
	if version == "" {
		logf("内核版本输出无法解析（沿用 state 里的 %q）：%s", st.XrayVersion, truncate(out, 200))
		return
	}
	if version == st.XrayVersion {
		return
	}

	old := st.XrayVersion
	st.XrayVersion = version
	if err := saveState(p.State, st); err != nil {
		// 内存里已经是真实版本（本次注册/心跳会报对），只是下次启动还得再探测一次。
		logf("写回 state.json 失败（内核版本 %s → %s，本次仍按新版本上报）：%v", old, version, err)
		return
	}
	logf("内核版本已校准：%s → %s（实测 xray version）", old, version)
}
