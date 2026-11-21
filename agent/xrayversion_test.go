package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 真实 `xray version` 首行样例（v26.x 实测格式），后面还有两行构建信息。
const sampleXrayVersionOutput = "Xray 26.7.28 (Xray, Penetrates Everything.) Custom (go1.25.0 linux/amd64)\n" +
	"A unified platform for anti-censorship.\n"

// parseXrayVersion 取首行第二个字段；大小写 / v 前缀 / 多行 / 脏输出都要认得对、认得稳。
func TestParseXrayVersion(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"标准输出", sampleXrayVersionOutput, "26.7.28"},
		{"带 v 前缀", "Xray v26.4.15 (Xray, Penetrates Everything.)\n", "26.4.15"},
		{"小写前缀", "xray 26.3.27 (Xray, Penetrates Everything.)\n", "26.3.27"},
		{"只有首行", "Xray 26.7.28\n", "26.7.28"},
		{"空输出", "", ""},
		{"首行不是 xray", "Something else 1.2.3\n", ""},
		{"缺版本字段", "Xray\n", ""},
		{"首行空白", "\nXray 26.7.28\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseXrayVersion(c.out); got != c.want {
				t.Fatalf("parseXrayVersion(%q) = %q，期望 %q", c.out, got, c.want)
			}
		})
	}
}

// writeTestState 在临时目录写一份 state.json 并返回路径。
func writeTestState(t *testing.T, xrayVersion string) (Paths, string) {
	t.Helper()
	dir := t.TempDir()
	p := Paths{Dir: dir, State: filepath.Join(dir, "state.json")}
	st := &State{HubURL: "http://127.0.0.1:8080", NodeSecret: "s", XrayVersion: xrayVersion, KernelArch: "linux-64"}
	if err := saveState(p.State, st); err != nil {
		t.Fatalf("写 state.json 失败: %v", err)
	}
	return p, p.State
}

// stubProbe 替换探测实现，测试结束后恢复。
func stubProbe(t *testing.T, out string, err error) {
	t.Helper()
	orig := probeXrayVersionOutput
	t.Cleanup(func() { probeXrayVersionOutput = orig })
	probeXrayVersionOutput = func(Paths) (string, error) { return out, err }
}

// 手工替换内核（26.3.27 → 26.4.15）后，state.json 必须被写回实测版本，
// 否则心跳一直报旧版本，面板节点列表永远显示 26.3.27。
func TestSyncXrayVersionUpdatesState(t *testing.T) {
	p, path := writeTestState(t, "26.3.27")
	stubProbe(t, sampleXrayVersionOutput, nil)

	st, err := loadState(path)
	if err != nil {
		t.Fatalf("读 state.json 失败: %v", err)
	}
	syncXrayVersion(p, st)

	if st.XrayVersion != "26.7.28" {
		t.Fatalf("内存里的版本应更新为 26.7.28，实际 %q", st.XrayVersion)
	}
	// 落盘才算数：下次启动 / 心跳读的是文件。
	reloaded, err := loadState(path)
	if err != nil {
		t.Fatalf("重新读 state.json 失败: %v", err)
	}
	if reloaded.XrayVersion != "26.7.28" {
		t.Fatalf("state.json 应写回 26.7.28，实际 %q", reloaded.XrayVersion)
	}
	// 其他字段不能被写坏。
	if reloaded.HubURL != "http://127.0.0.1:8080" || reloaded.NodeSecret != "s" {
		t.Fatalf("state.json 其他字段被改动: %+v", reloaded)
	}
}

// 版本一致时不重写文件（避免每次启动都动 state.json，也方便运维看修改时间判断是否变过）。
func TestSyncXrayVersionNoopWhenSame(t *testing.T) {
	p, path := writeTestState(t, "26.7.28")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	stubProbe(t, sampleXrayVersionOutput, nil)

	st, _ := loadState(path)
	syncXrayVersion(p, st)

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("版本没变却重写了 state.json")
	}
}

// 探测失败 / 输出解析不出来时保留旧值，绝不写坏 state.json、也不阻断启动。
func TestSyncXrayVersionKeepsOldOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
	}{
		{"执行失败", "", errors.New("exec: not found")},
		{"输出不合法", "garbage output\n", nil},
		{"空输出", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, path := writeTestState(t, "26.3.27")
			stubProbe(t, tc.out, tc.err)

			st, _ := loadState(path)
			syncXrayVersion(p, st)

			if st.XrayVersion != "26.3.27" {
				t.Fatalf("失败时应保留旧版本 26.3.27，实际 %q", st.XrayVersion)
			}
			reloaded, err := loadState(path)
			if err != nil {
				t.Fatalf("state.json 不应被写坏: %v", err)
			}
			if reloaded.XrayVersion != "26.3.27" {
				t.Fatalf("state.json 应保持 26.3.27，实际 %q", reloaded.XrayVersion)
			}
		})
	}
}
