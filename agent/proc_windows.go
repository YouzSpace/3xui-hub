//go:build windows

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// amRoot Windows 无 root 概念（生产 agent 只跑 Linux）。
func amRoot() bool { return false }

// processAlive 进程存活检查（Windows：GetExitCodeProcess == STILL_ACTIVE）。
// 注：os.FindProcess 在 Windows 上对已退出进程也可能成功，不能用它判断存活。
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// processCmdline Windows 无 /proc，跳过 cmdline 校验。
func processCmdline(int) (string, bool) {
	return "", false
}

// defaultScanProcEntries Windows 无 /proc，无法按命令行回扫内核；返回空表。
// 生产环境为 Linux（走 proc_unix.go 的 /proc 回扫），Windows 仅本地开发/测试。
func defaultScanProcEntries() []procEntry {
	return nil
}

func spawnAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}

// killProcess Windows：直接 TerminateProcess。
func killProcess(pid int, force bool) {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
}

// listenPortConns Windows 本地开发环境跳过（Linux 生产走 proc_unix.go）。
func listenPortConns(map[int]bool) int {
	return 0
}
