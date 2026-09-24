package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type gitFile struct {
	Path      string `json:"path"`
	Status    string `json:"status"`
	Staged    bool   `json:"staged"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}
type gitSnapshotData struct {
	IsRepo     bool      `json:"is_repo"`
	Root       string    `json:"root,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Head       string    `json:"head,omitempty"` // HEAD short hash（unborn HEAD 为空；前端据此决定是否重拉历史）
	Detached   bool      `json:"detached,omitempty"`
	Ahead      int       `json:"ahead"`
	Behind     int       `json:"behind"`
	Staged     int       `json:"staged"`
	Modified   int       `json:"modified"`
	Untracked  int       `json:"untracked"`
	Conflicted int       `json:"conflicted"`
	Clean      bool      `json:"clean"`
	Files      []gitFile `json:"files"`
}

// gitWaitDelay C10（2026-09-18）：`git` 进程**成功退出后**等待 I/O 管道关闭的上限。
//
// **为什么必需**：`CombinedOutput()` 内部把 `cmd.Stdout` 指向 `io.Writer` ⇒ Go 建 `os.Pipe`
// + 拷贝协程 ⇒ `Wait()` **依赖管道关闭**。而 `gitRun` 会执行 `commit` / `checkout` ——
// 二者**触发 git hook**（`pre-commit`/`commit-msg`/`post-commit`/`post-checkout`），
// 而 hook 是**任意用户脚本**（husky / pre-commit / lint-staged 等），完全可以 fork 出
// **跨进程组存活**的后代并继承 stdout ⇒ 管道写端被持有 ⇒ `Wait()` 永不返回。
// 此时 `exec.CommandContext` 的 8s 超时**救不了**：它只杀**直接子进程**。
//
// 这正是 2026-09-18 **C8** 在 `sandbox.runExec` 上修掉的同一缺陷（生产实测挂起 602s）；
// 本处与 C10 已修的 `tools/builtin/worktree.go`、`skills/remote/install.go` 同型同源。
//
// 语义：进程退出后最多再等 `gitWaitDelay`，到点由 `os/exec` 强制关管道并使 `Wait` 返回
// （返回 `exec.ErrWaitDelay`）。代价是该情形下输出可能被后代截断 —— 调用点据此**不判失败**。
const gitWaitDelay = 3 * time.Second

func gitRun(ctx context.Context, ws string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	a := append([]string{"-C", ws}, args...)
	cmd := exec.CommandContext(c, "git", a...)
	// C10：见 gitWaitDelay 注释。CombinedOutput 形态**只做两件事**（与 C10 既定口径一致）：
	// 设 WaitDelay + 放行 ErrWaitDelay，不改写成显式 Stdout（无注入带锁缓冲的落点，
	// 且那是更大重构 —— 主会话在同类站点用 -race 未复现竞争）。
	cmd.WaitDelay = gitWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		// C10：ErrWaitDelay = 进程**已成功退出**（命令成功了），只是管道被进程组外的后代占着
		//（典型：hook 里起了后台进程）。**不得判失败** —— 否则「罕见挂起」被换成「罕见假失败」，
		// 比现状更糟（这是 C10 三处站点都用变异 B 类验证过的硬约束）。
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return string(out), nil
		}
		return string(out), err
	}
	return string(out), nil
}
func gitSnapshot(ctx context.Context, ws string) (gitSnapshotData, error) {
	var s gitSnapshotData
	root, err := gitRun(ctx, ws, "rev-parse", "--show-toplevel")
	if err != nil {
		return s, nil
	}
	s.IsRepo = true
	s.Root = strings.TrimSpace(root)
	branch, _ := gitRun(ctx, ws, "symbolic-ref", "--short", "-q", "HEAD")
	s.Branch = strings.TrimSpace(branch)
	s.Detached = s.Branch == ""
	if h, err := gitRun(ctx, ws, "rev-parse", "--short", "HEAD"); err == nil {
		s.Head = strings.TrimSpace(h) // unborn HEAD 时 git 报错：保持为空
	}
	raw, err := gitRun(ctx, ws, "status", "--porcelain=v1")
	if err != nil {
		return s, fmt.Errorf("git status: %s", firstLine(raw))
	}
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if len(line) < 3 {
			continue
		}
		xy, path := line[:2], strings.TrimSpace(line[3:])
		// porcelain rename 行形如 "R  old -> new"：只保留新路径
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		f := gitFile{Path: path}
		// X 列非空格即有暂存态；但 '??'（未跟踪）两列都是 '?'，不算暂存
		if xy[0] != ' ' && xy[0] != '?' {
			f.Staged = true
			s.Staged++
		}
		if xy[1] == '?' {
			f.Status = "untracked"
			s.Untracked++
		} else if xy[0] == 'U' || xy[1] == 'U' {
			f.Status = "conflicted"
			s.Conflicted++
		} else if xy[1] == 'D' || xy[0] == 'D' {
			f.Status = "deleted"
		} else if xy[0] == 'A' || xy[1] == 'A' {
			f.Status = "added"
		} else {
			f.Status = "modified"
			s.Modified++
		}
		s.Files = append(s.Files, f)
	}
	s.Clean = len(s.Files) == 0
	// numstat：为变更文件补充 +/- 行数（未暂存 + 已暂存各跑一次归并；
	// 二进制文件计数为 "-" → 记 0；未跟踪文件不出现在 diff 中 → 保持 0）
	applyNumstat := func(out string) {
		for _, line := range strings.Split(out, "\n") {
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) != 3 {
				continue
			}
			add, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
			del, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
			if add < 0 {
				add = 0
			}
			if del < 0 {
				del = 0
			}
			p := numstatPath(parts[2])
			for i := range s.Files {
				if s.Files[i].Path == p {
					s.Files[i].Additions += add
					s.Files[i].Deletions += del
				}
			}
		}
	}
	if out, err := gitRun(ctx, ws, "diff", "--numstat"); err == nil {
		applyNumstat(out)
	}
	if out, err := gitRun(ctx, ws, "diff", "--cached", "--numstat"); err == nil {
		applyNumstat(out)
	}
	return s, nil
}

// numstatPath 归一 numstat 第 3 列路径：rename 三种语法
// "old => new" / "{old => new}" / "head{old => new}tail" 都归一为最终路径
// （第三种 = head + new + tail）。
func numstatPath(col string) string {
	col = strings.TrimSpace(col)
	if i := strings.Index(col, "=>"); i >= 0 {
		head := ""
		if b := strings.Index(col, "{"); b >= 0 && b < i {
			head = col[:b]
		}
		tail := col[i+2:]
		newPart, rest := tail, ""
		if j := strings.Index(tail, "}"); j >= 0 {
			newPart, rest = strings.TrimSpace(tail[:j]), tail[j+1:]
		}
		col = strings.TrimSpace(head + newPart + rest)
	}
	return col
}
func gitDiff(ctx context.Context, ws, path string, staged bool) (string, error) {
	args := []string{"diff", "--no-ext-diff", "--no-color", "--", path}
	if staged {
		args = []string{"diff", "--cached", "--no-ext-diff", "--no-color", "--", path}
	}
	out, err := gitRun(ctx, ws, args...)
	const maxDiffBytes = 512 * 1024
	if len(out) > maxDiffBytes {
		out = out[:maxDiffBytes] + "\n[diff truncated]"
	}
	return out, err
}
func parseAheadBehind(ctx context.Context, ws string, s *gitSnapshotData) {
	out, err := gitRun(ctx, ws, "rev-list", "--left-right", "--count", "@{upstream}...HEAD")
	if err != nil {
		return
	}
	p := strings.Fields(out)
	if len(p) == 2 {
		s.Behind, _ = strconv.Atoi(p[0])
		s.Ahead, _ = strconv.Atoi(p[1])
	}
}

type gitCommit struct {
	Hash       string `json:"hash"`
	ShortHash  string `json:"short_hash"`
	Subject    string `json:"subject"`
	Author     string `json:"author"`
	Date       string `json:"date"`
	Graph      string `json:"graph,omitempty"`       // --graph 图示前缀（如 "* |"），前端按泳道着色渲染
	GraphAfter string `json:"graph_after,omitempty"` // 该提交之后的纯图延续行（如 "|\\"），保持拓扑可视
}

// firstLine 取命令输出首行（错误信息透传 git 原文，替代干巴巴的 "exit status N"）
func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// isHexRun 判断 s 是否全为十六进制字符
func isHexRun(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// —— 写操作（P1，docs/DESKTOP_GIT.md §4.2）：不经 shell、路径强制限制在 workspace 内 ——

// sanitizePaths 写操作路径安全校验：必须相对路径，Clean 后不得逃逸 workspace；≤200 条。
func sanitizePaths(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("paths 为空")
	}
	if len(raw) > 200 {
		return nil, fmt.Errorf("paths 过多（上限 200）")
	}
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) {
			return nil, fmt.Errorf("路径必须为相对路径: %s", p)
		}
		c := filepath.Clean(p)
		if c == ".." || strings.HasPrefix(c, "../") {
			return nil, fmt.Errorf("路径不得逃逸工作区: %s", p)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("paths 为空")
	}
	return out, nil
}

func gitStage(ctx context.Context, ws string, raw []string) (int, error) {
	paths, err := sanitizePaths(raw)
	if err != nil {
		return 0, err
	}
	out, err := gitRun(ctx, ws, append([]string{"add", "--"}, paths...)...)
	if err != nil {
		return 0, fmt.Errorf("git add: %s", firstLine(out))
	}
	return len(paths), nil
}

func gitUnstage(ctx context.Context, ws string, raw []string) (int, error) {
	paths, err := sanitizePaths(raw)
	if err != nil {
		return 0, err
	}
	out, err := gitRun(ctx, ws, append([]string{"reset", "-q", "HEAD", "--"}, paths...)...)
	if err != nil {
		return 0, fmt.Errorf("git reset: %s", firstLine(out))
	}
	return len(paths), nil
}

// gitDiscard 丢弃变更（破坏性）：tracked 恢复到 index 版本；untracked 用 clean 删除
// （显式 paths + -d 允许目录；不用 -x/-X，绝不波及列出的路径之外）。
func gitDiscard(ctx context.Context, ws string, raw []string, untracked bool) (int, error) {
	paths, err := sanitizePaths(raw)
	if err != nil {
		return 0, err
	}
	if untracked {
		out, err := gitRun(ctx, ws, append([]string{"clean", "-q", "-f", "-d", "--"}, paths...)...)
		if err != nil {
			return 0, fmt.Errorf("git clean: %s", firstLine(out))
		}
	} else {
		out, err := gitRun(ctx, ws, append([]string{"checkout", "-q", "--"}, paths...)...)
		if err != nil {
			return 0, fmt.Errorf("git checkout: %s", firstLine(out))
		}
	}
	return len(paths), nil
}

func gitCommitWorktree(ctx context.Context, ws, msg string, stageAll bool) (string, error) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return "", fmt.Errorf("提交信息不能为空")
	}
	if len(msg) > 2000 {
		return "", fmt.Errorf("提交信息过长（上限 2000 字符）")
	}
	if stageAll {
		if out, err := gitRun(ctx, ws, "add", "-A"); err != nil {
			return "", fmt.Errorf("git add -A: %s", firstLine(out))
		}
	}
	out, err := gitRun(ctx, ws, "commit", "-q", "-m", msg)
	if err != nil {
		return "", fmt.Errorf("git commit: %s", firstLine(out))
	}
	h, err := gitRun(ctx, ws, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %s", firstLine(h))
	}
	return strings.TrimSpace(h), nil
}

// —— 分支操作与仓库初始化（P2，docs/DESKTOP_GIT.md）——

// validBranchName 分支名安全校验：防 flag 注入（前导 -）与 ref 歧义字符；git 自身校验兜底
func validBranchName(name string) error {
	if name == "" || len(name) > 256 {
		return fmt.Errorf("分支名非法")
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("分支名非法: %s", name)
	}
	if strings.Contains(name, "..") || strings.Contains(name, "//") || strings.ContainsAny(name, " \t~^:?*[\\") {
		return fmt.Errorf("分支名非法: %s", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("分支名含控制字符")
		}
	}
	return nil
}

type gitBranchInfo struct {
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

func gitBranchList(ctx context.Context, ws string) ([]gitBranchInfo, error) {
	out, err := gitRun(ctx, ws, "branch", "--format=%(HEAD)%09%(refname:short)")
	if err != nil {
		return nil, fmt.Errorf("git branch: %s", firstLine(out))
	}
	var branches []gitBranchInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		branches = append(branches, gitBranchInfo{Name: strings.TrimSpace(parts[1]), Current: strings.TrimSpace(parts[0]) == "*"})
	}
	return branches, nil
}

func gitCheckoutBranch(ctx context.Context, ws, name string) (string, error) {
	if err := validBranchName(name); err != nil {
		return "", err
	}
	out, err := gitRun(ctx, ws, "checkout", "-q", name)
	if err != nil {
		return "", fmt.Errorf("git checkout: %s", firstLine(out))
	}
	return name, nil
}

// gitInitRepo 在工作区初始化仓库；已是仓库时报错（幂等保护，防误触覆盖认知）
func gitInitRepo(ctx context.Context, ws string) (bool, error) {
	if _, err := gitRun(ctx, ws, "rev-parse", "--git-dir"); err == nil {
		return false, fmt.Errorf("该目录已经是 Git 仓库")
	}
	out, err := gitRun(ctx, ws, "init", "-q")
	if err != nil {
		return false, fmt.Errorf("git init: %s", firstLine(out))
	}
	return true, nil
}

func gitLog(ctx context.Context, ws string, limit int, skip int) ([]gitCommit, error) {
	if limit < 1 || limit > 100 {
		limit = 30
	}
	if skip < 0 {
		skip = 0
	}
	// --graph：图示列写在每行最前（先图示、后 format 输出），记录行形如 "* | \x1e<hash>\x1f..."；
	// 提交之间的纯图延续行（|\、|/ 等）不含 \x1e，按行解析时挂到最近一条提交的 graph_after，
	// 保持分支拓扑可视。
	out, err := gitRun(ctx, ws, "log", "--graph", "--date=iso-strict",
		fmt.Sprintf("--skip=%d", skip), fmt.Sprintf("-%d", limit),
		"--pretty=format:%x1e%H%x1f%h%x1f%an%x1f%aI%x1f%s")
	if err != nil {
		// 空仓库（unborn HEAD）：git log 以 128 退出——视为空历史而非错误
		//（此前它一路透传成面板错误 "exit status 128"，把「还没有提交」误报成了故障）。
		if strings.Contains(out, "does not have any commits yet") {
			return nil, nil
		}
		return nil, fmt.Errorf("git log: %s", firstLine(out))
	}
	var commits []gitCommit
	cur := -1 // 当前提交在 commits 中的下标（延续行归属；-1 = 尚无提交）
	for _, line := range strings.Split(out, "\n") {
		before, body, isRecord := strings.Cut(line, "\x1e")
		if isRecord {
			// before = 图示前缀（如 "* |"）；body = hash\x1fshort\x1fauthor\x1fdate\x1fsubject\x1e
			p := strings.SplitN(body, "\x1f", 6)
			if len(p) >= 5 && len(p[0]) == 40 && isHexRun(p[0]) {
				commits = append(commits, gitCommit{Graph: strings.TrimRight(before, " "), Hash: p[0], ShortHash: p[1], Author: p[2], Date: p[3], Subject: p[4]})
				cur = len(commits) - 1
			} else {
				cur = -1
			}
			continue
		}
		// 纯图延续行：归属最近一条提交（保持拓扑）；空行跳过
		if cur >= 0 {
			t := strings.TrimRight(line, " ")
			if t != "" {
				if commits[cur].GraphAfter != "" {
					commits[cur].GraphAfter += "\n"
				}
				commits[cur].GraphAfter += t
			}
		}
	}
	return commits, nil
}

func gitCommitDiff(ctx context.Context, ws, hash string) (string, error) {
	if len(hash) < 7 || len(hash) > 64 {
		return "", fmt.Errorf("invalid commit hash")
	}
	for _, r := range hash {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return "", fmt.Errorf("invalid commit hash")
		}
	}
	out, err := gitRun(ctx, ws, "show", "--no-ext-diff", "--no-color", "--format=fuller", hash, "--")
	if len(out) > 512*1024 {
		out = out[:512*1024] + "\n[diff truncated]"
	}
	return out, err
}
