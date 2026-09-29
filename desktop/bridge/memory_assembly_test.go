package main

// 生产装配断言：Step 3/Step 4 在**真实装配路径**上生效。
//
// 为什么单独一个文件：memory 包有 60+ 单测，agents/tools 侧也各有测试，但它们各自
// 都**不知道对方存在**。一条没接上的线、一处写错的字段名，都会呈现为"全绿、功能
// 完全不工作"。本仓库吃过这个亏，assembly_wiring_test.go 开头记着：
// 「生产从未接线，长达数月 = 看起来有、实际失效」。
//
// 所以这里不自己拼 FileTools / 不自己调 Bootstrap —— 一律走生产函数
// newSessionEngine / buildLoop，用真实工具写出真实文件，再观察真实结果。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

// prodEngine 用生产函数 newSessionEngine 造一个工具引擎（memCfg 可选）。
//
// 20 个参数照抄既有测试的调用形态（见 mcp_test.go / plugins_test.go）——这里刻意
// 不用测试辅助函数封装，因为一旦封装，将来生产签名变化就不会编译失败，而会静默
// 走到别的分支上去。
func prodEngine(t *testing.T, ws string, memCfg ...memory.Config) *tools.Engine {
	t.Helper()
	m, _ := testManager(t)
	reg := subagent.NewRegistry()
	eng, _ := newSessionEngine(ws, "", nil, m.provCfg, "deepseek", m.provCfg.defaultModel(),
		string(provider.ReasoningEffortLevelLow), func() context.Context { return context.Background() },
		reg, events.NewQuestionWaiter(0), buildSkillsRegistry(ws), buildSubagentRegistry(t.TempDir()),
		m.runtime(ws).mcp, m.runtime(ws).plugins, profiles["code"], nil, nil, nil, nil, nil,
		memCfg...)
	return eng
}

// TestProductionEngineStampsMemoryOnWrite：生产装配的 write_file 写到记忆 topic 后，
// `modified` 必须被补上。
//
// 这是 Step 4 唯一的入口（官方形态：模型用标准文件工具写记忆，没有记忆专用工具）。
// 之前所有相关测试都是**手工**往 FileTools 上装钩子再断言 —— 那种测法下，
// 「生产忘了装钩子」这类最典型的失效完全测不出来，因为测试绕过了生产装配。
func TestProductionEngineStampsMemoryOnWrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	initGitRepo(t, ws)

	cfg := memory.Config{Directory: filepath.Join(home, ".go-code", "memory")}
	eng := prodEngine(t, ws, cfg)

	// 预置一个 topic（有 frontmatter，官方语义是补/更新 modified）
	res := memory.Bootstrap(context.Background(), ws, cfg)
	if !res.Resolved || res.Dir == nil {
		t.Skipf("本机无法派生记忆目录（%v），跳过", res.Err)
	}
	// memory 包**不建目录**（官方没有"启动即建目录"），全新机器上它并不存在
	if err := os.MkdirAll(res.Dir.Path, 0o700); err != nil {
		t.Fatalf("建记忆目录: %v", err)
	}
	topic := filepath.Join(res.Dir.Path, "relay_tunnel_id.md")
	body := "---\ntype: project\n---\n\nThe identifier is `q7-tunnel-corp`.\n"
	if err := os.WriteFile(topic, []byte(body), 0o600); err != nil {
		t.Fatalf("预置 topic: %v", err)
	}

	// 用生产引擎里注册的真实 write_file 写它
	tool, err := eng.GetTool(context.Background(), "write_file")
	if err != nil {
		t.Fatalf("write_file 未注册: %v", err)
	}
	args := `{"path":` + jsonQuote(topic) + `,"content":` + jsonQuote(body) + `}`
	if err := tool.ValidParams(context.Background(), "write_file", args); err != nil {
		t.Fatalf("参数校验失败: %v", err)
	}
	if _, err := tool.Call(context.Background(), "write_file", args); err != nil {
		t.Fatalf("write_file 调用失败: %v", err)
	}

	got, err := os.ReadFile(topic)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !strings.Contains(string(got), "modified:") {
		t.Fatalf("生产装配未补 modified —— Step 4 钩子没接上\n%s", got)
	}
	if !strings.Contains(string(got), "q7-tunnel-corp") {
		t.Fatalf("正文被改写\n%s", got)
	}
}

// TestProductionEngineRespectsSwitchOff：开关关时，同一条生产路径必须完全惰性。
//
// 配对上一条：钩子装上了不等于闸门正确。这里验的是「用户关了记忆，模型写记忆文件
// 时不会偷偷补戳」——漏了写侧开关 = 用户关了还在背后生成文件，比不实现更糟。
func TestProductionEngineRespectsSwitchOff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	initGitRepo(t, ws)

	off := false
	cfg := memory.Config{Directory: filepath.Join(home, ".go-code", "memory"), Enabled: &off}
	eng := prodEngine(t, ws, cfg)

	res := memory.Bootstrap(context.Background(), ws, cfg)
	// 开关关闭时 Bootstrap 直接短路，Dir 为 nil —— 用未关闭的配置解析出目录来造前置
	if res.Dir == nil {
		on := true
		probe := memory.Bootstrap(context.Background(), ws, memory.Config{
			Directory: cfg.Directory, Enabled: &on,
		})
		if probe.Dir == nil {
			t.Skipf("本机无法派生记忆目录（%v），跳过", probe.Err)
		}
		res = probe
	}
	if err := os.MkdirAll(res.Dir.Path, 0o700); err != nil {
		t.Fatalf("建记忆目录: %v", err)
	}
	topic := filepath.Join(res.Dir.Path, "x.md")
	body := "---\ntype: project\n---\n\nx\n"
	if err := os.WriteFile(topic, []byte(body), 0o600); err != nil {
		t.Fatalf("预置: %v", err)
	}

	tool, _ := eng.GetTool(context.Background(), "write_file")
	args := `{"path":` + jsonQuote(topic) + `,"content":` + jsonQuote(body) + `}`
	if _, err := tool.Call(context.Background(), "write_file", args); err != nil {
		t.Fatalf("write_file 调用失败: %v", err)
	}
	got, _ := os.ReadFile(topic)
	if strings.Contains(string(got), "modified:") {
		t.Fatalf("开关关闭时仍补了 modified\n%s", got)
	}
	entries, _ := os.ReadDir(res.Dir.Path)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			t.Fatalf("开关关闭时建了锁侧车文件：%s", e.Name())
		}
	}
}

// TestMemoryIsNotSweptByCronRetention：cron 保留期清理不得删到记忆文件。
//
// 官方 storage-location 明写：会话保留期清理「excludes the memory files in the
// memory directory」。本仓库的清理（cronClean）只作用于 cronSnapshotsRoot()，
// 两者路径不重叠 —— 但那是**巧合**，不是设计。任何一次把清理范围扩大到
// ~/.go-code 下的改动，都会静默吃掉用户全部记忆。
//
// 这条测试把「不重叠」钉成显式断言：它今天必然通过，价值在将来。
func TestMemoryIsNotSweptByCronRetention(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("appDataDirOverride", t.TempDir())

	memRoot, err := memory.MemoryRoot()
	if err != nil {
		t.Skipf("取不到记忆根: %v", err)
	}
	sweepRoot := cronSnapshotsRoot()
	if strings.HasPrefix(memRoot, sweepRoot+string(filepath.Separator)) || memRoot == sweepRoot {
		t.Fatalf("记忆根 %q 落在 cron 清理根 %q 之内 —— 保留期清理会删掉用户全部记忆", memRoot, sweepRoot)
	}
	// 再钉一层：清理逻辑里不得出现指向记忆根的路径拼接
	if strings.Contains(sweepRoot, "memory") {
		t.Logf("提示：cron 清理根路径含 'memory' 字样，请确认不是误指记忆目录：%s", sweepRoot)
	}
}

// TestMemoryIsolationAcrossRepos：跨仓库隔离（协议 §9 的负向测试，此前一条未跑）。
//
// A 仓库写的记忆，B 仓库（不同 remote）必须看不到。这是「记忆存在 ~/.go-code/memory
// 这个**共享**根」这一设计唯一的风险点：一旦 ID 派生退化成按路径哈希，复制一份
// 仓库就会得到两份记忆，或者两个仓库串味。
func TestMemoryIsolationAcrossRepos(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := memory.Config{Directory: filepath.Join(home, ".go-code", "memory")}
	on := true
	cfg.Enabled = &on

	wsA, wsB := t.TempDir(), t.TempDir()
	initGitRepoWithRemote(t, wsA, "git@example.com:acme/alpha.git")
	initGitRepoWithRemote(t, wsB, "git@example.com:acme/beta.git")

	dirA := resolveMemDir(t, wsA, cfg)
	dirB := resolveMemDir(t, wsB, cfg)
	if dirA == dirB {
		t.Fatalf("两个不同 remote 的仓库解析到同一记忆目录 %q —— 隔离失效", dirA)
	}

	// A 写一条，B 的目录里必须没有
	if err := os.WriteFile(filepath.Join(dirA, "MEMORY.md"), []byte("- a_topic.md\n"), 0o600); err != nil {
		t.Fatalf("写 A 索引: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dirB, "MEMORY.md")); err == nil {
		t.Fatal("A 的记忆出现在 B 的目录里 —— 跨仓库泄漏")
	}
	if _, err := os.Stat(filepath.Join(dirA, "a_topic.md")); err == nil {
		t.Fatal("topic 文件凭空出现")
	}
}

// initGitRepoWithRemote 建带 remote 的仓库（记忆身份由 remote 规范化派生）。
func initGitRepoWithRemote(t *testing.T, dir, remote string) {
	t.Helper()
	initGitRepo(t, dir)
	cmd := exec.Command("git", "remote", "add", "origin", remote)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
}

func resolveMemDir(t *testing.T, ws string, cfg memory.Config) string {
	t.Helper()
	res := memory.Bootstrap(context.Background(), ws, cfg)
	if !res.Resolved || res.Dir == nil {
		t.Skipf("本机无法为 %s 派生记忆目录（%v），跳过", ws, res.Err)
	}
	if err := os.MkdirAll(res.Dir.Path, 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	return res.Dir.Path
}
