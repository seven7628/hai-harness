package runtime

// node_test.go：Node 探测（Ensure）的纯逻辑测试 —— 全部注入 RunCmd，**不 spawn
// 真进程**：版本解析/候选顺序/错误文案这些是逻辑，测它们不该依赖本机装没装 node。
//
// 「本机真有 node 时 Ensure 成功」由 execproc 的端到端测试间接覆盖（那里跑真
// node），本文件只管不依赖环境的那部分。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNode 造一个假的版本探测（记录被调用的可执行路径）。
type fakeNode struct {
	versions map[string]string // path → `node --version` 输出（空串 = 该候选不存在）
	calls    []string
}

func (f *fakeNode) run(_ context.Context, name string, _ ...string) (string, error) {
	f.calls = append(f.calls, name)
	if v, ok := f.versions[name]; ok {
		return v, nil
	}
	return "", fmt.Errorf("exec: %s: not found", name)
}

// execFile 造一个「存在且可执行」的假解释器文件（probe 只做 Stat + 版本自检）。
func execFile(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEnsurePicksVersionSatisfyingCandidate 候选里版本达标的那个被选中。
func TestEnsurePicksVersionSatisfyingCandidate(t *testing.T) {
	dir := t.TempDir()
	good := execFile(t, dir, "node")
	tooOld := execFile(t, dir, "old")
	f := &fakeNode{versions: map[string]string{
		good:        "v22.11.0\n",
		tooOld:      "v18.20.4\n",
		"/bin/nope": "",
	}}
	r := NewNodeRuntime(NodeConfig{RunCmd: f.run})
	t.Setenv("GO_CODE_NODE_CANDIDATES", strings.Join([]string{tooOld, good}, ":"))

	n, err := r.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if n.Path != good {
		t.Errorf("Path = %q, want %q", n.Path, good)
	}
	if n.Major != 22 {
		t.Errorf("Major = %d, want 22", n.Major)
	}
	if n.Version != "v22.11.0" {
		t.Errorf("Version = %q, want v22.11.0", n.Version)
	}
	if n.String() == "" {
		t.Error("String() 为空")
	}
}

// TestEnsureRejectsTooOldAndReportsTried 所有候选都过旧时，报错必须列出**试过什么**
// （模型据此判断「装了但太老」还是「压根没装」）。
func TestEnsureRejectsTooOldAndReportsTried(t *testing.T) {
	dir := t.TempDir()
	a := execFile(t, dir, "a")
	b := execFile(t, dir, "b")
	f := &fakeNode{versions: map[string]string{a: "v18.0.0", b: "v16.0.0"}}
	r := NewNodeRuntime(NodeConfig{RunCmd: f.run})
	t.Setenv("GO_CODE_NODE_CANDIDATES", a+":"+b)

	_, err := r.Ensure(context.Background())
	if err == nil {
		t.Fatal("全是过旧版本却成功了")
	}
	msg := err.Error()
	for _, want := range []string{"Node.js", ">= 20", "brew install node", a, b} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误缺 %q:\n%s", want, msg)
		}
	}
}

// TestEnsureMissingIsModelReadable 什么都没装：文案要让模型能自我修正
// （装什么 + 装完怎么确认 + 本工具不可用的范围）。
func TestEnsureMissingIsModelReadable(t *testing.T) {
	f := &fakeNode{}
	r := NewNodeRuntime(NodeConfig{RunCmd: f.run})
	t.Setenv("GO_CODE_NODE_CANDIDATES", filepath.Join(t.TempDir(), "absent", "node"))

	_, err := r.Ensure(context.Background())
	if err == nil {
		t.Fatal("候选全不存在却成功了")
	}
	msg := err.Error()
	for _, want := range []string{"Node.js", ">= 20", "brew install node", "nodejs.org", "node --version"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误缺 %q（模型无法据此自我修正）:\n%s", want, msg)
		}
	}
}

// TestEnsureCachesAfterFirstSuccess 惰性探测只做一次（编排脚本会 Promise.all
// 并发调用，每次 spawn 一个 `node --version` 是白付的 ~50ms）。
func TestEnsureCachesAfterFirstSuccess(t *testing.T) {
	dir := t.TempDir()
	good := execFile(t, dir, "node")
	f := &fakeNode{versions: map[string]string{good: "v20.0.0"}}
	r := NewNodeRuntime(NodeConfig{RunCmd: f.run})
	t.Setenv("GO_CODE_NODE_CANDIDATES", good)

	for i := 0; i < 3; i++ {
		if _, err := r.Ensure(context.Background()); err != nil {
			t.Fatalf("第 %d 次 Ensure: %v", i, err)
		}
	}
	if len(f.calls) != 1 {
		t.Errorf("探测次数 = %d, want 1（结果应被缓存）: %v", len(f.calls), f.calls)
	}
}

// TestEnsureConcurrentIsSingleProbe 并发冷启只探测一次，且无竞态
// （Phase 0 教训 2：惰性初始化必须在锁内）。
func TestEnsureConcurrentIsSingleProbe(t *testing.T) {
	dir := t.TempDir()
	good := execFile(t, dir, "node")
	f := &fakeNode{versions: map[string]string{good: "v21.0.0"}}
	r := NewNodeRuntime(NodeConfig{RunCmd: f.run})
	t.Setenv("GO_CODE_NODE_CANDIDATES", good)

	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := r.Ensure(context.Background())
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("并发 Ensure: %v", err)
		}
	}
	if len(f.calls) != 1 {
		t.Errorf("并发下探测次数 = %d, want 1: %v", len(f.calls), f.calls)
	}
}

// TestEnsureDoesNotCacheFailure 失败**不**缓存：用户装完 node 后同一个进程
// （长驻 desktop bridge）必须能立刻用上，不能要求重启应用。
func TestEnsureDoesNotCacheFailure(t *testing.T) {
	dir := t.TempDir()
	good := execFile(t, dir, "node")
	f := &fakeNode{versions: map[string]string{}} // 先全部不可用
	r := NewNodeRuntime(NodeConfig{RunCmd: f.run})
	t.Setenv("GO_CODE_NODE_CANDIDATES", good)

	if _, err := r.Ensure(context.Background()); err == nil {
		t.Fatal("首次应失败")
	}
	// 用户装好了。
	f.versions[good] = "v20.1.0"
	n, err := r.Ensure(context.Background())
	if err != nil {
		t.Fatalf("安装后仍未成功（失败被缓存了？）: %v", err)
	}
	if n.Major != 20 {
		t.Errorf("Major = %d, want 20", n.Major)
	}
}

// TestParseNodeVersion 版本解析容错（nvm shim 会打噪声行、部分构建不打 v 前缀）。
func TestParseNodeVersion(t *testing.T) {
	cases := []struct {
		in    string
		major int
		ok    bool
	}{
		{"v25.8.1\n", 25, true},
		{"v20\n", 20, true},
		{"20.11.1", 20, true},
		{"Now using node v22.1.0\nv22.1.0\n", 22, true}, // nvm shim 噪声
		{"", 0, false},
		{"no version here\n", 0, false},
	}
	for _, c := range cases {
		_, major, err := parseNodeVersion(c.in)
		if c.ok {
			if err != nil {
				t.Errorf("parseNodeVersion(%q) err = %v, want nil", c.in, err)
			} else if major != c.major {
				t.Errorf("parseNodeVersion(%q) major = %d, want %d", c.in, major, c.major)
			}
		} else if err == nil {
			t.Errorf("parseNodeVersion(%q) 竟成功（major=%d）", c.in, major)
		}
	}
}

// TestNewestInVersionsRoot nvm/fnm 式多版本根取版本号**最大**者 —— 不能按字典序
// （"v9" > "v10"）。
func TestNewestInVersionsRoot(t *testing.T) {
	root := t.TempDir()
	mk := func(ver string) {
		bin := filepath.Join(root, ver, "bin")
		if err := os.MkdirAll(bin, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "node"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk("v9.11.1")
	mk("v10.24.1")
	mk("v8.17.0")

	got, ok := newestInVersionsRoot(root)
	if !ok {
		t.Fatal("没找到任何版本")
	}
	want := filepath.Join(root, "v10.24.1", "bin", "node")
	if got != want {
		t.Errorf("取到 %q, want %q（版本号最大者）", got, want)
	}
}

// TestVersionGreaterSegs 逐段比较（字典序会误判）。
func TestVersionGreaterSegs(t *testing.T) {
	if !versionGreaterSegs([]int{20, 1}, []int{9, 11, 1}) {
		t.Error("20.1 应 > 9.11.1")
	}
	if versionGreaterSegs([]int{20}, []int{20, 0, 1}) {
		t.Error("20 应 < 20.0.1")
	}
	if versionGreaterSegs(nil, []int{1}) {
		t.Error("不可解析应小于一切")
	}
	if !versionGreaterSegs([]int{1}, nil) {
		t.Error("任何可解析版本应 > nil")
	}
}
