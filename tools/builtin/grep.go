package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/tools"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultGrepMaxResults = 200 // 单次搜索的匹配条数上限（超限提示缩小范围）
	maxGrepLineLength     = 200 // 单行内容输出截断（防长行撑爆结果）
)

type grepTool struct {
	tools.BaseTool
	*FileTools
	maxResults int
}

func newGrepTool(f *FileTools) *grepTool {
	return &grepTool{
		BaseTool: tools.BaseTool{
			Name_: "grep",
			Description_: "Recursively search the content of text files in the workspace (Go regular expression), outputting path:line:content matches (with line numbers, ready to read context via read_file). " +
				"Optional context shows N lines before/after each match (context lines marked path-LINE-content, match groups separated by --); include filters files by glob (e.g. *.go searches only Go files). " +
				"Skips .git and binary files by default; when more than 200 matches, prompts narrowing. " +
				"Use glob to find files by name, this tool to search by content.",
			Params_: tools.Obj(map[string]any{
				"pattern": tools.Str("Regular expression (Go regexp syntax, required)"),
				"path":    tools.Str("Start directory/file (relative to workspace root; default '.')"),
				"context": tools.Map{"type": "integer", "minimum": 0, "default": 0, "description": "Context lines to show before/after each match (default 0; must be >= 0)"},
				"include": tools.Str("File filter glob (e.g. '*.go', '**/*_test.go'; optional, only searches matching files)"),
			}, "pattern"),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		FileTools:  f,
		maxResults: defaultGrepMaxResults,
	}
}

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Context int    `json:"context"`
	Include string `json:"include"`
}

func (t *grepTool) ValidParams(_ context.Context, _, arguments string) error {
	var a grepArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return invalidJSON("grep", err.Error())
	}
	if a.Pattern == "" {
		return requiredArgument("grep", "pattern")
	}
	if a.Context < 0 {
		return recoveryError("invalid_context", "correct_arguments", fmt.Sprintf("context=%d", a.Context), "Set context to an integer >= 0, then retry grep.")
	}
	if a.Include != "" {
		if _, err := compileGlob(a.Include); err != nil {
			return fmt.Errorf("grep: invalid include glob %q: %w", a.Include, err)
		}
	}
	return nil
}

func (t *grepTool) Call(_ context.Context, _, arguments string) (string, error) {
	var a grepArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", recoveryError("invalid_pattern", "correct_arguments", fmt.Sprintf("invalid regular expression: %v", err), "Fix the Go regular expression, or use a literal escaped pattern. Do not repeat the same pattern.")
	}
	start := a.Path
	if start == "" {
		start = "."
	}
	root, err := t.resolve(start)
	if err != nil {
		return "", err
	}

	var results []grepFileResult
	total := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个文件不可读（权限等）：跳过，不中断搜索
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir // 仓库元数据不参与搜索
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(t.root, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		// include 过滤：glob 匹配文件名（*.go）或完整相对路径（**/test/*.go）任一命中即可
		if a.Include != "" {
			baseMatch, _ := matchGlob(a.Include, filepath.Base(rel))
			pathMatch, _ := matchGlob(a.Include, rel)
			if !baseMatch && !pathMatch {
				return nil
			}
		}
		fr, truncated, err := grepFile(re, path, rel, a.Context, &total, t.maxResults)
		if err != nil {
			return nil // 二进制/读失败：跳过该文件
		}
		if len(fr.matches) > 0 {
			results = append(results, fr)
		}
		if truncated {
			return fs.SkipAll // 命中预算已满：立即停止遍历
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return "", err
	}

	var b strings.Builder
	if total > t.maxResults {
		fmt.Fprintf(&b, "[grep: more than %d matches, showing first %d; narrow the pattern]\n", t.maxResults, t.maxResults)
	} else {
		fmt.Fprintf(&b, "[grep: %d match(es) in %d file(s)]\n", total, len(results))
	}
	for _, fr := range results {
		renderGrepResult(fr, a.Context, &b)
	}
	return b.String(), nil
}

// grepFileResult 单个文件的一次搜索：命中行号（1 基）+ 全部行内容（已按输出长度截断）。
type grepFileResult struct {
	rel     string
	lines   []string // 1 索引：lines[ln-1] = 第 ln 行
	matches []int    // 命中的行号（升序）
}

// grepFile 扫描单个文本文件：收集命中行号与行内容，返回是否提前截断（命中预算耗尽）。
// 二进制文件（头 8KB 含 \x00）返回 error（调用方跳过）。
func grepFile(re *regexp.Regexp, abs, rel string, ctx int, total *int, max int) (grepFileResult, bool, error) {
	var fr grepFileResult
	fr.rel = rel
	f, err := os.Open(abs)
	if err != nil {
		return fr, false, err
	}
	defer f.Close()

	head := make([]byte, 8192)
	n, _ := f.Read(head)
	if bytesContainsZero(head[:n]) {
		return fr, false, errors.New("binary file") // 跳过
	}
	if _, err := f.Seek(0, 0); err != nil {
		return fr, false, err
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	ln := 0
	for sc.Scan() {
		ln++
		content := sc.Text()
		if len(content) > maxGrepLineLength {
			content = content[:maxGrepLineLength] + "..."
		}
		fr.lines = append(fr.lines, content)
		if !re.Match(sc.Bytes()) {
			continue
		}
		*total++ // 真实匹配计数（无上限，供「more than N」提示）
		if *total > max {
			return fr, true, nil // 命中预算已满：本文件截断，调用方停止遍历
		}
		fr.matches = append(fr.matches, ln)
	}
	return fr, false, sc.Err()
}

// renderGrepResult 渲染一个文件的命中结果：命中行 path:line:content，上下文行 path-LINE-content，
// 命中组（含上下文的连续区间）之间用 -- 分隔（ripgrep 惯例）。
func renderGrepResult(fr grepFileResult, ctx int, b *strings.Builder) {
	if len(fr.matches) == 0 {
		return
	}
	// 命中行合并为上下文区间组（相邻/重叠合并）
	type group struct{ start, end int }
	var groups []group
	for _, m := range fr.matches {
		s, e := m-ctx, m+ctx
		if s < 1 {
			s = 1
		}
		if e > len(fr.lines) {
			e = len(fr.lines)
		}
		if n := len(groups); n > 0 && s <= groups[n-1].end+1 {
			if e > groups[n-1].end {
				groups[n-1].end = e
			}
		} else {
			groups = append(groups, group{s, e})
		}
	}
	matchSet := make(map[int]bool, len(fr.matches))
	for _, m := range fr.matches {
		matchSet[m] = true
	}
	for gi, g := range groups {
		if gi > 0 {
			b.WriteString("--\n")
		}
		for ln := g.start; ln <= g.end; ln++ {
			if matchSet[ln] {
				fmt.Fprintf(b, "%s:%d:%s\n", fr.rel, ln, fr.lines[ln-1])
			} else {
				fmt.Fprintf(b, "%s-%d-%s\n", fr.rel, ln, fr.lines[ln-1])
			}
		}
	}
}

func bytesContainsZero(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return false
}

var _ tools.Tool = (*grepTool)(nil)
