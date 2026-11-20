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
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
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
				"At most 200 matches are returned per call: when there are more, the result header says so and tells you the offset to pass back for the next page (or to narrow the pattern). " +
				"Use glob to find files by name, this tool to search by content.",
			Params_: tools.Obj(map[string]any{
				"pattern":   tools.Str("Regular expression (Go regexp syntax, required)"),
				"path":      tools.Str("Start directory/file (relative to workspace root; default '.')"),
				"context":   tools.Map{"type": "integer", "minimum": 0, "default": 0, "description": "Context lines to show before/after each match (default 0; must be >= 0)"},
				"include":   tools.Str("File filter glob (e.g. '*.go', '**/*_test.go'; optional, only searches matching files)"),
				"offset":    tools.Map{"type": "integer", "minimum": 0, "default": 0, "description": "Skip the first N matches and return the next page (default 0 = from the start). Use it when the result says there are more matches — pass back the offset it reports."},
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
	Offset   int    `json:"offset"`
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
	if a.Offset < 0 {
		return recoveryError("invalid_offset", "correct_arguments", fmt.Sprintf("offset=%d", a.Offset), "Set offset to an integer >= 0, then retry grep.")
	}
	if a.Include != "" {
		if _, err := compileGlob(a.Include); err != nil {
			return fmt.Errorf("grep: invalid include glob %q: %w", a.Include, err)
		}
	}
	return nil
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：声明 grep 的工具级时限。
//
// 引擎默认的 5 分钟兜底对 grep **不够用**：改造前 grep 不看 ctx，那 5 分钟
// 形同虚设（一次全仓 grep 实测 4 分钟起步）；现在 grep 会真的响应取消，
// 于是可以给它一个更紧的、模型可理解的时限（超时错误里写明「收窄搜索」）。
//
// 该时限落在可摘离的 liftableTimeout 上：任务 promote 成后台时自动解除。
func (t *grepTool) ToolTimeout() time.Duration { return searchToolTimeout }

func (t *grepTool) Call(ctx context.Context, _, arguments string) (string, error) {
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
	root, err := t.resolveAt(ctx, start)
	if err != nil {
		return "", err
	}

	// 忽略规则：内置黑名单 + 逐目录增量读 .gitignore。被忽略的目录整棵子树
	// 零 I/O（SkipDir ⇒ 连 read_dir 都不做）。显式给出的 path 不受规则约束
	// （与 rg 一致：命令行路径覆盖 ignore 规则）。
	ign := newIgnoreMatcher(t.root, !a.NoIgnore)

	col := &grepCollector{offset: a.Offset, max: t.maxResults}
	if err := t.search(ctx, re, root, a, ign, col); err != nil {
		return "", err
	}

	var b strings.Builder
	switch {
	case col.taken == 0 && col.offset > 0:
		// offset 越过总数：明确告诉模型「没有更多了」，别再空转
		fmt.Fprintf(&b, "[grep: no matches at offset %d (%d match(es) in %d file(s) in total); lower the offset or narrow the pattern]\n",
			col.offset, col.total, col.files)
	case col.truncated && col.offset > 0:
		fmt.Fprintf(&b, "[grep: more than %d matches, showing matches %d-%d (offset %d); use offset=%d to see the next page, or narrow the pattern]\n",
			col.end(), col.offset+1, col.offset+col.taken, col.offset, col.offset+col.taken)
	case col.truncated:
		fmt.Fprintf(&b, "[grep: more than %d matches, showing first %d; narrow the pattern]\n", col.max, col.max)
	case col.offset > 0:
		fmt.Fprintf(&b, "[grep: matches %d-%d (offset %d) in %d file(s)]\n",
			col.offset+1, col.offset+col.taken, col.offset, len(col.results))
	default:
		fmt.Fprintf(&b, "[grep: %d match(es) in %d file(s)]\n", col.total, len(col.results))
	}
	for _, fr := range col.results {
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
	cap     int    // 单行保留上限（字节）；超出的部分丢弃，该行仍被返回（见 nextLine）
	cut     bool   // 上一行返回的是**被截断**的内容（超出行上限）
}

func newGrepScanner() *grepScanner {
	return newGrepScannerWithCap(maxGrepLineBytes)
}

// newGrepScannerWithCap 建一个**单行上限**为 cap 字节的扫描器。
//
// cap 的意义：一行超过 cap 时，前 cap 字节保留、其余丢弃，**这一行仍会被返回**
// （内容是截断后的前缀）——
//   - 返回（而不是整行丢弃）⇒ 调用方的行号计数不错位（整行丢弃会让后面所有行号
//     往前挪，grep 报出来的行号就是错的）；
//   - read_file 用小 cap（64 KB）⇒ 单行超长时不会把 4 MB 塞进结果。
func newGrepScannerWithCap(cap int) *grepScanner {
	if cap <= 0 {
		cap = maxGrepLineBytes
	}
	return &grepScanner{
		br:   bufio.NewReaderSize(nil, grepReadBufferSize),
		head: make([]byte, grepHeadProbeSize),
		cap:  cap,
	}
}

// reset 切到下一个文件（复用同一个缓冲）。
func (s *grepScanner) reset(r io.Reader) {
	s.br.Reset(r)
	s.longLen = 0
	s.drop = false
	s.cut = false
}

// nextLine 读下一行（去掉行尾换行）。
//
// 返回的切片指向内部缓冲，**下一次调用后失效** —— 调用方要留存必须拷贝。
// ok=false 表示 EOF。
func (s *grepScanner) nextLine() ([]byte, bool) {
	s.cut = false
	for {
		chunk, err := s.br.ReadSlice('\n')
		if s.drop {
			// 正在丢弃一个超长行的剩余部分：找到换行才算这一行结束。
			//
			// ⚠️ 这一行**必须被返回**（内容 = 已累积的前 cap 字节）：调用方靠
			// nextLine 的返回值计行号 —— 整行丢弃（不返回）会让后面所有行号往前挪，
			// grep 报出来的行号就全是错的（2026-11-17 实测：文件里有一行 > 4 MB 时，
			// 它之后每一行的行号都少 1）。
			if bytes.IndexByte(chunk, '\n') >= 0 {
				s.drop = false
				content := s.long[:s.longLen]
				s.longLen = 0
				s.cut = true // 这一行超了上限：返回的是截断后的前缀
				return trimEOL(trimIncompleteRune(content)), true
			}
			if err == io.EOF {
				s.drop = false
				content := s.long[:s.longLen]
				s.longLen = 0
				s.cut = true
				return trimEOL(trimIncompleteRune(content)), true
			}
			continue
		}
		if err == bufio.ErrBufferFull {
			// 行超过缓冲：累积（上限 = cap）。
			s.appendLong(chunk)
			if s.longLen >= s.cap {
				s.drop = true // 剩余部分丢弃（这一行仍会被返回，见 drop 分支的注释）
			}
			continue
		}
		if s.longLen > 0 {
			// 超长行：累积到 s.long。此处一定是一行（可能内容为空）。
			s.appendLong(chunk)
			content := s.long[:s.longLen]
			s.longLen = 0
			// 超长行可能被**截断**在字符中间 ⇒ 去掉尾部残缺的 UTF-8 序列。
			return trimEOL(trimIncompleteRune(content)), true
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
		s.long = make([]byte, s.cap)
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

// trimIncompleteRune 去掉尾部**不完整**的 UTF-8 序列。
//
// 场景：超长行被截断（上限 maxGrepLineBytes）时，切口可能落在多字节字符中间
// ⇒ 该字符的残缺字节序列进了输出。它在 JSON 里会被替换成 U+FFFD，
// 相当于凭空多出一个乱码字符。
//
// 只处理「不完整」的情况：源码本来就是非法 UTF-8 的字节**原样保留**
// （那是文件本身的问题，不该由截断再改一次语义）。
func trimIncompleteRune(b []byte) []byte {
	n := len(b)
	// UTF-8 最长 4 字节 ⇒ 从尾部最多回退 3 字节就能找到那个字符的首字节。
	for i := 1; i < utf8.UTFMax && i <= n; i++ {
		c := b[n-i]
		if !utf8.RuneStart(c) {
			continue // 续字节：继续往前找
		}
		want := utf8SeqLen(c)
		switch {
		case want == 0:
			return b // 非法首字节：源码本身有问题，原样保留
		case i < want:
			return b[:n-i] // 不完整 ⇒ 去掉这个残缺字符
		default:
			return b // 完整
		}
	}
	return b
}

// utf8SeqLen 按首字节算出该字符应有的字节数（0 = 非法首字节）。
//
// 为什么不用 utf8.DecodeRune：它对「非法编码」和「不完整序列」都返回
// (RuneError, 1) —— 两者分不开，而我们要区分（前者是源码的问题，原样保留；
// 后者是**我们截断造成的**，必须去掉）。
func utf8SeqLen(c byte) int {
	switch {
	case c < 0x80:
		return 1
	case c < 0xC2:
		return 0 // 续字节 / 过长编码（C0、C1 非法）
	case c < 0xE0:
		return 2
	case c < 0xF0:
		return 3
	case c < 0xF4:
		return 4
	default:
		return 0 // 超出 U+10FFFF
	}
}

// grepCountFile 扫描单个文本文件的**第一遍**：数命中、记命中行号，**不留行内容**。
//
// 返回：
//   - lines：命中的行号（升序，最多 budget 个）；
//   - count：命中总数（达到 budget 时停读，capped=true 表示后面还有）；
//   - total：已读到的行数；
//   - capped：命中数达到 budget ⇒ 已停止读该文件。
//
// 二进制文件（头 8KB 含 \x00）返回 error（调用方跳过）。
func grepCountFile(ctx context.Context, re *regexp.Regexp, sc *grepScanner, abs, rel string, budget int) (lines []int, count, total int, capped bool, err error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, 0, 0, false, err
	}
	defer f.Close()

	// 二进制探测：读头 8KB。用 ReadAt（pread）—— **不移动文件偏移** ⇒
	// 后面可以直接从头扫，省掉改造前那次 Seek(0,0)（每文件少 2 次系统调用），
	// 也不需要 MultiReader 把头拼回去（少两次分配）。
	// 头缓冲跨文件复用（sc.head）⇒ 每文件不再分配 8 KB。
	n, _ := f.ReadAt(sc.head, 0)
	if bytesContainsZero(sc.head[:n]) {
		return nil, 0, 0, false, errors.New("binary file") // 跳过
	}
	sc.reset(f)

	for {
		line, ok := sc.nextLine()
		if !ok {
			break
		}
		total++
		// 取消/超时检查（按行数抽样：ctx.Err() 不是免费的 —— 引擎的
		// liftableTimeout.Err() 要加锁）。单个超大文件也得能被中断。
		if total%searchCtxCheckEvery == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return lines, count, total, capped, cerr
			}
		}
		if !re.Match(line) {
			continue
		}
		count++
		if count <= budget {
			lines = append(lines, total)
		}
		if count >= budget {
			// 命中数够了：不再读这个文件（后面的命中对窗口没影响，
			// 但足以判定「还有更多」）。
			capped = true
			break
		}
	}
	return lines, count, total, capped, nil
}

// grepCollectFile 扫描单个文本文件的**第二遍**：取「命中行 ± context」的并集内容。
//
// 已知命中行号（matchLines，第一遍算出来的），只需再读一遍文件把要输出的行取出来。
// 改造前也是这么做的（两遍扫描），区别是第一遍现在跑在并行 worker 里。
func grepCollectFile(ctx context.Context, sc *grepScanner, abs, rel string, matchLines []int, ctxLines, totalLines int) (grepFileResult, error) {
	fr := grepFileResult{rel: rel, total: totalLines, matches: matchLines}
	f, err := os.Open(abs)
	if err != nil {
		return fr, err
	}
	defer f.Close()
	sc.reset(f)

	groups := computeGrepGroups(matchLines, ctxLines, totalLines)
	fr.lines = make(map[int]string, len(matchLines)*(2*ctxLines+1))
	gi := 0
	ln := 0
	for {
		line, ok := sc.nextLine()
		if !ok {
			break
		}
		ln++
		if ln%searchCtxCheckEvery == 0 {
			if cerr := ctx.Err(); cerr != nil {
				return fr, cerr
			}
		}
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
	return fr, nil
}

// truncateGrepLine 把行内容截到输出上限（防长行撑爆结果）。
//
// 截断点必须落在 **UTF-8 字符边界**上：按字节切（`s[:200]`）可能把一个多字节
// 字符切成两半，残缺的字节序列在 JSON 里会被替换成 U+FFFD（?）——
// 等于凭空多出一个乱码字符，也会让「按字符数读结果」的模型看到错的东西。
func truncateGrepLine(s string) string {
	if len(s) <= maxGrepLineLength {
		return s
	}
	end := maxGrepLineLength
	// s[end] 是续字节 ⇒ 切口落在字符中间 ⇒ 往前退到该字符的首字节。
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "..."
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
var _ tools.ToolTimeoutProvider = (*grepTool)(nil)
