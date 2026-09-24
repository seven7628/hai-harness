package cron

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Run 账本里的单次运行记录（cron_runs.jsonl，只增不改，留审计）。
type Run struct {
	RunID        string    `json:"run_id"`
	JobID        string    `json:"job_id"`
	Cron         string    `json:"cron"`
	Prompt       string    `json:"prompt"`
	FiredAt      time.Time `json:"fired_at"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`    // 本次派生的新会话
	WorkspaceKey string    `json:"workspace_key,omitempty"` // 触发时所在工作区的 workspaceKey（独立历史路径维度）
	Status       RunStatus `json:"status"`
	Error        string    `json:"error,omitempty"`
	DurationMS   int64     `json:"duration_ms,omitempty"`
	FinalResult  string    `json:"final_result,omitempty"` // 最终结果摘要（agent_end 时写入）
}

// Ledger 每次运行账本：JSONL 追加写，仅追加不改写。
type Ledger struct {
	mu   sync.Mutex
	file string
	f    *os.File
	next int
}

// NewLedger 打开（或创建）账本文件；写入失败时静默降级（不影响主流程）。
func NewLedger(path string) *Ledger {
	l := &Ledger{file: path, next: 1}
	if path == "" {
		return l
	}
	if err := os.MkdirAll(dirOf(path), 0o755); err != nil {
		return l
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return l
	}
	l.f = f
	// 从已存在行推算下一个 run_id 序号
	_ = l.scanNext()
	return l
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// scanNext 读已有行，更新 next（最坏整文件扫一遍；账本通常小而快）。
func (l *Ledger) scanNext() error {
	f, err := os.Open(l.file)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Run
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			if n := idNum(r.RunID); n >= l.next {
				l.next = n + 1
			}
		}
	}
	return sc.Err()
}

// AppendStart 触发时入账一条 pending 记录，分配 run_id，返回对应 Run。
func (l *Ledger) AppendStart(job *Job, wsKey string, now time.Time) *Run {
	r := &Run{
		RunID:        l.nextRunID(),
		JobID:        job.ID,
		Cron:         job.Cron,
		Prompt:       job.Prompt,
		FiredAt:      now,
		WorkspaceKey: wsKey,
		Status:       StatusPending,
	}
	l.append(r)
	return r
}

// AppendSkipped 记录一次「因功能关闭而跳过」的运行（不触发、不删除任务）。
func (l *Ledger) AppendSkipped(job *Job, wsKey string, now time.Time) *Run {
	r := &Run{
		RunID:        l.nextRunID(),
		JobID:        job.ID,
		Cron:         job.Cron,
		Prompt:       job.Prompt,
		FiredAt:      now,
		WorkspaceKey: wsKey,
		Status:       StatusSkipped,
	}
	l.append(r)
	return r
}

// Start 把一条 pending 提升为 running 并绑定派生会话。
func (l *Ledger) Start(runID, sessionID string, now time.Time) {
	l.append(&Run{RunID: runID, SessionID: sessionID, FiredAt: now, Status: StatusRunning})
}

// Finish 结束一条运行：写入最终状态/耗时/错误/结果摘要。
func (l *Ledger) Finish(runID string, status RunStatus, errMsg, final string, started time.Time, now time.Time) {
	r := &Run{
		RunID:       runID,
		Status:      status,
		Error:       errMsg,
		FinalResult: final,
		FiredAt:     started,
		FinishedAt:  now,
		DurationMS:  now.Sub(started).Milliseconds(),
	}
	l.append(r)
}

func (l *Ledger) nextRunID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.next
	l.next++
	return "r" + itoa(id)
}

func (l *Ledger) append(r *Run) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return // 无文件（纯内存/IPC）→ 丢弃记录
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	_, _ = l.f.Write(append(b, '\n'))
}

// Close 关闭账本文件（宿主退出时）。
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		return l.f.Close()
	}
	return nil
}

// ReadAll 读取账本全部运行记录（按写入顺序）。用于清理扫描与桌面页历史展示。
// 无文件 / 读取失败返回空。
func (l *Ledger) ReadAll() []Run {
	l.mu.Lock()
	path := l.file
	l.mu.Unlock()
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Run
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Run
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// LatestRuns 折叠账本为「每次运行一条」：同一 run_id 的多行（pending → running →
// 最终状态）合并为一条——状态/耗时/结果取最后一行，JobID/SessionID/WorkspaceKey 等
// 基础字段取各行的非空值（Start/Finish 行只写部分字段，需跨行补全）。按 FiredAt 倒序
// （最新执行在前）。桌面「执行历史」列表与详情元数据的数据源；无文件/读取失败返回 nil。
func (l *Ledger) LatestRuns(jobID string) []Run {
	rows := l.ReadAll()
	merged := map[string]Run{}
	var order []string
	for _, r := range rows {
		cur, seen := merged[r.RunID]
		if !seen {
			order = append(order, r.RunID)
			cur = Run{RunID: r.RunID}
		}
		// 逐字段合并：非零值覆盖（后行优先；基础字段取最后出现的非空值）
		if r.JobID != "" {
			cur.JobID = r.JobID
		}
		if r.Cron != "" {
			cur.Cron = r.Cron
		}
		if r.Prompt != "" {
			cur.Prompt = r.Prompt
		}
		if !r.FiredAt.IsZero() {
			cur.FiredAt = r.FiredAt
		}
		if !r.FinishedAt.IsZero() {
			cur.FinishedAt = r.FinishedAt
		}
		if r.SessionID != "" {
			cur.SessionID = r.SessionID
		}
		if r.WorkspaceKey != "" {
			cur.WorkspaceKey = r.WorkspaceKey
		}
		if r.Status != "" {
			cur.Status = r.Status
		}
		if r.Error != "" {
			cur.Error = r.Error
		}
		if r.DurationMS != 0 {
			cur.DurationMS = r.DurationMS
		}
		if r.FinalResult != "" {
			cur.FinalResult = r.FinalResult
		}
		merged[r.RunID] = cur
	}
	out := make([]Run, 0, len(order))
	for _, id := range order {
		// 先合并再按 jobID 过滤：Start/Finish 行缺 JobID，须跨行补全后才能过滤
		if jobID != "" && merged[id].JobID != jobID {
			continue
		}
		out = append(out, merged[id])
	}
	// 倒序：最新触发在前（ReadAll 为写入序，同 run 多行在折叠后顺序 = 首次入账序 = fired_at 序）
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
