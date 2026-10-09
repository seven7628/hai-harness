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
	"unicode/utf8"
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
// 官方 memory#storage-location：
//
//	"Each project gets its own memory directory at ~/.claude/projects/<project>/memory/.
//	 The <project> path is derived from the git repository, so all worktrees and
//	 subdirectories within the same repo share one auto memory directory.
//	 **Outside a git repo, the project root is used instead.**"
//
// 派生顺序：
//
//  1. git rev-parse --show-toplevel —— 在 git 工作树内则确认仓库根；
//  2. 有 remote  → 由 remote URL 规范化出 ID（可辨认、跨克隆路径稳定）；
//  3. 无 remote  → 由 git rev-parse --git-common-dir 派生；
//  4. **不在 git 工作树内** → 由工作目录的绝对路径派生（官方明确回落到项目根）。
//
// 第 3 条是 worktree 共享的承重点：worktree 各自的 .git 是一个**文件**，指向主仓库的
// common dir，因此同一仓库的任意 worktree 都得到同一个 common dir → 同一个 ID。
// （用 toplevel 会让每个 worktree 各得一份记忆，直接违反官方共享语义。）
//
// # 第 4 条是 2026-09-30 补上的（官方有、我们没有）
//
// 在此之前非 git 目录直接返回 ErrNoGit、不启用记忆，理由记为《技术方案》§8 待定项 1
// "倾向默认关闭"。回原文核对后发现**官方明确规定了回落到项目根目录**，那条待定项
// 的担忧（"绝对路径哈希会让同一项目移动后失去记忆"）不成立——官方的
// sessions#where-transcripts-are-stored 就是这么做的：
//
//	"<project> is your working directory path with non-alphanumeric characters
//	 replaced by `-`. For a working directory whose converted name exceeds 200
//	 characters, Claude Code truncates the name to 200 characters and appends a
//	 hash of the full path."
//
// 即官方**接受**"项目移动后失去记忆"这个代价。误判形态与 P0 三条完全一致：
// 把"我们还没实现"记成了"官方没规定"（见 Wiki §5.3）。
//
// 我们与官方的**已知偏离**（刻意，且记在这里以免被当成 bug）：
//   - 官方把路径里每一个非字母数字字符都换成 `-`；我们保留 `.` 并加 `path-` 前缀。
//     理由是防撞车：remote 派生的 ID 形如 `github.com-owner-repo`，若路径派生也
//     允许点号，一个叫 `github.com-owner-repo` 的**目录**会与同名 remote 的仓库
//     静默共用一份记忆。前缀让 `ls ~/.go-code/memory/repos` 一眼能分清哪些是
//     路径作用域、哪些是仓库作用域——可辨认是我们这一层刻意的取舍（见 idFromRemote）。
//   - 截断长度用本包的 maxIDLen(80) 而非官方的 200：我们的 ID 形态不同，官方那个
//     上限是针对它自己的路径串长度调的，照搬没有意义。
func (r *Resolver) Resolve(ctx context.Context, workdir string) (Dir, error) {
	dir, err := normalizeDir(workdir)
	if err != nil {
		return Dir{}, err
	}
	root, err := ReposRoot()
	if err != nil {
		return Dir{}, err
	}

	top, gitErr := r.gitToplevel(ctx, dir)
	if gitErr != nil {
		// 官方回落：不在 git 仓库里 → 项目根目录就是工作目录本身。
		id := idFromPath(dir)
		if id == "" {
			return Dir{}, fmt.Errorf("memory: 由路径 %q 派生的身份为空：%w", dir, ErrNoIdentity)
		}
		return Dir{ID: id, Path: filepath.Join(root, id)}, nil
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
	return Dir{ID: id, Path: filepath.Join(root, id)}, nil
}

// gitToplevel 取 git 仓库根，失败时返回错误（**不**当作致命）。
//
// 单独成函数是为了让"不在 git 内"与"git 坏了"在调用点可分：前者走官方回落，
// 后者目前也回落（用户看不到 git 安装与否，而记忆不该因此整体失效）。
func (r *Resolver) gitToplevel(ctx context.Context, dir string) (string, error) {
	top, err := r.git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("%q 不在 git 工作树内（%v）", dir, err)
	}
	top = strings.TrimSpace(top)
	if top == "" {
		return "", fmt.Errorf("git rev-parse --show-toplevel 输出为空")
	}
	if !filepath.IsAbs(top) {
		// git 正常输出绝对路径；相对输出按 workdir 解析（防御性，保留可解释性）。
		top = filepath.Clean(filepath.Join(dir, top))
	}
	return top, nil
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
		// truncateTo 而不是 id[:n]：字节切片会把中文/日文仓库名切成半个 rune，
		// 产出的目录名让 os.MkdirAll 报 illegal byte sequence —— 该仓库整体失去记忆。
		id = truncateTo(id, maxIDLen-17) + "-" + shortHash(strings.Join(segs, "/"))
	}
	return id
}

// idFromPath 无 git 时由绝对路径派生 ID：`path-<路径，非字母数字折叠为 ->-<短哈希>`。
//
// 官方口径（sessions#where-transcripts-are-stored）：
//
//	"<project> is your working directory path with non-alphanumeric characters
//	 replaced by `-` … truncates the name to 200 characters and appends a hash
//	 of the full path."
//
// 与官方三处**刻意**的偏离，理由逐条写在这里以免后来者当成 bug 去"修正"：
//
//  1. **哈希永远拼上，不只在截断时。** 官方只在超长时补，于是 `/x/a-b` 与 `/x/a/b`
//     都变成 `x-a-b`——两个不同的项目**静默共用**一份记忆。官方那套目录名是给它自己
//     的会话转录用的，撞了只是会话列表混；而记忆撞车是把 A 项目的结论喂给 B 项目。
//     本文件 idFromGitDir 早就为此在名字里带哈希，这里沿用同一原则。
//  2. **保留点号。** 可辨认：`path-Users-me-github.com-x-repo` 一眼读得出是哪个目录。
//     与 idFromRemote 的可辨认取舍一致。
//  3. **加 `path-` 前缀。** 结构性地排除与 remote 派生 ID（`github.com-owner-repo`）
//     撞车——一个恰好叫这个名字的**目录**不该和同名 remote 的仓库共用记忆。
//     前缀还让 `ls ~/.go-code/memory/repos` 一眼分清哪些是路径作用域、哪些是仓库作用域。
//
// 连续 `-` 折叠为一个：`-` 与 `/` 都映射成 `-`，不折叠的话 `/x//y` 会长出一串 `---`。
// 折叠本身会制造歧义，但第 1 条的哈希已经把歧义消掉了。
func idFromPath(abs string) string {
	abs = filepath.ToSlash(strings.TrimSpace(abs))
	if abs == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("path-")
	for _, r := range abs {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	body := collapseDashes(strings.Trim(b.String(), "-"))
	// 折叠后可能整串都是 '-'（全非 ASCII 的路径）——那就没有可辨认部分了，只留哈希。
	if body == "" || strings.Trim(body, "path-.") == "" {
		return "path-" + shortHash(abs)
	}
	const h = 12 // shortHash 的十六进制位数
	if len(body) > maxIDLen-h-1 {
		body = truncateTo(body, maxIDLen-h-1)
	}
	return body + "-" + shortHash(abs)
}

// collapseDashes 把连续的 '-' 压成一个。
func collapseDashes(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if r == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		b.WriteRune(r)
	}
	return b.String()
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
		// 同上：字节截断会产生非法 UTF-8 目录名（见 truncateTo）。
		id = truncateTo(id, maxIDLen)
	}
	return id
}

// shortHash 路径短哈希（12 hex = 48 bit）：仅用于消歧，不承担身份语义。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// truncateTo 把 s 截到最多 n **字节**，且不切坏 rune（返回的串必为合法 UTF-8）。
//
// 为什么不能直接 s[:n]：那会把多字节 rune 切成半个，产出的目录名让 os.MkdirAll 报
// "illegal byte sequence"——非 ASCII 仓库名（中文/日文目录在 macOS/Linux 上常见）
// 会因此**整体失去记忆**：目录建不出来，模型写文件失败，Step 3/4 全链路沉默失效。
// maxIDLen 是字节口径，而 ID 的可辨识部分来自目录名，两者单位不同就必须在这里对齐。
func truncateTo(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// 从 n 往前找最近的 rune 起点：s[:i] 必是完整 rune 序列。
	for i := n; i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i]
		}
	}
	return ""
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
