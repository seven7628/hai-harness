## 12. 自测评估代码

> 分两类：**§12.1–12.7 自动化测试**（能证伪，随 PR 一起提交）、
> **§12.8 评估脚本**（用**真实会话事件日志**量化"模型还在写 `cd &&`"这件事，改造前取基线、改造后复测）。

### 12.1 `workdir/state_test.go`

```go
package workdir

import (
	"os"
	"path/filepath"
	"testing"
)

func mkSub(t *testing.T, root, name string) string {
	t.Helper()
	p := filepath.Join(root, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCurrentFallsBackToRoot 未 cd 时当前目录 = 项目根。
func TestCurrentFallsBackToRoot(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if got := s.Current(MainKey); got != root {
		t.Fatalf("Current = %q, want %q", got, root)
	}
}

// TestSetUpdatesOnlyItsPartition 子分区互不影响（对齐 Claude Code：子 agent 不继承父级 cd）。
func TestSetUpdatesOnlyItsPartition(t *testing.T) {
	root := t.TempDir()
	sub := mkSub(t, root, "sub")
	s := New(root)

	if _, changed, err := s.Set(MainKey, sub); err != nil || !changed {
		t.Fatalf("Set: changed=%v err=%v", changed, err)
	}
	if got := s.Current(MainKey); got != sub {
		t.Fatalf("Current(main) = %q, want %q", got, sub)
	}
	if got := s.Current("task-1"); got != root {
		t.Fatalf("子分区被影响：Current(task-1) = %q, want %q", got, root)
	}
}

// TestSetOutsideRootRejected 越界被拒且**不改变**记忆（Q2 = 重置策略的前置）。
func TestSetOutsideRootRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	s := New(root)

	_, changed, err := s.Set(MainKey, outside)
	if err != ErrOutside {
		t.Fatalf("err = %v, want ErrOutside", err)
	}
	if changed {
		t.Fatal("越界不应算作 changed")
	}
	if got := s.Current(MainKey); got != root {
		t.Fatalf("越界后 cwd = %q, want %q（记忆不能被越界污染）", got, root)
	}
}

// TestResetAndForget Reset 回项目根；Forget 清空分区。
func TestResetAndForget(t *testing.T) {
	root := t.TempDir()
	sub := mkSub(t, root, "sub")
	s := New(root)
	_, _, _ = s.Set(MainKey, sub)

	if old := s.Reset(MainKey); old != sub {
		t.Fatalf("Reset 返回 %q, want %q", old, sub)
	}
	if got := s.Current(MainKey); got != root {
		t.Fatalf("Reset 后 Current = %q, want %q", got, root)
	}
	_, _, _ = s.Set("task-1", sub)
	s.Forget("task-1")
	if got := s.Current("task-1"); got != root {
		t.Fatalf("Forget 后 Current = %q, want %q", got, root)
	}
}

// TestContainsContains 判断是否在项目根之内。
func TestContainsContains(t *testing.T) {
	root := t.TempDir()
	sub := mkSub(t, root, "sub")
	outside := t.TempDir()
	s := New(root)

	if !s.Contains(root) {
		t.Error("项目根本身应算在内")
	}
	if !s.Contains(sub) {
		t.Error("子目录应算在内")
	}
	if s.Contains(outside) {
		t.Error("项目根之外不应算在内")
	}
	// 同名前缀的兄弟目录不算在内（root=/a/b，dir=/a/bc）
	sib := filepath.Dir(root) + filepath.Base(root) + "x"
	if s.Contains(sib) {
		t.Errorf("前缀相同的兄弟目录 %q 不应算在内", sib)
	}
}

// TestEnvSnapshotTracking 环境快照的记录/读取。
func TestEnvSnapshotTracking(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if _, ok := s.SentEnv(MainKey); ok {
		t.Fatal("初始不应有快照")
	}
	e := Env{WorkingDir: root, Platform: "darwin"}
	s.MarkEnvSent(MainKey, e)
	got, ok := s.SentEnv(MainKey)
	if !ok || !got.Equal(e) {
		t.Fatalf("SentEnv = %+v, ok=%v; want %+v", got, ok, e)
	}
}
```

### 12.2 `workdir/env_test.go`

```go
package workdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderFullMentionsWorkingDir(t *testing.T) {
	e := Env{WorkingDir: "/repo/src", IsGitRepo: true, Platform: "darwin", Shell: "zsh", OSVersion: "Darwin 24.5.0"}
	s := RenderFull(e)
	for _, want := range []string{
		"# Environment",
		"Primary working directory: /repo/src",
		"Is a git repository: true",
		"Platform: darwin",
		"Shell: zsh",
		"OS Version: Darwin 24.5.0",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("RenderFull 缺少 %q:\n%s", want, s)
		}
	}
}

// TestRenderFullWorktreeSentence worktree 时必须有"不要 cd 回主检出"的告诫
//（对齐 Claude Code 的原文；这条直接关系到 worktree 隔离）。
func TestRenderFullWorktreeSentence(t *testing.T) {
	s := RenderFull(Env{WorkingDir: "/repo/.claude/worktrees/w1", IsWorktree: true})
	if !strings.Contains(s, "git worktree") || !strings.Contains(s, "Do NOT") {
		t.Errorf("worktree 缺少告诫句:\n%s", s)
	}
}

func TestRenderDelta(t *testing.T) {
	old := Env{WorkingDir: "/repo"}
	cur := Env{WorkingDir: "/repo/src"}
	s := RenderDelta(old, cur)
	if !strings.Contains(s, "# Environment update") {
		t.Errorf("缺少增量标记:\n%s", s)
	}
	if !strings.Contains(s, "Primary working directory: /repo/src") {
		t.Errorf("缺少新目录:\n%s", s)
	}
}

func TestEnvEqual(t *testing.T) {
	a := Env{WorkingDir: "/x", Platform: "darwin"}
	b := Env{WorkingDir: "/x", Platform: "darwin"}
	if !a.Equal(b) {
		t.Error("相同快照应相等")
	}
	b.WorkingDir = "/y"
	if a.Equal(b) {
		t.Error("不同目录不应相等")
	}
}

// TestSnapshotDetectsWorktree .git 是文件（worktree）vs 目录（主检出）。
func TestSnapshotDetectsWorktree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(root)
	if e := s.Snapshot(MainKey); !e.IsGitRepo || e.IsWorktree {
		t.Fatalf("主检出判定错：IsGitRepo=%v IsWorktree=%v", e.IsGitRepo, e.IsWorktree)
	}

	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2 := New(wt)
	if e := s2.Snapshot(MainKey); !e.IsGitRepo || !e.IsWorktree {
		t.Fatalf("worktree 判定错：IsGitRepo=%v IsWorktree=%v", e.IsGitRepo, e.IsWorktree)
	}
}
```

### 12.3 `tools/builtin/cwd_follow_test.go`（V1 / V3 / V6）

```go
package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/workdir"
)

// cwdCtx 造一个带 ToolContext 的 ctx（taskId 区分调用方）。
func cwdCtx(runID, taskID string) context.Context {
	return events.WithToolContextForRunTask(context.Background(), runID, runID, taskID, 0, nil, nil, nil)
}

// findTool 按名字找工具。
func findTool(t *testing.T, ts []tools.Tool, name string) tools.Tool {
	t.Helper()
	for _, x := range ts {
		if x.Name() == name {
			return x
		}
	}
	t.Fatalf("找不到工具 %q", name)
	return nil
}

// TestFileToolsFollowCwd cd 之后，文件工具的相对路径落在**新目录**（V1）。
//
// 这是本次改造的核心断言：今天文件工具锚在 workspace root，cd 之后 write 会写错地方。
func TestFileToolsFollowCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	ft := NewFileTools(wd)
	ts := ft.Tools()
	bash := findTool(t, ts, "bash")
	write := findTool(t, ts, "write_file")
	ctx := cwdCtx("run-1", "")

	// 未 cd 时：相对路径落在项目根（**旧行为逐字保留**）
	if _, err := write.Call(ctx, "write_file", `{"path":"a.txt","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("未 cd 时应落在项目根：%v", err)
	}

	// cd 之后：相对路径落在新目录
	if _, err := bash.Call(ctx, "bash", `{"command":"cd `+sub+`"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Call(ctx, "write_file", `{"path":"b.txt","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sub, "b.txt")); err != nil {
		t.Fatalf("cd 后应落在 %s：%v", sub, err)
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); err == nil {
		t.Fatal("cd 后不应再落在项目根（否则就是静默错位）")
	}
}

// TestCwdIsolatedPerCaller 子 agent 的 cd 不影响主 agent（V3，文件工具维度）。
func TestCwdIsolatedPerCaller(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	ft := NewFileTools(wd)
	ts := ft.Tools()
	bash := findTool(t, ts, "bash")
	write := findTool(t, ts, "write_file")

	mainCtx := cwdCtx("run-main", "")
	subCtx := cwdCtx("run-sub", "task-123")

	if _, err := bash.Call(subCtx, "bash", `{"command":"cd `+sub+`"}`); err != nil {
		t.Fatal(err)
	}
	// 主 agent 仍然在项目根
	if _, err := write.Call(mainCtx, "write_file", `{"path":"main.txt","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "main.txt")); err != nil {
		t.Fatalf("主 agent 的写应落在项目根：%v", err)
	}
	// 子 agent 在新目录
	if _, err := write.Call(subCtx, "write_file", `{"path":"sub.txt","content":"x"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sub, "sub.txt")); err != nil {
		t.Fatalf("子 agent 的写应落在 %s：%v", sub, err)
	}
}

// TestCwdResetOutsideRoot cd 越界 ⇒ 重置回项目根 + 结果里明确提示（V6 / Q2）。
func TestCwdResetOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	wd := workdir.New(root)
	ft := NewFileTools(wd)
	ts := ft.Tools()
	bash := findTool(t, ts, "bash")
	ctx := cwdCtx("run-1", "")

	out, err := bash.Call(ctx, "bash", `{"command":"cd `+outside+`"}`)
	if err != nil {
		t.Fatalf("越界 cd 不应报错（对齐 Claude Code：重置而非拒绝）：%v", err)
	}
	if !strings.Contains(out, "Shell cwd was reset to") {
		t.Fatalf("结果里应有重置提示：\n%s", out)
	}
	if got := wd.Current(""); got != root {
		t.Fatalf("越界后 cwd = %q, want %q", got, root)
	}
}

// TestResolveStillRejectsEscapeWorkspace 相对路径**不得逃出项目根**（R3 安全边界不变）。
func TestResolveStillRejectsEscapeWorkspace(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	ft := NewFileTools(wd)
	write := findTool(t, ft.Tools(), "write_file")
	ctx := cwdCtx("run-1", "")

	// cwd = root/sub 时，../../etc/passwd 必须被拒
	if _, _, err := wd.Set("", sub); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Call(ctx, "write_file", `{"path":"../../etc/passwd","content":"x"}`); err == nil {
		t.Fatal("逃出项目根的相对路径必须被拒")
	}
}
```

### 12.4 `agents/cwd_env_test.go`（V2 / V8 / V9）

```go
package agents

import (
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/workdir"
)

// reqHasEnvText 判断请求里有没有含指定文本的 user 消息。
func reqHasEnvText(req *provider.StreamRequest, needle string) bool {
	for _, m := range req.Messages {
		if m.Role != core.User {
			continue
		}
		for _, c := range m.Content {
			if strings.Contains(c.Content, needle) {
				return true
			}
		}
	}
	return false
}

// reqSystemText 取请求里 system 消息的文本。
func reqSystemText(req *provider.StreamRequest) string {
	for _, m := range req.Messages {
		if m.Role == core.System {
			for _, c := range m.Content {
				return c.Content
			}
		}
	}
	return ""
}

// TestEnvBlockInjectedAndNotInSystem 环境块进**对话**、不进 system（R1 + R2）。
func TestEnvBlockInjectedAndNotInSystem(t *testing.T) {
	root := t.TempDir()
	wd := workdir.New(root)
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		reminderStopRound("ok"),
	}}

	loop := newTestLoop(t, p, WithWorkDirState(wd))
	if _, err := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage("hi")}, nil, &RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) == 0 {
		t.Fatal("没有捕获到请求")
	}
	req := p.requests[0]
	if !reqHasEnvText(req, "Primary working directory: "+root) {
		t.Fatalf("环境块未注入对话：\n%+v", req.Messages)
	}
	if strings.Contains(reqSystemText(req), "Primary working directory") {
		t.Fatal("环境块不许进 system（会破 provider 前缀缓存）")
	}
}

// TestEnvNotResentWhenUnchanged cwd 没变 ⇒ 不重复发（省 token）。
func TestEnvNotResentWhenUnchanged(t *testing.T) {
	root := t.TempDir()
	wd := workdir.New(root)
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		reminderStopRound("ok"),
		reminderStopRound("ok"),
	}}
	loop := newTestLoop(t, p, WithWorkDirState(wd))
	ctx := context.Background()
	if _, err := loop.RunStream(ctx, []core.Message{core.NewUserMessage("a")}, nil, &RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.RunStream(ctx, []core.Message{core.NewUserMessage("b")}, nil, &RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) < 2 {
		t.Fatalf("请求数 = %d, want >= 2", len(p.requests))
	}
	if !reqHasEnvText(p.requests[0], "Primary working directory") {
		t.Fatal("首轮应有完整环境块")
	}
	if reqHasEnvText(p.requests[1], "Primary working directory") {
		t.Fatal("cwd 未变时不该重复发环境块")
	}
}

// TestEnvUpdateAfterCwd cwd 变了 ⇒ 发增量块。
func TestEnvUpdateAfterCwd(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	wd := workdir.New(root)
	// 模拟 bash 的 cd 记忆
	if _, _, err := wd.Set("", sub); err != nil {
		t.Fatal(err)
	}
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		reminderStopRound("ok"),
	}}
	loop := newTestLoop(t, p, WithWorkDirState(wd))
	if _, err := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage("hi")}, nil, &RunOptions{}); err != nil {
		t.Fatal(err)
	}
	req := p.requests[0]
	if !reqHasEnvText(req, "Primary working directory: "+sub) {
		t.Fatalf("未反映新 cwd：\n%+v", req.Messages)
	}
}
```

> ⚠️ `newTestLoop` 是**待补的测试夹具**（照 `agents/agent_loop_test.go` 里既有的 loop 构造改写即可），
> `reminderStopRound` / `captureRoundsProvider` 复用 `agents/agent_loop_test.go` 的既有定义。

### 12.5 V4：切档位（rebuildLoop）后 cwd 保留

```go
// desktop/bridge/cwd_rebuild_test.go
package bridge

// TestCwdSurvivesLoopRebuild 切模型/切人格（rebuildLoop）后 cwd 仍在（Q4 = 保留）。
//
// 回归点：cwd 记忆原先存在 BashTool 实例上，而实例每次 FileTools.Tools() 都新建
// ⇒ rebuildLoop 会把 cd 记忆清空。
func TestCwdSurvivesLoopRebuild(t *testing.T) {
	// 1. 建会话 → 拿 bs
	// 2. bash cd 到 sub
	// 3. bs.rebuildLoop(...)（换模型/换人格）
	// 4. 断言 bs 的 workdir.Current("") == sub
	// 5. 断言下一次 bash 命令的 cmd.Dir == sub
}
```

> 骨架：具体调用待实现时按 `desktop/bridge/main.go:6668` `rebuildLoop` 的实际签名补全。

### 12.6 V5：checkpoint / replay 不受影响

沿用我们做 events↔checkpoint 等价性时的做法（参考 `desktop/bridge/events_toolcalls_equivalence_test.go`）：

```go
// desktop/bridge/cwd_checkpoint_equivalence_test.go
package bridge

// TestCwdChangeDoesNotAlterCheckpoint cwd 变化**不改变** checkpoint 字节。
//
// 原理：cwd 状态是**内存态**（不进 ac.Messages ⇒ 不进 journal ⇒ 不进 checkpoint），
// 环境块是 ephemeral（不进 ac.Messages）⇒ 两条路产出的 checkpoint 必须逐字节相同。
func TestCwdChangeDoesNotAlterCheckpoint(t *testing.T) {
	// 1. 造一个会话，跑一轮：bash cd + 若干工具
	// 2. 记录 LoadRecordForView / MarshalBodyWithSpans 的字节
	// 3. 同样的事件流，但中间多了/少了 cd
	// 4. 断言两边字节相同
}
```

### 12.7 V7：worktree 端到端（P1）

```go
// tools/builtin/worktree_enter_test.go
package builtin

// TestWorktreeEnterSwitchesCwd enter 把会话 cwd 切到 worktree；
// exit 还原；进入非本会话创建的 worktree 被拒（对齐 Claude Code 的 Scope 规则）。
func TestWorktreeEnterSwitchesCwd(t *testing.T) {
	root := t.TempDir()
	// git init + commit（保证能建 worktree）
	// create → 记入 created
	// enter(path) → wd.Current("") == path
	// write_file("x.txt") → 落在 worktree 内
	// exit() → wd.Current("") == root
	// enter(外部目录) → 报错
}
```

### 12.8 评估脚本：`scripts/eval_cwd_redundancy.py`

> **目的**：用**真实会话事件日志**量化用户反馈的那个现象 ——「模型总是在写 `cd xxx && xxx`」。
> 改造前取基线，改造后复测，看指标是否下降。**这是唯一能证明"用户体感改善了"的量化证据。**

```python
#!/usr/bin/env python3
"""评估 agent 的「重复 cd」冗余度。

数据源：~/.go-code/events/<ws>/<sid>.jsonl（明文或 .gz 封存）。
指标：
  M1 cd_prefix_ratio   以 `cd ` 开头的 bash 命令占比        （越低越好）
  M2 cd_compound_ratio `cd X && ...` 形式的 bash 命令占比    （越低越好）
  M3 redundant_cd      「重复 cd 到同一目录」的次数/占比     （越低越好）

用法：
  python3 scripts/eval_cwd_redundancy.py                    # 全量
  python3 scripts/eval_cwd_redundancy.py --limit 200        # 只看最大的 200 个会话
  python3 scripts/eval_cwd_redundancy.py --root /path/ev    # 指定 events 根目录
  python3 scripts/eval_cwd_redundancy.py --baseline out.json --compare new.json
"""
import argparse
import collections
import glob
import gzip
import json
import os
import re
import sys

CD_RE = re.compile(r'^\s*cd\s+(.+)$')
TOOL_START = "tool_run_start"


def open_event_file(path):
    if path.endswith(".gz"):
        return gzip.open(path, "rt", encoding="utf-8", errors="replace")
    return open(path, "r", encoding="utf-8", errors="replace")


def session_files(root):
    for p in glob.glob(os.path.join(root, "*", "*.jsonl")):
        yield p
    for p in glob.glob(os.path.join(root, "*", "*.jsonl.gz")):
        yield p


def analyze_session(path):
    """返回该会话的 (bash_total, cd_prefix, cd_compound, redundant_cd, targets)。"""
    bash_total = 0
    cd_prefix = 0
    cd_compound = 0
    redundant = 0
    last_target = None
    # 按 (session, run) 分组跟踪"上一次 cd 的目标"——run 变了就是新的 agent 上下文
    per_run = {}
    for line in open_event_file(path):
        try:
            j = json.loads(line)
        except Exception:
            continue
        if j.get("event_type") != TOOL_START:
            continue
        if j.get("name") != "bash":
            continue
        args = j.get("arguments") or ""
        try:
            cmd = (json.loads(args) or {}).get("command") or ""
        except Exception:
            cmd = ""
        if not cmd.strip():
            continue
        bash_total += 1
        m = CD_RE.match(cmd)
        if not m:
            continue
        cd_prefix += 1
        rest = m.group(1)
        target = rest.split("&&")[0].strip()
        if "&&" in rest:
            cd_compound += 1
        run = j.get("run_id") or ""
        if per_run.get(run) == target:
            redundant += 1
        per_run[run] = target
    return bash_total, cd_prefix, cd_compound, redundant


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", default=os.path.expanduser("~/.go-code/events"))
    ap.add_argument("--limit", type=int, default=0, help="只看最大的 N 个会话（0=全量）")
    ap.add_argument("--baseline", help="基线 JSON（--compare 时用）")
    ap.add_argument("--compare", help="对照 JSON")
    ap.add_argument("--out", help="把本次结果写成 JSON")
    args = ap.parse_args()

    files = list(session_files(args.root))
    if args.limit:
        files.sort(key=lambda p: (os.path.getsize(p.replace(".gz", "")) if os.path.exists(p.replace(".gz", "")) else 0), reverse=True)
        files = files[: args.limit]

    tot = collections.Counter()
    per_session = []
    for p in files:
        try:
            b, c, cc, r = analyze_session(p)
        except Exception as e:
            print("skip %s: %s" % (p, e), file=sys.stderr)
            continue
        if b == 0:
            continue
        tot["bash"] += b
        tot["cd_prefix"] += c
        tot["cd_compound"] += cc
        tot["redundant"] += r
        per_session.append((c / b, b, c, cc, r, os.path.basename(p)))

    result = {
        "sessions": len(per_session),
        "bash_total": tot["bash"],
        "cd_prefix": tot["cd_prefix"],
        "cd_compound": tot["cd_compound"],
        "redundant_cd": tot["redundant"],
        "cd_prefix_ratio": (tot["cd_prefix"] / tot["bash"]) if tot["bash"] else 0.0,
        "cd_compound_ratio": (tot["cd_compound"] / tot["bash"]) if tot["bash"] else 0.0,
        "redundant_cd_ratio": (tot["redundant"] / tot["bash"]) if tot["bash"] else 0.0,
    }

    print("会话数        : %d" % result["sessions"])
    print("bash 命令总数 : %d" % result["bash_total"])
    print("M1 cd 前缀占比 : %.2f%%  (%d)" % (100 * result["cd_prefix_ratio"], result["cd_prefix"]))
    print("M2 cd&& 占比   : %.2f%%  (%d)" % (100 * result["cd_compound_ratio"], result["cd_compound"]))
    print("M3 重复 cd     : %d  (%.2f%%)" % (result["redundant_cd"], 100 * result["redundant_cd_ratio"]))

    per_session.sort(reverse=True)
    if per_session:
        print("\ncd 前缀占比最高的 10 个会话：")
        for ratio, b, c, cc, r, name in per_session[:10]:
            print("  %5.1f%%  bash=%-5d cd=%-4d 冗余=%-3d %s" % (100 * ratio, b, c, r, name[:48]))

    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            json.dump(result, f, ensure_ascii=False, indent=2)
        print("\n结果已写入 %s" % args.out)

    if args.baseline and args.compare:
        with open(args.baseline, encoding="utf-8") as f:
            base = json.load(f)
        with open(args.compare, encoding="utf-8") as f:
            cur = json.load(f)
        print("\n=== 基线 vs 现在 ===")
        for k in ("cd_prefix_ratio", "cd_compound_ratio", "redundant_cd_ratio"):
            b, c = base.get(k, 0), cur.get(k, 0)
            delta = (c - b) * 100
            arrow = "↓ 好" if delta < 0 else ("↑ 差" if delta > 0 else "= 不变")
            print("  %-22s %.2f%% → %.2f%%  (%+.2f 个百分点) %s" % (k, 100 * b, 100 * c, delta, arrow))


if __name__ == "__main__":
    main()
```

**跑法**：

```bash
# 改造前基线（现在就能跑，events 里有真实历史）
python3 scripts/eval_cwd_redundancy.py --out /tmp/cwd_baseline.json

# 改造后（用新版本跑一段真实会话后）
python3 scripts/eval_cwd_redundancy.py --out /tmp/cwd_after.json
python3 scripts/eval_cwd_redundancy.py --baseline /tmp/cwd_baseline.json --compare /tmp/cwd_after.json
```

### 12.9 人工验收清单（真实 bridge E2E）

| # | 步骤 | 期望 |
|---|---|---|
| E1 | 起一个会话，让 agent `cd` 到子目录 | 后续 bash 的 pwd 是新目录 |
| E2 | 同上，然后让它 `write_file("t.txt")` | 文件落在**新目录** |
| E3 | 看下一轮 LLM 请求的 messages | 末尾有一条 `# Environment` / `# Environment update`，含新目录 |
| E4 | 看 system 消息 | **不含** cwd（字节不变 ⇒ 前缀缓存不破） |
| E5 | 故意 `cd /tmp`（越界） | 工具结果含 "Shell cwd was reset to ..."；后续仍在项目根 |
| E6 | 子 agent 里 `cd` | 主 agent 的目录不受影响 |
| E7 | 切模型（rebuildLoop） | cwd **不丢** |
| E8 | 关掉应用重开会话 | cwd 回到项目根（内存态，符合 Q3），但模型仍被告知当前目录 |
| E9 | P1：worktree create → enter | cwd 切到 worktree；文件写在 worktree 内；环境块含 "git worktree" 告诫 |

---
