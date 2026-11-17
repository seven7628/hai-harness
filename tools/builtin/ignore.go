package builtin

// 搜索工具（grep / glob）的忽略规则。
//
// 为什么需要（2026-11-17 实测）：go-code 仓 116,431 个文件里，72,829 个在
// node_modules（2.06 GB）、7,929 个在 .go-code/sessions（2.65 GB，是 agent 自己
// 的会话 transcript）、5,080 个在 .gocache（1.01 GB）。全仓一次 grep 实测 4m4s，
// 而只搜 6 个源码目录只要 341ms —— **715×**。
//
// 对标 ripgrep 的 ignore crate（crates/ignore/src/），取三条要害：
//  1. **忽略判定发生在打开文件之前** ⇒ 被忽略的目录整棵子树零 I/O
//     （fs.SkipDir ⇒ 连 read_dir 都不做）。这里靠 WalkDir 的回调返回 SkipDir 实现。
//  2. **.gitignore 逐目录增量读**（不是预读全树）—— Cline 用 globby 的
//     gitignore:true 预读全树所有 .gitignore，把扩展宿主 V8 撑到 OOM 崩溃。
//  3. 语义取 gitignore 的常用子集：# 注释 / ! 反选 / 尾 / 只匹配目录 /
//     前导 / 锚定到所在目录 / 无 / 则匹配任意层级的 basename / ** / **后匹配者胜**。
//
// 与 ripgrep 的一致性：**命令行显式给出的路径不受忽略规则约束**
// （rg --help: "File paths specified on the command line override glob and ignore rules"）。

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// builtinIgnoreDirs 内置黑名单：依赖与构建产物目录。
//
// 为什么要有内置黑名单而不只靠 .gitignore：不是每个仓库都把依赖目录写进
// .gitignore（新建仓库的默认 .gitignore 常常只有编辑器配置），而这几个目录
// 恰好是文件数与体积的大头。
//
// 刻意**不含** vendor：vendor 里是被 vendored 的源码（可能是本项目在维护的
// 第三方 fork），grep 它是有意义的；要不要跳由仓库自己的 .gitignore 决定。
var builtinIgnoreDirs = map[string]bool{
	"node_modules":  true,
	".git":          true,
	".svn":          true,
	".hg":           true,
	"dist":          true,
	"build":         true,
	"out":           true,
	"target":        true,
	".next":         true,
	".nuxt":         true,
	".cache":        true,
	".venv":         true,
	"venv":          true,
	"__pycache__":   true,
	".mypy_cache":   true,
	".pytest_cache": true,
	".ruff_cache":   true,
	".tox":          true,
	".gradle":       true,
}

// ignoreRule 一条忽略规则（已按 gitignore 语义归一化）。
type ignoreRule struct {
	segs    []string // 模式按 / 切段；"**" 表示任意层
	negate  bool     // ! 前缀 = 反选（重新包含）
	dirOnly bool     // 尾 / = 只匹配目录
	baseRel string   // 规则来源目录（相对工作区根）；"" = 根
}

// scopedRule 带作用域的规则（baseRel 决定相对路径怎么算）。
type scopedRule = ignoreRule

// ignoreMatcher 忽略规则匹配器。
//
// 结构刻意做成「按目录组织 + 缓存展开」：WalkDir 是深度优先，父目录一定先于
// 子目录被访问 ⇒ 进目录时就能把父链规则展开好并缓存 ⇒ 每个文件只需一次
// map 查找，不用为每个文件重走一遍祖先链。
type ignoreMatcher struct {
	root    string // 工作区根（绝对路径）
	enabled bool   // 总开关；false = 什么都不忽略（include_ignored 模式）
	builtin bool   // 是否启用内置黑名单

	own    map[string][]ignoreRule // 目录（绝对路径）→ 该目录 .gitignore 的规则
	cache  map[string][]scopedRule // 目录（绝对路径）→ 含父链的生效规则（父在前）
	loaded map[string]bool         // 已尝试读取过 .gitignore 的目录
}

// newIgnoreMatcher 造匹配器。enabled=false 时不忽略任何东西（include_ignored）。
func newIgnoreMatcher(root string, enabled bool) *ignoreMatcher {
	return &ignoreMatcher{
		root:    root,
		enabled: enabled,
		builtin: enabled,
		own:     map[string][]ignoreRule{},
		cache:   map[string][]scopedRule{},
		loaded:  map[string]bool{},
	}
}

// ignoredDir 判断目录是否应被整棵剪掉。
//
// 契约：这是**逐路径**谓词，不查祖先。被忽略目录里的子路径同样返回 false ——
// 那没关系：遍历时父目录已被 SkipDir 剪掉，子路径根本不会被访问。
// （唯一例外是遍历根：显式 path 覆盖忽略规则，根不被剪。）
func (m *ignoreMatcher) ignoredDir(abs string) bool {
	if !m.enabled {
		return false
	}
	if m.builtin && builtinIgnoreDirs[filepath.Base(abs)] {
		return true
	}
	return m.match(abs, true)
}

// ignoredFile 判断文件是否应被跳过（同 ignoredDir 的逐路径契约）。
func (m *ignoreMatcher) ignoredFile(abs string) bool {
	if !m.enabled {
		return false
	}
	return m.match(abs, false)
}

// match 走一遍规则链：后匹配者胜（gitignore 语义）。
func (m *ignoreMatcher) match(abs string, isDir bool) bool {
	rel, ok := m.relToRoot(abs)
	if !ok {
		return false // 不在工作区根下（理论上不会发生）
	}
	rules := m.rulesFor(filepath.Dir(abs))
	if len(rules) == 0 {
		return false
	}
	ignored := false
	// 倒序遍历 → **最后命中的规则说了算**（gitignore 语义）⇒ 命中即停。
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		sub, ok := underBase(rel, r.baseRel)
		if !ok {
			continue
		}
		if matchIgnoreSegs(r.segs, strings.Split(sub, "/")) {
			return !r.negate
		}
	}
	return ignored
}

// relToRoot 把绝对路径换成相对工作区根的斜杠路径。
func (m *ignoreMatcher) relToRoot(abs string) (string, bool) {
	rel, err := filepath.Rel(m.root, abs)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return "", true
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// rulesFor 返回某目录生效的完整规则链（父目录在前、子目录在后）。
func (m *ignoreMatcher) rulesFor(dir string) []scopedRule {
	if rs, ok := m.cache[dir]; ok {
		return rs
	}
	var rs []scopedRule
	// 父链：从目录向上到工作区根（含）。根之上不取 —— 与 git「仓库根之上的
	// .gitignore 不生效」一致（ripgrep 的 require_git=true 同款）。
	if dir == m.root {
		rs = nil
	} else if parent := filepath.Dir(dir); parent != dir && strings.HasPrefix(dir, m.root) {
		rs = append(rs, m.rulesFor(parent)...)
	}
	rs = append(rs, m.ownRules(dir)...)
	m.cache[dir] = rs
	return rs
}

// ownRules 读某目录的 .gitignore（只读一次）。
func (m *ignoreMatcher) ownRules(dir string) []ignoreRule {
	if m.loaded[dir] {
		return m.own[dir]
	}
	m.loaded[dir] = true
	baseRel, ok := m.relToRoot(dir)
	if !ok {
		return nil
	}
	rules := readIgnoreFile(filepath.Join(dir, ".gitignore"), baseRel)
	m.own[dir] = rules
	return rules
}

// readIgnoreFile 读一个 .gitignore 文件并解析成规则。
func readIgnoreFile(path, baseRel string) []ignoreRule {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rules []ignoreRule
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	for sc.Scan() {
		if r, ok := parseIgnorePattern(sc.Text(), baseRel); ok {
			rules = append(rules, r)
		}
	}
	return rules
}

// parseIgnorePattern 按 gitignore 语义解析一行。
func parseIgnorePattern(line, baseRel string) (ignoreRule, bool) {
	s := strings.TrimRight(line, " \t")
	if s == "" || strings.HasPrefix(s, "#") {
		return ignoreRule{}, false
	}
	negate := false
	if strings.HasPrefix(s, "!") {
		negate = true
		s = s[1:]
	} else if strings.HasPrefix(s, "\\!") || strings.HasPrefix(s, "\\#") {
		s = s[1:] // 转义的 ! / # 是字面量
	}
	dirOnly := false
	if strings.HasSuffix(s, "/") {
		dirOnly = true
		s = strings.TrimSuffix(s, "/")
	}
	// 前导 / = 锚定到所在目录（gitignore：模式以 / 开头或中间含 / 时，
	// 相对于该 .gitignore 所在目录）。锚定后**不能**再补 '**/' 前缀。
	anchored := strings.HasPrefix(s, "/")
	s = strings.TrimPrefix(s, "/")
	if s == "" {
		return ignoreRule{}, false
	}
	// 无 '/' 且非锚定的模式匹配任意层级的 basename（gitignore 语义）⇒ 补 '**/' 前缀。
	if !anchored && !strings.Contains(s, "/") {
		s = "**/" + s
	}
	return ignoreRule{
		segs:    strings.Split(s, "/"),
		negate:  negate,
		dirOnly: dirOnly,
		baseRel: baseRel,
	}, true
}

// underBase 判断 rel 是否在 baseRel 之下，是则返回相对 baseRel 的路径。
func underBase(rel, baseRel string) (string, bool) {
	if baseRel == "" {
		return rel, true
	}
	if rel == baseRel {
		return "", true
	}
	if strings.HasPrefix(rel, baseRel+"/") {
		return rel[len(baseRel)+1:], true
	}
	return "", false
}

// matchIgnoreSegs 模式段 × 路径段的 DP 匹配（无回溯 ⇒ 无指数退化）。
//
// 不用 matchSegments（那是 glob 用的、按 '**' 试所有分割点的朴素回溯）：
// .gitignore 的模式来自仓库内容，**必须**是线性复杂度，否则一个手写的
// `**/**/**/x` 就能把每次搜索拖成指数级。
//
// dp[i][j] = 模式前 i 段能否匹配路径前 j 段。
func matchIgnoreSegs(pat, segs []string) bool {
	// DP 的列数要覆盖路径段数（** 的递推要访问 dp[i+1][j-1]，j 到 len(segs)），
	// 行数覆盖模式段数 ⇒ 尺寸取两者的较大者 +1。
	n := len(pat) + 1
	if len(segs)+1 > n {
		n = len(segs) + 1
	}
	const maxStack = 64
	var bufA, bufB [maxStack]bool
	var a, b []bool
	if n <= maxStack {
		a, b = bufA[:n], bufB[:n]
	} else {
		a, b = make([]bool, n), make([]bool, n)
	}
	// a[j] = dp[i][j]
	a[0] = true
	for i := 0; i < len(pat); i++ {
		for j := range b {
			b[j] = false
		}
		if pat[i] == "**" {
			// ** 匹配零段：dp[i+1][j] = dp[i][j]
			for j := 0; j <= len(segs); j++ {
				b[j] = a[j]
			}
			// ** 匹配多段：dp[i+1][j] |= dp[i+1][j-1]
			for j := 1; j <= len(segs); j++ {
				if b[j-1] {
					b[j] = true
				}
			}
		} else {
			for j := 1; j <= len(segs); j++ {
				if a[j-1] {
					if ok, err := path.Match(pat[i], segs[j-1]); err == nil && ok {
						b[j] = true
					}
				}
			}
		}
		a, b = b, a
	}
	return a[len(segs)]
}
