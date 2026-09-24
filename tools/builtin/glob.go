package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/seven7628/hai-harness/tools"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
)

const defaultGlobMaxResults = 200

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
				"Returns a list of matching relative paths (in traversal order). Use grep to search file contents.",
			Params_: tools.Obj(map[string]any{
				"pattern":     tools.Str("glob pattern (required; ** matches any depth, * matches any characters within one level, ? one char)"),
				"path":        tools.Str("Start directory (relative to workspace root; default '.')"),
				"max_results": tools.Map{"type": "integer", "minimum": 1, "default": defaultGlobMaxResults, "description": "Result cap (default 200; must be >= 1; narrow the pattern when exceeded)"},
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
}

func (t *globTool) ValidParams(_ context.Context, _, arguments string) error {
	var a globArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return invalidJSON("glob", err.Error())
	}
	if a.Pattern == "" {
		return requiredArgument("glob", "pattern")
	}
	return nil
}

func (t *globTool) Call(_ context.Context, _, arguments string) (string, error) {
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
	start := a.Path
	if start == "" {
		start = "."
	}
	root, err := t.resolve(start)
	if err != nil {
		return "", err
	}

	var files []string
	total := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 不可读目录/文件：跳过，不中断
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
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
		total++
		if total > max {
			return fs.SkipAll // 已收集满：立即停止
		}
		files = append(files, rel)
		return nil
	})
	if total > max {
		total = max
	}

	var b strings.Builder
	if total >= max && total > 0 {
		fmt.Fprintf(&b, "[glob: %s, showing %d file(s); narrow the pattern]\n", a.Pattern, total)
	} else {
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
