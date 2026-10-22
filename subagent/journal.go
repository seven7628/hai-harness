package subagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// 子任务输出 journal（JSONL）：单个后台任务的过程审计与进度感知文件。
// 设计依据 docs/SUBAGENT_OUTPUT_JOURNAL_TASKOUTPUT.md §3.1：
//   - append-only，一条记录 = 一个物理行（json.Marshal 转义文本内换行）
//   - 单写者（D5：进程内仅任务 goroutine 持有写句柄；跨进程由会话级 flock 传递性保证）
//   - 进度行不 fsync（可容忍丢最新几行，对齐 bridge logEventLine 取舍）；
//     result 行 fsync（交付物兜底，对齐 store_file 可靠性优先）
//   - 读侧容忍半截/损坏行（对齐 store_file.go tailLastState 的健壮性语义）

// taskIDPattern 后台任务 id 强校验：Registry 生成的 "task-<8位随机>"（如 task-abc123fg）。
// 隐含不含路径分隔符与 ".."，比 FileStore.path 的 sessionId 校验更严（防 journal 路径穿越的双保险之一，
// 另一保险在宿主 journalPath 闭包）。随机 ID 杜绝跨 Registry/会话的 task-1/task-2 递增复用碰撞。
var taskIDPattern = regexp.MustCompile(`^task-[a-z0-9]{8}$`)

// ValidTaskID 报告 id 是否为合法后台任务 id。
func ValidTaskID(id string) bool { return taskIDPattern.MatchString(id) }

// journalRecordType 判定一条记录是否属于 journal 自身（progress/audit 旁路）。
//
// 必要性（会话级恢复引入）：`<taskId>.jsonl` 现在**同时承载**两套记录 —— journal 的
// start/turn/result/status 行，以及会话级状态的 history/state 行（见 store.go 与
// docs §13.2）。journal 的读侧（ReadTail/ReadAll）必须只认前者，否则：
//   - TaskOutput 的 records 会掺进 history/state 行，而 state 行携带**完整上下文**，
//     单行可达 MB 级 —— 输出体积与解析成本都会被污染；
//   - turn 计数（total_lines 口径）会把状态行算成「进展」，progress 唤醒语义失真。
func journalRecordType(t string) bool {
	switch t {
	case "start", "turn", "result", "status":
		return true
	default:
		return false
	}
}

// turnTextCap turn 记录文本上限（runes）。D6：journal 用于进度感知，超长截断；
// 全量文本留在子上下文与完成推送里。
const turnTextCap = 4000

// taskTextCap start 记录里**原始任务全文**（JournalRecord.Task）的上限（runes）：
// agent_resume 靠它重建交接简报，必须拿回任务的原始意图 —— 200 字符的进度摘要
// （JournalRecord.Text）不够用。上限只为防单条记录无界膨胀（任务描述正常在几 KB 内）。
const taskTextCap = 8000

// truncateRunes 按 rune 截断到 limit；返回截断后文本与是否发生截断。
func truncateRunes(s string, limit int) (string, bool) {
	r := []rune(s)
	if len(r) <= limit {
		return s, false
	}
	return string(r[:limit]), true
}

// toolCallNames 提取工具调用名列表（turn 记录用）。
func toolCallNames(calls []core.ToolCall) []string {
	if len(calls) == 0 {
		return nil
	}
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Name)
	}
	return names
}

// producedFileTools 「会写出/修改文件」的工具名 → 其参数里的路径键（按优先级）。
//
// 保守白名单：只收确定会写盘的编辑类工具。判不出来的一律不收（宁可少报一个文件，
// 也不要把 read_file/grep 的入参当成产出 —— 那会让 files 变成半份访问日志）。
var producedFileTools = map[string][]string{
	"write_file":     {"path"},
	"edit_file":      {"path"},
	"multi_edit":     {"path"},
	"apply_patch":    {"path"},
	"create_file":    {"path"},
	"notebook_edit":  {"notebook_path", "path"},
	"notebook_write": {"notebook_path", "path"},
}

// producedFiles 从本轮工具调用里提取「写出/修改的文件」路径（去重、保序）。
// 参数解析失败/无路径键 → 跳过该调用（判不出来就不算，与 isVerifyCommand 同一取舍）。
func producedFiles(calls []core.ToolCall) []string {
	if len(calls) == 0 {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range calls {
		keys, ok := producedFileTools[c.Name]
		if !ok || c.Arguments == "" {
			continue
		}
		var args map[string]any
		if json.Unmarshal([]byte(c.Arguments), &args) != nil {
			continue
		}
		for _, k := range keys {
			p, ok := args[k].(string)
			if !ok || p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
			break // 一个调用只算一个目标文件（notebook_* 的两个键是同一份文档的别名）
		}
	}
	return out
}

// JournalRecord 子任务输出日志的一行记录（按 Type 区分）：
//   - start：任务启动（Name/Task 摘要由 runBackground 填入 Text 等通用字段；
//     Task 字段另存原始任务全文，供 agent_resume 重建交接简报）
//   - turn：一轮 LLM 决策进度（Round 序号 + 截断文本 + 工具名列表；不含压缩轮，
//     压缩器不经 handler 发 events.LLMEnd —— 见文档 §1.4-1）
//   - result：终态（Status/Final/Error/Usage；唯一 fsync 行）
//   - status：中间状态翻转预留位（本期不发）
type JournalRecord struct {
	Type        string    `json:"type"`
	Ts          time.Time `json:"ts"`
	TaskId      string    `json:"task_id"`
	Name        string    `json:"name,omitempty"`          // start：任务短标签
	ParentRunID string    `json:"parent_run_id,omitempty"` // start：发起方运行 RunId（嵌套派生区分层级，§3.7）
	Depth       int       `json:"depth,omitempty"`         // start：运行深度
	// Task 原始任务全文（start 记录；agent_resume 的交接简报来源）。与 Text 的分工：
	// Text 是给人和进度看的截断摘要（200 字符），Task 是恢复要用的完整指令。
	Task string `json:"task,omitempty"`
	// ResumedFrom 本任务是恢复哪个任务而来（start 记录；空 = 原生派发）。
	// 恢复链由此可追溯，TaskList/TaskInfo 同字段透出。
	ResumedFrom string   `json:"resumed_from,omitempty"`
	Round       int      `json:"round,omitempty"`      // turn：第几轮 LLM 决策（1-based）
	Text        string   `json:"text,omitempty"`       // turn：本轮内容（截断）；result：最终回复
	ToolNames   []string `json:"tool_names,omitempty"` // turn：本轮工具名列表
	// Files 本轮**写出/修改**的文件路径（turn：本轮；result：整次运行累计去重）。
	//
	// 为什么需要它：TaskOutput 此前只能告诉主 Agent「跑到第几轮、用了哪些工具名」，
	// 而「这次委派到底产出了什么」——改了哪些文件——它无从得知，只能再去翻子会话
	// 全文（那是另一套大 JSON）。产出的文件是委派的交付面之一，必须在进度视图里直给。
	// 只收写入类工具（write_file/edit_file/...）的 path：读文件不是产出（读了哪些文件
	// 数量级完全不同，混进来会让「产出」失去意义）。
	Files     []string    `json:"files,omitempty"`
	Status    string      `json:"status,omitempty"` // result/status：completed|failed|interrupted|abandoned
	Error     string      `json:"error,omitempty"`
	Usage     *core.Usage `json:"usage,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
}

// TaskJournal 单任务 journal 写入器。单写者模型下内部仅需一把锁顺序化 Append。
// nil 接收者安全（Append/Close 返回错误/no-op），便于「未启用落盘」降级路径零分支调用。
type TaskJournal struct {
	mu sync.Mutex
	f  *os.File
}

// OpenTaskJournal 打开（必要时创建）journal 文件：O_APPEND 追加语义 —— 同路径重复打开
// （进程重启后 task-N 复用文件名，D7/E6）不截断旧内容，两段生命周期记录共存、以 Ts 区分。
func OpenTaskJournal(path string) (*TaskJournal, error) {
	if path == "" {
		return nil, errors.New("journal: empty path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("journal: mkdir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("journal: open %s: %w", path, err)
	}
	return &TaskJournal{f: f}, nil
}

// Append 追加一条记录：marshal 整行 + 单次 Write（O_APPEND 下偏移推进原子，
// 读侧最多见半截行、不见交错行）。Type=="result" 时 fsync。
func (j *TaskJournal) Append(rec JournalRecord) error {
	if j == nil || j.f == nil {
		return errors.New("journal: closed")
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("journal: marshal: %w", err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return errors.New("journal: closed")
	}
	if _, err := j.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("journal: write: %w", err)
	}
	if rec.Type == "result" {
		if err := j.f.Sync(); err != nil {
			return fmt.Errorf("journal: sync result row: %w", err)
		}
	}
	return nil
}

// Close 关闭底层文件（幂等）。
func (j *TaskJournal) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

// JournalTail journal 尾部读取结果。
type JournalTail struct {
	Records []JournalRecord // 末尾 n 条合法记录（文件序）
	// Total 合法记录数：TotalExact=true 时是全量精确值（total_lines 口径），否则是
	// 读窗口内的下界（文件超过 journalTailMaxBytes 时才会出现）。
	Total      int
	TotalExact bool
}

// journalTailChunk 反向读窗口起步大小；journalTailMaxBytes 单次读取上限（超过则 Total
// 退化为窗口内计数）——正常任务 journal 远小于该上限。
const (
	journalTailChunk    = 64 << 10
	journalTailMaxBytes = 4 << 20
)

// ReadTail 读 journal 末尾 n 条合法记录 +（窗口内）合法记录总数。
//
// 实现：**从文件尾部反向分块读**（64KB 起，按需翻倍到凑够 n 条完整记录 / 读到文件头 /
// 撞上 journalTailMaxBytes），只对窗口内的行做 JSON 解析。旧实现是前向全量扫描 + 逐行
// 反序列化：单次 TaskOutput 的成本与 journal 大小线性相关（长任务 journal 到 MB 级，
// 且工具会被反复调用），而调用方真正要的只是末尾几条。
//
// 契约与旧实现一致：损坏行 / 半截行（含文件尾正在写入的那条）跳过不计；文件不存在返回
// 零值（任务刚登记尚未写盘是正常态，E4）；末尾顺序保持文件序。唯一差异是 Total 的口径
// 说明（见上，TotalExact 区分）。
func ReadTail(path string, n int) (JournalTail, error) {
	if n <= 0 {
		n = 1
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return JournalTail{}, nil
		}
		return JournalTail{}, fmt.Errorf("journal tail %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return JournalTail{}, fmt.Errorf("journal stat %s: %w", path, err)
	}
	size := st.Size()
	if size == 0 {
		return JournalTail{}, nil
	}

	window := int64(journalTailChunk)
	for {
		if window > size {
			window = size
		}
		start := size - window
		exact := start == 0
		buf := make([]byte, window)
		if _, rerr := f.ReadAt(buf, start); rerr != nil && !errors.Is(rerr, io.EOF) {
			return JournalTail{}, fmt.Errorf("journal read %s: %w", path, rerr)
		}
		// 窗口起点落在行中间（start>0）→ 丢掉首段半截行，避免被误当成损坏记录统计
		if start > 0 {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			} else {
				buf = nil // 整窗口都在一条超长行内 → 继续扩大窗口
			}
		}
		recs, total := parseJournalWindow(buf, n)
		if len(recs) >= n || exact || window >= journalTailMaxBytes {
			return JournalTail{Records: recs, Total: total, TotalExact: exact}, nil
		}
		window *= 2
	}
}

// ReadAll 读 journal 全部合法记录（文件序）。与 ReadTail 的分工：Tail 服务「进度感知」
// （只需末尾几条、成本随文件大小线性），All 服务「恢复运行」（agent_resume 要用完整轮次
// 重建交接简报，缺头部等于丢掉任务原始意图）。损坏行/半截行同样跳过（同一判据）。
// 文件不存在返回空切片（调用方按「无 journal」处理，不是错误）。
func ReadAll(path string) ([]JournalRecord, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("journal read all %s: %w", path, err)
	}
	out := make([]JournalRecord, 0, 16)
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var rec JournalRecord
		if json.Unmarshal(trimmed, &rec) == nil && journalRecordType(rec.Type) {
			out = append(out, rec)
		}
	}
	return out, nil
}

// parseJournalWindow 解析窗口内的完整行：返回末尾 n 条合法记录（文件序）与窗口内合法
// 记录数。合法 = 可 JSON 解析且带 Type（与旧实现同判据：`{}` 之类视为损坏行）。
func parseJournalWindow(buf []byte, n int) ([]JournalRecord, int) {
	window := make([]JournalRecord, n)
	size, pos, total := 0, 0, 0
	for len(buf) > 0 {
		var line []byte
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			line, buf = buf[:i], buf[i+1:]
		} else {
			line, buf = buf, nil
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		var rec JournalRecord
		if json.Unmarshal(trimmed, &rec) == nil && journalRecordType(rec.Type) {
			total++
			window[pos] = rec
			pos = (pos + 1) % n
			if size < n {
				size++
			}
		}
	}
	out := make([]JournalRecord, 0, size)
	for i := 0; i < size; i++ {
		out = append(out, window[(pos-size+i+n*2)%n]) // +n*2 防负（size≤n，+n 已足；双倍保险）
	}
	return out, total
}

// ── 按行查询（TaskOutput mode=lines）──────────────────────────────────────────
//
// 子任务的那个 .jsonl **同时**是：进度日志（start/turn/result 行）、全量消息历史
// （history 行，含工具参数与结果全文）、上下文快照（state 行）。默认视图只给「进度 +
// 产出 + 文件」——那是主 Agent 每次都要的；**细节要看时**它应按行自己去取，而不是由
// TaskOutput 每次把整份大 JSON 塞进上下文（实测一条 turn 记录的 usage 字段就够占满一屏，
// 而 history 单行可达 MB 级）。
//
// 因此这里提供「按行区间读原始行」：行号就是**物理行号**（1-based，与 wc -l 口径一致），
// 读侧容忍半截行/损坏行（返回 raw 预览而不报错），并对每行做长度截断 —— 一行是 12 字节
// 还是 12MB，都不能让一次工具调用把上下文打爆。

// LineRangeCap 单次按行查询的行数上限（防「一次拉 5000 行」）。
const LineRangeCap = 200

// JournalLine 一行原始记录（按行查询的返回项）。
type JournalLine struct {
	Line int    `json:"line"`           // 物理行号（1-based）
	Type string `json:"type,omitempty"` // start|turn|result|status|history|state|(空=无法解析)
	// 以下为 journal 记录行（start/turn/result）的结构化投影 —— 与默认视图同一口径，
	// 但不含 usage（token/成本不在工具输出里出现）。
	Round     int      `json:"round,omitempty"`
	Text      string   `json:"text,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Files     []string `json:"files,omitempty"`
	Status    string   `json:"status,omitempty"`
	Error     string   `json:"error,omitempty"`
	Bytes     int      `json:"bytes"`
	Truncated bool     `json:"truncated,omitempty"`
	// History/state 行（以及解析不出结构的行）只给截断预览：它们不是给人读的进度，
	// 逐字段展开会把整条消息（含工具结果全文）铺满上下文。
	Preview string `json:"preview,omitempty"`
}

// LineRange 一次按行查询的结果。
type LineRange struct {
	From     int           `json:"from"`
	To       int           `json:"to"`
	Total    int           `json:"total_lines"`
	Lines    []JournalLine `json:"lines"`
	MaxChars int           `json:"max_line_chars"`
}

// ReadLineRange 读 [from,to] 区间（1-based，闭区间）的物理行。
//
// 实现：从文件头顺序扫（行号无法随机定位），逐行切分；读完整个文件以给出精确
// total_lines（对 MB 级文件 << 100ms，且只在 model 显式按行查时发生）。
// 每行内容截断到 maxChars（rune），截断标记 Truncated。
func ReadLineRange(path string, from, to, maxChars int) (LineRange, error) {
	if path == "" {
		return LineRange{}, errors.New("journal: empty path")
	}
	if from < 1 {
		from = 1
	}
	if to < from {
		to = from
	}
	if to-from+1 > LineRangeCap {
		to = from + LineRangeCap - 1
	}
	if maxChars <= 0 {
		maxChars = 400
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LineRange{From: from, To: to, MaxChars: maxChars, Lines: []JournalLine{}}, nil
		}
		return LineRange{}, fmt.Errorf("journal open %s: %w", path, err)
	}
	defer f.Close()

	out := LineRange{From: from, To: to, MaxChars: maxChars, Lines: []JournalLine{}}
	r := bufio.NewReaderSize(f, 64<<10)
	n := 0
	for {
		raw, rerr := r.ReadBytes('\n')
		if len(raw) == 0 && rerr != nil {
			break
		}
		line := bytes.TrimRight(raw, "\n")
		if len(bytes.TrimSpace(line)) > 0 {
			n++
			if n >= from && n <= to {
				out.Lines = append(out.Lines, projectLine(n, line, maxChars))
			}
		}
		if rerr != nil {
			break
		}
	}
	out.Total = n
	return out, nil
}

// projectLine 把一行原始内容投影成 JournalLine：journal 记录行给结构化字段，
// 其余（history/state/损坏行）只给截断预览。
func projectLine(n int, raw []byte, maxChars int) JournalLine {
	ln := JournalLine{Line: n, Bytes: len(raw)}
	var rec JournalRecord
	if json.Unmarshal(raw, &rec) == nil && journalRecordType(rec.Type) {
		ln.Type = rec.Type
		ln.Round = rec.Round
		ln.Status = rec.Status
		ln.Error = rec.Error
		ln.Tools = rec.ToolNames
		ln.Files = rec.Files
		text, trunc := truncateRunes(rec.Text, maxChars)
		ln.Text = text
		ln.Truncated = trunc || rec.Truncated
		return ln
	}
	// 非 journal 行：只判类型 + 截断预览（history 行可能上 MB，绝不整行返回）
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &head)
	ln.Type = head.Type
	preview, trunc := truncateRunes(string(raw), maxChars)
	ln.Preview = preview
	ln.Truncated = trunc
	return ln
}

// ── 进度投影（TaskOutput 默认视图的数据源）─────────────────────────────────────
//
// 为什么不是 ReadTail/ReadAll：默认视图要的是「进度 + 产出 + 文件 + 行号索引」——
// 需要**整份扫描**（轮数、累计产出文件、start/result 行、以及每轮的物理行号），
// 但只需要保留极少数记录（末尾 N 轮 + start + result）。ReadAll 会把文件里
// history/state 那些 MB 级的全量消息**也整行反序列化并留在内存**，而这里对它们
// 只需要「知道是第几行、是什么类型」。
//
// 语义取舍：一次 O(文件) 的顺序扫描（实测 MB 级 < 100ms），换掉「每次调用都返回
// 一大坨原始 JSON」。数据量在磁盘上不动，只是不再往上下文里灌。

// JournalProgress 一次扫描得到的进度投影。
type JournalProgress struct {
	Start        *JournalRecord  // start 行（任务原始意图；无则 nil）
	Turns        []JournalRecord // 末尾 keepTurns 条 turn（文件序）
	TurnLines    []int           // 与 Turns 一一对应的物理行号（1-based）
	Result       *JournalRecord  // result 行（终态；无则 nil）
	ResultLine   int             // result 行的物理行号（0 = 无）
	Rounds       int             // turn 行总数（= 已完成决策轮数）
	Lines        int             // 物理行数（整个文件）
	Records      int             // journal 记录行数（start/turn/result/status）
	HistoryFirst int             // 第一条 history 行号（0 = 无 history）
	HistoryLast  int             // 最后一条 history 行号
	StateLast    int             // 最后一条 state 行号（当前上下文快照）
	// AllFiles 整次运行的产出文件（全部 turn 累计去重，保序）——即使只保留末尾
	// 几轮，产出清单也是全量的（那是委派的交付面，不能被 max_records 截断）。
	AllFiles []string
	// LastText 最近一段**非空**正文（全部 turn 里最后一个有文本的），连同它的轮次与行号。
	//
	// 为什么单独记：工具型子 agent 的常态是「连着几十轮只有工具调用、正文为空」——
	// 只留末尾几轮时 output 会空着，而它明明说过话（实测 84 轮里 6 轮有正文）。
	// 「它说了什么」是委派方最需要的一段，不能因为最后几轮没说话就整段消失。
	LastText      string
	LastTextRound int
	LastTextLine  int
}

// ReadProgress 顺序扫描一次 journal/会话文件，产出进度投影。keepTurns <= 0 → 全部轮次
// （受调用方上限保护）。文件不存在 → 零值（调用方按「暂无输出」处理，不是错误）。
func ReadProgress(path string, keepTurns int) (JournalProgress, error) {
	var pg JournalProgress
	if path == "" {
		return pg, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return pg, nil
		}
		return pg, fmt.Errorf("journal open %s: %w", path, err)
	}
	defer f.Close()

	r := bufio.NewReaderSize(f, 64<<10)
	for {
		raw, rerr := r.ReadBytes('\n')
		if len(raw) == 0 && rerr != nil {
			break
		}
		line := bytes.TrimSpace(raw)
		if len(line) > 0 {
			pg.Lines++
			switch t := lineType(line); t {
			case "history":
				// 全量消息行：只登记行号（按行查询时由 model 自己决定读哪几行）
				if pg.HistoryFirst == 0 {
					pg.HistoryFirst = pg.Lines
				}
				pg.HistoryLast = pg.Lines
			case "state":
				pg.StateLast = pg.Lines
			case "start", "turn", "result", "status":
				var rec JournalRecord
				if json.Unmarshal(line, &rec) != nil || !journalRecordType(rec.Type) {
					break
				}
				pg.Records++
				switch rec.Type {
				case "start":
					if pg.Start == nil {
						rc := rec
						pg.Start = &rc
					}
				case "turn":
					pg.Rounds++
					pg.Turns = append(pg.Turns, rec)
					pg.TurnLines = append(pg.TurnLines, pg.Lines)
					if strings.TrimSpace(rec.Text) != "" {
						pg.LastText, pg.LastTextRound, pg.LastTextLine = rec.Text, rec.Round, pg.Lines
					}
					for _, f := range rec.Files {
						if !pg.hasFile(f) {
							pg.AllFiles = append(pg.AllFiles, f)
						}
					}
				case "result":
					rc := rec
					pg.Result = &rc
					pg.ResultLine = pg.Lines
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	if keepTurns > 0 && len(pg.Turns) > keepTurns {
		pg.Turns = pg.Turns[len(pg.Turns)-keepTurns:]
		pg.TurnLines = pg.TurnLines[len(pg.TurnLines)-keepTurns:]
	}
	return pg, nil
}

// hasFile 产出文件去重判据（线性扫：清单通常只有个位数，map 反而更慢更啰嗦）。
func (pg *JournalProgress) hasFile(path string) bool {
	for _, f := range pg.AllFiles {
		if f == path {
			return true
		}
	}
	return false
}

// lineType 快速读出一行的 type 字段（不整行反序列化 —— history 行可达 MB 级）。
// 本文件的写法固定为 `{"type":"<t>",...`（session store + TaskJournal 同一格式）；
// 形态不符时退回轻量解析，仍失败 → ""（损坏行）。
func lineType(line []byte) string {
	const pre = `{"type":"`
	if bytes.HasPrefix(line, []byte(pre)) {
		rest := line[len(pre):]
		if i := bytes.IndexByte(rest, '"'); i > 0 {
			return string(rest[:i])
		}
		return ""
	}
	var h struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &h) == nil {
		return h.Type
	}
	return ""
}
