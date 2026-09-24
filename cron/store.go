package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// RunStatus 一次触发的运行状态（账本 cron_runs.jsonl）。
type RunStatus string

const (
	StatusPending RunStatus = "pending" // 到点已入账、待投递
	StatusRunning RunStatus = "running" // 已新建 Session 开始执行
	StatusSuccess RunStatus = "success" // 执行成功（agent_end 正常）
	StatusError   RunStatus = "error"   // 执行异常/失败
	StatusSkipped RunStatus = "skipped" // 定时任务功能关闭，本次跳过（不触发、不删除）
	StatusTimeout RunStatus = "timeout" // 执行超时
)

// Job 一条定时任务定义（持久化于 ~/.go-code/cron.json）。
type Job struct {
	ID        string    `json:"id"`
	Cron      string    `json:"cron"`
	Prompt    string    `json:"prompt"`
	Recurring bool      `json:"recurring"` // false = 一次性
	Paused    bool      `json:"paused"`    // true = 暂停（到点不触发、不计数；任务与历史保留）
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Workspace string    `json:"workspace"`            // 触发时新建 Session 所在工作区
	SessionID string    `json:"session_id,omitempty"` // 创建时所在会话（信息性；触发时新建会话）

	// IM 推送目标（可选）：运行结果推送到指定 IM 群（cron 侧选渠道+群绑定）。
	// 结构与 im.Chat 一致但独立定义——cron 包不依赖 im 包（bridge 装配时转换）。
	IMChat *IMChat `json:"im_chat,omitempty"`

	// 运行态/聚合字段（桌面端展示；Edit 保留计数）
	NextRun    time.Time  `json:"next_run,omitempty"`
	LastRun    *time.Time `json:"last_run,omitempty"`
	LastStatus RunStatus  `json:"last_status,omitempty"`
	RunCount   int        `json:"run_count"`
	ExpireAt   *time.Time `json:"expire_at,omitempty"` // 7 天过期（recurring）
}

// IMChat 定时任务 IM 推送目标（轻量结构，避免 cron 依赖 im 包）。
type IMChat struct {
	Gateway string `json:"gateway"` // feishu / telegram / …
	ChatID  string `json:"chat_id"` // 平台侧 chat id
}

// Store 进程内定时任务库（跨会话、跨工作区；加锁并发安全）。
type Store struct {
	mu   sync.Mutex
	jobs []*Job
	next int
	file string   // 持久化文件；空 = 纯内存（不落盘）
	tmp  *os.File // 追加账本的临时缓冲（ledger 用，另行管理）
	led  *Ledger  // 运行账本（可为 nil，纯内存时无账本）

	// onDelete 任务被删除时的回调（宿主用于清理该任务的独立历史目录 cron_sessions/...）。
	// 在持锁状态下调用；回调不得再调用 Store 方法（防死锁）。
	onDelete func(job *Job)
}

// OnDelete 注册任务删除回调（宿主清理独立历史目录用）。
func (s *Store) OnDelete(fn func(job *Job)) { s.onDelete = fn }

// Option 配置 Store 行为。
type Option func(*Store)

// WithFileStore 启用文件持久化（jobs 写 file，账本写 ledgerPath）。
func WithFileStore(file, ledgerPath string) Option {
	return func(s *Store) {
		s.file = file
		s.led = NewLedger(ledgerPath)
	}
}

// New 新建定时任务库。
func New(opts ...Option) *Store {
	s := &Store{next: 1}
	for _, o := range opts {
		o(s)
	}
	// 启动时若持久化存在则恢复
	if s.file != "" {
		s.restoreFromFile()
	}
	return s
}

// EnableLedger 绑定（或替换）运行账本。
func (s *Store) EnableLedger(l *Ledger) { s.led = l }

// Ledger 返回绑定（或 nil）的运行账本，供宿主调度器写每次运行状态。
func (s *Store) Ledger() *Ledger { return s.led }

// SpecOf 编译某 job 的 cron；job 非法则该工具已拦截，此处容错返回 nil。
func (s *Store) SpecOf(job *Job) *Spec {
	sp, _ := Parse(job.Cron)
	return sp
}

// Add 新增任务；one-shot（recurring=false）需已 pinned 分/时/日/月。
// workspace 为触发时新建 Session 的工作区；sid 为创建时所在会话（信息性）。
func (s *Store) Add(cronExpr, prompt string, recurring bool, workspace, sid string, now time.Time) (*Job, error) {
	if err := validateJob(cronExpr, prompt); err != nil {
		return nil, err
	}
	sp, _ := Parse(cronExpr)
	next, ok := sp.NextAfter(now)
	if !ok {
		return nil, fmt.Errorf("cron: 表达式在未来无可达触发时间: %q", cronExpr)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job := &Job{
		ID:        fmt.Sprintf("c%d", s.next),
		Cron:      cronExpr,
		Prompt:    prompt,
		Recurring: recurring,
		CreatedAt: now,
		Workspace: workspace,
		SessionID: sid,
		NextRun:   next,
	}
	if recurring {
		exp := now.Add(defaultExpiry)
		job.ExpireAt = &exp
	}
	s.next++
	s.jobs = append(s.jobs, job)
	s.persistLocked()
	return johnClone(job), nil
}

// Get 按 id 取任务（nil 若不存在）。
func (s *Store) Get(id string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return johnClone(j)
		}
	}
	return nil
}

// List 返回全部任务副本（按创建顺序）。
func (s *Store) List() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, johnClone(j))
	}
	return out
}

// Update 更新任务（prompt/cron 传 nil=不变；recurring 传 nil=不变；paused 传 nil=不变）。
// cron 变更时从 now 重算 NextRun；ID 不变（运行历史/计数保留）。
func (s *Store) Update(id string, prompt *string, cronExpr *string, recurring *bool, paused *bool, now time.Time) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var target *Job
	for _, j := range s.jobs {
		if j.ID == id {
			target = j
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("cron: 任务 %q 不存在", id)
	}
	expr := target.Cron
	if cronExpr != nil {
		expr = *cronExpr
		if err := validateJob(expr, *promptOr(target.Prompt, prompt)); err != nil {
			return nil, err
		}
	}
	newPrompt := target.Prompt
	if prompt != nil {
		newPrompt = *prompt
		if err := validateJob(target.Cron, newPrompt); err != nil {
			return nil, err
		}
	}
	if cronExpr != nil {
		sp, _ := Parse(expr)
		next, ok := sp.NextAfter(now)
		if !ok {
			return nil, fmt.Errorf("cron: 表达式在未来无可达触发时间: %q", expr)
		}
		target.NextRun = next
		target.Cron = expr
	}
	if prompt != nil {
		target.Prompt = newPrompt
	}
	if recurring != nil {
		target.Recurring = *recurring
	}
	if paused != nil {
		target.Paused = *paused
	}
	target.UpdatedAt = now
	s.persistLocked()
	return johnClone(target), nil
}

// SetIMChat 设置/清除任务的 IM 推送目标（nil = 清除绑定）。返回更新后的任务。
// 内部深拷贝：调用方后续修改传入对象不影响存储。
func (s *Store) SetIMChat(id string, imChat *IMChat) (*Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var target *Job
	for _, j := range s.jobs {
		if j.ID == id {
			target = j
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("cron: 任务 %q 不存在", id)
	}
	if imChat == nil {
		target.IMChat = nil
	} else {
		ic := *imChat
		target.IMChat = &ic
	}
	target.UpdatedAt = time.Now()
	s.persistLocked()
	return johnClone(target), nil
}

// Delete 删除任务：从活跃库移除（停止触发）；触发 onDelete 回调
// （宿主据此清理该任务的独立历史目录 cron_sessions/<wsKey>/<jobID>/）。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.jobs {
		if j.ID == id {
			s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
			s.persistLocked()
			if s.onDelete != nil {
				s.onDelete(j)
			}
			return nil
		}
	}
	return fmt.Errorf("cron: 任务 %q 不存在", id)
}

// Advance 调度器推进某个 job 的调度态（到点/跳过/一次性自删/过期自删）。
// fired=true：本次真的触发（按 id 回写内部任务，更新 next_run/run_count/last）。
// fired=false：功能关闭跳过触发（标记 last=skipped，不推进 next、不计数、不删除）。
// 返回 (clone, shouldDelete)：shouldDelete=true 表示一次性已触发或 recurring 已过期，
// 宿主随后按需 Delete。线程安全并落盘。
func (s *Store) Advance(id string, fired bool, now time.Time) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var target *Job
	for _, j := range s.jobs {
		if j.ID == id {
			target = j
			break
		}
	}
	if target == nil {
		return nil, false
	}
	if !fired { // 跳过：保留定义与计数，但把 next 推到下一个未来匹配（避免每 tick 重复 skipped）
		target.LastRun = &now
		target.LastStatus = StatusSkipped
		if sp, err := Parse(target.Cron); err == nil {
			if next, ok2 := sp.NextAfter(now); ok2 {
				target.NextRun = next
			}
		}
		s.persistLocked()
		return johnClone(target), false
	}
	target.RunCount++
	target.LastRun = &now
	target.LastStatus = StatusRunning
	if sp, err := Parse(target.Cron); err == nil {
		if next, ok2 := sp.NextAfter(now); ok2 {
			target.NextRun = next
		}
	}
	if !target.Recurring {
		s.persistLocked()
		return johnClone(target), true // 一次性：本次触发后删除
	}
	if target.ExpireAt != nil && !now.Before(*target.ExpireAt) {
		s.persistLocked()
		return johnClone(target), true // 7 天过期：最后触发一次后删除
	}
	s.persistLocked()
	return johnClone(target), false
}

// MarkFinished 运行结束后回写任务状态（LastStatus/LastRun）：
// Advance 触发时置 LastStatus=running，结束后必须由宿主调用本方法更新为最终
// 状态（success/error/timeout），否则任务列表会永远显示「执行中」。
func (s *Store) MarkFinished(id string, status RunStatus, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.LastStatus = status
			t := now
			j.LastRun = &t
			s.persistLocked()
			return
		}
	}
}

// Persist 强制落盘（调度器批量推进后兜底调用）。纯内存 store（无文件）为 no-op。
func (s *Store) Persist() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistLocked()
}

// SetNextRun 精确拨动某任务的下次触发时间（宿主调试/恢复用；如重启后按需对齐）。
// 仅供精确控制调度时使用；常规编辑走 Update（由 cron 自动重算）。
func (s *Store) SetNextRun(id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			j.NextRun = at
			s.persistLocked()
			return nil
		}
	}
	return fmt.Errorf("cron: 任务 %q 不存在", id)
}

// persistLocked 原子写 cron.json（调用方须持锁）。
func (s *Store) persistLocked() {
	if s.file == "" {
		return
	}
	b, err := json.MarshalIndent(s.jobs, "", "  ")
	if err != nil {
		return
	}
	atomicWrite(s.file, b)
}

// restoreFromFile 启动时从 cron.json 恢复任务定义。
func (s *Store) restoreFromFile() {
	raw, err := os.ReadFile(s.file)
	if err != nil {
		return
	}
	var jobs []*Job
	if json.Unmarshal(raw, &jobs) != nil {
		return
	}
	s.jobs = jobs
	s.next = 1
	for _, j := range s.jobs {
		if n := idNum(j.ID); n >= s.next {
			s.next = n + 1
		}
	}
}

// atomicWrite 写临时文件后原子改名（避免半写）。
func atomicWrite(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cron-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func idNum(id string) int {
	n := 0
	for _, c := range id {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}

func johnClone(j *Job) *Job {
	if j == nil {
		return nil
	}
	c := *j
	if j.IMChat != nil {
		ic := *j.IMChat
		c.IMChat = &ic
	}
	return &c
}

func promptOr(base string, p *string) *string {
	if p != nil {
		return p
	}
	return &base
}

func validateJob(cronExpr, prompt string) error {
	if cronExpr == "" {
		return fmt.Errorf("cron: 表达式不能为空")
	}
	if prompt == "" {
		return fmt.Errorf("cron: prompt 不能为空")
	}
	_, err := Parse(cronExpr)
	return err
}
