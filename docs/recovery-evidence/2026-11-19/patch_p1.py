p = 'tools/builtin/worktree.go'
s = open(p, encoding='utf-8').read()

# ---------- 1) struct + 构造器 ----------
old = '''type GitWorktreeTool struct {
	tools.BaseTool
	root string // 工作区根目录（git 仓库根）
'''
new = '''type GitWorktreeTool struct {
	tools.BaseTool
	root string // 工作区根目录（git 仓库根）

	// wd 是**会话级**的 cwd 状态（与 bash / 文件工具共用同一个）。
	//
	// 2026-11-19：进入 worktree 要把**会话的工作目录**切过去，
	// 否则模型进了隔离目录，bash 与文件工具却还在主检出 ⇒ 又一处错位。
	wd *workdir.State
'''
assert old in s, 'struct'
s = s.replace(old, new, 1)

old = '''// NewGitWorktreeTool 创建 git_worktree 工具；root 为工作区根目录（绝对路径）。
func NewGitWorktreeTool(root string) *GitWorktreeTool {
	t := &GitWorktreeTool{'''
new = '''// NewGitWorktreeTool 创建 git_worktree 工具；root 为工作区根目录（绝对路径）。
//
// ⚠️ **独立状态**（自己建一个 workdir.State）：不与其它工具共享 cwd。
// 生产路径用 NewGitWorktreeToolWithWorkdir。
func NewGitWorktreeTool(root string) *GitWorktreeTool {
	return NewGitWorktreeToolWithWorkdir(workdir.New(root))
}

// NewGitWorktreeToolWithWorkdir 创建 git_worktree 工具，共享会话级 cwd 状态。
//
// 生产路径用它：进入 worktree 时切的是**会话的**工作目录
// ⇒ bash 与文件工具跟着走（对齐 Claude Code 的 EnterWorktree）。
func NewGitWorktreeToolWithWorkdir(wd *workdir.State) *GitWorktreeTool {
	root := wd.Root()
	t := &GitWorktreeTool{'''
assert old in s, 'ctor'
s = s.replace(old, new, 1)

# 构造器尾部：挂 wd
old = '''		root:      root,
		gitOnce:   sync.Once{},'''
if old not in s:
    # 找 root 赋值处
    import re
    m = re.search(r'\n\t\troot: filepath\.Clean\(root\),\n', s)
    assert m, 'root assign'
    s = s[:m.end()] + '\t\twd:        wd,\n' + s[m.end():]
else:
    s = s.replace(old, '''		root:      root,
		wd:        wd,
		gitOnce:   sync.Once{},''', 1)

# import
old = '''	"github.com/seven7628/hai-harness/tools"'''
assert old in s, 'import'
s = s.replace(old, '''	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"''', 1)

# ---------- 2) action 分派：加 enter / exit ----------
old = '''	switch a.Action {
	case "create":
		return t.create(ctx, a)
	case "list":
		return t.list(ctx)
	case "remove":
		return t.remove(ctx, a)
	default:
		return "", fmt.Errorf("git_worktree: unknown action %q (expected create|list|remove)", a.Action)
	}'''
new = '''	switch a.Action {
	case "create":
		return t.create(ctx, a)
	case "enter":
		return t.enter(ctx, a)
	case "exit":
		return t.exit(ctx)
	case "list":
		return t.list(ctx)
	case "remove":
		return t.remove(ctx, a)
	default:
		return "", fmt.Errorf("git_worktree: unknown action %q (expected create|enter|exit|list|remove)", a.Action)
	}'''
assert old in s, 'dispatch'
s = s.replace(old, new, 1)

# ---------- 3) create：创建后切进去 ----------
old = '''	out, err := runGit(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("git_worktree: create failed: %w\\n%s", err, out)
	}
	return fmt.Sprintf("Worktree created: %s\\n%s", target, out), nil
}'''
new = '''	out, err := runGit(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("git_worktree: create failed: %w\\n%s", err, out)
	}
	// 创建后**切进去**（对齐 Claude Code 的 EnterWorktree：
	// "creates an isolated git worktree and switches into it"）。
	// ⇒ 后续 bash 与文件工具都在这个隔离目录里工作。
	notice := ""
	if _, _, serr := t.wd.Set(cwdKey(ctx), target); serr == nil {
		notice = workdir.Notice(target)
	}
	return fmt.Sprintf("Worktree created: %s\\n%s%s", target, out, notice), nil
}

// enter 切进一个已存在的 worktree（把**会话的工作目录**切过去）。
//
// 范围规则（与 Claude Code 的 Scope 规则同型、但略宽）：
// 只允许进入**本工具管理的 worktrees 目录**下的 worktree
// （{workspace}/.go-code/worktrees/<name>）—— 不接受任意绝对路径。
// Claude Code 要求"必须是本会话 EnterWorktree 建的"，但那依赖它的会话记录；
// hai's 工具实例会随引擎重建而重建（切模型/切人格），记不住 ⇒ 用目录边界约束。
func (t *GitWorktreeTool) enter(ctx context.Context, a worktreeArgs) (string, error) {
	target, err := t.worktreeDir(a.Name)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(target)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("git_worktree: worktree %q does not exist (list existing ones with \\"list\\")", target)
	}
	if _, _, err := t.wd.Set(cwdKey(ctx), target); err != nil {
		return "", fmt.Errorf("git_worktree: cannot enter %q: %w", target, err)
	}
	return fmt.Sprintf("Working directory is now %s\\n(the worktree is the isolated copy you should work in)", target), nil
}

// exit 退出 worktree：把会话的工作目录还原回工作区根。
func (t *GitWorktreeTool) exit(ctx context.Context) (string, error) {
	old := t.wd.Reset(cwdKey(ctx))
	if old == "" || old == t.root {
		return fmt.Sprintf("Already at the workspace root: %s", t.root), nil
	}
	return fmt.Sprintf("Working directory is now %s (back to the workspace root)", t.root), nil
}'''
assert old in s, 'create'
s = s.replace(old, new, 1)

# ---------- 4) remove：删掉当前所在的 worktree ⇒ 目录没了 ⇒ 还原回根 ----------
old = '''	out, err := runGit(ctx, "-C", t.root, "worktree", "remove", target)
	if err != nil {
		return "", fmt.Errorf("git_worktree: remove failed: %w\\n%s", err, out)
	}
	return fmt.Sprintf("Worktree removed: %s\\n%s", target, out), nil
}'''
new = '''	out, err := runGit(ctx, "-C", t.root, "worktree", "remove", target)
	if err != nil {
		return "", fmt.Errorf("git_worktree: remove failed: %w\\n%s", err, out)
	}
	// 删掉的是**当前所在**的 worktree ⇒ 工作目录会指向不存在的路径 ⇒ 还原回根。
	notice := ""
	if cur := t.wd.Current(cwdKey(ctx)); cur == target {
		t.wd.Reset(cwdKey(ctx))
		notice = workdir.Notice(t.root)
	}
	return fmt.Sprintf("Worktree removed: %s\\n%s%s", target, out, notice), nil
}'''
assert old in s, 'remove'
s = s.replace(old, new, 1)

# ---------- 5) 工具描述：说明新动作与切换语义 ----------
old = '''			Description_: "Manage Git worktrees (create/list/remove) to develop multiple tasks in parallel in isolated directories. " +'''
new = '''			Description_: "Manage Git worktrees (create/enter/exit/list/remove) to develop multiple tasks in parallel in isolated directories. " +'''
assert old in s, 'desc1'
s = s.replace(old, new, 1)

old = '''				"\\nEXAMPLES:\\n" +
				"  {\\"action\\":\\"create\\",\\"name\\":\\"feature-x\\"}                        // new worktree on current HEAD\\n" +
				"  {\\"action\\":\\"create\\",\\"name\\":\\"hotfix\\",\\"start_point\\":\\"main\\"}   // new worktree based on branch 'main'\\n" +
				"  {\\"action\\":\\"list\\"}                                            // show existing worktrees\\n" +
				"  {\\"action\\":\\"remove\\",\\"name\\":\\"feature-x\\"}                     // remove that worktree",'''
new = '''				"\\nCreating a worktree also SWITCHES the session's working directory into it (like a cd): later bash commands and file writes happen inside that isolated copy, not in the main checkout. Use 'enter' to switch into an existing worktree, 'exit' to come back to the workspace root.\\n" +
				"\\nEXAMPLES:\\n" +
				"  {\\"action\\":\\"create\\",\\"name\\":\\"feature-x\\"}                        // new worktree on current HEAD, then switch into it\\n" +
				"  {\\"action\\":\\"create\\",\\"name\\":\\"hotfix\\",\\"start_point\\":\\"main\\"}   // new worktree based on branch 'main'\\n" +
				"  {\\"action\\":\\"enter\\",\\"name\\":\\"feature-x\\"}                        // switch into an existing worktree\\n" +
				"  {\\"action\\":\\"exit\\"}                                                // switch back to the workspace root\\n" +
				"  {\\"action\\":\\"list\\"}                                            // show existing worktrees\\n" +
				"  {\\"action\\":\\"remove\\",\\"name\\":\\"feature-x\\"}                     // remove that worktree",'''
assert old in s, 'desc2'
s = s.replace(old, new, 1)

old = '''				"action":      tools.Map{"type": "string", "enum": []string{"create", "list", "remove"}, "description": "Operation: create (new worktree), list (list existing), remove (remove a worktree)"},'''
new = '''				"action":      tools.Map{"type": "string", "enum": []string{"create", "enter", "exit", "list", "remove"}, "description": "Operation: create (new worktree, then switch into it), enter (switch into an existing worktree), exit (switch back to the workspace root), list (list existing), remove (remove a worktree)"},'''
assert old in s, 'desc3'
s = s.replace(old, new, 1)

open(p, 'w', encoding='utf-8').write(s)
print('worktree.go ok')

# ---------- 6) bridge 装配 ----------
p = 'desktop/bridge/main.go'
s = open(p, encoding='utf-8').read()
old = 'builtin.NewGitWorktreeTool(workspace)'
new = 'builtin.NewGitWorktreeToolWithWorkdir(wd)'
assert old in s, 'bridge worktree'
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('bridge ok')
