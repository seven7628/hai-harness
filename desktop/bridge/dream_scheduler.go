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

// dreamMaxRunDuration 一轮巡检的最长存活时间，超过就由看门狗强制放闩。
//
// 为什么需要（2026-11 真机验收后的健壮性缺口）：闩只在两处释放——触发失败路径
// 与 `dreamSessionDone`（挂在 AgentEnd 上）。只要有一轮没等到 AgentEnd（派生会话
// 被中断/被 panic 打死/provider 侧把 goroutine 收掉），闩就**永远**为 true，
// 之后每一夜都被"Dream 正在运行中"挡在门外，直到进程重启。实测一轮 7 个库的
// 巡检约 11 分钟，90 分钟给了 8 倍余量：正常的慢不是故障，卡住才是。
const dreamMaxRunDuration = 90 * time.Minute

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
	// 先兜底放闩，再判到点：一轮卡死的巡检不该让后面所有夜晚都排不上队。
	m.dreamWatchdog(now)
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
	// 先让闩知道"这一轮是谁"：万一没有 AgentEnd，看门狗要据此给记录收口。
	m.dreamNoteSession(sid)
	dir := filepath.Join(dreamSessionsRoot(), sid)
	if err := os.MkdirAll(dir, 0o700); err != nil { // 派生会话含记忆正文，0700
		m.dreamRelease()
		appendDreamRun(dreamRun{StartedAt: now, FinishedAt: time.Now(), Provider: s.Provider, Model: s.Model, SessionID: sid, Error: err.Error()})
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
		// 起不来的这一轮也留一条（带 FinishedAt + 错误）：用户要看见"昨晚没跑成"，
		// 而不是第二天早上对着空记录猜。
		appendDreamRun(dreamRun{StartedAt: now, FinishedAt: time.Now(), Provider: s.Provider, Model: s.Model, SessionID: sid, Error: err.Error()})
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
	// 台账：每次工具结果都记进来（repos + 时间线 + 归档事实），收口时一起取走。
	// 快照在**收口这一刻**取（不是闭包创建时）：闭包里再调 capture，才能看到
	// 最后几次工具调用——它们与 AgentEnd 只隔几条事件。
	bs.dreamTally = newDreamTally()
	bs.dreamSessionDone = func(errMsg, final string) {
		m.finishDreamRun(sid, errMsg, final, bs.dreamTally.capture())
	}

	// 先记"已开始"：巡检跑到一半进程被杀，设置页也应当看得到这一轮（FinishedAt 空）。
	appendDreamRun(dreamRun{
		StartedAt: now, Provider: s.Provider, Model: s.Model, SessionID: sid,
	})

	if err := bs.s.Ask(context.Background(), core.NewUserMessage(core.Content{
		Type: core.ContentTypeText, Content: dreamPrompt(loadDreamState()),
	})); err != nil {
		bs.dreamSessionDone = nil
		m.dreamRelease()
		completeDreamRun(sid, time.Now(), "", err.Error(), nil, dreamRunDetail{})
		m.emitDreamChanged()
		return nil, fmt.Errorf("投递 Dream 任务失败: %w", err)
	}
	bs.wakeRun()
	m.emitDreamChanged() // 设置页据此把「运行中」点亮 / 刷新记录
	return bs, nil
}

// finishDreamRun 派生会话 AgentEnd 后收口（emit 尾部调用，见 main.go）。
//
// final 是模型收尾正文的**完整版**：摘要归摘要（列表一行），正文归 Result
// （详情视图看原文，保留换行）——压成摘要就再也还原不回来了。
//
// 顺序是刻意的：先落运行记录（用户面的账，失败会体现在记录里），再写跨轮状态
// （只影响下一轮的任务书，写失败仅打 stderr）；状态写不进不该让这一轮看起来失败。
func (m *manager) finishDreamRun(sessionID, errMsg, final string, cap dreamTallyCapture) {
	summary := dreamSummaryOf(final, cap.Repos)
	completeDreamRun(sessionID, time.Now(), summary, errMsg, cap.Repos, dreamRunDetail{Result: final, Steps: cap.Steps})
	updateDreamState(cap.Repos, cap.Facts, final)
	m.dreamRelease()
	m.emitDreamChanged()
}

// dreamSummaryOf 把模型收尾正文压成一行摘要（设置页的运行记录只显示一行）。
//
// 取前 200 字而不是第一条非空行：模型经常先来一句"已完成巡检："再分点列，
// 只要第一行就只剩这句开场白；而整段塞进设置页会把列表撑成一个日志窗。
//
// 2026-11 真机：真实模型的收尾正文常常是一张 Markdown 表格
// （`| 库 | 条目 | 证据要点 | 动作 |` + `|---|---|---|---|`），压完空白后"结果"列
// 就是**一串竖线**，信息量为零。所以这里做三件事：丢掉表格行与分隔线、丢掉
// 只说话不报数的开场白、整段只剩表格时用**宿主台账**（repos：碰过哪些库、归档了几条）
// 凑一句可读的话——不去解析模型文字。
func dreamSummaryOf(final string, repos []dreamRepoResult) string {
	var body []string
	for _, line := range strings.Split(final, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || isDreamTableLine(t) {
			continue
		}
		body = append(body, t)
	}
	// 开场白被全部丢掉的极端情况（正文真的只有一句"巡检完成。"）：台账也没有时
	// 至少把原话还回去，别把用户的记录变成一片空白。
	fallback := dreamCollapse(body)
	for len(body) > 0 && isDreamPreamble(body[0]) {
		body = body[1:]
	}
	s := dreamCollapse(body)
	if s == "" {
		if t := dreamTallySummary(repos); t != "" {
			return t
		}
		s = fallback
	}
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + "…"
	}
	return s
}

// dreamCollapse 把多行压成一行（前端按单行渲染，不压会在列表里撑出多行）。
func dreamCollapse(lines []string) string {
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// isDreamTableLine Markdown 表格行 / 分隔行。
//
// 以 `|` 开头的一律算表格（`|---|`、`| a | b |` 都在内）；不带竖线的
// `---`、`:--:` 这类分隔行也算——模型写表时常把表头省掉。
func isDreamTableLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, "|") {
		return true
	}
	hasDash := false
	for _, r := range line {
		switch r {
		case '-':
			hasDash = true
		case ':', ' ', '\t', '|':
		default:
			return false
		}
	}
	return hasDash
}

// dreamPreambleMarkers 收尾正文里"只说话不报数"的开场白前缀。
var dreamPreambleMarkers = []string{
	"巡检完成", "巡检完毕", "巡检结束", "已完成巡检", "本轮巡检", "本次巡检",
	"以下是", "下面是", "这里是", "如下是", "结果如下",
}

// isDreamPreamble 判断一行是不是**纯**开场白：以这些词开头、不含数字、且不长。
//
// 含数字的（"已完成巡检：3 个库，归档 2 条"）是结论而不是开场白，必须留下——
// 那正是"结果"列该显示的东西。只掐开头连续的几句，正文里的句子不动。
func isDreamPreamble(line string) bool {
	hit := false
	for _, m := range dreamPreambleMarkers {
		if strings.HasPrefix(line, m) {
			hit = true
			break
		}
	}
	if !hit || len([]rune(line)) > 40 {
		return false
	}
	for _, r := range line {
		if r >= '0' && r <= '9' {
			return false
		}
	}
	return true
}

// dreamTallySummary 用宿主台账凑一句总结（模型正文只有表格时的回落）。
//
// 数来自 memory_dream 真的被调用过这件事（见 dream_tally.go），不是模型的自述。
func dreamTallySummary(repos []dreamRepoResult) string {
	if len(repos) == 0 {
		return ""
	}
	archived := 0
	for _, r := range repos {
		archived += r.Archived
	}
	if archived > 0 {
		return fmt.Sprintf("已处理 %d 个记忆库，归档 %d 条", len(repos), archived)
	}
	return fmt.Sprintf("已处理 %d 个记忆库，没有需要归档的条目", len(repos))
}

// dreamAcquire 取"正在跑"的闩；已有巡检在跑则返回 false。
func (m *manager) dreamAcquire() bool {
	m.dreamMu.Lock()
	defer m.dreamMu.Unlock()
	if m.dreamBusy {
		return false
	}
	m.dreamBusy = true
	m.dreamSince = time.Now()
	m.dreamRunSID = ""
	return true
}

// dreamNoteSession 记下本轮会话 id：看门狗强制放闩时要用它把记录补成"失败"。
func (m *manager) dreamNoteSession(sid string) {
	if sid == "" {
		return
	}
	m.dreamMu.Lock()
	m.dreamRunSID = sid
	m.dreamMu.Unlock()
}

// dreamRelease 放开闩（AgentEnd / 触发失败两条路径都要走到）。
func (m *manager) dreamRelease() {
	m.dreamMu.Lock()
	m.dreamBusy = false
	m.dreamSince = time.Time{}
	m.dreamRunSID = ""
	m.dreamMu.Unlock()
}

// dreamWatchdog 兜底放闩：一轮巡检超过 dreamMaxRunDuration 还没有收口，就强制释放，
// 并把那条"已开始"的记录补成失败（用户能看见"昨晚那一轮卡住了"，而不是只有一条
// 永远停在"运行中"的记录）。
//
// 刻意**不去杀**那个派生会话：卡住的原因可能是模型/provider 正在慢慢吐 token，
// 杀它除了制造一条"被中断"的记录之外没有收益；这里要修的是"闩泄漏导致后续排不上队"。
// 会话若之后自己结束，finishDreamRun 的收口是幂等的（记录已不再有 FinishedAt 为零，
// dreamRelease 也重复无害）。
func (m *manager) dreamWatchdog(now time.Time) {
	m.dreamMu.Lock()
	if !m.dreamBusy || m.dreamSince.IsZero() || now.Sub(m.dreamSince) < dreamMaxRunDuration {
		m.dreamMu.Unlock()
		return
	}
	sid, started := m.dreamRunSID, m.dreamSince
	m.dreamBusy, m.dreamSince, m.dreamRunSID = false, time.Time{}, ""
	m.dreamMu.Unlock()

	msg := fmt.Sprintf("运行超过 %s 仍未结束（%s 起跑），闩已强制释放；今天不会自动重试",
		dreamMaxRunDuration, started.Format("15:04"))
	fmt.Fprintf(os.Stderr, "dream: %s（派生会话 %s）\n", msg, sid)
	if sid != "" {
		// 看门狗收口没有正文也没有步骤（那一轮还在跑，台账归它自己）。
		completeDreamRun(sid, now, "", msg, nil, dreamRunDetail{})
	}
	m.emitDreamChanged()
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
// （记录与设置始终以 dream.json / dream_runs.jsonl 为准）。
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

// dreamWorkspaceRootOverride 测试注入（生产恒为空）：工作目录根落到临时目录。
var dreamWorkspaceRootOverride string

// dreamWorkspaceRoot 巡检工作目录的父目录。
//
// **必须落在 ~/.go-code 之外**（2026-11 真机验收教训）：沙箱与文件工具都拒绝
// `~/.go-code`（settings.json 可能含密钥），只放行 memory / plugins / runtime 三个
// 子树。工作目录放在 `~/.go-code/dream_workspace` 时，派生会话的 cwd 恰好是被拒的
// 路径，于是**每一次** bash 都以
//
//	shell-init: error retrieving current directory: getcwd: cannot access parent directories: Operation not permitted
//
// 开头，glob 直接回 "which is go-code's own config/secret directory and is protected"。
// 模型当时靠 `cd ~/.go-code/memory/repos` 硬绕过去了，但那是一轮巡检里 8 次无效
// 工具调用——不该让模型替宿主的布局买单。
//
// 用 UserCacheDir 而不是 os.TempDir：缓存目录是"按用户、可清理、放可再生数据"的
// 语义位置，正好对应这个空工作目录；/tmp 在部分平台会被定期清空（也无妨，MkdirAll
// 会重建）。路径仍然**固定**：m.runtime(path) 按路径缓存运行态（技能表 / MCP 实例 /
// 配置 watcher），每晚换路径就是每晚泄漏一份装配。
func dreamWorkspaceRoot() string {
	if dreamWorkspaceRootOverride != "" {
		return dreamWorkspaceRootOverride
	}
	if dir, err := os.UserCacheDir(); err == nil && dir != "" {
		return filepath.Join(dir, "go-code")
	}
	return filepath.Join(os.TempDir(), "go-code")
}

// dreamWorkspacePath 工作目录的绝对路径（纯函数，不建目录：装配期比较路径时用它）。
func dreamWorkspacePath() string {
	return filepath.Join(dreamWorkspaceRoot(), "dream-workspace")
}

// isDreamWorkspacePath 判断某个工作区是不是 Dream 的空工作目录。
//
// 用途：不给它注入"当前项目记忆"（见 buildLoop）。空目录没有记忆，注入只会顺手
// 在 ~/.go-code/memory/repos 下建出一个空库，把巡检清单搞脏——真机验收里模型就
// 把那个空库当成第 9 个记忆库认真处理了一遍。
func isDreamWorkspacePath(workspace string) bool {
	if strings.TrimSpace(workspace) == "" {
		return false
	}
	return filepath.Clean(workspace) == filepath.Clean(dreamWorkspacePath())
}

// dreamWorkspaceDir 巡检派生会话的工作目录（固定空目录，不随用户当前项目漂移）。
//
// 取舍：Dream 的作用域是**全部**记忆库，不是当前项目。给它一个项目工作区当 cwd，
// 模型会先读那个项目的 AGENTS.md / 记忆 / 文件树，然后拿"这个项目的标准"去判断
// 别人的记忆——这正是任务书里要避免的偏移。所以用一个空目录（位置见 dreamWorkspaceRoot）。
func dreamWorkspaceDir() string {
	dir := dreamWorkspacePath()
	_ = os.MkdirAll(dir, 0o700)
	return dir
}
