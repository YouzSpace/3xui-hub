//go:build !windows

package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// amRoot 是否以 root 运行（装 iptables 这类系统改动的前置条件）。
func amRoot() bool { return os.Geteuid() == 0 }

// processAlive 进程存活检查（Unix：signal 0）。
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// processCmdline 读取进程命令行（/proc/PID/cmdline；Linux 专属）。
func processCmdline(pid int) (string, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// defaultScanProcEntries 遍历 /proc 收集进程表（pid + cmdline）。
// 读不到 cmdline 的进程（内核线程 / 已退出 / 权限不足）跳过；读到的解析为普通字符串
// （NUL 分隔由调用方的分隔逻辑处理）。
func defaultScanProcEntries() []procEntry {
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	entries := make([]procEntry, 0, len(dirs))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(d.Name())
		if err != nil || pid <= 0 {
			continue
		}
		cmd, ok := processCmdline(pid)
		if !ok || cmd == "" {
			continue
		}
		entries = append(entries, procEntry{Pid: pid, Cmdline: cmd})
	}
	return entries
}

// spawnAttrs 启动属性（新会话，脱离 agent 进程组）。
func spawnAttrs() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// killProcess 发送信号（force=false → TERM；true → KILL）。
func killProcess(pid int, force bool) {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(pid, sig)
}

// listenPortConns 统计本机监听端口上的 ESTABLISHED 连接数（/proc/net/tcp{,6}）。
func listenPortConns(ports map[int]bool) int {
	if len(ports) == 0 {
		return 0
	}
	count := 0
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		count += countEstablishedIn(f, ports)
	}
	return count
}
