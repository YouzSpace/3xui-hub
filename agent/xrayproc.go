package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// xrayOpMu 串行化「启动 / 停止 / 重启」整段序列。
// 看护循环的拉起（startXray）与配置变更触发的 restartXray 并发时只放行一个，
// 杜绝同一内核被起两遍（线上「同一秒两次启动 xray 内核」→ 端口冲突崩溃循环）。
var xrayOpMu sync.Mutex

// 进程表 / 内核操作的可注入实现：生产走各平台默认（Linux 读 /proc）；
// 测试替换后判定不依赖 /proc、也不真起内核，可在 Windows 上稳定复现并发与接管行为。
var (
	procAlive       = processAlive
	scanProcEntries = defaultScanProcEntries
	spawnXray       = defaultSpawnXray
	killProc        = killProcess
)

// procEntry 一条进程记录（pid + 命令行）。
type procEntry struct {
	Pid     int
	Cmdline string
}

// pidfilePid 返回记账 pidfile 里确实存活的 pid（0 = 无有效记账）。
// 双保险：pidfile 命中 + 进程存活 + cmdline 含 xray（防 pid 复用误杀）。
func pidfilePid(p Paths) int {
	raw := readFileString(p.Pid)
	if raw == "" {
		return 0
	}
	pid, err := strconv.Atoi(raw)
	if err != nil || pid <= 0 {
		return 0
	}
	if !procAlive(pid) {
		return 0
	}
	if cmd, ok := processCmdline(pid); ok && cmd != "" && !strings.Contains(cmd, "xray") {
		return 0
	}
	return pid
}

// isXrayRunCmdline 判定命令行是否为本节点内核：含 xray 可执行、run 子命令、且 -c 指向本节点 config。
// 兼容 /proc 的 NUL 分隔与普通空格分隔两种写法。
func isXrayRunCmdline(cmdline, configPath string) bool {
	if configPath == "" {
		return false
	}
	toks := strings.FieldsFunc(cmdline, func(r rune) bool { return r == 0 || r == ' ' || r == '\t' })
	var hasBin, hasRun, hasCfg bool
	for i, t := range toks {
		if strings.Contains(t, "xray") {
			hasBin = true
		}
		if t == "run" {
			hasRun = true
		}
		if t == "-c" && i+1 < len(toks) && samePath(toks[i+1], configPath) {
			hasCfg = true
		}
	}
	return hasBin && hasRun && hasCfg
}

func samePath(a, b string) bool {
	return a == b || filepath.Clean(a) == filepath.Clean(b)
}

// findLiveXray 扫描进程表，返回正在运行本节点 `xray run -c <config>` 的内核 pid（0 = 无）。
// 进程表命中即视为存活（僵尸进程 cmdline 为空，天然不匹配）。
func findLiveXray(p Paths) int {
	for _, e := range scanProcEntries() {
		if e.Pid > 0 && isXrayRunCmdline(e.Cmdline, p.Config) {
			return e.Pid
		}
	}
	return 0
}

// xrayPid 返回当前 xray 存活 pid（0 = 不在运行）。
// 记账不可信时（pidfile 缺失 / 记账 pid 已死）回扫进程表：发现本节点内核即接管记账
// （写回 pidfile），避免「记账 pid 死了但真进程仍占着端口」被看护循环判为未运行而无限拉起。
func xrayPid(p Paths) int {
	if pid := pidfilePid(p); pid > 0 {
		return pid
	}
	if pid := findLiveXray(p); pid > 0 {
		if err := atomicWriteFile(p.Pid, []byte(strconv.Itoa(pid)), 0o644); err != nil {
			logf("接管内核记账失败（写 pidfile）: %v", err)
		}
		return pid
	}
	return 0
}

// startXray 启动内核（并发安全：已有存活内核则接管记账、不重复拉起）。
func startXray(p Paths) error {
	xrayOpMu.Lock()
	defer xrayOpMu.Unlock()
	return startXrayLocked(p)
}

func startXrayLocked(p Paths) error {
	if xrayPid(p) > 0 {
		return nil
	}
	if !fileExists(p.Config) {
		return fmt.Errorf("无 config，暂不能启动内核")
	}

	logf("启动 xray 内核")
	pid, err := spawnXray(p)
	if err != nil {
		return err
	}
	if err := atomicWriteFile(p.Pid, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		logf("pidfile 写入失败: %v", err)
	}
	return nil
}

// defaultSpawnXray 真正拉起内核（nohup 语义：脱离会话，agent 退出不带走），返回子进程 pid。
func defaultSpawnXray(p Paths) (int, error) {
	logFile, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("打开 xray 日志失败: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(p.XrayBin, "run", "-c", p.Config)
	cmd.Dir = p.Dir // geoip/geosite.dat 在 NODE_DIR，systemd 下 cwd 是 / 会导致 geo 规则失效
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = spawnAttrs()
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("启动失败: %w", err)
	}

	pid := cmd.Process.Pid
	// 回收子进程（防僵尸）；退出记录日志
	go func() {
		err := cmd.Wait()
		logf("xray 进程退出（pid=%d）: %v", pid, err)
	}()

	time.Sleep(time.Second)
	return pid, nil
}

// stopXray 停止内核（TERM 等待 ≤8s，再 KILL）。
func stopXray(p Paths) {
	xrayOpMu.Lock()
	defer xrayOpMu.Unlock()
	stopXrayLocked(p)
}

func stopXrayLocked(p Paths) {
	pid := xrayPid(p)
	if pid <= 0 {
		os.Remove(p.Pid)
		return
	}
	killProc(pid, false)
	for i := 0; i < 16; i++ {
		if !procAlive(pid) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if procAlive(pid) {
		killProc(pid, true)
	}
	// 等它从进程表消失（≤4s）：将死进程仍带 cmdline，会让紧接着的 start 误判为存活而跳过拉起
	for i := 0; i < 20 && findLiveXray(p) == pid; i++ {
		time.Sleep(200 * time.Millisecond)
	}
	os.Remove(p.Pid)
}

// restartXray 停 + 启作为一个整体序列（同一把锁）：并发下不双内核，且旧内核先清理，新配置必定生效。
func restartXray(p Paths) {
	xrayOpMu.Lock()
	defer xrayOpMu.Unlock()
	stopXrayLocked(p)
	if err := startXrayLocked(p); err != nil {
		logf("重启内核失败: %v", err)
	}
}

// superviseLoop 看护循环：xray 挂了拉起；日志超 5MB 裁剪保留末尾 1MB。
func superviseLoop(p Paths) {
	for {
		if xrayPid(p) == 0 {
			logf("检测到 xray 未在运行，重新拉起")
			if err := startXray(p); err != nil {
				logf("拉起失败: %v", err)
			}
		}

		if st, err := os.Stat(p.Log); err == nil && st.Size() > 5*1024*1024 {
			trimLogTail(p.Log, 1024*1024)
		}

		time.Sleep(10 * time.Second)
	}
}

// trimLogTail 截断日志文件保留末尾 keep 字节。
func trimLogTail(path string, keep int64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if int64(len(b)) <= keep {
		return
	}
	_ = atomicWriteFile(path, b[int64(len(b))-keep:], 0o644)
}

// xrayTest 语法校验新配置（成功返回 nil）。
func xrayTest(p Paths, configPath string) error {
	cmd := exec.Command(p.XrayBin, "-test", "-c", configPath)
	cmd.Dir = p.Dir // 同上：geo 数据文件从节点目录读取
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xray -test 失败: %s", truncate(string(out), 400))
	}
	return nil
}
