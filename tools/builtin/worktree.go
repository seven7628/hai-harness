package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// worktreeBaseRel git worktree 统一存放目录（相对工作区根）：
// {workspace}/.go-code/worktrees —— 目录不存在时工具自动创建。
const worktreeBaseRel = ".go-code" + string(os.PathSeparator) + "worktrees"

// GitWorktreeTool 创建 / 列出 / 移除 Git Worktree，用于多任务并行时在隔离目录进行代码开发。
//
// 设计要点：
//   - 所有 worktree 统一放在 {workspace}/.go-code/worktrees 下（多任务并行的
//     安全隔离区，目录自动创建），创建时必须指定 worktree 名字（name）；
//   - 目标路径绑定工作区根：name 解析后的绝对路径必须落在 worktrees 目录内
//     （拒绝 ".." / 绝对路径等逃逸），git 命令以工作区根为仓库上下文执行；
//   - 仅在 Code 模式注册（desktop bridge 装配：profile.withBash）。
type GitWorktreeTool struct {
	tools.BaseTool
	root string // 工作区根目录（git 仓库根）

	// wd 是**会话级**的 cwd 状态（与 bash / 文件工具共用同一个）。
	//
	// 2026-11-19（P1）：进入 worktree 要把**会话的工作目录**切过去，
	// 否则模型进了隔离目录，bash 与文件工具却还在主检出 ⇒ 又一处错位。
	wd *workdir.State

	// 「是不是 git 工作树」的判定**只查一次**（root 在会话内不变）：
	// 改造前每次调用都跑一个 `git rev-parse` 进程（list 是最高频动作 ⇒
	// 每次都付一次进程启动 + git 二进制加载，~15–50 ms）。
	gitOnce sync.Once
	gitErr  error
}

// NewGitWorktreeToolWithWorkdir 创建**接会话级 cwd 状态**的 git_worktree 工具。
//
// wd = 会话级 cwd 状态（workdir.State）：只用它的**工作区根**（worktree 统一放在
// {workspace}/.go-code/worktrees 下，是工作区级约定，不随 cd 变）。
//
// wd 为 nil ⇒ 退化成 NewGitWorktreeTool("")，不 panic。
func NewGitWorktreeToolWithWorkdir(wd *workdir.State) *GitWorktreeTool {
	if wd == nil {
		// 兜底：自己建一个（不与别的工具共享 cwd，但 enter/exit 仍可用）。
		return NewGitWorktreeTool("")
	}
	t := NewGitWorktreeTool(wd.Root())
	t.wd = wd
	return t
}

// NewGitWorktreeTool 创建 git_worktree 工具；root 为工作区根目录（绝对路径）。
func NewGitWorktreeTool(root string) *GitWorktreeTool {
	t := &GitWorktreeTool{
		BaseTool: tools.BaseTool{
			Name_: "git_worktree",
			Description_: "Manage Git worktrees (create/enter/exit/list/remove) to develop multiple tasks in parallel in isolated directories. " +
				"\nWHEN TO USE: use 'create' when you need to start an independent parallel task (a separate checkout) without blocking or disturbing the main working tree — e.g. you have a long task and don't want it to conflict with your current edits, or you want a clean isolated environment per task. Use 'list' to check which worktrees already exist (always list before creating to avoid duplicate names). Use 'remove' to clean up a finished worktree." +
				"\nAll worktrees live under {workspace}/.go-code/worktrees (auto-created if missing). Creating always requires a unique 'name' (a short single-segment identifier, e.g. \"feature-x\"); an optional 'start_point' (branch/commit/tag) bases the new worktree on that ref." +
				"\nCreating a worktree also SWITCHES the session's working directory into it (like a cd): later bash commands and file writes happen inside that isolated copy, not in the main checkout. Use 'enter' to switch into an existing worktree and 'exit' to come back to the workspace root. Only worktrees under {workspace}/.go-code/worktrees can be entered.\n" +
				"\nEXAMPLES:\n" +
				"  {\"action\":\"create\",\"name\":\"feature-x\"}                        // new worktree on current HEAD, then switch into it\n" +
				"  {\"action\":\"create\",\"name\":\"hotfix\",\"start_point\":\"main\"}   // new worktree based on branch 'main'\n" +
				"  {\"action\":\"enter\",\"name\":\"feature-x\"}                        // switch into an existing worktree\n" +
				"  {\"action\":\"exit\"}                                                // switch back to the workspace root\n" +
				"  {\"action\":\"list\"}                                            // show existing worktrees\n" +
				"  {\"action\":\"remove\",\"name\":\"feature-x\"}                     // remove that worktree",
			Params_: tools.Obj(map[string]any{
				"action":      tools.Map{"type": "string", "enum": []string{"create", "enter", "exit", "list", "remove"}, "description": "Operation: create (new worktree, then switch into it), enter (switch into an existing worktree), exit (switch back to the workspace root), list (list existing), remove (remove a worktree)"},
				"name":        tools.Str("Worktree name (unique, single path segment under .go-code/worktrees, e.g. \"feature-x\"). Required for create and remove."),
				"start_point": tools.Str("Optional branch/commit/tag to base the new worktree on (create only)."),
			}, "action"),
			CanParallel_: false,
		},
		root: filepath.Clean(root),
		// 独立状态：不与其它工具共享 cwd（生产路径用 NewGitWorktreeToolWithWorkdir）。
		// 有了它 enter/exit 在任何用法下都不会打到 nil。
		wd: workdir.New(root),
	}
	return t
}

type worktreeArgs struct {
	Action     string `json:"action"`
	Name       string `json:"name"`
	StartPoint string `json:"start_point"`
}

// worktreeDir 校验 name 并解析为 worktrees 目录内的绝对路径（拒绝逃逸）。
func (t *GitWorktreeTool) worktreeDir(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("git_worktree: name is required")
	}
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "~") {
		return "", fmt.Errorf("git_worktree: worktree name must be relative, got %q", name)
	}
	base := filepath.Join(t.root, worktreeBaseRel)
	target := filepath.Clean(filepath.Join(base, name))
	if target != base && !strings.HasPrefix(target, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("git_worktree: worktree name %q escapes the worktrees directory", name)
	}
	return target, nil
}

// ensureWorktreesDir 确保 worktrees 目录存在（不存在则创建）。
func (t *GitWorktreeTool) ensureWorktreesDir() (string, error) {
	base := filepath.Join(t.root, worktreeBaseRel)
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", fmt.Errorf("git_worktree: create worktrees dir: %w", err)
	}
	return base, nil
}

// ensureGitRepo 前置检查：t.root 必须在 git 工作树内。
// 为什么值得一次额外 git 调用：非 git 工作区（如本机主工作区）会让 create/list/remove
// 全部以 git 原始报错失败，模型只能看到 "致命错误：不是 git 仓库" 而不知如何自救；
// 这里换成可操作的说明，并说明 worktree 能力依赖 git。
func (t *GitWorktreeTool) ensureGitRepo(ctx context.Context) error {
	// 只查一次（sync.Once）：root 在会话内不变，判定结果不会变；
	// 改造前每次调用都跑一个 git 进程（list 最高频 ⇒ 每次 15–50 ms 的进程启动）。
	t.gitOnce.Do(func() {
		out, err := runGit(ctx, "-C", t.root, "rev-parse", "--is-inside-work-tree")
		if err != nil {
			t.gitErr = fmt.Errorf(
				"git_worktree: 当前工作区不是 git 仓库（%s 内未找到 .git），因此 worktree 功能不可用。"+
					"请在 git 仓库内使用本工具；若本会话的任务不需要 worktree，请改用普通文件工具在该工作区内工作。"+
					"（git 原始输出: %s）", t.root, strings.TrimSpace(out))
			return
		}
		if strings.TrimSpace(out) != "true" {
			t.gitErr = fmt.Errorf("git_worktree: %s 不是 git 工作树（rev-parse 返回 %q），worktree 功能不可用", t.root, strings.TrimSpace(out))
		}
	})
	return t.gitErr
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明工具级时限。
//
// create 是一次完整 checkout（大仓可能几分钟）；引擎默认 5 分钟兜底，
// 但这里给 120s 更明确的语义（超时会被判失败并让模型知道「太慢」而不是「坏了」）。
func (t *GitWorktreeTool) ToolTimeout() time.Duration { return 120 * time.Second }

func (t *GitWorktreeTool) ValidParams(_ context.Context, _, arguments string) error {
	var a worktreeArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("git_worktree: %w", err)
	}
	switch a.Action {
	case "create":
		if strings.TrimSpace(a.Name) == "" {
			return errors.New("git_worktree: name is required when creating a worktree")
		}
	case "enter":
		if strings.TrimSpace(a.Name) == "" {
			return errors.New("git_worktree: name is required when entering a worktree")
		}
	case "exit":
	case "remove":
		if strings.TrimSpace(a.Name) == "" {
			return errors.New("git_worktree: name is required when removing a worktree")
		}
	case "list":
	default:
		if strings.TrimSpace(a.Action) == "" {
			return errors.New("git_worktree: action is required (create|enter|exit|list|remove)")
		}
		return fmt.Errorf("git_worktree: unknown action %q (expected create|enter|exit|list|remove)", a.Action)
	}
	return nil
}

func (t *GitWorktreeTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a worktreeArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	// exit 只动会话 cwd（不碰 git）⇒ 非 git 工作区也必须可用。
	if a.Action == "exit" {
		return t.exit(ctx)
	}
	// 前置检查：非 git 工作区里其余动作都会失败，先给出可操作说明（见 ensureGitRepo）。
	if err := t.ensureGitRepo(ctx); err != nil {
		return "", err
	}
	switch a.Action {
	case "create":
		return t.create(ctx, a)
	case "enter":
		return t.enter(ctx, a)
	case "list":
		return t.list(ctx)
	case "remove":
		return t.remove(ctx, a)
	default:
		return "", fmt.Errorf("git_worktree: unknown action %q (expected create|enter|exit|list|remove)", a.Action)
	}
}

func (t *GitWorktreeTool) create(ctx context.Context, a worktreeArgs) (string, error) {
	target, err := t.worktreeDir(a.Name)
	if err != nil {
		return "", err
	}
	if _, err := t.ensureWorktreesDir(); err != nil {
		return "", err
	}
	args := []string{"-C", t.root, "worktree", "add", target}
	if strings.TrimSpace(a.StartPoint) != "" {
		args = append(args, strings.TrimSpace(a.StartPoint))
	}
	out, err := runGit(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("git_worktree: create failed: %w\n%s", err, out)
	}
	// 创建后**切进去**（对齐 Claude Code 的 EnterWorktree：
	// "creates an isolated git worktree and switches into it"）
	// ⇒ 后续 bash 与文件工具都在这个隔离目录里工作。
	notice := t.switchTo(ctx, target)
	return fmt.Sprintf("Worktree created: %s\n%s%s", target, out, notice), nil
}

// switchTo 把**会话的工作目录**切到 dir（dir 必须在本工具管理的 worktrees 目录内），
// 返回给模型看的回执（空 = 没切）。
//
// 范围规则（对齐 Claude Code 的 Scope 规则、用**目录边界**实现）：
// 只允许进入 {workspace}/.go-code/worktrees/<name>。
// 为什么不记「本会话创建过哪些」：hai 的工具实例随引擎重建而重建（切模型/切人格），
// 记不住 —— 目录边界是稳定且等价的约束（见设计文档 §16.2 D5）。
func (t *GitWorktreeTool) switchTo(ctx context.Context, dir string) string {
	base := filepath.Clean(filepath.Join(t.root, worktreeBaseRel))
	abs := filepath.Clean(dir)
	if abs != base && !strings.HasPrefix(abs, base+string(os.PathSeparator)) {
		return ""
	}
	prev := ""
	if t.wd != nil {
		prev = t.wd.Current(cwdKey(ctx))
		t.wd.Set(cwdKey(ctx), abs)
	}
	notice := "\nWorking directory is now " + abs
	if prev != "" && prev != abs {
		notice += " (was " + prev + ")"
	}
	return notice
}

// enter 切进一个已存在的 worktree（把**会话的工作目录**切过去）。
//
// 只接受本工具管理目录下的 worktree（name 经 worktreeDir 校验，拒绝逃逸）；
// 不接受任意绝对路径 —— 越界由 switchTo 兜底。
func (t *GitWorktreeTool) enter(ctx context.Context, a worktreeArgs) (string, error) {
	target, err := t.worktreeDir(a.Name)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(target)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("git_worktree: worktree %q does not exist (list existing ones with \"list\")", target)
	}
	notice := t.switchTo(ctx, target)
	if notice == "" {
		return "", fmt.Errorf("git_worktree: %q is not a managed worktree (they live under %s)", target, filepath.Join(t.root, worktreeBaseRel))
	}
	return "Working directory is now " + target +
		"\n(the worktree is the isolated copy you should work in)", nil
}

// exit 退出 worktree：把会话的工作目录还原回工作区根。
func (t *GitWorktreeTool) exit(ctx context.Context) (string, error) {
	cur := ""
	if t.wd != nil {
		cur = t.wd.Current(cwdKey(ctx))
	}
	if cur == "" || cur == t.root {
		return fmt.Sprintf("Already at the workspace root: %s", t.root), nil
	}
	if t.wd != nil {
		t.wd.Reset(cwdKey(ctx))
	}
	return fmt.Sprintf("Working directory is now %s (back to the workspace root)", t.root), nil
}

func (t *GitWorktreeTool) list(ctx context.Context) (string, error) {
	out, err := runGit(ctx, "-C", t.root, "worktree", "list")
	if err != nil {
		return "", fmt.Errorf("git_worktree: list failed: %w\n%s", err, out)
	}
	if out == "" {
		return "No worktrees.", nil
	}
	return out, nil
}

func (t *GitWorktreeTool) remove(ctx context.Context, a worktreeArgs) (string, error) {
	target, err := t.worktreeDir(a.Name)
	if err != nil {
		return "", err
	}
	// 必须在 git remove **之前**取当前 cwd：目录一删，State.Current 就会自愈
	//（见 workdir.Current 的"目录没了 ⇒ 回落根"）⇒ 再取已经是根，判不出"删的是当前所在"。
	wasHere := t.wd != nil && filepath.Clean(t.wd.Current(cwdKey(ctx))) == filepath.Clean(target)
	out, err := runGit(ctx, "-C", t.root, "worktree", "remove", target)
	if err != nil {
		return "", fmt.Errorf("git_worktree: remove failed: %w\n%s", err, out)
	}
	// 删掉的是**当前所在**的 worktree ⇒ 会话 cwd 指向了不存在的路径 ⇒ 还原回根。
	notice := ""
	if wasHere {
		t.wd.Reset(cwdKey(ctx))
		notice = cwdChangeNotice(target, t.root)
	}
	return fmt.Sprintf("Worktree removed: %s\n%s%s", target, out, notice), nil
}

// gitWaitDelay 子命令**成功退出后**等待 I/O 管道关闭的上限（C10 同型站点，2026-09-18）。
//
// **为什么必需**：`cmd.Stdout = &out`（io.Writer）⇒ Go 建 `os.Pipe` + 拷贝协程 ⇒
// `Wait()` **依赖管道关闭**（os/exec 的 awaitGoroutines 要等到管道 EOF 才返回）。
// 而 `exec.CommandContext` 在 ctx 到期时**只杀直接子进程**，git（或其拉起的钩子、
// credential helper、传输进程）fork 出的后代仍存活并握住写端 ⇒ `Wait()` 永不返回
// —— **超时在这里是失效的**（超时管不住一个「进程自己已退出、但管道被后代持有」的等待）。
//
// 这正是 2026-09-18 **C8** 在 `sandbox.runExec` 上修掉的同一缺陷（生产实测挂起 602s）；
// 本处是同型站点，故采用同一处方（与 runtime/python.go 的 pyWaitDelay 逐字同型，
// 也与 sandbox.waitDelayAfterKill 同值）。
//
// 语义：进程退出后最多再等 `gitWaitDelay`，到点由 `os/exec` 强制关管道并使 `Wait` 返回
// （返回 `exec.ErrWaitDelay`）。**代价**：该情形下输出可能被进程组外的后代截断 ——
// 由调用点如实注明（见 gitWaitDelayNote），而**不是**把成功印成失败。
const gitWaitDelay = 3 * time.Second

// gitWaitDelayNote ErrWaitDelay 情形下追加在**成功**输出尾部的说明（模型可见）。
const gitWaitDelayNote = "[exec: WaitDelay 到期 —— 输出可能被进程组外的后代截断]"

// lockedBuffer 并发安全的输出缓冲（C10 同型站点）。
//
// **为什么不是裸 `strings.Builder`**：`WaitDelay` 到期时 `Wait` 会在**拷贝协程仍可能写入**
// 的情况下返回（os/exec 关掉管道后仍会 `<-c.goroutineErr`，但主流程随即读缓冲），
// 此时主流程读缓冲是**数据竞争** —— C8 在 `sandbox` 侧用 `-race` 实测到 `WARNING: DATA RACE`
// （读：极端分支取部分输出；写：`io.copyBuffer` 的拷贝协程）。
//
// 本处**未复现**该竞争（主会话实测窗口为微秒级，`-race` 下没抓到），属**防御性**加固：
// 不主张它是本次有界性修复的承重部分（承重的是 gitWaitDelay）。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	cut bool // 已超上限：后面的内容被丢弃
}

// gitOutputCap 单条 git 输出的内存上限（字节）。
//
// 改造前**无上限**：git 的输出（`git worktree list` 的路径、超长错误信息）全部进内存，
// 而引擎的 20 KB 闸门在这之后才生效 ⇒ 一次异常输出就能吃掉大量内存。
const gitOutputCap = 1 << 20

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() >= gitOutputCap {
		b.cut = true
		return len(p), nil // 丢弃后续内容（**不报错**：命令本身继续跑）
	}
	n, err := b.buf.Write(p)
	if b.buf.Len() >= gitOutputCap {
		b.cut = true
	}
	return n, err
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if b.cut {
		s += "\n[git output truncated at 1 MiB]"
	}
	return s
}

// runGit 执行 git 命令，捕获 stdout+stderr。命令超时跟随 ctx（引擎兜底 / 批超时）。
func runGit(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	// out/errb 分开缓冲：成功只看 stdout、失败时合并两者报错（保持既有语义）。
	// 用带锁缓冲而非 strings.Builder —— 见 lockedBuffer 注释。
	var out, errb lockedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	// C10：进程退出后最多再等 gitWaitDelay 即强制关管道并返回 —— 根治「后代持管道
	// ⇒ Wait 永不返回」（与 C8 在 sandbox.runExec、runtime.defaultRunCmd 的处方一致）。
	cmd.WaitDelay = gitWaitDelay
	err := cmd.Run()
	if err != nil {
		// C10：ErrWaitDelay = 进程**已成功退出**，只是管道被进程组外的后代占着。
		// 这不判失败（os/exec 仅在进程成功退出且未走 Cancel 时才返回该错误）；旧行为
		// 在此**会一直挂住** —— 超时管不住「进程已退出、管道被后代持有」的等待。
		// 如实返回输出并注明可能被截断：有界返回 + 明说截断 > 无限期等待。
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			s := strings.TrimSpace(out.String())
			if s == "" {
				return gitWaitDelayNote, nil
			}
			return s + "\n" + gitWaitDelayNote, nil
		}
		return strings.TrimSpace(out.String() + errb.String()), err
	}
	return strings.TrimSpace(out.String()), nil
}

var _ tools.Tool = (*GitWorktreeTool)(nil)
