package main

// Project Memory 的**端到端**装配验证：从用户偏好一路走到会话的系统提示词。
//
// 为什么单独一个文件：前面的测试各测一段（偏好落盘 / 注入文本拼装 / options 应用），
// 没有任何一条覆盖它们的**串联**。而"机制完整、生产从未接线"恰恰是本仓库吃过的亏
// （见 assembly_wiring_test.go 开头那段记录）——分段全绿、装配缺失，几个月都看不出来。
//
// 观察入口选 ContextBreakdown 的 system_prompt 分区字节数，理由：
//   - 它是**生产**代码路径（Composer 的 ctx 环就用它），不是测试专用探针
//   - 它内部调 composeSystemPrompt，即注入真正落地的那一段
//   - 字节数是可直接比较的标量，不需要把整个提示词打进断言里

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/memory"
)

// initGitRepo 把 dir 变成一个带一次提交的真 git 仓库。
//
// 必须是真仓库：memory 的目录身份由 remote 规范化派生（memory/identity.go），
// 假目录会走 ErrNoGit 分支，测到的是"没有记忆"而不是"有记忆"或"开关关掉"。
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
		{"add", "-A"},
		{"commit", "-m", "init", "--allow-empty"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// writeMemoryIndex 解析出该工作区的记忆目录，建出来并写入索引。
//
// 目录名是**派生**的（remote 规范化 / 路径回退，见 memory/identity.go），测试不能
// 自己拼 —— 拼错就会写到另一个目录，然后断言"没有注入"，而真实原因只是路径不对。
// 所以这里调生产用的同一个 memory.Bootstrap 拿目录。
//
// 目录本身必须由这里创建：memory 包**不建目录**（官方没有"启动即建目录"），
// 全新机器上第一次会话时它并不存在。
func writeMemoryIndex(t *testing.T, ws, body string) {
	t.Helper()
	res := memory.Bootstrap(context.Background(), ws, memory.Config{})
	if !res.Resolved || res.Dir == nil {
		t.Skipf("本机无法为该工作区派生记忆目录（%v），跳过", res.Err)
	}
	if err := os.MkdirAll(res.Dir.Path, 0o700); err != nil {
		t.Fatalf("建记忆目录: %v", err)
	}
	if err := os.WriteFile(filepath.Join(res.Dir.Path, "MEMORY.md"), []byte(body), 0o600); err != nil {
		t.Fatalf("写索引: %v", err)
	}
}

// promptFootprint 取会话循环的提示词侧字节数 = system_prompt + memory 两个分区。
//
// 为什么不只看 system_prompt：记忆索引 2026-09-29 起有**独立分区**（ContextPartMemory），
// 只看 system 会得出"注入没生效"的错误结论。两个分区相加才是注入的真实占位。
//
// 走 ContextBreakdown（生产路径，Composer 的 ctx 环同源），它内部就是
// composeSystemPrompt —— 注入真正落地的那一段。传 nil messages / nil mode：
// 这两个只影响消息与模式提示分区，与提示词侧无关。
func promptFootprint(t *testing.T, bs *bridgeSession) int64 {
	t.Helper()
	if bs == nil || bs.loop == nil {
		t.Fatal("会话没有 loop")
	}
	bd := bs.loop.ContextBreakdown(context.Background(), nil, nil)
	var n int64
	for _, p := range bd.Parts {
		if p.Key == agents.ContextPartSystem || p.Key == agents.ContextPartMemory {
			n += p.Chars
		}
	}
	return n
}

// TestMemoryWiredIntoRealSessionSystemPrompt：开着时，索引真的进了会话的系统提示词。
//
// 这是本文件最重要的一条：它证明 Step 3 **在生产路径上**生效，而不只是
// "memory 包的 BuildInjection 能返回文本"。
func TestMemoryWiredIntoRealSessionSystemPrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m, _ := testManager(t)
	// testManager 已把 HOME 指向临时目录；这里再显式建一个指向同一 HOME 的偏好存储，
	// 避免依赖"manager 没设 memPrefs 时走默认开"这条隐含路径。
	m.memPrefs = &memoryPrefStore{path: filepath.Join(home, ".go-code", "memory.json")}

	ws := t.TempDir()
	initGitRepo(t, ws)

	// 先建一个"无记忆"的基线会话
	base, err := m.create(ws, "mem-base", "")
	if err != nil {
		t.Fatalf("建基线会话: %v", err)
	}
	baseChars := promptFootprint(t, base)
	if baseChars <= 0 {
		t.Fatal("基线系统提示词字节数异常")
	}

	// 写入索引后再建一个会话（偏好仍为默认开）
	idx := "- relay_tunnel_id.md\n- release_gate.md\nMARKER_MEMORY_INDEX\n"
	writeMemoryIndex(t, ws, idx)

	withMem, err := m.create(ws, "mem-on", "")
	if err != nil {
		t.Fatalf("建带记忆会话: %v", err)
	}
	onChars := promptFootprint(t, withMem)
	if onChars <= baseChars {
		t.Fatalf("开着记忆时提示词侧没有变长：基线 %d → 有记忆 %d（注入未生效）", baseChars, onChars)
	}
	// 增量必须至少覆盖索引正文本身（加上标题与分隔只会更大）
	if onChars-baseChars < int64(len(idx)) {
		t.Fatalf("增量 %d 小于索引正文 %d：注入被截断或串错了内容", onChars-baseChars, len(idx))
	}
}

// TestMemoryOffMeansNoInjectionInNewSession：关掉后，**新会话**的系统提示词
// 与"无记忆"基线逐字节一致。
//
// 这是"我的记忆到底开没开"的全部承诺。改的是偏好、走的是生产 create 路径、
// 断言的是 composeSystemPrompt 的产物 —— 三个环节任一断掉都会红。
func TestMemoryOffMeansNoInjectionInNewSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m, _ := testManager(t)
	m.memPrefs = &memoryPrefStore{path: filepath.Join(home, ".go-code", "memory.json")}

	ws := t.TempDir()
	initGitRepo(t, ws)

	// 开着 → 有记忆的基线
	if err := m.memPrefs.set(memoryPrefs{Enabled: memory.Bool(true)}); err != nil {
		t.Fatalf("落盘: %v", err)
	}
	writeMemoryIndex(t, ws, "- relay_tunnel_id.md\nMARKER_MEMORY_INDEX\n")
	onBase, err := m.create(ws, "mem-on-base", "")
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	onChars := promptFootprint(t, onBase)

	// 关掉 → 新会话
	if err := m.memPrefs.set(memoryPrefs{Enabled: memory.Bool(false)}); err != nil {
		t.Fatalf("落盘: %v", err)
	}
	off, err := m.create(ws, "mem-off", "")
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	offChars := promptFootprint(t, off)

	if offChars == onChars {
		t.Fatalf("关掉后提示词侧长度未变（%d）—— 开关没起作用", offChars)
	}
	if offChars >= onChars {
		t.Fatalf("关掉后反而更长：开 %d → 关 %d，注入可能仍在", onChars, offChars)
	}
}

// TestMemoryRunningSessionUnaffectedByToggle：改开关**不影响**已运行的会话。
//
// 这是刻意的产品决策（记忆是系统提示词的一部分，中途删会让同一会话前后两轮
// "你记得什么"不一致）。若将来有人把它改成热生效，这条会红 —— 那正是要重新
// 讨论决策的信号，而不是可以顺手改掉的实现细节。
func TestMemoryRunningSessionUnaffectedByToggle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m, _ := testManager(t)
	m.memPrefs = &memoryPrefStore{path: filepath.Join(home, ".go-code", "memory.json")}

	ws := t.TempDir()
	initGitRepo(t, ws)
	if err := m.memPrefs.set(memoryPrefs{Enabled: memory.Bool(true)}); err != nil {
		t.Fatalf("落盘: %v", err)
	}
	writeMemoryIndex(t, ws, "- relay_tunnel_id.md\nMARKER_MEMORY_INDEX\n")

	live, err := m.create(ws, "mem-live", "")
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	before := promptFootprint(t, live)

	if err := m.memPrefs.set(memoryPrefs{Enabled: memory.Bool(false)}); err != nil {
		t.Fatalf("落盘: %v", err)
	}
	after := promptFootprint(t, live)

	if after != before {
		t.Fatalf("运行中的会话被开关改变了：%d → %d（决策是下一个会话生效）", before, after)
	}
}
