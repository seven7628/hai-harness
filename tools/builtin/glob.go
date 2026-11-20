package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/tools"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const defaultGlobMaxResults = 200

// maxGlobMaxResults max_results 的**硬上限**（模型传再大也按这个算）。
//
// 为什么（第 5 轮爬山第 6 步）：max_results 是模型可控的，而 schema 只声明了
// minimum 没声明 maximum ⇒ 一次调用就能把整个工作区的文件名收进内存（内存放大
// 的口子）。2000 条 ≈ 10 页默认值，够模型用；想看更多就翻页（offset）。
const maxGlobMaxResults = 2000

// globTool 按文件名/路径模式（glob）查找工作区内的文件。
// 对标 Claude Code Glob / Codex glob：模型「按名找文件」的高频原语，
// 与 grep（按内容搜）互补 —— 定位用 glob，内容匹配用 grep。
type globTool struct {
	tools.BaseTool
	*FileTools
	maxResults int
}

func newGlobTool(f *FileTools) *globTool {
	return &globTool{
		BaseTool: tools.BaseTool{
			Name_: "glob",
			Description_: "Find files in the workspace by name/path pattern (glob) — the preferred tool for locating files by name (far more efficient than recursive directory traversal). " +
				"Supports ** for recursive matching across levels: **/*.test.go finds all tests, src/**/*.go finds all Go files under src, *.md finds only md files at the root (use **/*.md for recursive). " +
				"Skips .git, .gitignore'd paths and dependency/build directories (node_modules etc.) by default — pass no_ignore=true to list them anyway. " +
				"Returns a list of matching relative paths (in traversal order). " +
				"At most max_results (default 200) files are returned per call: when there are more, the result header says so and tells you the offset to pass back for the next page (or to narrow the pattern). " +
				"Use grep to search file contents.",
			Params_: tools.Obj(map[string]any{
				"pattern":     tools.Str("glob pattern (required; ** matches any depth, * matches any characters within one level, ? one char)"),
				"path":        tools.Str("Start directory (relative to workspace root; default '.')"),
				"max_results": tools.Map{"type": "integer", "minimum": 1, "default": defaultGlobMaxResults, "description": "Result cap **per page** (default 200; must be >= 1; narrow the pattern when exceeded)"},
				"offset":      tools.Map{"type": "integer", "minimum": 0, "default": 0, "description": "Skip the first N matching files and return the next page (default 0 = from the start). Use it when the result says there are more matches — pass back the offset it reports."},
				"no_ignore":   tools.Map{"type": "boolean", "default": false, "description": "Also list ignored paths (node_modules/, .gitignore'd paths, build output). Default false — ignored directories are skipped entirely, which is what makes a repo-wide search fast."},
			}, "pattern"),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		FileTools:  f,
		maxResults: defaultGlobMaxResults,
	}
}

type globArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
	Offset     int    `json:"offset"`
	NoIgnore   bool   `json:"no_ignore"`
}

func (t *globTool) ValidParams(_ context.Context, _, arguments string) error {
	var a globArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return invalidJSON("glob", err.Error())
	}
	if a.Pattern == "" {
		return requiredArgument("glob", "pattern")
	}
	if a.Offset < 0 {
		return recoveryError("invalid_offset", "correct_arguments", fmt.Sprintf("offset=%d", a.Offset), "Set offset to an integer >= 0, then retry glob.")
	}
	return nil
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明 glob 的工具级时限。
// 语义与 grep 一致（见 search_timeout.go）。
func (t *globTool) ToolTimeout() time.Duration { return searchToolTimeout }

func (t *globTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a globArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	if _, err := compileGlob(a.Pattern); err != nil {
		return "", fmt.Errorf("glob: invalid pattern %q: %w", a.Pattern, err)
	}
	max := t.maxResults
	if a.MaxResults > 0 {
		max = a.MaxResults
	}
	// 硬上限（第 5 轮爬山第 6 步）：max_results 是**模型可控**的，而 schema 只写了
	// minimum 没写 maximum ⇒ 模型传 10000000 时，工具会把整个工作区的文件名收进
	// 内存（一次调用就是一次内存放大）。grep 用的是固定 200（不可调）⇒ 这里给一个
	// 够用但有界的上限：想看更多就翻页（offset），别一次拉一大把。
	if max > maxGlobMaxResults {
		max = maxGlobMaxResults
	}
	start := a.Path
	if start == "" {
		start = "."
	}
	root, err := t.resolveAt(ctx, start)
	if err != nil {
		return "", err
	}

	var files []string
	total := 0 // 已见命中数（含被 offset 跳过的）
	offset := a.Offset
	// 忽略规则：内置黑名单 + 逐目录增量读 .gitignore。被忽略的目录整棵子树
	// 零 I/O（SkipDir ⇒ 连 read_dir 都不做）。显式给出的 path 不受规则约束。
	ign := newIgnoreMatcher(t.root, !a.NoIgnore)
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr // 被取消/超时：立刻停止遍历
		}
		if err != nil {
			return nil // 不可读目录/文件：跳过，不中断
		}
		if d.IsDir() {
			if p != root && ign.ignoredDir(p) {
				return fs.SkipDir
			}
			return nil
		}
		if p != root && ign.ignoredFile(p) {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(t.root, p)
		if err != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		ok, err := matchGlob(a.Pattern, rel)
		if err != nil || !ok {
			return nil
		}
		// 分页语义（与 grep 一致）：全局第 k 个命中落在 [offset+1, offset+max] 收进本页；
		// k ≤ offset 跳过但继续扫；k > offset+max 停止遍历（后面还有）。
		total++
		if total > offset+max {
			return fs.SkipAll // 本页已满且还有更多：立即停止
		}
		if total > offset {
			files = append(files, rel)
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		if ctx.Err() != nil {
			return "", toolCancelledError(ctx, "glob")
		}
		return "", walkErr
	}

	taken := len(files)
	truncated := total > offset+max
	var b strings.Builder
	switch {
	case taken == 0 && offset > 0:
		// offset 越过总数：明确告诉模型「没有更多了」，别再空转
		fmt.Fprintf(&b, "[glob: %s, no files at offset %d (%d file(s) in total); lower the offset or narrow the pattern]\n",
			a.Pattern, offset, total)
	case truncated && offset > 0:
		fmt.Fprintf(&b, "[glob: %s, showing files %d-%d (offset %d); use offset=%d for the next page, or narrow the pattern]\n",
			a.Pattern, offset+1, offset+taken, offset, offset+taken)
	case truncated:
		fmt.Fprintf(&b, "[glob: %s, showing first %d file(s); use offset=%d for the next page, or narrow the pattern]\n",
			a.Pattern, max, max)
	case offset > 0:
		fmt.Fprintf(&b, "[glob: %s, files %d-%d (offset %d)]\n", a.Pattern, offset+1, offset+taken, offset)
	default:
		fmt.Fprintf(&b, "[glob: %s, %d file(s)]\n", a.Pattern, total)
	}
	for _, f := range files {
		b.WriteString(f)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// compileGlob 预校验 glob 模式（每个非 ** 段须为合法 path.Match 模式）。
func compileGlob(pattern string) (string, error) {
	pattern = filepath.ToSlash(pattern)
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return "", err
		}
	}
	return pattern, nil
}

// matchGlob 判断 name 是否匹配 glob 模式（** 支持任意层级递归）。
// 语义对齐 doublestar 常用子集：
//   - ** 匹配零或多个路径段（/**/ 也匹配无中间层）；
//   - * ? [seq] 在单个段内按 path.Match 语义（* 不跨 /）；
//   - 模式默认相对工作区根匹配。
func matchGlob(pattern, name string) (bool, error) {
	pattern = filepath.ToSlash(pattern)
	name = filepath.ToSlash(name)
	if pattern == "" {
		return name == "", nil
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, segs []string) (bool, error) {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// ** 匹配零或多个段：尝试所有分割点，任一成功即命中
			for i := 0; i <= len(segs); i++ {
				m, err := matchSegments(pat[1:], segs[i:])
				if err != nil {
					return false, err
				}
				if m {
					return true, nil
				}
			}
			return false, nil
		}
		if len(segs) == 0 {
			return false, nil
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0, nil
}

var _ tools.Tool = (*globTool)(nil)
var _ tools.ToolTimeoutProvider = (*globTool)(nil)
