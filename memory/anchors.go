package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Dream v1.5：把记忆里引用的**代码锚点**拿到真实仓库里核对一遍。
//
// # 为什么这是最该先做的一步
//
// v1 的"陈旧"只检查"索引里的文件还在不在磁盘上"——那是悬空链接，几乎恒为 0。
// 真正会让一条记忆变成废纸的是**它引用的代码已经不在了**：文件被删、被改名、
// 包结构重排。这类失效不需要会话日志就能判，成本最低、信号最强。
//
// # 为什么先说"能判"再说"判准"
//
// 只核对**文件是否存在**，不核对行号、也不核对符号。理由：
//   - 行号会因为任何一次编辑而漂移（仓库里有一条记忆专门讲这个），拿漂移判"失效"
//     会制造一片假阳性；
//   - 符号级核对需要语言解析，不同语言不同规则，收益远小于"文件还在不在"。
//
// 所以本文件只回答一个问题：**这条记忆引用的文件，在这个仓库里还找得到吗？**
// 找不到的进"锚点失效"桶，附上引用原文，供人判断是"文件改名了"还是"这条记忆过时了"。

// 代码引用：`path/to/file.go:123` 或裸的 `file.go`。只认代码/配置类扩展名，
// 免得把 `README.md`、`types.d.ts` 之外的散文路径也算进来。
//
// **必须是反引号包起来的**。裸写的路径大量是散文里的举例（`scripts/renamed.py` 这种
// 根本不是真文件），不加这条会把"讲故事用的文件名"算成失效锚点，把比例抬到七成以上。
var codeRefPattern = regexp.MustCompile("`([A-Za-z0-9_./\\-]+\\.(?:go|ts|tsx|js|jsx|py|rb|java|kt|sql|proto|yaml|yml|json|sh))(?::(\\d+))?`")

// CodeRef 是一条从记忆正文里抽出来的代码引用。
type CodeRef struct {
	Path string // 原样路径（可能只有 basename，也可能是相对路径）
	Line int    // 0 = 没写行号
}

// DeadAnchor 是一条**在仓库里找不到**的引用。
type DeadAnchor struct {
	File string
	Ref  string // 原样引用，便于人去搜
}

// AnchorReport 是一个记忆库的锚点核对结果。
type AnchorReport struct {
	Root    string
	Scanned int // 参与核对的条目数
	Refs    int // 抽到的引用总数
	Dead    []DeadAnchor
	ByFile  map[string]int // 每个文件有多少条死引用
	// RefsByFile 每个文件抽到多少条引用（用来算"存活率"，死引用数单独看不懂）。
	RefsByFile map[string]int
	Unbound    bool // 没有绑定仓库路径，整段跳过
	RepoFiles  int  // 仓库里的文件数（0 且 Unbound=false 说明列不出来）
}

// repoBinding 是 `.state/repo.json` 的内容：记忆目录 ↔ 本地仓库路径。
//
// 为什么需要它：repo-id 是派生的（`git.<host>-<org>-<repo>` 或 `path-<路径>-<hash>`），
// **不可逆**。没有这份绑定，做梦连"该去哪个代码库核对"都不知道。
type repoBinding struct {
	RepoID string `json:"repo_id"`
	Root   string `json:"root"`
	// Workspace 是**会话实际的工作区**（常常是仓库的父目录）。
	//
	// 为什么单独存：记忆按 git 仓库分目录，但会话的 cwd 往往是父级工作区，
	// 记忆里引用的文件可能落在兄弟仓库里（实测 agent-server-g 的记忆大量引用
	// cybertron-gogroup 下其它子仓库的路径）。只按单仓核对会把它们全判成"失效"。
	Workspace string `json:"workspace,omitempty"`
	// Workspaces 是这个仓库**全部**已知的工作区（同一仓库可以在多个目录/工作树里被打开）。
	//
	// 为什么一个不够：实测 agent-server-g 的记忆，读取发生在
	// `/Users/admin/cybertron-gogroup/agent-server-g` 里，而仓库身份的解析是从 git 来的；
	// 只记一个"父工作区"会让整段转录统计落空（129 个会话文件里只认出 1 条记忆）。
	Workspaces []string `json:"workspaces,omitempty"`
	Bound      string   `json:"bound"`
}

// bindingPath 返回绑定文件路径。
func bindingPath(d Dir) string { return filepath.Join(d.Path, stateDirName, "repo.json") }

// BindRepo 把某工作区解析出的记忆目录与它的本地仓库路径绑起来（幂等）。
//
// 由命令行走一次即可：`memory_dream --bind <工作区路径>`。
func (c Config) BindRepo(ctx context.Context, workdir string) (Dir, error) {
	return c.BindRepoAt(ctx, workdir, "")
}

// BindRepoAt 额外记录会话工作区（空 = 与仓库同路径）。锚点核对优先在工作区里找文件。
func (c Config) BindRepoAt(ctx context.Context, workdir, workspace string) (Dir, error) {
	c = c.withDefaults()
	d, err := resolveDir(ctx, workdir, c)
	if err != nil {
		return Dir{}, err
	}
	root, err := filepath.Abs(workdir)
	if err != nil {
		return Dir{}, err
	}
	ws := workspace
	if ws != "" {
		if abs, err := filepath.Abs(ws); err == nil {
			ws = abs
		}
	}
	// 把之前记录过的工作区并进来（重复绑定不同工作区时不该互相覆盖）。
	prev, _ := c.binding(d)
	wss := append([]string{}, prev.Workspaces...)
	if prev.Workspace != "" {
		wss = append(wss, prev.Workspace)
	}
	if ws != "" {
		wss = append(wss, ws)
	}
	wss = uniqStrings(wss)
	b := repoBinding{RepoID: d.ID, Root: root, Workspace: ws, Workspaces: wss,
		Bound: c.Now().UTC().Format(time.RFC3339)}
	raw, err := json.Marshal(b)
	if err != nil {
		return Dir{}, err
	}
	dir := filepath.Dir(bindingPath(d))
	if err := c.MkdirAll(dir, 0o755); err != nil {
		return Dir{}, err
	}
	if err := c.atomicWrite(bindingPath(d), raw); err != nil {
		return Dir{}, err
	}
	return d, nil
}

// uniqStrings 去重并保持稳定顺序。
func uniqStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// RepoRoot 读出绑定的本地仓库路径。
func (c Config) RepoRoot(d Dir) (string, bool) {
	b, ok := c.binding(d)
	if !ok {
		return "", false
	}
	return b.Root, true
}

// BindRepoDiscovered 绑定仓库，并自动发现它的全部工作区（主树 / worktree / 克隆）。
func (c Config) BindRepoDiscovered(ctx context.Context, workdir string) (Dir, []string, error) {
	d, err := c.BindRepoAt(ctx, workdir, "")
	if err != nil {
		return Dir{}, nil, err
	}
	wss, err := c.DiscoverWorkspaces(ctx, d)
	if err != nil || len(wss) == 0 {
		return d, wss, err
	}
	return d, wss, c.addWorkspaces(d, wss)
}

// addWorkspaces 把发现到的工作区并进绑定（幂等）。
func (c Config) addWorkspaces(d Dir, wss []string) error {
	b, ok := c.binding(d)
	if !ok {
		return nil
	}
	b.Workspaces = uniqStrings(append(b.Workspaces, wss...))
	if b.Workspace == "" && len(b.Workspaces) > 0 {
		b.Workspace = b.Workspaces[0]
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	return c.withDefaults().atomicWrite(bindingPath(d), raw)
}

// DiscoverWorkspaces 扫描 events 目录里出现过的所有 workspace，把**解析到本仓库**的挑出来。
//
// 判据是真正的身份解析（git remote / common dir），不是路径前缀猜——同一个仓库可能有
// 主树、worktree、克隆三处工作区，只有解析才能把它们归到一起。
func (c Config) DiscoverWorkspaces(ctx context.Context, d Dir) ([]string, error) {
	c = c.withDefaults()
	root := c.eventsRoot()
	if root == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	dirs, err := c.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, de := range dirs {
		if !de.IsDir() {
			continue
		}
		ws := c.firstWorkspace(filepath.Join(root, de.Name()))
		if ws == "" || seen[ws] {
			continue
		}
		seen[ws] = true
		got, err := resolveDir(ctx, ws, c)
		if err != nil || got.ID != d.ID {
			continue
		}
		out = append(out, ws)
	}
	sort.Strings(out)
	return out, nil
}

// firstWorkspace 取一个 events 子目录里第一条带 workspace 的事件。
func (c Config) firstWorkspace(dir string) string {
	files, _ := c.ReadDir(dir)
	for _, fe := range files {
		if fe.IsDir() || !isEventLogName(fe.Name()) {
			continue
		}
		raw, err := c.readEventFile(filepath.Join(dir, fe.Name()))
		if err != nil {
			continue
		}
		if len(raw) > 64*1024 {
			raw = raw[:64*1024]
		}
		if m := workspaceField.FindSubmatch(raw); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// workspaceField 从事件里抠出 workspace 值（只用于发现，不用于判定）。
var workspaceField = regexp.MustCompile(`"workspace"\s*:\s*"([^"]+)"`)

// binding 读绑定文件。
func (c Config) binding(d Dir) (repoBinding, bool) {
	raw, err := c.withDefaults().ReadFile(bindingPath(d))
	if err != nil {
		return repoBinding{}, false
	}
	var b repoBinding
	if err := json.Unmarshal(raw, &b); err != nil || b.Root == "" {
		return repoBinding{}, false
	}
	return b, true
}

// CheckAnchors 核对一个记忆库里的代码引用。
//
// 只读：不写任何文件，也不改绑定。
func (c Config) CheckAnchors(d Dir) (AnchorReport, error) {
	c = c.withDefaults()
	rep := AnchorReport{ByFile: map[string]int{}, RefsByFile: map[string]int{}}
	b, ok := c.binding(d)
	if !ok {
		rep.Unbound = true
		return rep, nil
	}
	// 优先在**会话工作区**里找：记忆按仓库分目录，但会话 cwd 常常是它的父目录，
	// 引用的文件可能落在兄弟仓库里。只在单仓里找会把这类全判成失效。
	root := b.Root
	if b.Workspace != "" {
		root = b.Workspace
	}
	rep.Root = root
	files, err := c.RepoFiles(root)
	if err != nil {
		return rep, err
	}
	rep.RepoFiles = len(files)
	set := make(map[string]struct{}, len(files))
	bases := map[string]int{}
	for _, f := range files {
		set[f] = struct{}{}
		bases[filepath.Base(f)]++
	}

	entries, err := c.ReadDir(d.Path)
	if err != nil && !isNotExist(err) {
		return rep, err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.EqualFold(filepath.Ext(name), ".md") || strings.EqualFold(name, IndexFileName) {
			continue
		}
		raw, err := c.ReadFile(filepath.Join(d.Path, name))
		if err != nil {
			continue
		}
		rep.Scanned++
		refs := extractCodeRefs(string(raw))
		rep.RefsByFile[name] = len(refs)
		for _, ref := range refs {
			rep.Refs++
			if resolves(ref.Path, set, bases) {
				continue
			}
			rep.Dead = append(rep.Dead, DeadAnchor{File: name, Ref: refText(ref)})
			rep.ByFile[name]++
		}
	}
	sort.Slice(rep.Dead, func(i, j int) bool {
		if rep.Dead[i].File != rep.Dead[j].File {
			return rep.Dead[i].File < rep.Dead[j].File
		}
		return rep.Dead[i].Ref < rep.Dead[j].Ref
	})
	return rep, nil
}

// extractCodeRefs 抽出正文里的代码引用（去重、保序）。
func extractCodeRefs(body string) []CodeRef {
	seen := map[string]bool{}
	var out []CodeRef
	for _, m := range codeRefPattern.FindAllStringSubmatch(body, -1) {
		p := strings.TrimPrefix(strings.TrimSpace(m[1]), "./")
		if p == "" || seen[p] || isNonRepoRef(p) {
			continue
		}
		seen[p] = true
		ref := CodeRef{Path: p}
		if m[2] != "" {
			var n int
			fmt.Sscanf(m[2], "%d", &n)
			ref.Line = n
		}
		out = append(out, ref)
	}
	return out
}

// isNonRepoRef 排除"根本不是这个仓库里的路径"的引用。
//
// 三类实测噪声：
//
//	① 通配片段 —— `*_test.go` / `_test.go`，是"所有测试文件"的说法，不是某个文件；
//	② 绝对路径 —— `/tmp/imgprobe.py`、`/Users/…`，探针脚本与外部位置；
//	③ 以 `.` 开头的隐藏路径（`.state/...`），它们不是代码。
//
// 不排除的话，报告里的"失效锚点"会全是这些，真正需要人看的那几条会被淹掉。
func isNonRepoRef(p string) bool {
	if strings.HasPrefix(p, "_") || strings.HasPrefix(p, "*") || strings.HasPrefix(p, "/") || strings.HasPrefix(p, ".") {
		return true
	}
	return false
}

// resolves 判断引用能否在仓库文件表里落地。
//
// 三种写法都算命中：完整相对路径、任意后缀路径（记忆常写 `api/x.go` 而仓库里是
// `internal/api/x.go`）、以及 basename（`x.go`）。第三种只要存在同名文件就算命中——
// 歧义不是失效的证据。
func resolves(p string, set map[string]struct{}, bases map[string]int) bool {
	p = strings.TrimPrefix(strings.TrimSpace(p), "./")
	if p == "" {
		return false
	}
	if _, ok := set[p]; ok {
		return true
	}
	// 只有**带目录**的引用才做后缀匹配：`api/x.go` 可以合理地落在 `internal/api/x.go`。
	// 纯 basename（`x.go`）不做后缀匹配，否则"仓库里有两个 x.go"会被任何一条命中而
	// 掩盖歧义——那时"能找到"这个结论本身就是猜的。
	if strings.Contains(p, "/") {
		suffix := "/" + p
		for f := range set {
			if strings.HasSuffix(f, suffix) {
				return true
			}
		}
		return false
	}
	// 纯 basename 只要仓库里**还有同名文件**就算活着：我们判的是"这条记忆引用的东西
	// 还在不在"，不是在判"引用是否精确"。同名多份时精确性无从确定，但那不是失效的
	// 证据——把歧义当失效会把"改过名但还在"的文件误报成尸体。
	return bases[filepath.Base(p)] >= 1
}

// refText 还原一条引用的展示形态（带行号时保留行号，方便人去搜）。
func refText(r CodeRef) string {
	if r.Line > 0 {
		return fmt.Sprintf("%s:%d", r.Path, r.Line)
	}
	return r.Path
}

// listRepoFiles 列工作区里的全部文件（相对路径）。
//
// **用文件系统遍历，不用 git ls-files。** 理由：我们判的是"这条记忆引用的文件在不在
// 磁盘上"，而 git 看不见两类文件——`.git/info/exclude` 里排除的（实测 go-code 有
// 800+ 个长期未提交的 *_test.go 就在这里）和刚写还没 add 的。用 git 列会让这些
// 记忆全被判成"锚点失效"。
//
// 跳过 `.git` / `node_modules` / `dist` / `build`：它们是构建产物与版本库内部，
// 不属于"这个仓库的代码"。worktree 目录**不跳**——很多记忆是在 worktree 里写的。
func listRepoFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, de os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if de.IsDir() {
			switch de.Name() {
			case ".git", "node_modules", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if rel, rerr := filepath.Rel(root, p); rerr == nil {
			files = append(files, rel)
		}
		return nil
	})
	return files, err
}
