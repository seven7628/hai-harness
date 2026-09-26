package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/seven7628/hai-harness/core"
)

// 工具结果文本瘦身（D）：read/edit 折叠 + bash 简化 + grep/glob 压缩。
//
// 与 slim.go 三件套（去重/spill/占位）的关系：那三件是「按体积阈值」的无差别
// 处置（超阈值的整块搬走或抹掉），回答不了「这个文件被改了几处、改了什么」；
// 本文件是「按工具语义」的有信息处置 —— 折叠掉的内容用**可寻址的摘要**替代，
// 模型仍然知道：有多少变更、变更在哪些行、内容长什么样，需要时用一次
// read_file/grep 精确取回。摘要只描述事实，不做 LLM 推断（零成本、零幻觉）。
//
// 三条不变式（与 C 节一致）：
//  1. 只改 ac.Messages 里的文本，core.ToolResult 原样发出 —— 宿主 UI 展示不受影响；
//  2. 替换不增删消息元素（下标不失效）→ 下一轮请求字节一次断层，之后稳定；
//  3. 任何被折叠的内容都有明确的取回路径（read_file/grep 参数写进占位文本），
//     不留「模型不知道东西去哪了」的暗坑。

// D 段配置（挂在 SlimConfig 上；零值 = 该项关闭，保持 WithToolResultSlim 的
// 既有语义 —— 启用 slim 不等于启用 D 段所有能力）。
type slimTextConfig struct {
	// read 折叠：同一文件的旧读取结果被新读取/edit 取代时替换为摘要。
	ReadFold bool
	// bash：剥离 ANSI 转义序列（cargo/npm/go test 的彩色输出是纯 token 噪声）。
	BashStripANSI bool
	// bash：连续相同行折叠（≥ BashRepeatLines 行折成 1 行 + 计数）。
	BashFoldRepeat bool
	// bash：同一命令重复执行且输出逐字相同 → 旧结果替换为指向新结果的引用。
	BashRerunDedup bool
	// grep/glob：同一 pattern 重复搜索且结果逐字相同 → 旧结果折叠为引用。
	SearchRerunDedup bool
	// grep/glob：单文件命中行数超过此值 → 中段折叠（保留首尾各 GrepPerFileHead/Tail）。
	GrepPerFileMax  int
	GrepPerFileHead int
	GrepPerFileTail int
}

func (c *slimTextConfig) normalize() {
	if c.GrepPerFileMax <= 0 {
		c.GrepPerFileMax = 0 // 默认关
	}
	if c.GrepPerFileHead <= 0 {
		c.GrepPerFileHead = 8
	}
	if c.GrepPerFileTail <= 0 {
		c.GrepPerFileTail = 8
	}
}

// WithSlimText 启用 D 段（语义化工具结果瘦身）。空参 = 全部子能力开启 + 默认阈值。
// 产品按需挑开：例如只想折叠 read/edit 就传 &SlimTextOptions{ReadFold: true}。
func WithSlimText(opts SlimTextOptions) Option {
	return func(c *Config) {
		if c.Slim == nil {
			c.Slim = &SlimConfig{}
		}
		c.Slim.normalize()
		c.Slim.Text = opts.toConfig()
	}
}

// SlimTextOptions D 段子能力的按需开关（nil = 全部开启）。
type SlimTextOptions struct {
	ReadFold         *bool
	BashStripANSI    *bool
	BashFoldRepeat   *bool
	BashRerunDedup   *bool
	SearchRerunDedup *bool
	GrepPerFileMax   int
	GrepPerFileHead  int
	GrepPerFileTail  int
}

func (o SlimTextOptions) toConfig() *slimTextConfig {
	t := &slimTextConfig{
		ReadFold:         o.ReadFold == nil || *o.ReadFold,
		BashStripANSI:    o.BashStripANSI == nil || *o.BashStripANSI,
		BashFoldRepeat:   o.BashFoldRepeat == nil || *o.BashFoldRepeat,
		BashRerunDedup:   o.BashRerunDedup == nil || *o.BashRerunDedup,
		SearchRerunDedup: o.SearchRerunDedup == nil || *o.SearchRerunDedup,
		GrepPerFileMax:   o.GrepPerFileMax,
		GrepPerFileHead:  o.GrepPerFileHead,
		GrepPerFileTail:  o.GrepPerFileTail,
	}
	t.normalize()
	return t
}

// bashRepeatThreshold 连续相同行达到该行数才折叠：2~3 次的重复多半是真实结构
// （如连续三个空行、连续三行 `}`），折叠它们会丢掉形状信息。
const bashRepeatThreshold = 4

// ---------------------------------------------------------------------------
// 分发入口
// ---------------------------------------------------------------------------

// slimToolResult 工具结果入上下文前的语义化瘦身（D 段）。newIdx = 本次结果
// 将占据的 ac.Messages 下标。返回替换进上下文的文本；折叠旧消息时直接改写
// ac.Messages 中的既有元素（不增删）。
//
// 顺序：read 折叠 → bash 简化 → search 压缩。三类互斥（按 tc.Name 分派），
// 不存在同一段文本被两套规则改写的情形。
func (a *AgentLoop) slimToolResult(ac *AgentContext, tc core.ToolCall, text string, newIdx int) string {
	s := ac.slim
	if s == nil || s.cfg.Text == nil {
		return text
	}
	switch tc.Name {
	case "read_file":
		if s.cfg.Text.ReadFold {
			text = a.foldRead(ac, tc, text, newIdx)
		}
	case "write_file", "edit_file":
		// 写结果本身已是极短的一行统计（"[edit_file: ... (+3 -1)]"），只记账；
		// 真正的收益发生在后续 read 折叠（模型重读被自己改过的文件时）。
		added, removed, ok := parseEditStats(text)
		ac.slim.noteEdit(pathArg(tc.Arguments), added, removed, ok)
	case "bash":
		if s.cfg.Text.BashStripANSI {
			text = stripANSI(text)
		}
		if s.cfg.Text.BashFoldRepeat {
			text = foldRepeatedLines(text)
		}
		if s.cfg.Text.BashRerunDedup {
			text = a.dedupRerun(ac, tc, text, newIdx, "同一命令重复执行，输出与消息 #%d 逐字相同")
		}
	case "grep", "glob":
		if s.cfg.Text.SearchRerunDedup {
			text = a.dedupRerun(ac, tc, text, newIdx, "同一搜索重复执行，结果与消息 #%d 逐字相同")
		}
		if s.cfg.Text.GrepPerFileMax > 0 {
			text = foldPerFileMatches(text, s.cfg.Text)
		}
	}
	return text
}

// dedupRerun 通用「重复调用去重」：key 相同且文本逐字相同 → 旧结果替换为指向
// 本次（#newIdx）的引用。与 slim.go 的 recordRead 同机制（替换不增删），
// 区别是这里按「工具名 + 参数」而非 path 匹配，覆盖 bash/grep/glob。
// 输出不同则不动 —— 编译失败→修复→重编是真实的信息演进，不能吞。
func (a *AgentLoop) dedupRerun(ac *AgentContext, tc core.ToolCall, text string, newIdx int, msg string) string {
	key := rerunKey(tc)
	if key == "" {
		return text
	}
	h := textHash(text)
	prev, ok := ac.slim.rerunIndex[key]
	// 首次出现也要落锚 —— 否则「第二次执行相同」永远无锚可比（去重静默失效）。
	ac.slim.rerunIndex[key] = rerunRef{index: newIdx, hash: h}
	if !ok || prev.index == newIdx {
		return text
	}
	if prev.hash != h {
		return text // 输出不同 = 真实的信息演进（编译失败→修复），不动
	}
	if prev.index >= 0 && prev.index < len(ac.Messages) {
		if m := ac.Messages[prev.index]; m.Role == core.Tool {
			ac.Messages[prev.index] = core.NewToolMessage(m.ToolCallId, fmt.Sprintf(msg, newIdx))
		}
	}
	return text
}

// rerunKey 工具名 + 完整参数的稳定键：区分「同命令重跑」与「参数不同的两次搜索」。
func rerunKey(tc core.ToolCall) string {
	if tc.Arguments == "" {
		return tc.Name
	}
	return tc.Name + "\x00" + tc.Arguments
}

func pathArg(arguments string) string { return readPathFromArgs(arguments) }

// textHash 结果文本指纹（C 段去重与 D 段重跑去重共用）。
func textHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// read/edit 折叠
// ---------------------------------------------------------------------------

// readRange 一次 read_file 实际返回的行号区间（从结果头行解析，而非从参数解析：
// 引擎会对越界区间做归一化，参数不是最终事实）。
type readRange struct {
	start, end int
	valid      bool
}

var readHeaderRe = regexp.MustCompile(`^\[read_file: (.+), (\d+) lines total, showing (\d+)-(\d+)\]`)

// parseReadHeader 解析 read_file 结果首行。结果经过 spill/ANSI 处理时首行可能
// 已变（spill 预览会加提示行）—— 解析失败即 valid=false，走保守路径（不折叠）。
func parseReadHeader(text string) (path string, r readRange) {
	line := text
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		line = text[:i]
	}
	m := readHeaderRe.FindStringSubmatch(line)
	if m == nil {
		return "", readRange{}
	}
	s, err1 := strconv.Atoi(m[3])
	e, err2 := strconv.Atoi(m[4])
	if err1 != nil || err2 != nil {
		return m[1], readRange{}
	}
	return m[1], readRange{start: s, end: e, valid: true}
}

// covers 旧区间是否被新区间完全包含：只有「新读取覆盖了旧读取的每一行」才允许
// 折叠旧结果。局部读（L10-L40）之后读另一段（L100-L140）是互补而非取代，
// 折叠前者等于丢内容 —— 这是本段最容易写错的地方，故单独成函数并配对测试。
func covers(newer, older readRange) bool {
	if !newer.valid || !older.valid {
		return false
	}
	return newer.start <= older.start && newer.end >= older.end
}

// fileEditStat 单文件自上次读取以来的编辑计数与行级增减。
type fileEditStat struct {
	count      int
	added      int
	removed    int
	statsKnown bool // 增删行是否解析成功（false = 摘要里不显示行级数字）
}

// foldRead read_file 结果入上下文时的折叠：
//  1. 同 path 的旧读取 —— 被本次区间覆盖，或期间该文件被本 Run 编辑过 ——
//     替换为变更摘要（编辑次数 + 增删行 + 取回路径）；
//  2. 本次结果正常入历史（保持「最近一次读取」可检索），并登记为该 path 的锚。
//
// 不覆盖且无编辑时旧结果原样保留（两次读不同片段是互补信息）。
func (a *AgentLoop) foldRead(ac *AgentContext, tc core.ToolCall, text string, newIdx int) string {
	path, rng := parseReadHeader(text)
	if path == "" {
		// spill/异常文本：退回按参数 path 记账，但不做覆盖判断（保守）。
		path = pathArg(tc.Arguments)
	}
	if path == "" {
		return text
	}
	cur := ac.slim.editGen[path]

	for _, old := range ac.slim.reads[path] {
		if old.index == newIdx || old.folded {
			continue
		}
		superseded := covers(rng, old.rng) // 本次读取包含旧区间
		invalidated := old.gen < cur       // 旧读取之后该文件被本 Run 改过
		if !superseded && !invalidated {
			continue
		}
		if old.index >= 0 && old.index < len(ac.Messages) {
			if m := ac.Messages[old.index]; m.Role == core.Tool {
				ac.Messages[old.index] = core.NewToolMessage(m.ToolCallId, renderReadFold(path, old.rng, ac.slim.editStat(path), newIdx, superseded))
				ac.slim.markFolded(path, old.index)
			}
		}
	}
	ac.slim.reads[path] = append(ac.slim.reads[path], readAnchor{index: newIdx, rng: rng, gen: cur})
	return text
}

// renderReadFold 折叠占位文本：回答「有多少变更、变更的是什么、在哪」。
func renderReadFold(path string, r readRange, st fileEditStat, newIdx int, superseded bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[read_file %s%s 的旧结果已折叠", path, rangeSuffix(r))
	if st.count > 0 {
		if st.statsKnown {
			fmt.Fprintf(&b, "：该文件此后被本会话编辑 %d 次（+%d -%d 行）", st.count, st.added, st.removed)
		} else {
			fmt.Fprintf(&b, "：该文件此后被本会话编辑 %d 次", st.count)
		}
		if superseded {
			b.WriteString("，且最新读取已覆盖本区间")
		}
	} else if superseded {
		b.WriteString("：最新读取已覆盖本区间")
	}
	fmt.Fprintf(&b, "。需要内容用 read_file %s（可带 start_line/end_line）重取，或直接看消息 #%d]", path, newIdx)
	return b.String()
}

func rangeSuffix(r readRange) string {
	if !r.valid {
		return ""
	}
	return fmt.Sprintf(" L%d-L%d", r.start, r.end)
}

// editStatsRe 写工具结果里的行级增减：edit_file → "(+3 -1)"，write_file →
// "written (+10 -2)" / "appended (+4)"（builtin.go 的三种文案）。
var editStatsRe = regexp.MustCompile(`\(\+(\d+)(?:\s+-(\d+))?\)`)

// parseEditStats 从写工具结果解析增删行。解析不到（文案变化/未来新增格式）时
// 返回 ok=false —— 此时折叠摘要省略行级数字，而不是显示 "+0 -0"（那会被模型
// 读成「确实没改动」，是比缺失更糟的假事实）。
func parseEditStats(text string) (added, removed int, ok bool) {
	m := editStatsRe.FindStringSubmatch(text)
	if m == nil {
		return 0, 0, false
	}
	a, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, 0, false
	}
	r := 0
	if m[2] != "" {
		if v, err := strconv.Atoi(m[2]); err == nil {
			r = v
		}
	}
	return a, r, true
}

// noteEdit 写工具成功后记账：编辑代数 +1，增删行累加。stats 解析失败只记
// count —— 计数本身即摘要信息（"被编辑过 3 次"），只是没有行级细节。
func (s *slimState) noteEdit(path string, added, removed int, ok bool) {
	if path == "" {
		return
	}
	st := s.editStat(path)
	st.count++
	if ok {
		st.added += added
		st.removed += removed
		st.statsKnown = true
	}
	s.editGen[path]++
	s.editStatPut(path, st)
}

func (s *slimState) editStat(path string) fileEditStat {
	if s.edits == nil {
		return fileEditStat{}
	}
	return s.edits[path]
}

func (s *slimState) editStatPut(path string, st fileEditStat) {
	if s.edits == nil {
		s.edits = make(map[string]fileEditStat)
	}
	s.edits[path] = st
}

func (s *slimState) markFolded(path string, index int) {
	for i := range s.reads[path] {
		if s.reads[path][i].index == index {
			s.reads[path][i].folded = true
			return
		}
	}
}

// ---------------------------------------------------------------------------
// bash 简化
// ---------------------------------------------------------------------------

// ansiRe CSI 与 OSC 转义序列：cargo/npm/go test/jest 的彩色输出对模型是纯噪声
// （一个进度条可含数百个字节的 \x1b[...m），且会干扰 grep/去重的逐字比较。
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

func stripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	return ansiRe.ReplaceAllString(s, "")
}

// foldRepeatedLines 连续相同行折叠：≥bashRepeatThreshold 行折成「首行 + 计数」。
// 只处理**连续**重复（非连续行序无意义，折叠会破坏 grep 可读性）。
func foldRepeatedLines(s string) string {
	if !strings.Contains(s, "\n\n\n") && !strings.Contains(s, "\n") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		j := i + 1
		for j < len(lines) && lines[j] == lines[i] {
			j++
		}
		if n := j - i; n >= bashRepeatThreshold && strings.TrimSpace(lines[i]) != "" {
			out = append(out, lines[i], fmt.Sprintf("[同一行重复 %d 次，已折叠]", n-1))
		} else {
			out = append(out, lines[i:j]...)
		}
		i = j
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------------------
// grep/glob 压缩
// ---------------------------------------------------------------------------

// grepHeaderRe grep/glob 结果的统计头行（tools/builtin/grep.go 渲染）。
var grepHeaderRe = regexp.MustCompile(`^\[(grep|glob): `)

// foldPerFileMatches 单文件命中过多时折叠中段：命中行带行号，模型可用 grep
// 精确取回，因此折叠是**可寻址**的（占位行写明省略了多少行）。分隔符 "--"
// （renderGrepResult 的命中组分隔）作为文件块边界；统计头行原样保留（它是
// 「共命中多少」的唯一来源，折叠掉就等于丢了总量事实）。
func foldPerFileMatches(s string, cfg *slimTextConfig) string {
	if cfg.GrepPerFileMax <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	// strings.Split 产生的尾部空元素不是内容行（它是结尾换行的产物）——计进
	// 块大小会让「省略 N 行」的计数虚高一行。
	trailing := lines[len(lines)-1] == ""
	if trailing {
		lines = lines[:len(lines)-1]
	}
	out := make([]string, 0, len(lines)+1)
	i := 0
	// 头行（若在首行）单独透传，不参与折叠计数
	if len(lines) > 0 && grepHeaderRe.MatchString(lines[0]) {
		out = append(out, lines[0])
		i = 1
	}
	for i < len(lines) {
		// 收集一个文件的连续命中块（以 "--" 为界）
		j := i
		for j < len(lines) && lines[j] != "--" {
			j++
		}
		block := lines[i:j]
		if n := len(block); n > cfg.GrepPerFileMax {
			head, tail := cfg.GrepPerFileHead, cfg.GrepPerFileTail
			if head+tail >= n {
				out = append(out, block...)
			} else {
				out = append(out, block[:head]...)
				out = append(out, fmt.Sprintf("[本文件其余 %d 行命中已折叠：用 grep 收窄 pattern 或缩小 path 精确取回]", n-head-tail))
				out = append(out, block[n-tail:]...)
			}
		} else {
			out = append(out, block...)
		}
		i = j
	}
	if trailing {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}
