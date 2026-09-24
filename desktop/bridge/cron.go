package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/cron"
	"github.com/seven7628/hai-harness/im"
	"github.com/seven7628/hai-harness/session"
)

// —— 定时任务宿主接线（desktop/bridge 层）——
// 内核 cron 包提供存储/解析/账本/调度纯逻辑；本文件把调度 goroutine、工具门控、
// host 命令、事件推送接到 bridge manager 上。触发 = 新建 Session + Ask(prompt) + wakeRun
// （与用户从对话框输入同一条执行通道，仅触发源不同）。

// startCron 初始化全局定时任务库并启动调度 goroutine（进程生命周期内常驻）。
func (m *manager) startCron() {
	m.cron = cron.New(cron.WithFileStore(homeCfgPath("cron.json"), homeCfgPath("cron_runs.jsonl")))
	// 任务删除 → 连带清理该任务的独立 sessions 历史目录（用户要求）。
	m.cron.OnDelete(func(job *cron.Job) {
		if job == nil {
			return
		}
		dir := filepath.Join(cronSnapshotsRoot(), workspaceKey(job.Workspace), job.ID)
		_ = os.RemoveAll(dir)
	})
	m.cronCtx, m.cronCancel = context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(cron.PollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.cronTick(time.Now())
			case <-m.cronCtx.Done():
				return
			}
		}
	}()
}

// stopCron 停止调度 goroutine 并关闭账本文件（宿主退出时）。
func (m *manager) stopCron() {
	if m.cronCancel != nil {
		m.cronCancel()
	}
	if m.cron != nil {
		if l := m.cron.Ledger(); l != nil {
			_ = l.Close()
		}
	}
}

// cronTick 一轮调度：遍历到点任务 —— enabled 触发（新建会话投递）；disabled 跳过（不删）。
func (m *manager) cronTick(now time.Time) {
	if m.cron == nil {
		return
	}
	cs := loadCronSettings()
	led := m.cron.Ledger()
	for _, j := range cron.Due(m.cron.List(), now) {
		wsKey := workspaceKey(j.Workspace)
		if !cs.Enabled {
			job, _ := m.cron.Advance(j.ID, false, now)
			if led != nil {
				led.AppendSkipped(job, wsKey, now)
			}
			m.emitCronEvent("cron_run_status", map[string]any{
				"run_id": "", "job_id": j.ID, "status": cron.StatusSkipped, "session_id": "",
			})
			continue
		}
		runID := ""
		if led != nil {
			runID = led.AppendStart(j, wsKey, now).RunID
		}
		sid := m.genID()
		// 派生会话的会话/事件存储重定向到独立目录（~/.go-code/cron_sessions/<wsKey>/<jobID>）：
		// 不进工作区 events/sessions 命名空间 → 不出现在工作区对话历史（独立记录、本地化存储）。
		cronDir := filepath.Join(cronSnapshotsRoot(), wsKey, j.ID)
		_ = os.MkdirAll(cronDir, 0o700) // cron 会话快照含敏感内容，0700
		bs, err := m.createOpts(j.Workspace, sid, "", sessionOpts{
			storeDir:  cronDir,
			eventsDir: cronDir,
			cronEvDir: cronDir,
			wsKey:     wsKey,
			noRecord:  true, // 触发即跑、跑完归档：无需 checkpoint 崩溃恢复
		})
		if err != nil {
			_, _ = m.cron.Advance(j.ID, true, now)
			if led != nil {
				led.Finish(runID, cron.StatusError, err.Error(), "", now, time.Now())
			}
			m.emitCronEvent("cron_run_status", map[string]any{
				"run_id": runID, "job_id": j.ID, "status": cron.StatusError, "session_id": sid,
				"error": err.Error(),
			})
			continue
		}
		// 标记派生会话归属（AgentEnd 时回写账本 + 推最终状态 + 归档历史快照）
		bs.cronRun = runID
		bs.cronLedger = led
		bs.cronStore = m.cron
		bs.cronFiredAt = now
		bs.cronJobID = j.ID
		bs.isCron = true // 独立归 Cron 管理：不进 workspace 会话列表、不被 workspace 级操作误伤
		// IM 推送绑定（可选）：任务配了 im_chat → 进程内挂镜像（push），
		// 运行结果（正文/工具/流式）自动推送到指定 IM 群。不落盘——
		// 每次触发是新 session id，落盘无意义（镜像表是 im-config.json 持久字段）。
		if j.IMChat != nil && m.imBridge != nil {
			if rt := m.imPlugin.Runtime(); rt != nil {
				rt.Router.Update(imCfgWithMirror(rt.Config, sid, j.IMChat))
			}
		}
		if led != nil {
			led.Start(runID, sid, now)
		}
		_ = bs.s.Ask(context.Background(), core.NewUserMessage(core.Content{
			Type: core.ContentTypeText, Content: j.Prompt,
		}))
		bs.wakeRun()
		job, del := m.cron.Advance(j.ID, true, now)
		m.emitCronEvent("cron_fired", map[string]any{
			"run_id": runID, "job_id": j.ID, "session_id": sid,
			"prompt": job.Prompt, "cron": job.Cron, "next_run": job.NextRun,
		})
		if del {
			_ = m.cron.Delete(j.ID)
		}
	}
	// 一次性兜底落盘（各 Advance 内已落盘；此处保险）
	m.cron.Persist()
	// 自动清理超龄派生会话文件
	if cs.AutoClean {
		m.cronClean(now)
	}
}

// cronSnapshotsRoot 定时任务独立历史根目录（~/.go-code/cron_sessions）。
func cronSnapshotsRoot() string {
	return filepath.Join(appDataDir(), "cron_sessions")
}

// imCfgWithMirror 在配置副本上追加 cron 会话的 IM 镜像（push 单向），返回新配置。
// 不修改原配置（cron 镜像进程内动态，不落盘）。
func imCfgWithMirror(cfg im.Config, sessionID string, ic *cron.IMChat) im.Config {
	cfg.Mirrors = append(cfg.Mirrors, im.MirrorConfig{
		SessionID: sessionID,
		Chat:      im.Chat{Gateway: ic.Gateway, ChatID: ic.ChatID},
		Mode:      im.MirrorPush,
	})
	return cfg
}

// cronClean 按运行账本找出超龄（默认 30 天）的派生会话，删除其独立历史文件
// （cron_sessions/<wsKey>/<jobID>/ 下的会话/事件/快照文件）。
// 任务定义（cron.json）与运行账本（cron_runs.jsonl）保留不动。
// 返回实际删除的文件数（供 cron_clean 命令回报）。
func (m *manager) cronClean(now time.Time) int {
	if m.cron == nil {
		return 0
	}
	led := m.cron.Ledger()
	if led == nil {
		return 0
	}
	cfg := cron.DefaultRetention()
	root := cronSnapshotsRoot()
	cleaned := 0
	for _, r := range cron.CleanupCandidatesByRun(led.ReadAll(), now, cfg.KeepDays) {
		// r.SessionID 与 r.JobID 均来自账本（宿主生成：s-<nano>-<seq> / c<n>；路径安全）
		if err := session.ValidateSessionID(r.SessionID); err != nil {
			continue
		}
		if r.JobID == "" {
			continue
		}
		dir := filepath.Join(root, r.WorkspaceKey, r.JobID)
		if removeIfExists(filepath.Join(dir, r.SessionID+".jsonl")) {
			cleaned++
		}
		// 派生会话的子任务 journal 连带清理（统计删除的 journal 文件数）
		cleaned += removeSubagentJournalsCount(dir, r.SessionID)
	}
	return cleaned
}

// removeIfExists 删除文件（存在且成功 → true；不存在/失败 → false）。
func removeIfExists(p string) bool {
	err := os.Remove(p)
	return err == nil
}

// removeSubagentJournalsCount 删除某会话子任务 journal 目录（<storeDir>/<sid>/agents），
// 返回删除的 journal 文件数（用于清理计数）。
func removeSubagentJournalsCount(storeDir, sid string) int {
	if err := session.ValidateSessionID(sid); err != nil {
		return 0
	}
	base := subagentsBase(storeDir, sid)
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0 // 无 agents 目录（未派生子任务）→ 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// 每个子任务一个目录（含 journal 文件）；RemoveAll 成功计 1
		if os.RemoveAll(filepath.Join(base, e.Name())) == nil {
			n++
		}
	}
	return n
}

// emitCronEvent 推宿主级 cron 事件（cron_fired / cron_run_status / cron_changed）。
func (m *manager) emitCronEvent(kind string, data map[string]any) {
	ev := map[string]any{"event_type": kind}
	for k, v := range data {
		ev[k] = v
	}
	m.out.writeLine(ev)
}

// finishCronRun 由派生会话 AgentEnd 调用：回写账本最终状态 + 更新任务状态 + 推 cron_run_status。
// 消息历史不落独立快照文件——详情页实时从事件日志（cron_sessions/<wsKey>/<jobID>/<sid>.jsonl）
// 解析，事件日志即唯一历史源（天然完整，无归档时机丢消息问题）。
func (bs *bridgeSession) finishCronRun(status cron.RunStatus, errMsg, final string) {
	if bs.cronLedger != nil && bs.cronRun != "" {
		bs.cronLedger.Finish(bs.cronRun, status, errMsg, final, bs.cronFiredAt, time.Now())
	}
	// 任务状态回写：Advance 触发时置 running，这里更新为最终状态（success/error），
	// 否则任务列表的状态会永远停在「执行中」。
	if bs.cronJobID != "" && bs.cronStore != nil {
		bs.cronStore.MarkFinished(bs.cronJobID, status, time.Now())
	}
	bs.out.writeLine(map[string]any{
		"event_type": "cron_run_status", "run_id": bs.cronRun, "job_id": bs.cronJobID,
		"status": status, "session_id": bs.id, "error": errMsg, "final_result": final,
		"workspace": bs.workspace,
	})
}

// —— host 命令处理（桌面端管理页：列表 / 编辑 / 删除 / 手动清理；不经 LLM）——

// cronListData 生成 cron_list 响应数据：任务定义 + 运行态 + 功能开关。
func (m *manager) cronListData() map[string]any {
	cs := loadCronSettings()
	jobs := []map[string]any{}
	if m.cron != nil {
		for _, j := range m.cron.List() {
			jobs = append(jobs, map[string]any{
				"id": j.ID, "cron": j.Cron, "prompt": j.Prompt, "recurring": j.Recurring, "paused": j.Paused,
				"workspace": j.Workspace, "created_at": j.CreatedAt,
				"next_run": j.NextRun, "last_run": j.LastRun, "last_status": j.LastStatus,
				"run_count": j.RunCount, "expire_at": j.ExpireAt,
				"im_chat": j.IMChat,
			})
		}
	}
	return map[string]any{
		"enabled":    cs.Enabled,
		"auto_clean": cs.AutoClean,
		"jobs":       jobs,
	}
}

// cronRunsData 返回某任务（或全部）的运行记录（桌面页「执行历史」）。
// 账本对同一次运行追加多行（pending→running→最终），此处折叠为每次运行一条
// （取最终状态），并按触发时间倒序（最新执行在前）。
func (m *manager) cronRunsData(jobID string) []map[string]any {
	if m.cron == nil {
		return nil
	}
	led := m.cron.Ledger()
	if led == nil {
		return nil
	}
	var out []map[string]any
	for _, r := range led.LatestRuns(jobID) {
		out = append(out, map[string]any{
			"run_id": r.RunID, "job_id": r.JobID, "fired_at": r.FiredAt, "session_id": r.SessionID,
			"status": r.Status, "error": r.Error, "duration_ms": r.DurationMS, "final_result": r.FinalResult,
		})
	}
	return out
}

// cronRunDetailData 返回某次执行的会话历史（详情页数据源）。
// 与主对话页恢复完全同构：直接返回 cron_sessions/<wsKey>/<jobID>/<sid>.jsonl
// 事件日志聚合的 snapshot（同一事件流 → 前端 materializeSnapshot 重建视图 →
// Narrative 渲染，消息/工具卡片/思考块样式 100% 复用主对话页）。
func (m *manager) cronRunDetailData(jobID, runID string) map[string]any {
	out := map[string]any{
		"job_id": jobID, "run_id": runID,
		"snapshot": nil, "found": false,
	}
	if m.cron == nil || jobID == "" || runID == "" {
		return out
	}
	// 从账本找该运行行（拿 workspace_key 与基础元数据）——取该 run 的「最后一行」
	// （折叠后 = 最终状态/耗时/结果；避免取到 pending 中间行）。
	var row *cron.Run
	led := m.cron.Ledger()
	if led != nil {
		for _, r := range led.LatestRuns(jobID) {
			if r.RunID == runID {
				rr := r
				row = &rr
				break
			}
		}
	}
	if row == nil {
		return out
	}
	// 事件日志聚合为 snapshot（与 new_session 恢复同一数据源语义）
	if row.SessionID != "" {
		evPath := filepath.Join(cronSnapshotsRoot(), row.WorkspaceKey, jobID, row.SessionID+".jsonl")
		if snap, ok := m.buildSnapshotFromFile(evPath); ok {
			out["snapshot"] = snap
			out["found"] = true
			out["session_id"] = row.SessionID
		}
	}
	out["status"] = row.Status
	out["fired_at"] = row.FiredAt
	out["duration_ms"] = row.DurationMS
	out["final_result"] = row.FinalResult
	out["error"] = row.Error
	return out
}

// buildSnapshotFromFile 从任意事件日志文件聚合 snapshot（与 buildSnapshot 同逻辑，
// 但事件源为显式路径——cron 独立历史目录复用主对话页恢复管线）。
func (m *manager) buildSnapshotFromFile(evPath string) (map[string]any, bool) {
	return buildSnapshotFromPath(evPath)
}
