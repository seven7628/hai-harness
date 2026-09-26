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
)

// 身份解析的哨兵错误（调用点用 errors.Is 判别，不解析错误字符串）。
var (
	// ErrNoGit 工作目录不在 git 工作树内，且未配置 fallback。
	ErrNoGit = errors.New("memory: not a git repository")
	// ErrNoHome 取不到 home 目录，且未显式配置 Root。
	ErrNoHome = errors.New("memory: home directory unavailable")
	// ErrNoIdentity 无法确定仓库身份（root 为空、且 fallback 也不合法）。
	ErrNoIdentity = errors.New("memory: cannot determine repository identity")
)

// DefaultGitTimeout 单次 git 子进程的超时。
//
// 边界：身份解析是**每次 Open 都要做的**前台调用，不做进程池/缓存；
// 3s 足够本地 rev-parse 出结果，超时即视为无 git 身份（走 fallback 或报错），
// 不让仓库挂掉（或者用户配了奇怪的 credential helper 而卡住）阻塞会话打开。
const DefaultGitTimeout = 3 * time.Second

// gitWaitDelay git 进程退出后关闭管道的等待上限（对齐 skills/remote 的 C10 处方）。
const gitWaitDelay = 3 * time.Second

// Identity 一个仓库的记忆身份。
//
// ID 是记忆目录名（root/repos/<id>），**必须**稳定且路径安全：
//   - 有远端：ID 来自规范化后的 remote URL（跨机器、跨克隆路径一致，
//     换机器 pull 一次 clone 就复用同一份记忆）；
//   - 无远端：ID 来自 toplevel 绝对路径的短哈希 + 末级目录名（可读前缀），
//     同一台机器上稳定。
type Identity struct {
	// ID 记忆目录名（已过 validateRepoID）。
	ID string
	// RepoRoot git toplevel 绝对路径（无 git 且走 fallback 时 = 工作目录）。
	RepoRoot string
	// GitCommonDir `git rev-parse --git-common-dir`（相对 toplevel 的会转绝对；
	// 不可解析时为空）。worktree/子模块场景下这是**共用** git 目录。
	GitCommonDir string
	// RemoteURL 规范化后的远端 URL（无远端为空）。
	RemoteURL string
	// Workdir 解析时的输入工作目录（绝对路径）。
	Workdir string
	// ViaGit 为真表示身份来自真实 git 仓库；为假表示走了 fallback（非 git 目录）。
	ViaGit bool
}

// GitIdentityResolver 从工作目录解析 git 仓库身份（默认实现）。
//
// 解析顺序：--show-toplevel → --git-common-dir → remote get-url origin。
// 三步都是**可失败**的独立步骤：拿到 toplevel 后身份即已确定（ID 可由路径推出），
// 后两步只补全元信息，失败不降级为错误——身份解析不该因为"没有 remote"而整体失败。
type GitIdentityResolver struct {
	// Run git 执行出口（空 = 默认实现）。测试注入假 git，不依赖环境里的 git。
	Run func(ctx context.Context, dir string, args ...string) ([]byte, error)
	// FallbackID 无 git 仓库时使用的目录名（空 = 返回 ErrNoGit）。
	FallbackID string
	// Timeout 单次 git 调用超时（<=0 = DefaultGitTimeout）。
	Timeout time.Duration
}

// NewGitResolver 返回默认身份解析器。
func NewGitResolver(fallbackID string) *GitIdentityResolver {
	return &GitIdentityResolver{FallbackID: fallbackID}
}

// Resolve 实现 IdentityResolver。
func (r *GitIdentityResolver) Resolve(ctx context.Context, workdir string) (Identity, error) {
	dir, err := normalizeDir(workdir)
	if err != nil {
		return Identity{}, err
	}
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	// 1) toplevel：这一步失败 = 目录不在 git 工作树内（或 git 未安装）。
	out, err := r.git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return r.fallback(dir, err)
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return r.fallback(dir, fmt.Errorf("memory: empty --show-toplevel output: %w", err))
	}
	if !filepath.IsAbs(top) {
		// git 正常输出绝对路径；相对输出按 workdir 解析（防御性，保留可解释性）。
		top = filepath.Clean(filepath.Join(dir, top))
	}

	ident := Identity{RepoRoot: top, Workdir: dir, ViaGit: true}

	// 2) git-common-dir：失败只留空（worktree 之外的普通克隆必有，worktree 场景
	//    用于识别"多个工作树共享一个 .git"）。
	if out, err := r.git(ctx, top, "rev-parse", "--git-common-dir"); err == nil {
		ident.GitCommonDir = resolveGitDir(top, strings.TrimSpace(string(out)))
	}
	// 3) remote：origin 优先；没有 origin 退到任意一个 remote（抓 upstream/push 名字）。
	for _, remote := range []string{"origin", ""} {
		args := []string{"remote", "get-url"}
		if remote != "" {
			args = append(args, remote)
		} else {
			args = append(args, "--all")
		}
		out, err := r.git(ctx, top, args...)
		if err != nil {
			continue
		}
		if u := firstLine(string(out)); u != "" {
			ident.RemoteURL = normalizeRemote(u)
			break
		}
	}

	ident.ID = repoIDFrom(ident)
	if err := validateRepoID(ident.ID); err != nil {
		return Identity{}, fmt.Errorf("memory: derived repo id %q invalid: %w", ident.ID, err)
	}
	return ident, nil
}

// fallback 非 git 目录的退路：配置了 FallbackID 就用它，否则返回 ErrNoGit。
//
// 边界：fallback 不做路径/远程推导，ID 就是一个固定名字——这意味着"所有非 git 目录
// 共用同一份记忆"。因此它只在产品明确单仓库场景（或测试）里打开。
func (r *GitIdentityResolver) fallback(dir string, cause error) (Identity, error) {
	fb := strings.TrimSpace(r.FallbackID)
	if fb == "" {
		return Identity{}, fmt.Errorf("memory: %q is not inside a git work tree (%v): %w", dir, cause, ErrNoGit)
	}
	fb = sanitizeID(fb)
	if err := validateRepoID(fb); err != nil {
		return Identity{}, fmt.Errorf("memory: fallback id %q invalid: %w", r.FallbackID, err)
	}
	return Identity{ID: fb, RepoRoot: dir, Workdir: dir, ViaGit: false}, nil
}

// git 统一出口：注入优先 + 超时 + WaitDelay + 认证提示禁用。
func (r *GitIdentityResolver) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultGitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if r.Run != nil {
		return r.Run(ctx, dir, args...)
	}
	return runGit(ctx, dir, args...)
}

// runGit 默认 git 执行：CombinedOutput + GIT_TERMINAL_PROMPT=0 + WaitDelay。
//
// 为什么 GIT_TERMINAL_PROMPT=0：身份解析绝不能因"远端要密码"而挂在 stdin 上
// （rev-parse --show-toplevel 本身不需要网络，但环境里配了 askpass 时会尝试）。
// 为什么放行 ErrWaitDelay：进程**已成功退出**只是管道被后代占着（对齐 skills/remote
// 的 gitCombined），此时输出仍是可信的机器可解析值。
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = gitWaitDelay
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			return out, nil
		}
	}
	return out, err
}

// resolveGitDir 把 --git-common-dir 的输出转绝对路径。
// 相对输出是相对 toplevel 的（git 文档口径）；空/无法解析返回 ""。
func resolveGitDir(top, out string) string {
	if out == "" {
		return ""
	}
	if filepath.IsAbs(out) {
		return filepath.Clean(out)
	}
	return filepath.Clean(filepath.Join(top, out))
}

// repoIDFrom 从身份推出目录名：远端优先，其次路径哈希。
func repoIDFrom(id Identity) string {
	if id.RemoteURL != "" {
		return repoIDFromRemote(id.RemoteURL)
	}
	// 无 remote 的 worktree 共享 git common dir；优先用它派生目录名，
	// 这样同一 Repository 的多个 worktree 落到同一份 Project Memory。
	if id.GitCommonDir != "" {
		return repoIDFromPath(id.GitCommonDir)
	}
	return repoIDFromPath(id.RepoRoot)
}

// repoIDFromRemote 远端 URL → 目录名。
//
// 形态：`<host>-<owner>-<repo>`（全小写）。选它而不是哈希是为了**可辨认**——用户
// 打开 ~/.go-code/memory/repos 一眼知道哪份记忆属于哪个仓库。
// 远端形态千奇百怪（ssh/scp/深层 group/纯本地路径），解析失败时退到哈希，
// 不允许"猜"出一个可能撞车的名字。
func repoIDFromRemote(u string) string {
	s := normalizeRemote(u)
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	var segs []string
	for _, p := range parts {
		if p == "" {
			continue
		}
		p = strings.ToLower(p)
		p = sanitizeID(p)
		if p == "" {
			continue
		}
		segs = append(segs, p)
	}
	if len(segs) == 0 {
		return ""
	}
	// 只取 host + 末两段：owner/repo 足够辨认，再深（monorepo 子group）只会把
	// 目录名拉长到失去可读性。丢掉中间段不损失唯一性（同 host 下 owner/repo 重复
	// 概率极低；真撞了就是两个仓库共用记忆，属于可接受的已知边界）。
	if len(segs) > 3 {
		segs = append([]string{segs[0]}, segs[len(segs)-2:]...)
	}
	id := strings.Join(segs, "-")
	if len(id) > maxRepoIDLen {
		// 超长（很深的 group path）：保留首段 + 末段 + 短哈希尾部，仍可辨认。
		sum := shortHash(s)
		id = segs[0] + "-" + segs[len(segs)-1] + "-" + sum
	}
	return id
}

// repoIDFromPath 无远端时：末级目录名 + 路径短哈希。
//
// 为什么必须带哈希：`~/work/api` 与 `~/tmp/api` 同名；纯末级目录名会撞车并**静默共用**
// 一份记忆。哈希取自 toplevel 绝对路径，故同机稳定、跨机不同（跨机一致性靠 remote，
// 无 remote 的仓库本来也没有跨机语义）。
func repoIDFromPath(root string) string {
	r := filepath.ToSlash(filepath.Clean(root))
	base := path.Base(r)
	base = sanitizeID(base)
	sum := shortHash(r)
	if base == "" {
		return "repo-" + sum
	}
	id := base + "-" + sum
	if len(id) > maxRepoIDLen {
		id = base[:maxRepoIDLen-9] + "-" + sum
	}
	return id
}

// shortHash 短哈希（sha256 前 8 hex 字符）。仅用于目录名消歧，不是安全用途。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// normalizeRemote 规范化远端 URL，使同一仓库的多种 URL 写法落到同一个 ID：
//
//	https://github.com/o/r.git      → github.com/o/r
//	git@github.com:o/r.git         → github.com/o/r
//	ssh://git@github.com:22/o/r    → github.com/o/r
//
// 去 scheme、user[:port]、.git 后缀、尾斜杠；host 小写。path 部分保持大小写
// （GitHub 大小写不敏感，但 GitLab 上 `O/R` 与 `o/r` 可能是两个不同项目——
// 小写化会造成误合并，宁可两条记忆也不丢区分度）。
// 本地路径/裸路径（file:///x、/x）原样返回规范化形式，由调用点按路径处理。
func normalizeRemote(u string) string {
	s := strings.TrimSpace(u)
	if s == "" {
		return ""
	}
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		s = s[:i] // 只取第一行：`remote get-url` 不会换行，防御性截断
	}
	// file:// 与本地绝对路径：直接当路径处理。
	if strings.HasPrefix(s, "file://") {
		return filepath.ToSlash(filepath.Clean(strings.TrimPrefix(s, "file://")))
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../") {
		return filepath.ToSlash(filepath.Clean(s))
	}
	// scp 形态 git@host:path（无 // ）：转成 host/path。
	if i := strings.Index(s, ":"); i > 0 && !strings.Contains(s[:i], "/") && !strings.Contains(s, "://") {
		userHost := s[:i]
		host := userHost
		if at := strings.Index(userHost, "@"); at >= 0 {
			host = userHost[at+1:]
		}
		s = host + "/" + strings.TrimPrefix(s[i+1:], "/")
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	// 去掉 user@ 与 :port（ssh://git@host:22/path 形态）。
	if at := strings.Index(s, "@"); at >= 0 && !strings.Contains(s[:at], "/") {
		s = s[at+1:]
	}
	if i := strings.Index(s, "/"); i >= 0 {
		host, rest := s[:i], s[i+1:]
		if c := strings.Index(host, ":"); c >= 0 { // 端口
			host = host[:c]
		}
		s = strings.ToLower(host) + "/" + rest
	} else {
		s = strings.ToLower(s)
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return strings.TrimSuffix(s, "/")
}

// firstLine 取输出第一行非空内容（git 输出可能带尾随换行）。
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if v := strings.TrimSpace(line); v != "" {
			return v
		}
	}
	return ""
}

// normalizeDir 把工作目录转绝对路径并确认是目录。
// 空串 = 进程 cwd（调用点显式传 workdir 时不依赖 cwd，测试可预测）。
func normalizeDir(dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("memory: empty work directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("memory: resolve %q: %w", dir, err)
	}
	abs = filepath.Clean(abs)
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("memory: work directory %q: %w", abs, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("memory: work directory %q is not a directory", abs)
	}
	return abs, nil
}
