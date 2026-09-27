package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// DefaultGitTimeout 单次 git 子进程超时。
//
// 身份解析发生在**会话打开路径上**，不能让仓库状态（奇怪的 credential helper、
// 慢速网络盘上的 .git）把会话卡住；3s 足够本地 rev-parse 出结果，到点即视为解析失败。
const DefaultGitTimeout = 3 * time.Second

// gitWaitDelay git 进程退出后关闭 stdout/stderr 管道的等待上限。
//
// 与 tools/builtin/worktree.go 的 C10 处方同型：git 可能 fork 出持有管道写端的后代
// （credential helper 等），此时 Wait() 依赖管道 EOF 才返回，超时形同虚设。
// 到点由 os/exec 强制关管道；ErrWaitDelay 不判失败（进程已成功退出，输出可信）。
const gitWaitDelay = 3 * time.Second

// maxIDLen 目录名长度上限。超长（很深的 group 路径）时截断并补哈希后缀，
// 避免目录名长到失去可辨认性——可辨认是刻意保留的：用户列一眼要看得出哪份记忆属于哪个仓库。
const maxIDLen = 80

// Resolver 由工作目录解析仓库身份。零值可用（Run 为 nil 时执行真实 git）。
type Resolver struct {
	// Run git 执行出口（nil = 真实 git 子进程）。仅测试注入用。
	Run func(ctx context.Context, dir string, args ...string) (string, error)
	// Timeout 单次 git 调用超时（<=0 = DefaultGitTimeout）。
	Timeout time.Duration
}

// New 返回默认解析器。
func New() *Resolver { return &Resolver{} }

// Resolve 是 Resolve 的包级便捷入口。
func Resolve(ctx context.Context, workdir string) (Dir, error) { return New().Resolve(ctx, workdir) }

// Resolve 由工作目录（通常是 session 的 workdir）解析 Project Memory 的目录。
//
// 官方 memory#storage-location 只规定了「<project> 由 git 仓库派生，同一仓库的所有
// worktree 与子目录共享一份记忆」，没有规定 ID 的具体字符串形态。本包据此的派生顺序：
//
//  1. git rev-parse --show-toplevel —— 确认在 git 工作树内，否则 ErrNoGit；
//  2. 有 remote  → 由 remote URL 规范化出 ID（可辨认、跨克隆路径稳定）；
//  3. 无 remote  → 由 git rev-parse --git-common-dir 派生。
//
// 第 3 条是 worktree 共享的承重点：worktree 各自的 .git 是一个**文件**，指向主仓库的
// common dir，因此同一仓库的任意 worktree 都得到同一个 common dir → 同一个 ID。
// （用 toplevel 会让每个 worktree 各得一份记忆，直接违反官方共享语义。）
func (r *Resolver) Resolve(ctx context.Context, workdir string) (Dir, error) {
	dir, err := normalizeDir(workdir)
	if err != nil {
		return Dir{}, err
	}
	top, err := r.git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Dir{}, fmt.Errorf("memory: %q 不在 git 工作树内（%v）：%w", dir, err, ErrNoGit)
	}
	top = strings.TrimSpace(top)
	if top == "" {
		return Dir{}, fmt.Errorf("memory: --show-toplevel 输出为空：%w", ErrNoGit)
	}
	if !filepath.IsAbs(top) {
		// git 正常输出绝对路径；相对输出按 workdir 解析（防御性，保留可解释性）。
		top = filepath.Clean(filepath.Join(dir, top))
	}

	// remote 优先：跨机器、跨克隆路径一致，换机器 clone 一次即可复用同一份记忆。
	// 官方未规定「多个 remote 取哪个」——本包取 origin，没有 origin 时取配置里的第一个。
	remote := r.remoteURL(ctx, top)
	id := ""
	if remote != "" {
		id = idFromRemote(remote)
	}
	if id == "" {
		// 无 remote（或 remote 形态无法规范化）→ 用 git common dir，worktree 共享的关键。
		common, err := r.git(ctx, top, "rev-parse", "--git-common-dir")
		if err != nil {
			return Dir{}, fmt.Errorf("memory: 读取 --git-common-dir 失败（%v）：%w", err, ErrNoIdentity)
		}
		id = idFromGitDir(top, strings.TrimSpace(common))
	}
	if id == "" {
		return Dir{}, fmt.Errorf("memory: 由 toplevel %q 派生的身份为空：%w", top, ErrNoIdentity)
	}

	root, err := ReposRoot()
	if err != nil {
		return Dir{}, err
	}
	return Dir{ID: id, Path: filepath.Join(root, id)}, nil
}

// remoteURL 取规范化前的 remote URL：origin 优先，否则第一个已配置的 remote。
// 取不到返回空——**不是错误**：无 remote 的本地仓库是合法场景，走 git common dir 分支。
func (r *Resolver) remoteURL(ctx context.Context, top string) string {
	if out, err := r.git(ctx, top, "remote", "get-url", "origin"); err == nil {
		if u := firstLine(out); u != "" {
			return u
		}
	}
	out, err := r.git(ctx, top, "remote")
	if err != nil {
		return ""
	}
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if out, err := r.git(ctx, top, "remote", "get-url", name); err == nil {
			if u := firstLine(out); u != "" {
				return u
			}
		}
	}
	return ""
}

// git 统一出口：注入优先 + 超时 + WaitDelay + 禁用交互式认证。
//
// 身份解析**绝不能**因为远端要密码而挂在 stdin 上——rev-parse 本身不需要网络，
// 但环境里配了 askpass 时 git 会尝试交互。
func (r *Resolver) git(ctx context.Context, dir string, args ...string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultGitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if r.Run != nil {
		out, err := r.Run(ctx, dir, args...)
		return strings.TrimSpace(out), err
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = gitWaitDelay
	out, err := cmd.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		// 进程已成功退出，只是管道被后代占着：输出仍是可信的机器可解析值。
		return strings.TrimSpace(string(out)), nil
	}
	return strings.TrimSpace(string(out)), err
}

// idFromRemote 由 remote URL 规范化出目录名。
//
// 官方没有规定 ID 形态（只说「由 git 仓库派生」）。这里取 `<host>-<owner>-<repo>`：
// 可辨认 > 短。用户列 ~/.go-code/memory/repos 时能直接认出哪份记忆属于哪个仓库；
// 跨机器一致由 remote 本身提供，不需要再加哈希。
func idFromRemote(u string) string {
	host, p := splitRemoteURL(u)
	if host == "" || p == "" {
		return ""
	}
	segs := []string{host}
	for _, s := range strings.Split(p, "/") {
		if s := sanitizeSegment(s); s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) == 1 {
		return ""
	}
	id := strings.Join(segs, "-")
	if len(id) > maxIDLen {
		id = id[:maxIDLen-17] + "-" + shortHash(strings.Join(segs, "/"))
	}
	return id
}

// splitRemoteURL 把各种 remote 形态拆成 (host, 仓库路径)。
//
// 覆盖官方生态里真实存在的写法：scp 式（git@host:owner/repo.git）、带 scheme 的
// ssh/https/git、以及本地路径 remote（file:///… 或裸绝对路径——本地裸路径没有 host，
// 返回空 host，由调用方退回 git common dir 分支，不猜名字）。
func splitRemoteURL(u string) (host, repoPath string) {
	u = strings.TrimSpace(u)
	if u == "" {
		return "", ""
	}
	rest := u
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
		// 去掉 userinfo（git@ / oauth2:token@）：token 不能进目录名。
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		host, rest, _ = strings.Cut(rest, "/")
		if i := strings.IndexByte(host, ':'); i >= 0 { // 去掉端口
			host = host[:i]
		}
		repoPath = rest
	} else if i := strings.Index(rest, ":"); i >= 0 && !strings.Contains(rest[:i], "/") && !strings.HasPrefix(rest, "/") {
		// scp 式：[user@]host:path
		h := rest[:i]
		if at := strings.LastIndex(h, "@"); at >= 0 {
			h = h[at+1:]
		}
		host, repoPath = h, rest[i+1:]
	} else {
		return "", ""
	}
	host = sanitizeSegment(host)
	repoPath = strings.TrimSuffix(strings.Trim(repoPath, "/"), ".git")
	return host, repoPath
}

// idFromGitDir 无 remote 时由 git common dir 派生 ID：`<目录名>-<路径短哈希>`。
//
// 为什么必须带哈希：`~/work/api` 与 `~/tmp/api` 目录名相同，纯名字会撞车并**静默共用**
// 一份记忆——那比记不上更糟。哈希取自 common dir 的绝对路径，故同机稳定、跨机不同
// （跨机一致性由 remote 提供；无 remote 的仓库本来也没有跨机语义）。
//
// 目录名取 common dir 的父目录名：普通克隆的 common dir 是 `<repo>/.git`，用父目录名才
// 是仓库名而不是 ".git"。
func idFromGitDir(top, commonDir string) string {
	if commonDir == "" {
		return ""
	}
	abs := commonDir
	if !filepath.IsAbs(abs) {
		// 相对输出以 toplevel 为基准（git 文档口径）。
		abs = filepath.Clean(filepath.Join(top, commonDir))
	}
	abs = filepath.ToSlash(abs)
	base := sanitizeSegment(path.Base(abs))
	if base == "" || base == ".git" {
		base = sanitizeSegment(path.Base(path.Dir(abs)))
	}
	h := shortHash(abs)
	if base == "" {
		return h
	}
	id := base + "-" + h
	if len(id) > maxIDLen {
		id = id[:maxIDLen]
	}
	return id
}

// shortHash 路径短哈希（12 hex = 48 bit）：仅用于消歧，不承担身份语义。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// sanitizeSegment 把一段路径/主机名压成安全的单段目录名片段：小写，
// 字母数字与 `. - _` 保留，其余（含 `/`、Windows 盘符冒号）压成 `-`。
func sanitizeSegment(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	return strings.Trim(b.String(), "-")
}

// normalizeDir 把输入工作目录规范成绝对路径，并在存在时解析符号链接。
//
// 解析符号链接的原因与 desktop/bridge 的 workspaceKey 相同：macOS 上 /tmp 是
// /private/tmp 的链接，同一目录的不同拼写会派生出不同 ID——记忆目录必须是唯一的那个。
func normalizeDir(workdir string) (string, error) {
	if strings.TrimSpace(workdir) == "" {
		return "", fmt.Errorf("memory: 工作目录为空：%w", ErrNoIdentity)
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return "", fmt.Errorf("memory: 解析工作目录 %q 失败：%v：%w", workdir, err, ErrNoIdentity)
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = resolved
	}
	return abs, nil
}

// firstLine 取 git 输出的第一行非空内容。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
