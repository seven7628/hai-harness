package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/tools"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultGrepMaxResults = 200 // 单次搜索的匹配条数上限（超限提示缩小范围）
	maxGrepLineLength     = 200 // 单行内容输出截断（防长行撑爆结果）
	// maxGrepLineBytes 单行的字节上限（超出的行被截断后继续扫）。
	// 与改造前 bufio.Scanner 的 maxTokenSize 同量级 —— 唯一的行为差异是
	// 改造前超长行会让**整个文件**被丢弃（连已找到的命中一起丢），
	// 改造后只是那一行被截断（见 grepScanner.nextLine）。
	maxGrepLineBytes = 4 << 20
	// grepReadBufferSize 行读取器的缓冲大小（跨文件复用，见 grepScanner）。
	grepReadBufferSize = 64 << 10
	// grepHeadProbeSize 二进制探测读取的头部字节数（跨文件复用）。
	grepHeadProbeSize = 8192
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
				"Skips .git, .gitignore'd paths, dependency/build directories (node_modules etc.) and binary files by default — that is what keeps a repo-wide search fast; pass no_ignore=true to search them anyway. " +
				"When more than 200 matches, prompts narrowing. " +
				"Use glob to find files by name, this tool to search by content.",
			Params_: tools.Obj(map[string]any{
				"pattern":   tools.Str("Regular expression (Go regexp syntax, required)"),
				"path":      tools.Str("Start directory/file (relative to workspace root; default '.')"),
				"context":   tools.Map{"type": "integer", "minimum": 0, "default": 0, "description": "Context lines to show before/after each match (default 0; must be >= 0)"},
				"include":   tools.Str("File filter glob (e.g. '*.go', '**/*_test.go'; optional, only searches matching files)"),
				"no_ignore": tools.Map{"type": "boolean", "default": false, "description": "Also search ignored paths (node_modules/, .gitignore'd paths, build output). Default false — ignored directories are skipped entirely, which is what makes a repo-wide search fast."},
			}, "pattern"),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		FileTools:  f,
		maxResults: defaultGrepMaxResults,
	}
}

type grepArgs struct {
	Pattern  string `json:"pattern"`
	Path     string `json:"path"`
	Context  int    `json:"context"`
	Include  string `json:"include"`
	NoIgnore bool   `json:"no_ignore"`
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
	// 行读取器：整个搜索共用一个（缓冲跨文件复用 ⇒ 每文件不再分配 64 KB）。
	sc := newGrepScanner()
	// 忽略规则：内置黑名单 + 逐目录增量读 .gitignore。被忽略的目录整棵子树
	// 零 I/O（SkipDir ⇒ 连 read_dir 都不做）。显式给出的 path 不受规则约束
	// （与 rg 一致：命令行路径覆盖 ignore 规则）。
	ign := newIgnoreMatcher(t.root, !a.NoIgnore)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 单个文件不可读（权限等）：跳过，不中断搜索
		}
		if d.IsDir() {
			if path != root && ign.ignoredDir(path) {
				return fs.SkipDir // 整棵子树剪掉：不遍历、不打开
			}
			return nil
		}
		if path != root && ign.ignoredFile(path) {
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
		fr, truncated, err := grepFile(re, sc, path, rel, a.Context, &total, t.maxResults)
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

// grepFileResult 单个文件的一次搜索：命中行号（1 基）+ **需要输出**的行内容。
//
// lines 是稀疏的（map[行号]内容）：只存命中行及其上下文档位。
// 改造前这里是稠密的 []string（整个文件的每一行都留着）—— 一个零命中的
// 50 万行文件会白占 73.6 MB / 50 万次分配（2026-11-17 实测）。
type grepFileResult struct {
	rel     string
	total   int            // 文件总行数
	lines   map[int]string // 行号 → 该行内容（只存需要输出的行）
	matches []int          // 命中的行号（升序）
}

// grepGroup 一段连续输出的行区间（命中行 ± context 合并后）。
type grepGroup struct{ start, end int }

// grepScanner 一次 grep 调用内的行读取器。
//
// 为什么用 bufio.Reader 而不是 bufio.Scanner：
//
//   - Scanner **没有 Reset**（Go 1.25 src/bufio/scan.go 只有
//     Err/Bytes/Text/Scan/Buffer/Split）⇒ 每个文件都得 new 一个 Scanner，
//     连带 new 一个 64 KB buffer。实测真实源码上 **97 KB/文件**的分配
//     （源文件平均才 14 KB），全仓 grep 分配 11 GB。
//   - Reader **有 Reset(r) 且复用同一个 buffer**
//     （src/bufio/bufio.go: `b.reset(b.buf, r)`）⇒ 缓冲跨文件复用。
//
// 为什么用 ReadSlice('\n') 而不是 Scan：
// ReadSlice 返回**内部缓冲的切片**（零拷贝）⇒ 匹配阶段完全不分配；
// Scan 每次都要拷一份字符串（sc.Text()）。
//
// 代价：ReadSlice 返回的切片**在下一次调用后失效** ⇒ 需要留存时必须拷贝。
type grepScanner struct {
	br      *bufio.Reader
	head    []byte // 二进制探测用的头部缓冲（跨文件复用）
	long    []byte // 超长行的累积缓冲（按上限一次性分配，见 appendLong）
	longLen int    // long 已用字节数
	drop    bool   // 正在丢弃一个超长行的剩余部分
}

func newGrepScanner() *grepScanner {
	return &grepScanner{
		br:   bufio.NewReaderSize(nil, grepReadBufferSize),
		head: make([]byte, grepHeadProbeSize),
	}
}

// reset 切到下一个文件（复用同一个缓冲）。
func (s *grepScanner) reset(r io.Reader) {
	s.br.Reset(r)
	s.longLen = 0
	s.drop = false
}

// nextLine 读下一行（去掉行尾换行）。
//
// 返回的切片指向内部缓冲，**下一次调用后失效** —— 调用方要留存必须拷贝。
// ok=false 表示 EOF。
func (s *grepScanner) nextLine() ([]byte, bool) {
	for {
		chunk, err := s.br.ReadSlice('\n')
		if s.drop {
			// 正在丢弃一个超长行的剩余部分：找到换行才算这一行结束。
			if bytes.IndexByte(chunk, '\n') >= 0 {
				s.drop = false
				s.longLen = 0
				if err == io.EOF {
					return nil, false
				}
				continue // 下一行
			}
			if err == io.EOF {
				s.drop = false
				s.longLen = 0
				return nil, false
			}
			continue
		}
		if err == bufio.ErrBufferFull {
			// 行超过缓冲：累积（上限 maxGrepLineBytes）。
			s.appendLong(chunk)
			if s.longLen >= maxGrepLineBytes {
				s.drop = true // 剩余部分丢弃
			}
			continue
		}
		if s.longLen > 0 {
			// 超长行：累积到 s.long。此处一定是一行（可能内容为空）。
			s.appendLong(chunk)
			content := s.long[:s.longLen]
			s.longLen = 0
			return trimEOL(content), true
		}
		if err == io.EOF {
			if len(chunk) == 0 {
				return nil, false // 真的读完了，且没有残余内容
			}
			return trimEOL(chunk), true // 最后一行（可能没有换行符结尾）
		}
		if len(chunk) == 0 {
			continue // 读到 0 字节且无错 → 再试一次
		}
		// 注意：**空行也是一行**（bufio.ScanLines 同样返回空行）——
		// 这里不能因为 trimEOL 后为空就跳过。
		return trimEOL(chunk), true
	}
}

// appendLong 把 chunk 追加到超长行缓冲。
//
// 缓冲**按上限一次性分配**（首次遇到超长行时才分配，之后一直复用），
// 之后只用 copy 填充 —— 不用 append：append 扩容会把已累积的内容**整体拷贝**
// 一遍（4 MB 的单行 ⇒ 十几次扩容、累计拷贝 ~8 MB，且每次都是一次新分配）。
// 这里的长度由 longLen 单独跟踪，缓冲本身永不增长。
func (s *grepScanner) appendLong(chunk []byte) {
	if s.long == nil {
		s.long = make([]byte, maxGrepLineBytes)
	}
	n := copy(s.long[s.longLen:], chunk)
	s.longLen += n
}

// trimEOL 去掉行尾换行符。
//
// 连行尾 \r 一起去掉 —— 与 bufio.ScanLines 的 dropCR 语义一致（改造前用
// Scanner，CRLF 文件的行内容不含 \r；只去 \n 会让 CRLF 文件的匹配结果与
// 输出都多出一个 \r）。
func trimEOL(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
	}
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	return b
}

// grepFile 扫描单个文本文件：收集命中行号与需要输出的行内容，返回是否提前截断
// （命中预算耗尽）。二进制文件（头 8KB 含 \x00）返回 error（调用方跳过）。
//
// 两遍扫描：
//
//	① 流式扫一遍，**只记命中行号**（零留存）；
//	② 仅当有命中时才再读一遍，把「命中行 ± context」的并集取出来。
//
// 零命中文件因此不留任何行内容（改造前会留整个文件的所有行）。
func grepFile(re *regexp.Regexp, sc *grepScanner, abs, rel string, ctx int, total *int, max int) (grepFileResult, bool, error) {
	var fr grepFileResult
	fr.rel = rel
	f, err := os.Open(abs)
	if err != nil {
		return fr, false, err
	}
	defer f.Close()

	// 二进制探测：读头 8KB。用 ReadAt（pread）—— **不移动文件偏移** ⇒
	// 后面可以直接从头扫，省掉改造前那次 Seek(0,0)（每文件少 2 次系统调用），
	// 也不需要 MultiReader 把头拼回去（少两次分配）。
	// 头缓冲跨文件复用（sc.head）⇒ 每文件不再分配 8 KB。
	n, _ := f.ReadAt(sc.head, 0)
	if bytesContainsZero(sc.head[:n]) {
		return fr, false, errors.New("binary file") // 跳过
	}
	sc.reset(f)

	// ① 第一遍：只记命中行号。
	var matches []int
	ln := 0
	truncated := false
	for {
		line, ok := sc.nextLine()
		if !ok {
			break
		}
		ln++
		if !re.Match(line) {
			continue
		}
		*total++ // 真实匹配计数（无上限，供「more than N」提示）
		if *total > max {
			truncated = true // 命中预算已满：本文件截断，调用方停止遍历
			break
		}
		matches = append(matches, ln)
	}
	fr.matches = matches
	fr.total = ln
	if len(matches) == 0 {
		return fr, truncated, nil // 零命中：不留任何内容
	}

	// ② 第二遍：只取需要输出的行（命中行 ± context 的并集）。
	//    回到文件头重来一遍（页缓存命中，代价≈一次 seek + 一次读）。
	if _, err := f.Seek(0, 0); err != nil {
		return fr, false, err
	}
	sc.reset(f)
	groups := computeGrepGroups(matches, ctx, ln)
	fr.lines = make(map[int]string, len(matches)*(2*ctx+1))
	gi := 0
	ln = 0
	for {
		line, ok := sc.nextLine()
		if !ok {
			break
		}
		ln++
		for gi < len(groups) && ln > groups[gi].end {
			gi++
		}
		if gi >= len(groups) {
			break // 后面的行都不需要
		}
		if ln < groups[gi].start {
			continue
		}
		fr.lines[ln] = truncateGrepLine(string(line))
	}
	return fr, truncated, nil
}

// truncateGrepLine 把行内容截到输出上限（防长行撑爆结果）。
func truncateGrepLine(s string) string {
	if len(s) > maxGrepLineLength {
		return s[:maxGrepLineLength] + "..."
	}
	return s
}

// computeGrepGroups 把命中行合并为上下文区间组（相邻/重叠合并）。
func computeGrepGroups(matches []int, ctx, totalLines int) []grepGroup {
	var groups []grepGroup
	for _, m := range matches {
		s, e := m-ctx, m+ctx
		if s < 1 {
			s = 1
		}
		if e > totalLines {
			e = totalLines
		}
		if n := len(groups); n > 0 && s <= groups[n-1].end+1 {
			if e > groups[n-1].end {
				groups[n-1].end = e
			}
		} else {
			groups = append(groups, grepGroup{s, e})
		}
	}
	return groups
}

// renderGrepResult 渲染一个文件的命中结果：命中行 path:line:content，上下文行 path-LINE-content，
// 命中组（含上下文的连续区间）之间用 -- 分隔（ripgrep 惯例）。
func renderGrepResult(fr grepFileResult, ctx int, b *strings.Builder) {
	if len(fr.matches) == 0 {
		return
	}
	groups := computeGrepGroups(fr.matches, ctx, fr.total)
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
				fmt.Fprintf(b, "%s:%d:%s\n", fr.rel, ln, fr.lines[ln])
			} else {
				fmt.Fprintf(b, "%s-%d-%s\n", fr.rel, ln, fr.lines[ln])
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
