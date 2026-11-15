package main

// surface_agent_test.go —— 插件 Agent 通道的 Go 侧验收（09-agent-bridge §5.1 / §6.6 / §8，P1c）。
//
// 这个文件钉的是**协议与边界**，不是「能跑」：
//
//	1. 入口校验链的顺序与封闭 reason（not_declared / denied / quota / invalid_input / no_session）；
//	2. agent.session 造出的是 runtimePlugin 会话（P1a 装配），不造项目 runtime；
//	3. agent.run 的会话归属：跨插件/项目会话一律 denied，不给别家建会话；
//	4. 配额：并发 1、每小时上限、单 run 超时（三项都可注入，不靠睡真实时长）；
//	5. 回执先行 + 事件信封（seq 单调、surface_id/run_id/session_id 正确、终态后停投）；
//	6. agent.cancel → agent.error{cancelled}（**不发 done**）；
//	7. tool.result 只做形状 + 审计。
//
// 全部从**生产入口**进（m.dispatch → handleSurfaceAgent），断言读的是 stdout 上真正
// 写出去的那几行 JSON —— 绕过 dispatch 就等于绕过「命令名 → 判定顺序」这一整层，
// 而那层才是插件实际撞上的东西（00-conventions §4.7，与 surfaces_command_test 同一条纪律）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// ---- 装置 ----

// agentCommands agent.run 需要 manifest 声明的四个 name。
var agentCommands = []string{
	surfaceAgentSessionName, surfaceAgentRunName,
	surfaceAgentCancelName, surfaceAgentToolResult,
}

// agentPayload `surface_agent` 的 payload 形状（id 由 main 按 token 注入，测试直接给）。
func agentPayload(id, name string, args map[string]any) map[string]any {
	p := map[string]any{"id": id, "name": name}
	if args != nil {
		p["args"] = args
	}
	return p
}

// surfaceEventLines 从 stdout 行里挑出 surface_agent_event（可按 run_id 过滤）。
// 返回的每项是 `{surface_id, event}`：信封的两层都留着，方便断言归属没串。
func surfaceEventLines(lines []map[string]any, runID string) []map[string]any {
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		if l["event_type"] != surfaceAgentEventType {
			continue
		}
		d, _ := l["data"].(map[string]any)
		if d == nil {
			continue
		}
		ev, _ := d["event"].(map[string]any)
		if ev == nil {
			continue
		}
		if runID != "" && ev["run_id"] != runID {
			continue
		}
		out = append(out, map[string]any{"surface_id": d["surface_id"], "event": ev})
	}
	return out
}

// surfaceEventType 事件信封里的 type。
func surfaceEventType(item map[string]any) string {
	ev, _ := item["event"].(map[string]any)
	s, _ := ev["type"].(string)
	return s
}

// surfaceEventData 事件信封里的 data 对象。
func surfaceEventData(item map[string]any) map[string]any {
	ev, _ := item["event"].(map[string]any)
	d, _ := ev["data"].(map[string]any)
	return d
}

// waitSurfaceTerminal 轮询 stdout（不清空）直到该 run 出现终态（done/error），
// 返回那一刻已经能看到的**全部** surface 事件（顺序 = 到达顺序）。
//
// 为什么不用 readLines：这条通道的断言依赖「ack 行 < 第一条事件行」这种**顺序**，
// 边读边清会把早到的 delta 抹掉（syncBuf.Take 的原子性只保证不丢已到达的，不保证
// 你读之前没被别人读走）。
func waitSurfaceTerminal(t *testing.T, buf *syncBuf, runID string, timeout time.Duration) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		evs := surfaceEventLines(readAllLines(buf), runID)
		for _, item := range evs {
			if typ := surfaceEventType(item); typ == surfaceAgentEvDone || typ == surfaceAgentEvError {
				return evs
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到 run %s 的终态事件（已收到 %d 条）：%v", runID, len(evs), evs)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dispatchAndResp 发一条命令并取回执，**不清缓冲**。
//
// 与 pluginResp（先 Reset）的分工：agent.cancel 会在 dispatch 内同步写终态事件
// （agent.error{cancelled}），Reset 会把那条事件一起抹掉 —— 那正是要断言的证据。
func dispatchAndResp(t *testing.T, m *manager, buf *syncBuf, id int, typ string, payload map[string]any) map[string]any {
	t.Helper()
	m.dispatch(cmd(id, typ, payload))
	for _, l := range readAllLines(buf) {
		if l["event_type"] == "command_response" && intNumOf(l["id"]) == id {
			return l
		}
	}
	t.Fatalf("%s 没有 id=%d 的 command_response", typ, id)
	return nil
}

// waitSurfaceFirstEvent 等第一条 surface 事件出现，返回那一刻 stdout 上的全部行
// （不清空 —— 顺序断言要连 command_response 的行号一起看）。
func waitSurfaceFirstEvent(t *testing.T, buf *syncBuf, timeout time.Duration) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		lines := readAllLines(buf)
		if len(surfaceEventLines(lines, "")) > 0 {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到任何 surface 事件（stdout 共 %d 行）：%v", len(lines), lines)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// agentDone 在事件序列里找 agent.done，返回它的 data。
func agentDone(t *testing.T, evs []map[string]any) map[string]any {
	t.Helper()
	for _, item := range evs {
		if surfaceEventType(item) == surfaceAgentEvDone {
			return surfaceEventData(item)
		}
	}
	t.Fatalf("没有 agent.done：%v", evs)
	return nil
}

// agentError 在事件序列里找 agent.error，返回它的 data。
func agentError(t *testing.T, evs []map[string]any) map[string]any {
	t.Helper()
	for _, item := range evs {
		if surfaceEventType(item) == surfaceAgentEvError {
			return surfaceEventData(item)
		}
	}
	t.Fatalf("没有 agent.error：%v", evs)
	return nil
}

// assertSurfaceSeq 断言同一 run 的 seq 严格单调递增（1 起）。
func assertSurfaceSeq(t *testing.T, evs []map[string]any) {
	t.Helper()
	want := 1
	for i, item := range evs {
		ev, _ := item["event"].(map[string]any)
		seq, _ := ev["seq"].(float64)
		if int(seq) != want {
			t.Fatalf("第 %d 条事件 seq = %v，want %d（seq 必须每 run 单调且不跳号）：%v", i, ev["seq"], want, ev)
		}
		want++
	}
}

// useAgentLoop 把下一批 surface_agent 装配出来的会话 loop 换成假 provider
// （生产恒 nil；见 surfaceAgentAssembleHook）。
func useAgentLoop(t *testing.T, fake provider.Provider, maxIter int) {
	t.Helper()
	prev := surfaceAgentAssembleHook
	surfaceAgentAssembleHook = func(bs *bridgeSession) {
		bs.s.SetLoop(agents.NewAgentLoop(
			agents.WithProvider(fake),
			agents.WithMaxIterations(maxIter),
		))
	}
	t.Cleanup(func() { surfaceAgentAssembleHook = prev })
}

// gateProvider 起跑就停住，等测试 close(release) 才回一句话 —— 用来把一条 run
// 按在「在跑」状态上验并发闸门，而不是 sleep 猜时机。
type gateProvider struct {
	started chan struct{}
	release chan struct{}
	reply   string

	once sync.Once
}

func newGateProvider(reply string) *gateProvider {
	return &gateProvider{started: make(chan struct{}), release: make(chan struct{}), reply: reply}
}

func (p *gateProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err() // 被 Interrupt/超时取消：交回 ctx 错误（loop 会收尾成 abort）
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	return h(provider.LLMEndEvent{
		FinishReason: core.FinishReasonStop,
		Content:      p.reply,
		Model:        model,
		Usage:        core.Usage{Input: 7, Output: 3},
	})
}

// surfaceAuditRows 只取 action 属于 Agent 通道的审计行（其余是 command 等其他动作）。
func surfaceAuditRows(t *testing.T, m *manager) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range auditLines(t, m) {
		switch rec["action"] {
		case "agent_run", "tool_result":
			out = append(out, rec)
		}
	}
	return out
}

// ---- ① agent.session：runtimePlugin 会话，不造项目 runtime ----

func TestSurfaceAgentSessionCreatesPluginSession(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, agentCommands)
	home := pluginHomePath("demo")

	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentSessionName, nil))
	if resp["ok"] != true {
		t.Fatalf("agent.session 失败：%v", resp)
	}
	data := pluginRespData(t, resp)
	sid, _ := data["session_id"].(string)
	if sid == "" {
		t.Fatalf("回执没有 session_id：%v", data)
	}

	// ① 会话在插件虚拟 Project 的运行时里，kind 必须是 runtimePlugin（P1a 的装配）。
	ws := m.pluginRuntimeIfExists("demo")
	if ws == nil {
		t.Fatal("agent.session 没有装配插件运行时（应当走 pluginRuntimeForHome）")
	}
	if ws.kind != runtimePlugin || ws.pluginID != "demo" {
		t.Fatalf("runtime = kind %v id %q，want runtimePlugin/demo", ws.kind, ws.pluginID)
	}
	bs := m.get(home, sid)
	if bs == nil {
		t.Fatalf("会话 %s 不在运行态", sid)
	}
	if bs.runtime != nil && bs.runtime.kind != runtimePlugin {
		t.Fatalf("会话 runtime.kind = %v，want runtimePlugin", bs.runtime.kind)
	}
	// record 键锚在 pluginID 上（P1a §4.3 第 2 点）：这是「插件会话」与「项目会话」的分水岭。
	if bs.wsKey != "plugin-demo" {
		t.Fatalf("wsKey = %q，want plugin-demo", bs.wsKey)
	}
	// ② 存储根是 plugin-sessions/<id>/sessions，且目录真的建出来了。
	if bs.runtime.storeDir != pluginSessionsDir("demo") {
		t.Fatalf("storeDir = %q，want %q", bs.runtime.storeDir, pluginSessionsDir("demo"))
	}
	if st, err := os.Stat(pluginSessionsDir("demo")); err != nil || !st.IsDir() {
		t.Fatalf("plugin-sessions/demo/sessions 没建出来：%v", err)
	}
	// ③ 出现在**插件项目**的会话列表里（左栏插件分组读同一份数据）；
	//    空会话此刻还没有 jsonl（Session 只在有消息后 persist），所以"有记录"以会话表为准。
	var listed bool
	for _, it := range m.listSessions(home) {
		if it["id"] == sid {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("新建的插件会话没出现在 listSessions(plugin home) 里")
	}
	// ④ 绝不造项目 runtime（09 §4.4 红线）。
	m.mu.Lock()
	_, leaked := m.workspaces[home]
	projectCount := len(m.workspaces)
	m.mu.Unlock()
	if leaked || projectCount != 0 {
		t.Fatalf("agent.session 把插件虚拟 home 建成了项目 runtime：%v", m.workspaces)
	}

	// ⑤ title 可选：给了就落到手动标题覆盖（会话列表读同一份），不花 token 生成标题。
	sessionManualTitles.set(workspaceKey(home), sid, "")
	resp = pluginResp(t, m, buf, 2, "surface_agent", agentPayload("demo", surfaceAgentSessionName,
		map[string]any{"title": "课表助手"}))
	data = pluginRespData(t, resp)
	sid2, _ := data["session_id"].(string)
	if sid2 == "" {
		t.Fatalf("带 title 的 agent.session 失败：%v", resp)
	}
	if got := sessionManualTitles.get(workspaceKey(home), sid2); got != "课表助手" {
		t.Fatalf("手动标题 = %q，want 课表助手", got)
	}
}

// ---- ② agent.run 的入口校验：reason 对应 + 一条 run 都不许起 ----

func TestSurfaceAgentRunValidation(t *testing.T) {
	t.Run("未声明 → not_declared", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", true, []string{surfaceAgentSessionName}) // 只声明 session
		resp := pluginResp(t, m, buf, 1, "surface_agent",
			agentPayload("demo", surfaceAgentRunName, map[string]any{"input": "hi"}))
		assertDenied(t, resp, surfaceReasonNotDeclared)
		assertNoSurfaceRun(t, m)
	})

	t.Run("停用 → denied", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", false, agentCommands) // 装着但 enabled=false
		resp := pluginResp(t, m, buf, 1, "surface_agent",
			agentPayload("demo", surfaceAgentRunName, map[string]any{"input": "hi"}))
		assertDenied(t, resp, surfaceReasonDenied)
		assertNoSurfaceRun(t, m)
	})

	t.Run("id 形状不合法 → denied（且不碰 manifest）", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", true, agentCommands)
		for i, bad := range []string{"", "../etc", "Demo", "a"} {
			resp := pluginResp(t, m, buf, i+1, "surface_agent",
				agentPayload(bad, surfaceAgentRunName, map[string]any{"input": "hi"}))
			assertDenied(t, resp, surfaceReasonDenied)
		}
		assertNoSurfaceRun(t, m)
	})

	t.Run("宿主表外的名字 → denied（声明了也不放行）", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", true, []string{"workspace_list", "agent.sleep"})
		for i, name := range []string{"workspace_list", "agent.sleep"} {
			resp := pluginResp(t, m, buf, i+1, "surface_agent", agentPayload("demo", name, nil))
			assertDenied(t, resp, surfaceReasonDenied)
		}
		assertNoSurfaceRun(t, m)
	})

	t.Run("跨 surface 的 session → denied", func(t *testing.T) {
		m, buf := testManager(t)
		writePluginSurfaces(t, m,
			pluginSurface{id: "alpha", enabled: true, commands: agentCommands},
			pluginSurface{id: "beta", enabled: true, commands: agentCommands},
		)
		resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("alpha", surfaceAgentSessionName, nil))
		sid, _ := pluginRespData(t, resp)["session_id"].(string)
		if sid == "" {
			t.Fatalf("alpha 建会话失败：%v", resp)
		}
		// beta 拿 alpha 的 session_id 起 run：跨插件劫持，必须 denied（§8.2）。
		resp = pluginResp(t, m, buf, 2, "surface_agent",
			agentPayload("beta", surfaceAgentRunName, map[string]any{"session_id": sid, "input": "hi"}))
		assertDenied(t, resp, surfaceReasonDenied)
		// 审计里不能泄漏「这个 session 属于 alpha」。
		for _, rec := range surfaceAuditRows(t, m) {
			if rec["action"] == "agent_run" && rec["ok"] == false && rec["session"] == sid {
				if reason, _ := rec["reason"].(string); strings.Contains(reason, "alpha") {
					t.Fatalf("审计/回执泄漏了会话归属：%v", rec)
				}
			}
		}
	})

	t.Run("项目会话 → denied（不得跨 Project）", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", true, agentCommands)
		project := t.TempDir()
		projectSID := newSessionID(t, m, buf, project)
		resp := pluginResp(t, m, buf, 2, "surface_agent",
			agentPayload("demo", surfaceAgentRunName, map[string]any{"session_id": projectSID, "input": "hi"}))
		assertDenied(t, resp, surfaceReasonDenied)
	})

	t.Run("session 不存在 → no_session", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", true, agentCommands)
		resp := pluginResp(t, m, buf, 1, "surface_agent",
			agentPayload("demo", surfaceAgentRunName, map[string]any{"session_id": "s-does-not-exist", "input": "hi"}))
		assertDenied(t, resp, surfaceReasonNoSession)
		assertNoSurfaceRun(t, m)
	})

	t.Run("空 input → invalid_input", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", true, agentCommands)
		for i, input := range []string{"", "   ", "\n\t "} {
			resp := pluginResp(t, m, buf, i+1, "surface_agent",
				agentPayload("demo", surfaceAgentRunName, map[string]any{"input": input}))
			assertDenied(t, resp, surfaceReasonInvalidInput)
		}
		assertNoSurfaceRun(t, m)
	})

	t.Run("input 超 32 KiB → quota", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", true, agentCommands)
		big := strings.Repeat("a", surfaceAgentMaxInputBytes+1)
		resp := pluginResp(t, m, buf, 1, "surface_agent",
			agentPayload("demo", surfaceAgentRunName, map[string]any{"input": big}))
		assertDenied(t, resp, surfaceReasonQuota)
		assertNoSurfaceRun(t, m)
	})

	t.Run("instructions 超 8 KiB → quota", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", true, agentCommands)
		big := strings.Repeat("b", surfaceAgentMaxInstrBytes+1)
		resp := pluginResp(t, m, buf, 1, "surface_agent",
			agentPayload("demo", surfaceAgentRunName, map[string]any{"input": "hi", "instructions": big}))
		assertDenied(t, resp, surfaceReasonQuota)
		assertNoSurfaceRun(t, m)
	})

	t.Run("schema/tools 出现也不报错（P1 忽略）", func(t *testing.T) {
		m, buf := testManager(t)
		fakeSurface(t, m, "demo", true, agentCommands)
		useAgentLoop(t, &evalProvider{reply: "ok"}, 2)
		resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentRunName,
			map[string]any{
				"input":  "hi",
				"schema": map[string]any{"type": "object"},
				"tools":  []any{"course_week"},
			}))
		if resp["ok"] != true {
			t.Fatalf("P1 必须忽略 schema/tools 而不是拒绝：%v", resp)
		}
		runID, _ := pluginRespData(t, resp)["run_id"].(string)
		waitSurfaceTerminal(t, buf, runID, 5*time.Second)
	})
}

// assertDenied 拒绝回执的逐字形状：{ok:false, code:<reason>, data:{ok:false, reason:<reason>}}。
func assertDenied(t *testing.T, resp map[string]any, reason string) {
	t.Helper()
	if resp["ok"] != false {
		t.Fatalf("ok = %v，want false（reason %s）：%v", resp["ok"], reason, resp)
	}
	if resp["code"] != reason {
		t.Fatalf("code = %v，want %s：%v", resp["code"], reason, resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data == nil || data["ok"] != false || data["reason"] != reason {
		t.Fatalf("data = %v，want {ok:false, reason:%s}", data, reason)
	}
}

// assertNoSurfaceRun 一条 run 都不许起：run 表空 + 没有任何 agent.* 事件。
func assertNoSurfaceRun(t *testing.T, m *manager) {
	t.Helper()
	hub := m.surfaceAgentHub()
	hub.mu.Lock()
	runs, active := len(hub.runs), len(hub.active)
	hub.mu.Unlock()
	if runs != 0 || active != 0 {
		t.Fatalf("被拒绝的调用却登记了 run（runs=%d active=%d）", runs, active)
	}
}

// ---- ③ 配额：并发 1 / 每小时上限 / 单 run 超时 ----

func TestSurfaceAgentQuotaConcurrency(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, agentCommands)
	gate := newGateProvider("慢回复")
	useAgentLoop(t, gate, 2)
	home := pluginHomePath("demo")

	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)
	if m.get(home, sid) == nil {
		t.Fatalf("会话 %s 没建出来", sid)
	}

	// 第一条 run：provider 停住 → 一直占着「每 surface 1 条」的槽。
	m.dispatch(cmd(2, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "第一条"})))
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("第一条 run 没跑到 provider（装配没走通）")
	}

	// 第二条：并发闸门 → quota，且不能悄悄排队（§8.3「超限 quota，不排队」）。
	// 它没带 session_id —— 被拒的调用**不该顺手建一个空会话**（否则插件重试就会攒一堆）。
	before := len(m.listSessions(home))
	resp = pluginResp(t, m, buf, 3, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"input": "第二条"}))
	assertDenied(t, resp, surfaceReasonQuota)
	if after := len(m.listSessions(home)); after != before {
		t.Fatalf("被 quota 拒绝的 run 建出了新会话（%d → %d）", before, after)
	}

	// 放行第一条 → 拿到 done；槽归还后第三条应当被接受（证明释放没漏）。
	close(gate.release)
	evs := waitSurfaceTerminal(t, buf, "", 10*time.Second)
	agentDone(t, evs)

	gate2 := newGateProvider("第二条")
	useAgentLoop(t, gate2, 2)
	m.dispatch(cmd(4, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "第三条"})))
	select {
	case <-gate2.started:
	case <-time.After(10 * time.Second):
		t.Fatal("并发槽没归还 —— 第三条 run 起不来")
	}
	m.dispatch(cmd(5, "surface_agent", agentPayload("demo", surfaceAgentCancelName,
		map[string]any{"run_id": waitRunID(t, buf)})))
	close(gate2.release)
}

// TestSurfaceAgentQuotaPerHour 每小时上限：注入 perHour=2（生产 20），第三次 quota。
//
// 注入而不是跑 21 次：配额判据是「同一 surface 一小时内起过几次」，注入小值验的是同一条
// 代码路径，还不用把用例拖成秒级。
func TestSurfaceAgentQuotaPerHour(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, agentCommands)
	useAgentLoop(t, &evalProvider{reply: "ok"}, 2)

	hub := m.surfaceAgentHub()
	hub.perHour = 2
	hub.now = func() time.Time { return time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC) }

	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)

	for i := 0; i < 2; i++ {
		resp = pluginResp(t, m, buf, i+2, "surface_agent", agentPayload("demo", surfaceAgentRunName,
			map[string]any{"session_id": sid, "input": "第 N 次"}))
		if resp["ok"] != true {
			t.Fatalf("第 %d 次 run 被拒：%v", i+1, resp)
		}
		runID, _ := pluginRespData(t, resp)["run_id"].(string)
		waitSurfaceTerminal(t, buf, runID, 10*time.Second)
	}

	resp = pluginResp(t, m, buf, 9, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "第三次"}))
	assertDenied(t, resp, surfaceReasonQuota)

	// 时钟往前拨一小时 → 窗口滑过去，配额恢复（证明判据是「最近一小时」而不是「一天」）。
	hub.now = func() time.Time { return time.Date(2026, 11, 14, 11, 0, 1, 0, time.UTC) }
	resp = pluginResp(t, m, buf, 10, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "过一小时后"}))
	if resp["ok"] != true {
		t.Fatalf("窗口滑过后仍被拒（配额不是滑动窗口？）：%v", resp)
	}
	runID, _ := pluginRespData(t, resp)["run_id"].(string)
	waitSurfaceTerminal(t, buf, runID, 10*time.Second)
}

// TestSurfaceAgentRunTimeout 单 run 上限：注入 50ms，到点中断 + agent.error{quota}。
func TestSurfaceAgentRunTimeout(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, agentCommands)
	gate := newGateProvider("永远不回")
	useAgentLoop(t, gate, 2)
	hub := m.surfaceAgentHub()
	hub.timeout = 50 * time.Millisecond

	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)

	m.dispatch(cmd(2, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "超时"})))
	evs := waitSurfaceTerminal(t, buf, waitRunID(t, buf), 10*time.Second)
	data := agentError(t, evs)
	if data["reason"] != surfaceReasonQuota {
		t.Fatalf("超时的 reason = %v，want quota", data["reason"])
	}
	for _, item := range evs {
		if surfaceEventType(item) == surfaceAgentEvDone {
			t.Fatal("超时的 run 不该发 agent.done")
		}
	}
	close(gate.release)
}

// waitRunID 取最近一次 run 的 id：回执（同步写，一定有）优先，事件兜底。
//
// 为什么不只读事件：被 gateProvider 按住的 run 还没吐任何 delta，此时能拿到 run_id 的
// 唯一来源就是那条 command_response —— 取消/超时用例正需要在这个时刻拿到 id。
func waitRunID(t *testing.T, buf *syncBuf) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lines := readAllLines(buf)
		id := ""
		for _, l := range lines {
			if l["event_type"] != "command_response" {
				continue
			}
			if d, _ := l["data"].(map[string]any); d != nil {
				if s, _ := d["run_id"].(string); s != "" {
					id = s
				}
			}
		}
		for _, item := range surfaceEventLines(lines, "") {
			ev, _ := item["event"].(map[string]any)
			if s, _ := ev["run_id"].(string); s != "" {
				id = s
			}
		}
		if id != "" {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到 run_id（stdout 共 %d 行）：%v", len(lines), lines)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---- ④ 回执先行 + 事件信封 + seq + cancel ----

func TestSurfaceAgentRunStreamsEvents(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, agentCommands)
	useAgentLoop(t, newSegmentProvider(
		segmentRound{chunks: []string{"你", "好"}, content: "你好"},
	), 2)

	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)
	if sid == "" {
		t.Fatalf("agent.session 失败：%v", resp)
	}

	m.dispatch(cmd(2, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "打个招呼"})))
	// 回执是同步写的，事件是 run goroutine 异步写的 → 先等第一条事件出现，
	// 再一次性取**整段** stdout 验顺序（边读边清会把回执那行的行号抹掉）。
	lines := waitSurfaceFirstEvent(t, buf, 10*time.Second)
	evs := surfaceEventLines(lines, "")
	if len(evs) == 0 {
		t.Fatal("run 没有产生任何 surface 事件")
	}
	ev0, _ := evs[0]["event"].(map[string]any)
	runID, _ := ev0["run_id"].(string)
	if runID == "" {
		t.Fatalf("事件缺 run_id：%v", ev0)
	}

	// ① 回执先行：command_response 的行号必须小于**同 run** 第一条事件的行号。
	ackAt, firstEvAt := -1, -1
	for i, l := range lines {
		if l["event_type"] == "command_response" && intNumOf(l["id"]) == 2 {
			ackAt = i
			if l["ok"] != true {
				t.Fatalf("agent.run 回执失败：%v", l)
			}
			data, _ := l["data"].(map[string]any)
			if data["run_id"] != runID || data["session_id"] != sid {
				t.Fatalf("回执 data = %v，want run_id=%s session_id=%s", data, runID, sid)
			}
		}
		if firstEvAt < 0 && l["event_type"] == surfaceAgentEventType {
			if item := surfaceEventLines([]map[string]any{l}, runID); len(item) > 0 {
				firstEvAt = i
			}
		}
	}
	if ackAt < 0 {
		t.Fatal("没有 agent.run 的 command_response")
	}
	if firstEvAt >= 0 && firstEvAt < ackAt {
		t.Fatalf("事件(%d) 早于回执(%d) —— 插件会在拿到 run_id 之前收到 delta", firstEvAt, ackAt)
	}

	// ② 终态 + 信封字段。
	evs = waitSurfaceTerminal(t, buf, runID, 10*time.Second)
	assertSurfaceSeq(t, evs)

	var text strings.Builder
	var usage map[string]any
	var sawDone bool
	for _, item := range evs {
		if item["surface_id"] != "demo" {
			t.Fatalf("surface_id = %v，want demo：%v", item["surface_id"], item)
		}
		ev, _ := item["event"].(map[string]any)
		if ev["run_id"] != runID || ev["session_id"] != sid {
			t.Fatalf("事件信封归属错误：%v（want run_id=%s session_id=%s）", ev, runID, sid)
		}
		switch surfaceEventType(item) {
		case surfaceAgentEvDelta:
			d := surfaceEventData(item)
			s, _ := d["text"].(string)
			text.WriteString(s)
		case surfaceAgentEvUsage:
			usage = surfaceEventData(item)
		case surfaceAgentEvDone:
			sawDone = true
		}
	}
	if got := text.String(); got != "你好" {
		t.Fatalf("delta 文本 = %q，want 你好（合并后总文本必须等于最终正文）", got)
	}
	if usage == nil {
		t.Fatalf("没有 agent.usage：%v", evs)
	}
	// segmentProvider 每轮回 Usage{20,10}；agent.usage 是**整条 run** 的合计（§6.6「结束时一次」）。
	if usage["input_tokens"] != float64(20) || usage["output_tokens"] != float64(10) {
		t.Fatalf("usage = %v，want {input_tokens:20, output_tokens:10}", usage)
	}
	if !sawDone {
		t.Fatal("没有 agent.done")
	}
	if got := agentDone(t, evs)["text"]; got != "你好" {
		t.Fatalf("agent.done.text = %v，want 你好", got)
	}
	// ③ 终态后停投：done 之后不再有事件；会话上的 run 指针已清。
	if surfaceEventType(evs[len(evs)-1]) != surfaceAgentEvDone {
		t.Fatalf("最后一条事件不是 agent.done：%v", evs[len(evs)-1])
	}
	bs := m.get(pluginHomePath("demo"), sid)
	if bs == nil || bs.surfaceAgentRun.Load() != nil {
		t.Fatal("run 终态后会话上的 surfaceAgentRun 没清空（下一轮会串台）")
	}
	// ④ 审计：{id, action:"agent_run", ok:true, session, run, bytes}。
	rows := surfaceAuditRows(t, m)
	var found bool
	for _, rec := range rows {
		if rec["action"] != "agent_run" || rec["ok"] != true || rec["run"] != runID {
			continue
		}
		found = true
		if rec["id"] != "demo" || rec["session"] != sid {
			t.Fatalf("审计行 = %v，want id=demo session=%s", rec, sid)
		}
		if _, has := rec["bytes"]; !has {
			t.Fatalf("审计行缺 bytes：%v", rec)
		}
		if _, has := rec["ts"]; !has {
			t.Fatalf("审计行缺 ts：%v", rec)
		}
	}
	if !found {
		t.Fatalf("没有 agent_run 的成功审计行：%v", rows)
	}
}

// TestSurfaceAgentCancel agent.cancel：回执 ok + agent.error{cancelled}（**不发 done**）。
func TestSurfaceAgentCancel(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, agentCommands)
	gate := newGateProvider("不该被看到")
	useAgentLoop(t, gate, 2)

	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)

	m.dispatch(cmd(2, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "长任务"})))
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("run 没跑到 provider")
	}
	runID := waitRunID(t, buf)

	resp = dispatchAndResp(t, m, buf, 3, "surface_agent",
		agentPayload("demo", surfaceAgentCancelName, map[string]any{"run_id": runID}))
	if resp["ok"] != true {
		t.Fatalf("agent.cancel 回执 = %v，want ok:true", resp)
	}

	evs := waitSurfaceTerminal(t, buf, runID, 10*time.Second)
	data := agentError(t, evs)
	if data["reason"] != surfaceReasonCancelled {
		t.Fatalf("取消的 reason = %v，want cancelled", data["reason"])
	}
	for _, item := range evs {
		if surfaceEventType(item) == surfaceAgentEvDone {
			t.Fatal("取消后仍然发了 agent.done（协议要求只发 agent.error）")
		}
	}
	// 终态后停投：provider 事后若还吐一段（abort 走到 loop 才落地），也不能再投。
	close(gate.release)
	time.Sleep(80 * time.Millisecond)
	if after := surfaceEventLines(readAllLines(buf), runID); len(after) != len(evs) {
		t.Fatalf("终态后又投了 %d 条事件：%v", len(after)-len(evs), after[len(evs):])
	}

	// 取消已完成/未知的 run：幂等回 ok（插件不该卡在错误分支里）。
	resp = dispatchAndResp(t, m, buf, 4, "surface_agent",
		agentPayload("demo", surfaceAgentCancelName, map[string]any{"run_id": runID}))
	if resp["ok"] != true {
		t.Fatalf("重复取消 = %v，want ok:true（幂等）", resp)
	}
	resp = dispatchAndResp(t, m, buf, 5, "surface_agent",
		agentPayload("demo", surfaceAgentCancelName, map[string]any{"run_id": "sr-不存在"}))
	if resp["ok"] != true {
		t.Fatalf("未知 run_id 的取消 = %v，want ok:true（幂等）", resp)
	}
}

// TestSurfaceAgentCancelCrossSurface 取消别人的 run → denied（越权探测不能回 ok）。
func TestSurfaceAgentCancelCrossSurface(t *testing.T) {
	m, buf := testManager(t)
	writePluginSurfaces(t, m,
		pluginSurface{id: "alpha", enabled: true, commands: agentCommands},
		pluginSurface{id: "beta", enabled: true, commands: agentCommands},
	)
	gate := newGateProvider("慢慢来")
	useAgentLoop(t, gate, 2)
	home := pluginHomePath("alpha")
	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("alpha", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)
	if m.get(home, sid) == nil {
		t.Fatal("alpha 会话没建出来")
	}
	m.dispatch(cmd(2, "surface_agent", agentPayload("alpha", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "长任务"})))
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("run 没跑到 provider")
	}
	runID := waitRunID(t, buf)

	resp = dispatchAndResp(t, m, buf, 3, "surface_agent",
		agentPayload("beta", surfaceAgentCancelName, map[string]any{"run_id": runID}))
	assertDenied(t, resp, surfaceReasonDenied)

	// alpha 自己还能取消（证明上一条拒绝只是归属判定，不是把 run 弄丢了）。
	resp = dispatchAndResp(t, m, buf, 4, "surface_agent",
		agentPayload("alpha", surfaceAgentCancelName, map[string]any{"run_id": runID}))
	if resp["ok"] != true {
		t.Fatalf("alpha 取消自己的 run = %v，want ok:true", resp)
	}
	waitSurfaceTerminal(t, buf, runID, 10*time.Second)
	close(gate.release)
}

// ---- ⑤ tool.result：只做形状 + 审计 ----

func TestSurfaceAgentToolResultShape(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, agentCommands)

	// ① 形状合法：即使没有任何等待中的 call 也回 ok（P1 不接 waiter）。
	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentToolResult,
		map[string]any{"call_id": "call-1", "ok": true, "result": map[string]any{"n": 1}}))
	if resp["ok"] != true {
		t.Fatalf("tool.result = %v，want ok:true", resp)
	}

	// ② 失败结果也是合法形状（ok:false + error），回执同样是 ok:true。
	resp = pluginResp(t, m, buf, 2, "surface_agent", agentPayload("demo", surfaceAgentToolResult,
		map[string]any{"call_id": "call-2", "ok": false, "error": "插件内部失败"}))
	if resp["ok"] != true {
		t.Fatalf("失败结果被拒 = %v（P1 只校验形状，不判断结果语义）", resp)
	}

	// ③ 缺 call_id → invalid_input（形状不合法才是错误）。
	resp = pluginResp(t, m, buf, 3, "surface_agent", agentPayload("demo", surfaceAgentToolResult,
		map[string]any{"ok": true}))
	assertDenied(t, resp, surfaceReasonInvalidInput)

	// ④ 审计：每次 tool.result 一行（**含被拒的那次** —— 拒绝也要留下痕迹，
	//    否则「插件到底回没回」这件事在审计里少一半），tool=call_id、ok 跟随结果。
	var rows []map[string]any
	for _, rec := range surfaceAuditRows(t, m) {
		if rec["action"] == "tool_result" {
			rows = append(rows, rec)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("tool_result 审计行数 = %d，want 3（两次回执 + 一次形状拒绝）：%v", len(rows), rows)
	}
	if rows[0]["tool"] != "call-1" || rows[0]["ok"] != true {
		t.Fatalf("audit[0] = %v，want {tool:call-1, ok:true}", rows[0])
	}
	if rows[1]["tool"] != "call-2" || rows[1]["ok"] != false {
		t.Fatalf("audit[1] = %v，want {tool:call-2, ok:false}", rows[1])
	}
	if rows[2]["ok"] != false || rows[2]["reason"] != surfaceReasonInvalidInput {
		t.Fatalf("audit[2] = %v，want {ok:false, reason:invalid_input}", rows[2])
	}
}

// ---- ⑥ 事件落盘纪律：surface_agent_event 不进会话事件日志 ----

// 09 §6.6：agent.delta 是 content_chunk 的**派生副本**，不落盘。这条断言防的是
// 「顺手把插件事件也写进 events.jsonl」—— 那会让 transcript 与插件投影互为副本，
// 而两者本来只有一份权威（content_chunk）。
func TestSurfaceAgentEventsNotLogged(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, agentCommands)
	useAgentLoop(t, newSegmentProvider(segmentRound{chunks: []string{"嗯"}, content: "嗯"}), 2)

	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)
	m.dispatch(cmd(2, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "说一声"})))
	runID := waitRunID(t, buf)
	waitSurfaceTerminal(t, buf, runID, 10*time.Second)

	// 跑完后会话事件日志里必须有 agent_end（普通会话同构），但不能有插件事件。
	evFile := filepath.Join(pluginEventsDir("demo"), sid+".jsonl")
	deadline := time.Now().Add(10 * time.Second)
	var raw []byte
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(evFile)
		if err == nil && strings.Contains(string(b), "agent_end") {
			raw = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if raw == nil {
		t.Fatalf("等不到会话事件日志里的 agent_end：%s", evFile)
	}
	if strings.Contains(string(raw), surfaceAgentEventType) {
		t.Fatalf("surface_agent_event 被写进了会话事件日志（派生副本必须只走 stdout）：%s", evFile)
	}
	// 会话 jsonl 落在 plugin-sessions/<id>/sessions（P1a 的目录锚点，agent.run 不另起一套）。
	if _, err := os.Stat(filepath.Join(pluginSessionsDir("demo"), sid+".jsonl")); err != nil {
		t.Fatalf("插件会话 jsonl 不在 plugin-sessions/demo/sessions：%v", err)
	}
}

// ---- ⑦ 审计形状：{id, action:"agent_run", ts, ok, session?, run?, bytes?, reason?} ----

func TestSurfaceAgentAuditShape(t *testing.T) {
	m, buf := testManager(t)
	fakeSurface(t, m, "demo", true, agentCommands)

	// 一次拒绝 + 一次成功：两类行都要对得上账（只有拒绝记录时无法回答"这插件跑过什么"）。
	pluginResp(t, m, buf, 1, "surface_agent",
		agentPayload("demo", surfaceAgentRunName, map[string]any{"input": ""}))
	resp := pluginResp(t, m, buf, 2, "surface_agent", agentPayload("demo", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)
	if sid == "" {
		t.Fatalf("agent.session 失败：%v", resp)
	}

	rows := surfaceAuditRows(t, m)
	if len(rows) != 2 {
		t.Fatalf("审计行数 = %d，want 2：%v", len(rows), rows)
	}
	var okRow, denyRow map[string]any
	for _, rec := range rows {
		if rec["ok"] == true {
			okRow = rec
		} else {
			denyRow = rec
		}
	}
	if denyRow == nil || denyRow["reason"] != surfaceReasonInvalidInput {
		t.Fatalf("拒绝行 = %v，want reason=invalid_input", denyRow)
	}
	if okRow == nil {
		t.Fatal("没有成功的审计行")
	}
	if _, has := okRow["ts"]; !has {
		t.Fatalf("审计行缺 ts：%v", okRow)
	}
	if okRow["id"] != "demo" || okRow["action"] != "agent_run" || okRow["session"] != sid {
		t.Fatalf("成功行 = %v，want {id:demo, action:agent_run, session:%s}", okRow, sid)
	}
	// ts 必须是 RFC3339（surfaceStore.writeAudit 统一补），否则前端时间线解析不了。
	ts, _ := okRow["ts"].(string)
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Fatalf("审计 ts %q 不是 RFC3339Nano：%v", ts, err)
	}
	// 审计是 JSONL 一行一条（追加写不能把上一行吃掉）。
	if raw, err := os.ReadFile(filepath.Join(m.surfaces.Dir(), "audit.jsonl")); err != nil {
		t.Fatalf("读 audit.jsonl: %v", err)
	} else if n := len(strings.Split(strings.TrimSpace(string(raw)), "\n")); n != len(auditLines(t, m)) {
		t.Fatalf("audit.jsonl 行数对不上：%d vs %d", n, len(auditLines(t, m)))
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(ts), &probe); err == nil {
		t.Fatalf("ts 被写成了对象：%v", probe)
	}
}
