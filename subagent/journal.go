package subagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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

// turnTextCap turn 记录文本上限（runes）。D6：journal 用于进度感知，超长截断；
// 全量文本留在子上下文与完成推送里。
const turnTextCap = 4000

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

// JournalRecord 子任务输出日志的一行记录（按 Type 区分）：
//   - start：任务启动（Name/Task 摘要由 runBackground 填入 Text 等通用字段）
//   - turn：一轮 LLM 决策进度（Round 序号 + 截断文本 + 工具名列表；不含压缩轮，
//     压缩器不经 handler 发 events.LLMEnd —— 见文档 §1.4-1）
//   - result：终态（Status/Final/Error/Usage；唯一 fsync 行）
//   - status：中间状态翻转预留位（本期不发）
type JournalRecord struct {
	Type        string      `json:"type"`
	Ts          time.Time   `json:"ts"`
	TaskId      string      `json:"task_id"`
	Name        string      `json:"name,omitempty"`          // start：任务短标签
	ParentRunID string      `json:"parent_run_id,omitempty"` // start：发起方运行 RunId（嵌套派生区分层级，§3.7）
	Depth       int         `json:"depth,omitempty"`         // start：运行深度
	Round       int         `json:"round,omitempty"`         // turn：第几轮 LLM 决策（1-based）
	Text        string      `json:"text,omitempty"`          // turn：本轮内容（截断）；result：最终回复
	ToolNames   []string    `json:"tool_names,omitempty"`    // turn：本轮工具名列表
	Status      string      `json:"status,omitempty"`        // result/status：completed|failed|interrupted|abandoned
	Error       string      `json:"error,omitempty"`
	Usage       *core.Usage `json:"usage,omitempty"`
	Truncated   bool        `json:"truncated,omitempty"`
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
		if json.Unmarshal(trimmed, &rec) == nil && rec.Type != "" {
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
