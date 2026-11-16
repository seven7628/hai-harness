package main

// Dream 调度器：到点用**内置 Agent 运行时**巡检应用内全部记忆库。
//
// 触发方式与 cron 逐字同一条链路（createOpts 派生会话 → Ask(任务书) → wakeRun）：
// Dream 要的是"模型自己看证据、自己裁决"，脚本做不了这件事——证据的解读需要模型，
// 而归档的执行留在宿主（memory_dream 的护栏）。所以这里只负责"什么时候起一个会话"，
// 怎么巡检写在 dreamPrompt() 里，怎么护栏写在 memory_dream 里。
//
// 与 cron 的差别只有三处：
//  1. 触发判定不是任务表，而是一条全局设置（enabled + 每天 HH:MM + LastRunDate）；
//  2. 作用域是**全应用**，不是某个工作区（见 dreamWorkspaceDir 的取舍）；
//  3. 运行记录写 ~/.go-code/dream_runs.jsonl，供设置页展示"每次都干了什么"。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// dreamPollInterval 轮询间隔。30 秒的理由与 cron 一致：跨过 HH:MM 之后最多晚
// 半分钟起跑，用户感知不到；而更密的轮询只是白读一次小 JSON。
const dreamPollInterval = 30 * time.Second

// startDreamScheduler 启动调度 goroutine（进程生命周期内常驻，与 startCron 同位）。
//
// 刻意不做"启动即补跑"：桥在 23:50 启动、03:00 的设置没到点就不该跑；
// 昨天错过的巡检也不该在今晚之前追一次（记忆不是账本，晚一天没有后果）。
func (m *manager) startDreamScheduler() {
	m.dreamCtx, m.dreamCancel = context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(dreamPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.dreamTick(time.Now())
			case <-m.dreamCtx.Done():
				return
			}
		}
	}()
}

// stopDreamScheduler 停止调度 goroutine（宿主退出时）。
func (m *manager) stopDreamScheduler() {
	if m.dreamCancel != nil {
		m.dreamCancel()
	}
}

// dreamTick 一轮判定：到点就起一次巡检。
//
// 判定全部交给 dreamDue（纯函数，可测）：开关 / 今天跑过没有 / 到点没有。
func (m *manager) dreamTick(now time.Time) {
	s := loadDreamSettings()
	if !dreamDue(s, now) {
		return
	}
	if _, err := m.fireDream(s, now); err != nil {
		// 到点却没跑成：不把 LastRunDate 回滚，否则 30 秒后又会撞一次同一个错误。
		// 设置页的运行记录里能看到这条错误（StartedAt 有、FinishedAt 也有）。
		fmt.Fprintf(os.Stderr, "dream: 本轮巡检未能启动: %v\n", err)
	}
}

// fireDream 起一次巡检：写"今天跑过" → 建派生会话 → 记录 → 投任务书。
//
// 顺序是刻意的：**先写 LastRunDate 再跑**。一次巡检可能跑十几分钟，而 ticker
// 30 秒一跳；先跑后写的话，长运行期间每一跳都会再起一个会话（模型账单 × N）。
func (m *manager) fireDream(s dreamSettings, now time.Time) (*bridgeSession, error) {
	// 记忆关着就没有"要维护的记忆库"：巡检会变成模型对着空目录瞎转。
	if m.memoryConfig().Disabled() {
		return nil, fmt.Errorf("项目记忆已关闭，没有需要巡检的记忆库")
	}
	// 串行化两条触发路径（ticker / 设置页「立即运行」）：两次巡检同时写同一批
	// 记忆库，就是两份互相看不见的证据 + 两份归档决策。
	if !m.dreamAcquire() {
		return nil, fmt.Errorf("Dream 正在运行中，等这一轮结束再试")
	}
	// 占位在锁外写：dream_set 也可能同时在写同一个文件（用户正在改设置）。
	// 只覆盖 LastRunDate 一个字段，其余以磁盘当前值为准。
	cur := loadDreamSettings()
	cur.LastRunDate = now.Format("2006-01-02")
	if err := saveDreamSettings(cur); err != nil {
		m.dreamRelease()
		return nil, fmt.Errorf("写入 Dream 运行日期失败: %w", err)
	}

	sid := m.genID()
	dir := filepath.Join(dreamSessionsRoot(), sid)
	if err := os.MkdirAll(dir, 0o700); err != nil { // 派生会话含记忆正文，0700
		m.dreamRelease()
		appendDreamRun(dreamRun{StartedAt: now, FinishedAt: time.Now(), Provider: s.Provider, Model: s.Model, Error: err.Error()})
		return nil, fmt.Errorf("创建 Dream 会话目录失败: %w", err)
	}
	wsPath := dreamWorkspaceDir()
	bs, err := m.createOpts(wsPath, sid, "", sessionOpts{
		storeDir:  dir,
		eventsDir: dir,
		cronEvDir: dir, // 事件日志独立目录（cron 语义别名，见 sessionOpts）
		wsKey:     workspaceKey(wsPath),
		noRecord:  true, // 触发即跑、跑完归档：无需 checkpoint 崩溃恢复
	})
	if err != nil {
		m.dreamRelease()
		appendDreamRun(dreamRun{StartedAt: now, FinishedAt: time.Now(), Provider: s.Provider, Model: s.Model, Error: err.Error()})
		return nil, err
	}

	// 设置里指定的 provider/model 覆盖派生会话的默认装配（用户可能不想让夜间巡检
	// 用白天那个贵模型）。两者都齐才重建：只给 provider 不给模型时，buildLoop 会拿
	// 空模型名去解析窗口/上限，比"沿用默认模型"更容易出错。
	if s.Provider != "" && s.Model != "" {
		bs.editModelCfg(s.Provider, s.Model, "")
		bs.setProviderExplicit(true)
		m.rebuildLoop(bs)
	}

	bs.isDream = true // 与 cron 派生会话同类：不进会话列表、不被 workspace 级操作误伤
	bs.dreamSessionDone = func(errMsg, final string) { m.finishDreamRun(sid, errMsg, final) }

	// 先记"已开始"：巡检跑到一半进程被杀，设置页也应当看得到这一轮（FinishedAt 空）。
	appendDreamRun(dreamRun{
		StartedAt: now, Provider: s.Provider, Model: s.Model, SessionID: sid,
	})

	if err := bs.s.Ask(context.Background(), core.NewUserMessage(core.Content{
		Type: core.ContentTypeText, Content: dreamPrompt(),
	})); err != nil {
		bs.dreamSessionDone = nil
		m.dreamRelease()
		completeDreamRun(sid, time.Now(), "", err.Error())
		m.emitDreamChanged()
		return nil, fmt.Errorf("投递 Dream 任务失败: %w", err)
	}
	bs.wakeRun()
	m.emitDreamChanged() // 设置页据此把「运行中」点亮 / 刷新记录
	return bs, nil
}

// finishDreamRun 派生会话 AgentEnd 后收口（emit 尾部调用，见 main.go）。
func (m *manager) finishDreamRun(sessionID, errMsg, final string) {
	summary := dreamSummaryOf(final)
	completeDreamRun(sessionID, time.Now(), summary, errMsg)
	m.dreamRelease()
	m.emitDreamChanged()
}

// dreamSummaryOf 把模型收尾正文压成一行摘要（设置页的运行记录只显示一行）。
//
// 取前 200 字而不是第一条非空行：模型经常先来一句"已完成巡检："再分点列，
// 只要第一行就只剩这句开场白；而整段塞进设置页会把列表撑成一个日志窗。
func dreamSummaryOf(final string) string {
	s := strings.TrimSpace(final)
	if s == "" {
		return ""
	}
	// 换行压成空格：前端按单行渲染，不压会在列表里撑出多行。
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// dreamAcquire 取"正在跑"的闩；已有巡检在跑则返回 false。
func (m *manager) dreamAcquire() bool {
	m.dreamMu.Lock()
	defer m.dreamMu.Unlock()
	if m.dreamBusy {
		return false
	}
	m.dreamBusy = true
	return true
}

// dreamRelease 放开闩（AgentEnd / 触发失败两条路径都要走到）。
func (m *manager) dreamRelease() {
	m.dreamMu.Lock()
	m.dreamBusy = false
	m.dreamMu.Unlock()
}

// dreamRunning 当前是否有巡检在跑（设置页据此禁掉「立即运行」）。
func (m *manager) dreamRunning() bool {
	m.dreamMu.Lock()
	defer m.dreamMu.Unlock()
	return m.dreamBusy
}

// emitDreamChanged 通知设置页刷新（运行开始/结束各一次）。
//
// 前端只是"收到就重拉 dream_get"，不带状态：避免让事件成为第二份真相
//（记录与设置始终以 dream.json / dream_runs.jsonl 为准）。
func (m *manager) emitDreamChanged() {
	if m.out == nil {
		return
	}
	m.out.writeLine(map[string]any{"event_type": "dream_changed", "running": m.dreamRunning()})
}

// dreamSessionsRoot 派生会话存储根（~/.go-code/dream_sessions）。
func dreamSessionsRoot() string {
	return filepath.Join(appDataDir(), "dream_sessions")
}

// dreamWorkspaceDir 巡检派生会话的工作目录（固定空目录，不随用户当前项目漂移）。
//
// 取舍：Dream 的作用域是**全部**记忆库，不是当前项目。给它一个项目工作区当 cwd，
// 模型会先读那个项目的 AGENTS.md / 记忆 / 文件树，然后拿"这个项目的标准"去判断
// 别人的记忆——这正是任务书里要避免的偏移。所以用一个空目录。
//
// 为什么是固定目录而不是每次 mktemp：m.runtime(path) 按路径缓存运行态，每条新路径
// 都会装配一整套（技能表 / MCP 实例 / 配置 watcher / 探活 goroutine）。每晚换一个
// 临时目录 = 每夜泄漏一份。固定目录则进程内只装配一次。
func dreamWorkspaceDir() string {
	dir := filepath.Join(appDataDir(), "dream_workspace")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}
