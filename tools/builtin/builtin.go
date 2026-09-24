// Package builtin 内置基础工具集：文件读写、目录浏览、Shell、HTTP。
//
// 安全模型（workspace 隔离）：
//   - 文件类工具构造时绑定工作区根目录，所有路径解析后必须落在根内（拒绝逃逸）；
//   - Shell 随工具集默认注册（初始 cwd = 工作区根），经进程隔离/沙箱执行 —— 注册即信任；
//   - HTTP 带超时，响应体截断。
package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/tools"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FileTools 绑定工作区根目录的文件工具集。
type FileTools struct {
	root string
	sbx  sandbox.Sandbox // bash 执行后端（nil = NoSandbox 进程隔离直通）

	// binaryHint 可选钩子：read_file 判出「二进制、不可解码为文本/图片」时，产出错误
	// 提示末尾的「下一步怎么办」句（参数 = 文件路径，宿主可按扩展名细分格式）。宿主按
	// **自身能力面**注入 —— 不注册 bash 的产品形态（桌面 Work persona）照默认句提示
	// bash，等于让模型去调不存在的工具、白耗轮次；nil = 默认提示 bash（库默认，既有
	// 用户不受影响）。
	binaryHint func(path string) string
}

// WithBashSandbox 注入 bash 工具的执行后端（如 macOS Seatbelt / Linux bwrap）。
// 工具本身不知道自己在沙箱里；nil 保持 NoSandbox 进程隔离（库默认，端口保持现状）。
func (f *FileTools) WithBashSandbox(sbx sandbox.Sandbox) *FileTools {
	f.sbx = sbx
	return f
}

// WithBinaryHint 注入二进制拒绝提示的尾句构造器（nil = 保持默认提示 bash）。
// 钩子风格同 BashTool.BeforeExec / LoadSkillTool.WithOnLoad：工具层只负责给出「这个
// 文件读不了」的事实（文件名/形态/体积仍由本层拼），后续建议由宿主按自己注册了哪些
// 工具来组织——库不认识 persona 等产品概念。
func (f *FileTools) WithBinaryHint(fn func(path string) string) *FileTools {
	f.binaryHint = fn
	return f
}

// defaultBinaryHint 未注入钩子时的尾句：指向 bash 的现成工具链（库默认行为，勿改）。
const defaultBinaryHint = "Use bash for other formats (e.g. `file`, `ffmpeg`, `sips`, `unzip -l`)"

// NewFileTools 创建文件工具集；root 为工作区根目录（绝对路径，不存在不报错）。
func NewFileTools(root string) *FileTools {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = filepath.Clean(root)
	}
	return &FileTools{root: filepath.Clean(abs)}
}

// Tools 返回工作区工具集（read_file / write_file / edit_file / grep / glob / bash）。
// bash 默认注册（命令经进程隔离执行），初始 cwd = 工作区根：bash 相对路径与文件
// 工具同锚（模型声明的 Current Workspace 即命令真实起点）。绑定仅决定「起点」，
// 不锁访问面 —— 命令内可随时 `cd 任意目录 && 执行`、绝对路径照常可用（已装包 /
// 桌面应用不受影响）；Seatbelt 策略按路径放行，与 cwd 无关。—— 危险防护从
// 「不注册」转向「注册 + 进程隔离」，模型无需显式要求即可使用。
func (f *FileTools) Tools() []tools.Tool {
	return []tools.Tool{
		newReadFileTool(f),
		newWriteFileTool(f),
		newEditFileTool(f),
		newGrepTool(f),
		newGlobTool(f),
		// cwd 传 f.root：bash 初始目录 = 工作区根（每条命令独立进程、从工作区出发；
		// AI 可在单条命令内 cd 切换，下条命令回到工作区起点——对齐 Claude Code 语义）。
		// appAllow 参数已弃用（2026-09：open 全放行），传 nil。
		NewBashTool(f.root, 0, f.sbx, nil),
	}
}

// Register 把文件工具集注册进引擎。
func (f *FileTools) Register(e *tools.Engine) {
	for _, t := range f.Tools() {
		e.RegisterTool(context.Background(), t)
	}
}

// resolve 校验并解析路径为绝对路径（问题十六：与沙箱/审批统一治理）。
//   - 相对路径 → 锚定 workspace root（既有行为；`..` 逃逸仍拒绝——相对语义下
//     工作区外无意义，出工作区请用绝对路径表达显式意图）；
//   - 绝对路径 → Clean 后原样采用：写盘合法性交由统一层治理（Seatbelt 管 bash
//     进程；mode.Approve 审批管所有工具调用），工具层不再自行锚死 workspace；
//   - go-code 自身配置/密钥目录（~/.go-code）显式拒绝——与 Seatbelt 的
//     defaultSensitivePaths 同一保护对象，防止模型篡改 harness 自身配置。
func (f *FileTools) resolve(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(raw) {
		p := filepath.Clean(raw)
		if err := denyHarnessConfigPath(p); err != nil {
			return "", err
		}
		return p, nil
	}
	base := f.root
	p := filepath.Clean(filepath.Join(base, raw))
	if p != base && !strings.HasPrefix(p, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes workspace root %q", raw, base)
	}
	return p, nil
}

// denyHarnessConfigPath 拒绝 go-code 自身配置/密钥路径（~/.go-code）：settings.json
// 可能含 api_key。无法定位 home 时放弃该保护（与旧行为一致，不阻塞正常路径）。
func denyHarnessConfigPath(abs string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	cfgDir := filepath.Clean(filepath.Join(home, ".go-code"))
	if abs == cfgDir || strings.HasPrefix(abs, cfgDir+string(os.PathSeparator)) {
		// 受管子树放行：plugins（Browser Use 等受管组件的安装/调试需要文件工具可达）
		// 与 runtime（受管 Python 运行时 ~/.go-code/runtime/python：venv 解释器与
		// 依赖库需可读、__pycache__ 需可写）—— 与沙箱 carve-out
		// （defaultOpenSubpaths）同语义，两处必须同步，否则沙箱能跑而文件工具读写不了。
		// .go-code 其余（settings.json 明文密钥 / sessions / events）仍保护。
		for _, sub := range []string{"plugins", "runtime"} {
			dir := filepath.Join(cfgDir, sub)
			if abs == dir || strings.HasPrefix(abs, dir+string(os.PathSeparator)) {
				return nil
			}
		}
		return fmt.Errorf("path %q is go-code's own config/secret directory and is protected", abs)
	}
	return nil
}

// ---------- read_file ----------

const defaultReadMaxLines = 200

type readFileTool struct {
	tools.BaseTool
	*FileTools
	maxLines int // 未指定 end_line 时的默认区间行数（默认 200）

	// maxImageBytes 单张内嵌图片 data URL 上限（0 = 用引擎默认闸门 defaultMaxImageBytes）。
	// 与 tools 引擎的图片闸门同口径：此处先降采样到达标，引擎那层才不会丢弃。
	// 只读（构造后不变）→ 并行调用安全。
	maxImageBytes int
}

func newReadFileTool(f *FileTools) *readFileTool {
	return &readFileTool{
		BaseTool: tools.BaseTool{
			Name_:        "read_file",
			Description_: "Read a text file and return numbered lines. path is required and relative to the workspace root. start_line and end_line are optional 1-based inclusive integers. Omit both for the first 200 lines; if only start_line is given, read up to 200 lines from there. If both are given in reverse order, the tool normalizes them to the ascending range and reports the normalization. Reading an image file (png/jpeg/gif) returns the image itself for direct viewing (do not pass start_line/end_line for images). Use glob to locate files and grep to search content.",
			Params_: tools.Obj(map[string]any{
				"path":       tools.Str("File path (relative to workspace root)"),
				"start_line": tools.Map{"type": "integer", "minimum": 1, "default": 1, "description": "First line, 1-based; omit for line 1"},
				"end_line":   tools.Map{"type": "integer", "minimum": 1, "description": "Last line, inclusive; omit to read the default 200-line window. If end_line is smaller than start_line, the tool normalizes the range to ascending order and reports it."},
			}, "path"),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		FileTools: f,
		maxLines:  defaultReadMaxLines,
	}
}

type readArgs struct {
	Path      string `json:"path"`
	StartLine *int   `json:"start_line"`
	EndLine   *int   `json:"end_line"`
}

// readHead 读文件前 n 字节（格式判别用：魔数只在前几十字节，大图不必整读）。
// 文件短于 n 时返回实际读到的部分（无错）。
func readHead(p string, n int) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, n)
	read, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return nil, err
	}
	return head[:read], nil
}

func (t *readFileTool) ValidParams(_ context.Context, _, arguments string) error {
	var a readArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return invalidJSON("read_file", err.Error())
	}
	if a.Path == "" {
		return requiredArgument("read_file", "path")
	}
	if (a.StartLine != nil && *a.StartLine < 1) || (a.EndLine != nil && *a.EndLine < 1) {
		start, end := "omitted", "omitted"
		if a.StartLine != nil {
			start = fmt.Sprintf("%d", *a.StartLine)
		}
		if a.EndLine != nil {
			end = fmt.Sprintf("%d", *a.EndLine)
		}
		return recoveryError("invalid_line_range", "correct_arguments", fmt.Sprintf("start_line=%s, end_line=%s; line numbers must be >= 1 when provided", start, end), "Use positive 1-based line numbers, or omit start_line/end_line for the default range.")
	}
	// A reversed range is recoverable without asking the model to repeat a tool
	// call. Call normalizes it below, preserving the requested interval while
	// avoiding the common end_line < start_line failure loop.
	return nil
}

func (t *readFileTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a readArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	start := 1
	if a.StartLine != nil {
		start = *a.StartLine
	}
	end := 0
	if a.EndLine != nil {
		end = *a.EndLine
	}
	normalizedRange := false
	if end > 0 && end < start {
		// Models occasionally swap the two line numbers after reading a grep
		// result. Treat the pair as an unordered selection rather than failing
		// the whole tool round; this is deterministic and has no side effect.
		start, end = end, start
		normalizedRange = true
	}
	if end < start {
		end = start + t.maxLines - 1
	}
	if err := statPathHint(p, "read_file"); err != nil {
		return "", err
	}

	// 图片分支：魔数识别 → 内嵌为 image 内容块（模型直接看图）。行区间参数对
	// 图片无意义（不按行切图），故只要指定了行区间即明确拒绝而不是静默忽略——
	// 让模型知道「这里读到了图，range 没生效」，避免它以为读的是文本片段。
	if head, ferr := readHead(p, 512); ferr == nil {
		if format, isImg := detectImageFormat(head); isImg {
			if a.StartLine != nil || a.EndLine != nil {
				return "", fmt.Errorf("read_file: %s is an image (%s); line ranges do not apply to images — call read_file with just the path to view it", a.Path, format.mime)
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return "", rerr
			}
			res, ierr := loadImageForModel(p, data, t.maxImageBytes)
			if ierr != nil {
				return "", ierr
			}
			tools.ImageSinkFrom(ctx).Add(res.block)
			return res.text, nil
		}
		// 非图片的二进制（可执行文件/压缩包/媒体容器）：明确拒绝而不是倒乱码。
		if looksBinary(head) {
			kind := "binary"
			if imageExtHint(p) {
				kind = "binary (image-like extension, but the content is not a decodable image)"
			}
			size := ""
			if st, serr := os.Stat(p); serr == nil {
				size = ", " + humanSize(int(st.Size()))
			}
			// 事实部分（文件名/形态/体积 + read_file 的能力边界）由本层拼；尾句「下一步
			// 怎么办」交给宿主注入的 hint —— 默认指 bash 的现成工具链，未注册 bash 的产品
			// 形态（桌面 Work persona）换成本会话真有的路径。空串 = 宿主明确不给建议（不回退
			// 默认：注入方已表明自己能力面里没有 bash，再提示它只会让模型调不存在的工具）。
			hint := defaultBinaryHint
			if t.binaryHint != nil {
				hint = t.binaryHint(a.Path)
			}
			tail := "" // 连前导空格一起省，空提示不留尾空格
			if hint != "" {
				tail = " " + hint
			}
			return "", fmt.Errorf("read_file: %s looks like a %s file%s; read_file only returns text or decodable images (png/jpeg/gif).%s", a.Path, kind, size, tail)
		}
	}

	total, lines, err := readLines(p, start, end)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[read_file: %s, %d lines total, showing %d-%d]\n", a.Path, total, start, start+len(lines)-1)
	if normalizedRange {
		fmt.Fprintf(&b, "[line range normalized from %d-%d to %d-%d]\n", *a.StartLine, *a.EndLine, start, end)
	}
	if a.EndLine == nil && start+len(lines) <= total {
		b.WriteString("[file has more lines; use start_line/end_line to read further]\n")
	}
	for i, l := range lines {
		fmt.Fprintf(&b, "%d: %s\n", start+i, l)
	}
	return b.String(), nil
}

// statPath 操作前预检查目标存在性（给 LLM 可操作提示，而非裸错误）。
func statPath(p, tool string) error {
	_, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return notFoundError(p, tool, "")
	}
	return err
}

// notFoundError 组装「目标不存在」错误文案（statPath 与 statPathHint 共用，两处不漂移）。
func notFoundError(p, tool, hint string) error {
	return fmt.Errorf("%s: file not found: %s%s (use glob to locate the file first, or inspect the workspace)", tool, p, hint)
}

// statPathHint 同 statPath，但「文件不存在」时附一条同目录近似条目建议（read_file 专用：
// 记错文件名是它最常见的失败，时间序列显示「文件不存在率」6-9% 不升不降）。
// 仅失败路径读目录 —— 成功路径零额外开销（read_file 是高频工具）。
// 其余情况（文件在、权限错等）与 statPath 逐字一致。
func statPathHint(p, tool string) error {
	_, err := os.Stat(p)
	if errors.Is(err, os.ErrNotExist) {
		return notFoundError(p, tool, similarPathHint(p))
	}
	return err
}

// maxPathSuggestions 「did you mean」最多列几条同目录候选：再多就变成噪声清单，
// 模型反而要在候选里做二次选择（建议的用途只是「你是不是记错了这个名字」）。
const maxPathSuggestions = 3

// similarPathHint 为「文件不存在」的路径挑一条可操作建议：在**同目录**（目录本身也不存在时
// 逐级上溯到最近的已存在目录）里找最像的条目名，返回 `; did you mean "x.go" …?`；
// 目录不可读 / 条目不相似时返回 ""（调用方退化到原文案，行为与新增前一致）。
// 复用 B6 的相似度纯函数（tools.SuggestSimilarFileNames：公共前缀 + 最长公共子串、
// 大小写不敏感、先剥扩展名），与工具名建议同一取舍：不够像就不猜。
// 只读目录：不写盘、**不替换目标路径** —— 建议是错误文案而非自动纠错（猜错文件比报错更糟，
// 同 GetTool 的 did-you-mean 取舍）；模型仍须自己发起一次调用。
func similarPathHint(p string) string {
	parent := filepath.Dir(p)
	dir, ok := nearestExistingDir(parent)
	if !ok {
		return ""
	}
	entries, err := os.ReadDir(dir) // 已按名字排序 → 建议文本确定、可断言（同 GetTool 的清单排序）
	if err != nil {
		return "" // 目录不可读（权限等）：不在一个错误里再叠一个错误
	}
	names := make([]string, 0, len(entries))
	isDir := make(map[string]bool, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
		isDir[e.Name()] = e.IsDir()
	}
	cands := tools.SuggestSimilarFileNames(missingComponent(p, dir), names, maxPathSuggestions)
	if len(cands) == 0 {
		return ""
	}
	// 目录候选带尾斜杠：告诉模型「这是目录、不是你要读的文件」（read_file 读目录只会得到
	// not_a_file，它该去 glob/列目录找目录里的文件）。
	for i, n := range cands {
		if isDir[n] {
			cands[i] = n + "/"
		}
	}
	return "; did you mean " + quoteCandidates(cands) + "?"
}

// missingComponent 取「该拿去比对的名字」：模型写错的到底是文件名还是目录名，取决于路径在
// 哪一段开始不存在。
//   - dir 就是 p 的父目录 → 比 p 的文件名（"repot.md" vs 同目录的 "report.md"）；
//   - 上溯过（父目录不存在）→ 比**第一个缺失的路径分量**（"componets/x.ts" 该比 "componets"，
//     而不是 x.ts：目录名写错时拿文件名去比只会得到无关候选）。
func missingComponent(p, dir string) string {
	if filepath.Clean(dir) == filepath.Clean(filepath.Dir(p)) {
		return filepath.Base(p)
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return filepath.Base(p)
	}
	if first, _, _ := strings.Cut(rel, string(os.PathSeparator)); first != "" && first != "." && first != ".." {
		return first
	}
	return filepath.Base(p)
}

// nearestExistingDir 从 dir 起逐级上溯，返回最近的已存在目录（目标路径的父目录常常也不存在：
// 模型写错目录名时，能在最近的真实目录里找到它想读的文件）。
// 一路到根都不存在 → ok=false（调用方不给建议，不 panic）。
func nearestExistingDir(dir string) (string, bool) {
	for {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // 已到根（filepath.Dir 对根返回自身）
		}
		dir = parent
	}
}

// quoteCandidates 把候选名拼成好读的短语：`"a"`、`"a" or "b"`、`"a", "b" or "c"`。
func quoteCandidates(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, fmt.Sprintf("%q", n))
	}
	if len(quoted) == 1 {
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}

// readLines 单遍扫描文件：统计总行数并收集 [start, end] 闭区间（含）的行。
// end 超出文件末尾时自然截断；超长行（> 4MB）按行截断处理（scanner 报错即失败）。
func readLines(p string, start, end int) (total int, lines []string, err error) {
	f, err := os.Open(p)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		total++
		if total >= start && total <= end {
			lines = append(lines, sc.Text())
		}
	}
	if err := sc.Err(); err != nil {
		return 0, nil, err
	}
	return total, lines, nil
}

// ---------- write_file ----------

type writeFileTool struct {
	tools.BaseTool
	*FileTools
	diff *core.FileDiff // 本次调用对文件的变更（engine 经 ToolDiffProvider 读取）
}

// Diff 实现 tools.ToolDiffProvider：返回最近一次成功调用的变更 diff。
func (t *writeFileTool) Diff() *core.FileDiff { return t.diff }

func newWriteFileTool(f *FileTools) *writeFileTool {
	return &writeFileTool{
		BaseTool: tools.BaseTool{
			Name_: "write_file",
			Description_: "Write a file (overwrites by default; append=true appends). To modify an existing file, prefer edit_file (sends only the changed fragment, far fewer tokens); use this tool to create new files or rewrite a whole file. " +
				"Overwrite is atomic (temp file + rename; concurrent readers never see partial content); concurrent write conflicts return an error (retryable). " +
				"Creates missing files and parent directories automatically; if you need to keep the old content before overwriting, back it up with read_file first.",
			Params_: tools.Obj(map[string]any{
				"path":    tools.Str("File path (relative to workspace root)"),
				"content": tools.Str("Text content to write (full new file content)"),
				"append":  map[string]any{"type": "boolean", "default": false, "description": "Append mode (default false = overwrite)"},
			}, "path", "content"),
			CanParallel_: false, // 写文件不并行（避免同文件竞争）
		},
		FileTools: f,
	}
}

type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Append  bool   `json:"append"`
}

func (t *writeFileTool) ValidParams(_ context.Context, _, arguments string) error {
	var a writeArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	if a.Path == "" || a.Content == "" {
		return errors.New("write_file: path and content are required")
	}
	return nil
}

func (t *writeFileTool) Call(_ context.Context, _, arguments string) (string, error) {
	t.diff = nil
	var a writeArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	if a.Append {
		// 追加（产物汇总修复，2026-09-15）：此前 append 分支不产 diff，导致
		// 追加写永远不进产出列表（AgentEnd.Artifacts）。现在产出 Added + 指纹。
		//
		// 注意语义边界：content 是**增量**而非文件全文——
		//   - Added  = 增量行数（正确）
		//   - SHA256 = **增量**指纹（用于变更判定：同样内容重复 append 可得同一值）
		//   - Size/Lines = **留空**：全量值需读整个文件，与「工具侧零额外 IO」冲突；
		//     宿主对空值不展示该维度（诚实优于错数）。
		// Created 由 Stat 判定（appendTo 用 O_CREATE，文件可能此前存在）。
		_, statErr := os.Stat(p)
		if err := appendTo(p, a.Content); err != nil {
			return "", err
		}
		created := errors.Is(statErr, os.ErrNotExist)
		if created {
			// 新建文件：追加内容即全文 → size/lines/指纹均为精确事实
			t.diff = finalizeDiff(&core.FileDiff{Path: a.Path, Added: strings.Count(a.Content, "\n")}, a.Content)
		} else {
			// 既有文件：content 是纯增量 → Added 是增量语义；
			// 全量 size/lines 需读整个文件（与「工具侧零额外 IO」冲突）→ 留空，
			// 宿主对空值不展示该维度（诚实优于错数）。指纹为增量指纹（变更判定有效）。
			t.diff = &core.FileDiff{
				Path:   a.Path,
				Added:  strings.Count(a.Content, "\n"),
				SHA256: contentFingerprint(a.Content),
			}
		}
		t.diff.Created = created
		return fmt.Sprintf("appended (+%d)", t.diff.Added), nil
	}
	// 覆盖：锁内读旧内容（若存在）→ 替换 → 算 diff。旧内容须在替换前读取，
	// 与 edit_file 同款锁内读改写全程无竞态；替换后旧内容不可再得（信息源头在工具）。
	lock, err := lockPath(p)
	if err != nil {
		return "", err
	}
	var old string
	if lock != nil {
		defer lock.Close()
		if old, err = readAllLocked(lock, p); err != nil {
			return "", err
		}
	}
	if err := replaceLocked(lock, p, a.Content); err != nil {
		return "", err
	}
	// Created：lock==nil ⇔ 文件此前不存在（lockPath 对不存在的文件返回 nil 锁）。
	// 不能用 Removed==0 推断（纯插入内容同样是 +N -0）。
	t.diff = finalizeDiff(computeFileDiff(a.Path, old, a.Content), a.Content)
	t.diff.Created = lock == nil
	return fmt.Sprintf("written (+%d -%d)", t.diff.Added, t.diff.Removed), nil
}

// ---------- edit_file ----------

type editFileTool struct {
	tools.BaseTool
	*FileTools
	diff *core.FileDiff // 本次调用对文件的变更（engine 经 ToolDiffProvider 读取）
}

// Diff 实现 tools.ToolDiffProvider：返回最近一次成功调用的变更 diff。
func (t *editFileTool) Diff() *core.FileDiff { return t.diff }

func newEditFileTool(f *FileTools) *editFileTool {
	return &editFileTool{
		BaseTool: tools.BaseTool{
			Name_:        "edit_file",
			Description_: "Replace exactly one occurrence in an existing text file. Read the file first and copy old_string exactly, including whitespace and line breaks. Use the shortest old_string that is still unique; uniqueness is more important than minimizing context. Keep the change minimal: new_string must contain only the necessary edits (ideally 1-3 lines) and must not rewrite unrelated content. If it matches zero or multiple times, no change is made. After an error never repeat the same arguments: read_file and refresh the edit. Use write_file only for intentional full-file replacement.",
			Params_: tools.Obj(map[string]any{
				"path":       tools.Str("File path (relative to workspace root)"),
				"old_string": tools.Str("Original text to replace (must appear exactly once; ideally 1-3 lines, just enough to be unique)"),
				"new_string": tools.Str("Replacement text"),
			}, "path", "old_string", "new_string"),
			CanParallel_: false,
		},
		FileTools: f,
	}
}

type editArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

func (t *editFileTool) ValidParams(_ context.Context, _, arguments string) error {
	var a editArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("edit_file: %w", err)
	}
	if a.Path == "" || a.OldString == "" {
		return recoveryError("missing_required_argument", "correct_arguments", "path and old_string are required", "Provide both path and old_string; read the file first if needed.")
	}
	return nil
}

func (t *editFileTool) Call(_ context.Context, _, arguments string) (string, error) {
	t.diff = nil
	var a editArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	if err := statPath(p, "edit_file"); err != nil {
		return "", err
	}
	// 读-改-写全程持锁：读的是锁定版本，替换不会覆盖并发写者的新内容
	lock, err := lockPath(p)
	if err != nil {
		return "", err
	}
	if lock != nil {
		defer lock.Close() // Close 即释放 flock（rename 之后）
	}
	s, err := readAllLocked(lock, p)
	if err != nil {
		return "", err
	}
	if n := strings.Count(s, a.OldString); n == 0 {
		return "", recoveryError("old_string_not_found", "read_file", fmt.Sprintf("old_string was not found in %s", a.Path), "Re-read the file and copy the exact current text (incl. whitespace); use the shortest unique snippet.")
	} else if n > 1 {
		return "", recoveryError("old_string_not_unique", "read_file", fmt.Sprintf("old_string appears %d times in %s", n, a.Path), "Add a bit of surrounding context to make it unique. Do not repeat the same arguments.")
	}
	replaced := strings.Replace(s, a.OldString, a.NewString, 1)
	if err := replaceLocked(lock, p, replaced); err != nil {
		return "", err
	}
	// 旧全文 s 与 replaced 均在手，免费产 diff（信息源头在工具）。
	// edit_file 入口已 statPath 要求文件存在 → Created 恒 false（modified）。
	t.diff = finalizeDiff(computeFileDiff(a.Path, s, replaced), replaced)
	return fmt.Sprintf("[edit_file: replaced 1 occurrence in %s (+%d -%d)]", a.Path, t.diff.Added, t.diff.Removed), nil
}

// readAllLocked 读取目标文件全文（持锁时直接复用锁定 fd，避免再次 open 竞态）。
func readAllLocked(lock *os.File, p string) (string, error) {
	if lock != nil {
		b, err := io.ReadAll(lock)
		return string(b), err
	}
	b, err := os.ReadFile(p)
	return string(b), err
}
