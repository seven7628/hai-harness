package main

// plugin_proxy_tool_test.go —— 插件工具链的 Go 侧验收（09-agent-bridge §6.4 / §7.3 / D10）。
//
// 这个文件钉的是**一条完整链路 + 五种失败**，不是「能跑」：
//
//	1. 注册矩阵：只有「插件虚拟 Project + 已启用 + manifest 读得到」才有 plugin__*；
//	2. 审批/只读声明透传（read_only 决定 RequiresApproval 与 ReadOnly）；
//	3. 全链路：模型 tool_calls → tool.call 事件 → 插件 tool.result → 结果回到模型；
//	4. 超时：插件不回 → 模型侧拿到 timeout，等待表清空（不泄漏）；
//	5. 跨插件：A 的 call_id 用 B 回 → denied，A 的等待者不被 resolve；
//	6. 终态：agent.cancel 时未回的工具立即失败（cancelled），不等满 deadline；
//	   + 重复 tool.result 幂等（第一次 resolve，第二次 no_such_call，不改结果）。
//
// 全部从**生产入口**进：注册走 buildPluginEngine、事件走 stdout 上真正写出的
// surface_agent_event、回执走 m.dispatch 的 surface_agent 命令。绕过其中任何一段，
// 测的就不是插件实际撞上的东西（00-conventions §4.7）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
)

// ---- 装置 ----

// toolSurface 一个装了 tools[] 的插件（index 条目 + manifest 一次写完）。
//
// 不用 fakeSurface 连调：它每次重写整份 index（单条），第二个插件会覆盖第一个 ——
// 跨插件用例恰恰要求「两个插件同时装着」。
type toolSurface struct {
	id       string
	enabled  bool
	tools    []map[string]any
	commands []string
	data     []string
}

// writeToolSurfaces 一次性写 N 个插件目录 + index.json（与 writePluginSurfaces 同款，
// 区别只在 manifest 里多了 tools）。commands/data 缺省 = agent 四件套 + tool.result /
// tool.call（绝大多数用例要的就是「通道都声明好了」）。
func writeToolSurfaces(t *testing.T, m *manager, items ...toolSurface) {
	t.Helper()
	root := m.surfaces.Dir()
	entries := make([]map[string]any, 0, len(items))
	for _, it := range items {
		dir := filepath.Join(root, it.id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("建 %s 失败: %v", dir, err)
		}
		cmds := it.commands
		if cmds == nil {
			cmds = agentCommands
		}
		data := it.data
		if data == nil {
			data = []string{surfaceToolCallEvent}
		}
		mf := map[string]any{
			"id": it.id, "name": map[string]string{"zh": it.id}, "version": "1.0.0",
			"api": 1, "entry": "panel.js", "commands": cmds, "data": data,
		}
		if it.tools != nil {
			mf["tools"] = it.tools
		}
		raw, err := json.Marshal(mf)
		if err != nil {
			t.Fatalf("marshal manifest: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "surface.json"), raw, 0o600); err != nil {
			t.Fatalf("写 surface.json: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "panel.js"), []byte("export const surface = {}\n"), 0o600); err != nil {
			t.Fatalf("写 panel.js: %v", err)
		}
		entries = append(entries, map[string]any{
			"id": it.id, "version": "1.0.0", "hash": strings.Repeat("a", 64),
			"source": "/tmp/src-" + it.id, "enabled": it.enabled,
			"installedAt": "2026-11-12T00:00:00Z",
		})
	}
	idx := map[string]any{"version": 1, "items": entries}
	raw, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), raw, 0o600); err != nil {
		t.Fatalf("写 index.json: %v", err)
	}
}

// fakeSurfaceTools 单插件版（不改 fakeSurface 的既有签名：那个还带着不带 tools 的老形状）。
func fakeSurfaceTools(t *testing.T, m *manager, id string, enabled bool, tools ...map[string]any) {
	t.Helper()
	writeToolSurfaces(t, m, toolSurface{id: id, enabled: enabled, tools: tools})
}

// toolSchema 一份最小合法 input（与 surfaces_test.go 的 wantObjSchema 同形，但这里
// 要能作为 map 直接进 manifest）。
func toolSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string", "description": "要回显的文本"}},
		"required":   []string{"q"},
	}
}

// toolDeclWith 一条工具声明；timeoutMS=0 表示走默认 10s。
func toolDeclWith(name, desc string, readOnly bool, timeoutMS int) map[string]any {
	d := map[string]any{"name": name, "description": desc, "input": toolSchema(), "read_only": readOnly}
	if timeoutMS != 0 {
		d["timeout_ms"] = timeoutMS
	}
	return d
}

// buildPluginEngineFor 走生产装配（buildPluginEngine = 插件会话的唯一引擎入口）。
func buildPluginEngineFor(t *testing.T, m *manager, ws *workspaceRuntime, sid string) *tools.Engine {
	t.Helper()
	reg, qw, _ := pluginToolsFixture(t)
	eng, _ := m.buildPluginEngine(ws, sid, m.provCfg.providerName(), m.provCfg.defaultModel(), "", nil,
		reg, qw, func() context.Context { return context.Background() })
	if eng == nil {
		t.Fatal("buildPluginEngine 返回 nil")
	}
	return eng
}

// toolNames 引擎里可见的工具名集合。
func toolNames(eng *tools.Engine) map[string]bool {
	out := map[string]bool{}
	for _, tl := range eng.Tools() {
		out[tl.Name()] = true
	}
	return out
}

// useAgentLoopWithEngine 把下一批 surface_agent 装配出来的 loop 换成「假 provider +
// **生产装配出来的插件引擎**」：全链路用例必须让模型真的走到 proxy 工具的 Call。
func useAgentLoopWithEngine(t *testing.T, m *manager, fake provider.Provider, maxIter int) {
	t.Helper()
	prev := surfaceAgentAssembleHook
	surfaceAgentAssembleHook = func(bs *bridgeSession) {
		if bs == nil || bs.runtime == nil {
			t.Fatal("装配钩子拿到 nil bs/runtime")
			return
		}
		eng := buildPluginEngineFor(t, m, bs.runtime, bs.id)
		bs.s.SetLoop(agents.NewAgentLoop(
			agents.WithProvider(fake),
			agents.WithToolEngine(eng),
			agents.WithMaxIterations(maxIter),
		))
	}
	t.Cleanup(func() { surfaceAgentAssembleHook = prev })
}

// startGateRun 起一条被 gateProvider 按住的 run（返回 run_id + sid + provider）。
//
// 「按住」而不是 sleep：工具调用用例要在**run 仍在跑**的时刻下发 tool.call ——
// 抢时机测试是 flaky 的根源。
func startGateRun(t *testing.T, m *manager, buf *syncBuf, id string) (runID, sid string, gate *gateProvider) {
	t.Helper()
	gate = newGateProvider("好的")
	useAgentLoop(t, gate, 2)
	resp := pluginResp(t, m, buf, 100, "surface_agent", agentPayload(id, surfaceAgentSessionName, nil))
	sid, _ = pluginRespData(t, resp)["session_id"].(string)
	if sid == "" {
		t.Fatalf("agent.session 失败：%v", resp)
	}
	m.dispatch(cmd(101, "surface_agent", agentPayload(id, surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "跑一下"})))
	runID = waitRunID(t, buf)
	select {
	case <-gate.started:
	case <-time.After(10 * time.Second):
		t.Fatal("run 没有起来（gateProvider 没被调用）")
	}
	t.Cleanup(func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	})
	return runID, sid, gate
}

// waitSurfaceToolCall 等一条**新的** tool.call 事件并返回它的 data。
//
// seen 是同一 run 内已经消费过的 call_id（同一 run 可以调多次工具，而事件缓冲不清空：
// 不跳过已见 id 会第二次拿到第一条 —— 后面的回执就全都对不上号）。
func waitSurfaceToolCall(t *testing.T, buf *syncBuf, runID string, seen map[string]bool, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, item := range surfaceEventLines(readAllLines(buf), runID) {
			if surfaceEventType(item) != surfaceToolCallEvent {
				continue
			}
			data := surfaceEventData(item)
			if id, _ := data["call_id"].(string); id != "" && !seen[id] {
				seen[id] = true
				return data
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到 run %s 的 %s 事件（stdout 共 %d 行）", runID, surfaceToolCallEvent,
				len(readAllLines(buf)))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// toolCallResult 一次后台工具调用的结局（文本/错误/耗时）。
type toolCallResult struct {
	text string
	err  error
	ms   int64
}

func callToolAsync(t *testing.T, eng *tools.Engine, name, args string) <-chan toolCallResult {
	t.Helper()
	out := make(chan toolCallResult, 1)
	tool, err := eng.GetTool(context.Background(), name)
	if err != nil {
		t.Fatalf("引擎里没有 %s：%v", name, err)
	}
	go func() {
		start := time.Now()
		text, cerr := tool.Call(context.Background(), name, args)
		out <- toolCallResult{text: text, err: cerr, ms: time.Since(start).Milliseconds()}
	}()
	return out
}

// ---- ① 注册矩阵 ----

func TestPluginProxyToolRegistrationMatrix(t *testing.T) {
	m, _ := pluginManager(t)
	fakeSurfaceTools(t, m, "demo", true, toolDeclWith("echo", "回显一段文本", true, 0))
	// 另一个插件用来证明「引擎只注册自己那份」——同一次 index 写入，两个都在。
	writeToolSurfaces(t, m,
		toolSurface{id: "demo", enabled: true, tools: []map[string]any{toolDeclWith("echo", "回显一段文本", true, 0)}},
		toolSurface{id: "beta", enabled: true, tools: []map[string]any{toolDeclWith("ping", "探活", true, 0)}},
	)

	ws, err := m.pluginRuntimeForHome(pluginHomePath("demo"))
	if err != nil {
		t.Fatalf("pluginRuntimeForHome: %v", err)
	}
	eng := buildPluginEngineFor(t, m, ws, "s-p2a-reg")
	names := toolNames(eng)
	if !names["plugin__demo__echo"] {
		t.Fatalf("插件引擎缺 plugin__demo__echo：%v", names)
	}
	if names["plugin__beta__ping"] {
		t.Fatalf("插件引擎出现了别的插件的工具：%v", names)
	}
	if _, err := eng.GetTool(context.Background(), "plugin__demo__echo"); err != nil {
		t.Fatalf("GetTool 取不到代理工具：%v", err)
	}

	// ToolParams 是**模型真正看到的** schema：必须与 manifest 的 input 一致。
	schemas, err := eng.ToolParams(context.Background())
	if err != nil {
		t.Fatalf("ToolParams: %v", err)
	}
	var got any
	var found bool
	for _, s := range schemas {
		if s.Name != "plugin__demo__echo" {
			continue
		}
		found = true
		raw, merr := json.Marshal(s.Parameters)
		if merr != nil {
			t.Fatalf("参数 schema 不能序列化：%v", merr)
		}
		if uerr := json.Unmarshal(raw, &got); uerr != nil {
			t.Fatalf("参数 schema 不是合法 JSON：%v", uerr)
		}
		if !strings.Contains(s.Description, "demo") {
			t.Fatalf("Description = %q，必须含插件 id（审批卡片靠它区分是谁）", s.Description)
		}
		if !strings.Contains(s.Description, "回显一段文本") {
			t.Fatalf("Description = %q，必须含 manifest 的 description", s.Description)
		}
	}
	if !found {
		t.Fatalf("ToolParams 里没有 plugin__demo__echo：%+v", schemas)
	}
	var want any
	rawWant, _ := json.Marshal(toolSchema())
	if err := json.Unmarshal(rawWant, &want); err != nil {
		t.Fatalf("want schema: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("模型侧 schema 与 manifest input 不一致：\ngot  = %#v\nwant = %#v", got, want)
	}

	// 停用：历史会话仍能装配引擎，但**不注册**工具（D10 + §5.3「撤销后工具 Hide」）。
	if rec := m.surfaces.SetEnabled("demo", false); rec.Status != surfaceApplied {
		t.Fatalf("SetEnabled 失败：%+v", rec)
	}
	engOff := buildPluginEngineFor(t, m, ws, "s-p2a-reg")
	if toolNames(engOff)["plugin__demo__echo"] {
		t.Fatalf("停用插件的引擎里仍有代理工具：%v", toolNames(engOff))
	}

	// 项目引擎：绝不含 plugin__*（D10）。
	proj := m.runtime(t.TempDir())
	if proj == nil {
		t.Fatal("项目 runtime 为 nil")
	}
	reg, qw, sk := pluginToolsFixture(t)
	bg := func() context.Context { return context.Background() }
	projEng, _ := newSessionEngine(proj.path, "s-p2a-proj", nil, nil, m.provCfg, m.provCfg.providerName(),
		m.provCfg.defaultModel(), "", bg, reg, qw, sk, proj.subagents, proj.mcp, proj.plugins,
		profiles["code"], nil, nil, nil, nil, nil)
	if len(toolNames(projEng)) == 0 {
		t.Fatal("项目引擎是空的（断言无意义）")
	}
	for name := range toolNames(projEng) {
		if strings.HasPrefix(name, pluginToolNamePrefix) {
			t.Fatalf("项目引擎出现了插件工具：%s", name)
		}
	}

	// Hub 引擎：同上（Hub 会话的工具面按 Agent Profile 策略，且不认识插件）。
	hubEng := hubEngineFor(t, m, m.hubRuntime("ag_p2a_tools"), agenthub.ToolPolicy{})
	for name := range toolNames(hubEng) {
		if strings.HasPrefix(name, pluginToolNamePrefix) {
			t.Fatalf("Hub 引擎出现了插件工具：%s", name)
		}
	}
}

// ---- ② 审批 / 只读声明 ----

func TestPluginProxyToolApprovalAndReadOnly(t *testing.T) {
	m, _ := pluginManager(t)
	writeToolSurfaces(t, m, toolSurface{id: "demo", enabled: true, tools: []map[string]any{
		toolDeclWith("echo", "只读回显", true, 0),
		toolDeclWith("write_note", "写一条便签", false, 0),
	}})
	ws, err := m.pluginRuntimeForHome(pluginHomePath("demo"))
	if err != nil {
		t.Fatalf("pluginRuntimeForHome: %v", err)
	}
	eng := buildPluginEngineFor(t, m, ws, "s-p2a-approve")

	probe := func(name string, wantReadOnly, wantApproval bool) {
		t.Helper()
		tool, err := eng.GetTool(context.Background(), name)
		if err != nil {
			t.Fatalf("GetTool(%s): %v", name, err)
		}
		ro, ok := tool.(interface{ ReadOnly() bool })
		if !ok {
			t.Fatalf("%s 没实现 ReadOnlyTool（plan 模式会把它当可写工具）", name)
		}
		if got := ro.ReadOnly(); got != wantReadOnly {
			t.Fatalf("%s ReadOnly = %v，want %v", name, got, wantReadOnly)
		}
		ap, ok := tool.(interface {
			RequiresApproval(context.Context, core.ToolCall) bool
		})
		if !ok {
			t.Fatalf("%s 没实现 ApprovalRequired（非只读工具会绕过审批）", name)
		}
		if got := ap.RequiresApproval(context.Background(), core.ToolCall{}); got != wantApproval {
			t.Fatalf("%s RequiresApproval = %v，want %v", name, got, wantApproval)
		}
		// P2c：非只读工具还必须实现 MandatoryApproval（Auto/Full Access 不能覆盖）；
		// 只读工具不实现（或返回 false），否则每次回显都弹审批卡。
		ma, ok := tool.(interface {
			MandatoryApproval(context.Context, core.ToolCall) bool
		})
		if !ok {
			t.Fatalf("%s 没实现 MandatoryApproval（Auto 模式会静默放行）", name)
		}
		if got := ma.MandatoryApproval(context.Background(), core.ToolCall{}); got != wantApproval {
			t.Fatalf("%s MandatoryApproval = %v，want %v", name, got, wantApproval)
		}
		// CanParallel 恒 false：V1 一次一个（结果通道按 run 关联）。
		if tool.CanParallel() {
			t.Fatalf("%s CanParallel = true，V1 必须是 false", name)
		}
		// 引擎兜底时限 = 声明 + 2s（waiter 必须先超时）。
		to, ok := tool.(interface{ ToolTimeout() time.Duration })
		if !ok {
			t.Fatalf("%s 没实现 ToolTimeoutProvider", name)
		}
		if got, want := to.ToolTimeout(), 10*time.Second+pluginToolWaiterGrace; got != want {
			t.Fatalf("%s ToolTimeout = %s，want %s", name, got, want)
		}
	}
	probe("plugin__demo__echo", true, false)
	probe("plugin__demo__write_note", false, true)
}

// ---- ③ 全链路：模型 → tool.call → 插件 tool.result → 模型 ----

// toolChainProvider 第一轮返回一次工具调用，第二轮把「模型收到的工具结果」记下来并回话。
type toolChainProvider struct {
	toolName string
	args     string
	final    string

	mu    sync.Mutex
	calls int
	reqs  []*provider.StreamRequest
}

func (p *toolChainProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	idx := p.calls
	p.calls++
	p.reqs = append(p.reqs, req)
	p.mu.Unlock()
	model := ""
	if req != nil {
		model = req.Model
	}
	rid := fmt.Sprintf("req_chain_%d", idx)
	if err := h(provider.LLMStartEvent{RequestId: rid, Model: model, Timestamp: time.Now()}); err != nil {
		return err
	}
	if idx == 0 {
		return h(provider.LLMEndEvent{
			RequestId: rid, FinishReason: core.FinishReasonToolCall, Model: model,
			ToolCalls: []core.ToolCall{{
				Id: "call_1", Name: p.toolName, Arguments: p.args,
			}},
			Usage: core.Usage{Input: 10, Output: 5},
		})
	}
	return h(provider.LLMEndEvent{
		RequestId: rid, FinishReason: core.FinishReasonStop, Content: p.final,
		Model: model, Usage: core.Usage{Input: 20, Output: 7},
	})
}

func (p *toolChainProvider) request(i int) *provider.StreamRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.reqs) {
		return nil
	}
	return p.reqs[i]
}

func TestPluginProxyToolCallRoundTrip(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurfaceTools(t, m, "demo", true, toolDeclWith("echo", "回显一段文本", true, 0))

	const pluginResult = "插件结果：42"
	fake := &toolChainProvider{
		toolName: "plugin__demo__echo", args: `{"q":"hi"}`, final: "done",
	}
	useAgentLoopWithEngine(t, m, fake, 4)

	resp := pluginResp(t, m, buf, 1, "surface_agent", agentPayload("demo", surfaceAgentSessionName, nil))
	sid, _ := pluginRespData(t, resp)["session_id"].(string)
	if sid == "" {
		t.Fatalf("agent.session 失败：%v", resp)
	}
	m.dispatch(cmd(2, "surface_agent", agentPayload("demo", surfaceAgentRunName,
		map[string]any{"session_id": sid, "input": "调一下插件工具"})))
	runID := waitRunID(t, buf)

	// tool.call 的载荷是协议的一部分（§6.6）：call_id 前缀、tool 是**插件本地名**、
	// args 原样、deadline/声明位都在。
	seen := map[string]bool{}
	call := waitSurfaceToolCall(t, buf, runID, seen, 10*time.Second)
	callID, _ := call["call_id"].(string)
	if !strings.HasPrefix(callID, "tc-") {
		t.Fatalf("call_id = %q，必须以 tc- 开头", callID)
	}
	if call["tool"] != "echo" {
		t.Fatalf("tool.call.tool = %v，want echo（插件本地名，不带 plugin__ 前缀）", call["tool"])
	}
	if args, _ := call["args"].(map[string]any); args["q"] != "hi" {
		t.Fatalf("tool.call.args = %v，want {q:hi}（原样透传）", call["args"])
	}
	if call["deadline_ms"] != float64(surfaceToolDefaultTimeoutMS) {
		t.Fatalf("deadline_ms = %v，want %d", call["deadline_ms"], surfaceToolDefaultTimeoutMS)
	}
	if call["read_only"] != true || call["requires_panel"] != false {
		t.Fatalf("声明位 = read_only:%v requires_panel:%v，want true/false", call["read_only"], call["requires_panel"])
	}

	// 插件回结果（dispatch 而不是 pluginResp：不要把事件读空）。
	result := map[string]any{"call_id": callID, "ok": true, "result": pluginResult}
	dispatchAndResp(t, m, buf, 3, "surface_agent", agentPayload("demo", surfaceAgentToolResult, result))
	evs := waitSurfaceTerminal(t, buf, runID, 10*time.Second)
	if got, _ := agentDone(t, evs)["text"].(string); got != "done" {
		t.Fatalf("agent.done.text = %q，want done", got)
	}

	// 结果真的进了模型上下文（第二轮的 tool 消息）。
	second := fake.request(1)
	if second == nil {
		t.Fatal("provider 只被调用了一次（工具轮之后没有第二轮）")
	}
	var toolMsg *core.Message
	for i := range second.Messages {
		if second.Messages[i].Role == core.Tool && second.Messages[i].ToolCallId == "call_1" {
			toolMsg = &second.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("第二轮请求里没有 call_1 的 tool 消息：%+v", second.Messages)
	}
	var text strings.Builder
	for _, c := range toolMsg.Content {
		text.WriteString(c.Content)
	}
	if !strings.Contains(text.String(), pluginResult) {
		t.Fatalf("模型收到的工具结果 = %q，want 含 %q", text.String(), pluginResult)
	}

	// 审计：一次调用一行 tool_call（模型侧收尾时写，带的 session/run/tool/bytes/ok 就是
	// 「谁调的、给了多大结果、成没成」）。插件侧只在异常时另记，避免一次调用两条。
	var callRows []map[string]any
	for _, rec := range auditLines(t, m) {
		if rec["action"] == "tool_call" {
			callRows = append(callRows, rec)
		}
	}
	if len(callRows) != 1 {
		t.Fatalf("tool_call 审计行数 = %d，want 1（成功调用只有模型侧一行）：%v", len(callRows), callRows)
	}
	okRow := callRows[0]
	if okRow["ok"] != true {
		t.Fatalf("成功的 tool_call 审计行 ok = %v，want true：%v", okRow["ok"], okRow)
	}
	if okRow["tool"] != "echo" || okRow["session"] != sid || okRow["run"] != runID {
		t.Fatalf("审计行 = %v，want {tool:echo, session:%s, run:%s}", okRow, sid, runID)
	}
	if bytes, _ := okRow["bytes"].(float64); int(bytes) != len(pluginResult) {
		t.Fatalf("审计 bytes = %v，want %d", okRow["bytes"], len(pluginResult))
	}

	// ⑧ 重复 tool.result 幂等：第一次已 resolve，第二次回 ok 但不再影响任何东西。
	before := len(auditLines(t, m))
	resp = dispatchAndResp(t, m, buf, 4, "surface_agent", agentPayload("demo", surfaceAgentToolResult, result))
	if resp["ok"] != true {
		t.Fatalf("重复 tool.result = %v，want ok:true（幂等）", resp)
	}
	rows := auditLines(t, m)
	if len(rows) != before+1 {
		t.Fatalf("重复回执应只多一行 no_such_call 审计：before=%d after=%d", before, len(rows))
	}
	if last := rows[len(rows)-1]; last["reason"] != surfaceToolReasonNoSuchCall || last["ok"] != false {
		t.Fatalf("重复回执审计 = %v，want {ok:false, reason:no_such_call}", last)
	}
	if n := m.surfaceAgentHub().pendingToolCalls(); n != 0 {
		t.Fatalf("等待表没清空：%d", n)
	}
}

// ---- ④ 超时：插件不回，模型拿到 timeout，等待表清空 ----

func TestPluginProxyToolCallTimeout(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurfaceTools(t, m, "demo", true, toolDeclWith("slow", "很慢的工具", true, 100))
	runID, sid, gate := startGateRun(t, m, buf, "demo")
	_ = runID

	ws, err := m.pluginRuntimeForHome(pluginHomePath("demo"))
	if err != nil {
		t.Fatalf("pluginRuntimeForHome: %v", err)
	}
	eng := buildPluginEngineFor(t, m, ws, sid)
	// 插件不回：先确认 tool.call 真的发出去了（否则这里测的是「没发」）。
	done := callToolAsync(t, eng, "plugin__demo__slow", `{}`)
	call := waitSurfaceToolCall(t, buf, runID, map[string]bool{}, 10*time.Second)
	if call["deadline_ms"] != float64(100) {
		t.Fatalf("deadline_ms = %v，want 100", call["deadline_ms"])
	}

	select {
	case res := <-done:
		if res.err == nil || !strings.Contains(res.err.Error(), "超时") {
			t.Fatalf("超时后 Call 返回 (%q, %v)，want timeout 错误", res.text, res.err)
		}
		if res.ms < 100 || res.ms > 5000 {
			t.Fatalf("超时耗时 = %dms，want 约 100ms（不能用满引擎兜底）", res.ms)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("工具没有在 deadline 附近返回（卡死或等满引擎兜底）")
	}
	if n := m.surfaceAgentHub().pendingToolCalls(); n != 0 {
		t.Fatalf("超时后等待表没清空：%d", n)
	}
	// 超时审计：reason 用的是封闭集合里的 timeout。
	var sawTimeout bool
	for _, rec := range auditLines(t, m) {
		if rec["action"] == "tool_call" && rec["reason"] == surfaceReasonTimeout {
			sawTimeout = true
		}
	}
	if !sawTimeout {
		t.Fatalf("没有 timeout 的 tool_call 审计行：%v", auditLines(t, m))
	}

	// run 本身不受影响：放行后照常收尾成 done（§6.4「超时 → 不中断 run」）。
	close(gate.release)
	evs := waitSurfaceTerminal(t, buf, runID, 10*time.Second)
	if got, _ := agentDone(t, evs)["text"].(string); got != "好的" {
		t.Fatalf("run 终态 text = %q，want 好的（工具超时不该中断 run）", got)
	}
}

// ---- ⑤ 跨插件：A 的 call_id 用 B 回 → denied，A 的等待者不被 resolve ----

func TestPluginProxyToolCrossPluginResult(t *testing.T) {
	m, buf := pluginManager(t)
	writeToolSurfaces(t, m,
		toolSurface{id: "demo", enabled: true, tools: []map[string]any{toolDeclWith("echo", "回显", true, 60000)}},
		toolSurface{id: "beta", enabled: true, tools: []map[string]any{toolDeclWith("ping", "探活", true, 60000)}},
	)
	runID, sid, gate := startGateRun(t, m, buf, "demo")

	ws, err := m.pluginRuntimeForHome(pluginHomePath("demo"))
	if err != nil {
		t.Fatalf("pluginRuntimeForHome: %v", err)
	}
	eng := buildPluginEngineFor(t, m, ws, sid)
	done := callToolAsync(t, eng, "plugin__demo__echo", `{"q":"hi"}`)
	call := waitSurfaceToolCall(t, buf, runID, map[string]bool{}, 10*time.Second)
	callID, _ := call["call_id"].(string)

	// beta 拿着 demo 的 call_id 回结果：越权探测 → denied，且 demo 的等待者**不动**。
	resp := dispatchAndResp(t, m, buf, 3, "surface_agent", agentPayload("beta", surfaceAgentToolResult,
		map[string]any{"call_id": callID, "ok": true, "result": "冒名结果"}))
	if resp["ok"] != false {
		t.Fatalf("跨插件回执 = %v，want ok:false", resp)
	}
	if code := str(resp, "code"); code != surfaceReasonDenied {
		t.Fatalf("跨插件回执 code = %q，want denied", code)
	}
	if n := m.surfaceAgentHub().pendingToolCalls(); n != 1 {
		t.Fatalf("跨插件回执动到了等待表：pending = %d，want 1", n)
	}
	select {
	case res := <-done:
		t.Fatalf("demo 的调用被跨插件回执 resolve 了：(%q, %v)", res.text, res.err)
	case <-time.After(150 * time.Millisecond):
		// 仍在等 = 正确
	}

	// 正确回执仍能完成。
	dispatchAndResp(t, m, buf, 4, "surface_agent", agentPayload("demo", surfaceAgentToolResult,
		map[string]any{"call_id": callID, "ok": true, "result": "真正的结果"}))
	select {
	case res := <-done:
		if res.err != nil || res.text != "真正的结果" {
			t.Fatalf("正确回执后 Call = (%q, %v)，want 真正的结果", res.text, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("正确回执没有 resolve 等待者")
	}
	close(gate.release)
	waitSurfaceTerminal(t, buf, runID, 10*time.Second)
}

// ---- ⑥ 终态：agent.cancel 时未回的工具立即失败（不等满 deadline）----

func TestPluginProxyToolCancelledOnRunEnd(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurfaceTools(t, m, "demo", true, toolDeclWith("echo", "回显", true, 60000))
	runID, sid, _ := startGateRun(t, m, buf, "demo")

	ws, err := m.pluginRuntimeForHome(pluginHomePath("demo"))
	if err != nil {
		t.Fatalf("pluginRuntimeForHome: %v", err)
	}
	eng := buildPluginEngineFor(t, m, ws, sid)
	done := callToolAsync(t, eng, "plugin__demo__echo", `{"q":"hi"}`)
	waitSurfaceToolCall(t, buf, runID, map[string]bool{}, 10*time.Second)

	// 取消 run：等待者必须**立刻**失败（deadline 是 60s，这里只给 2s 窗口）。
	m.dispatch(cmd(3, "surface_agent", agentPayload("demo", surfaceAgentCancelName,
		map[string]any{"run_id": runID})))
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatalf("取消后 Call = %q, nil，want 失败", res.text)
		}
		if res.ms > 2000 {
			t.Fatalf("取消后 %dms 才返回（应当立即失败，不能等满 60s deadline）", res.ms)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后工具仍然阻塞（终态没清理等待者）")
	}
	if n := m.surfaceAgentHub().pendingToolCalls(); n != 0 {
		t.Fatalf("取消后等待表没清空：%d", n)
	}

	// 取消是终态：run 以 agent.error{cancelled} 收尾（不发 done）。
	evs := waitSurfaceTerminal(t, buf, runID, 10*time.Second)
	if reason := agentError(t, evs)["reason"]; reason != surfaceReasonCancelled {
		t.Fatalf("agent.error.reason = %v，want cancelled", reason)
	}
	// 取消之后的迟到回执：幂等 ok + no_such_call 审计，不产生任何副作用。
	resp := dispatchAndResp(t, m, buf, 4, "surface_agent", agentPayload("demo", surfaceAgentToolResult,
		map[string]any{"call_id": "tc-late", "ok": true, "result": "迟到"}))
	if resp["ok"] != true {
		t.Fatalf("迟到回执 = %v，want ok:true（幂等）", resp)
	}
}

// ---- 结果整形：字符串原样 / 其它 JSON 序列化 / 超 64 KiB 截断 + marker ----

func TestPluginProxyToolResultShaping(t *testing.T) {
	m, buf := pluginManager(t)
	fakeSurfaceTools(t, m, "demo", true, toolDeclWith("echo", "回显", true, 60000))
	runID, sid, gate := startGateRun(t, m, buf, "demo")

	ws, err := m.pluginRuntimeForHome(pluginHomePath("demo"))
	if err != nil {
		t.Fatalf("pluginRuntimeForHome: %v", err)
	}
	eng := buildPluginEngineFor(t, m, ws, sid)
	tool, err := eng.GetTool(context.Background(), "plugin__demo__echo")
	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}

	// ValidParams：空串按 `{}`（模型经常发空参数），非 object 直接拒。
	if err := tool.ValidParams(context.Background(), tool.Name(), ""); err != nil {
		t.Fatalf("空参数串应等价 {}：%v", err)
	}
	if err := tool.ValidParams(context.Background(), tool.Name(), `[1,2]`); err == nil {
		t.Fatal("数组参数应当被拒（必须是 JSON object）")
	}

	// ① 结构化结果：非字符串 JSON 值序列化后交给模型。
	seen := map[string]bool{}
	done := callToolAsync(t, eng, "plugin__demo__echo", "")
	call := waitSurfaceToolCall(t, buf, runID, seen, 10*time.Second)
	// 空参数串同样原样下发为 {}
	if args, ok := call["args"].(map[string]any); !ok || len(args) != 0 {
		t.Fatalf("空参数应下发 {}，实际 %#v", call["args"])
	}
	dispatchAndResp(t, m, buf, 10, "surface_agent", agentPayload("demo", surfaceAgentToolResult,
		map[string]any{"call_id": call["call_id"], "ok": true, "result": map[string]any{"n": 1}}))
	select {
	case res := <-done:
		if res.err != nil || res.text != `{"n":1}` {
			t.Fatalf("结构化结果 = (%q, %v)，want {\"n\":1}", res.text, res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("结构化结果没有回到模型")
	}

	// ② 超 64 KiB：截断到上限并**带 marker**（不静默）。
	big := strings.Repeat("x", maxPluginToolResultBytes+4096)
	done = callToolAsync(t, eng, "plugin__demo__echo", `{"q":"big"}`)
	call = waitSurfaceToolCall(t, buf, runID, seen, 10*time.Second)
	dispatchAndResp(t, m, buf, 11, "surface_agent", agentPayload("demo", surfaceAgentToolResult,
		map[string]any{"call_id": call["call_id"], "ok": true, "result": big}))
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("超长结果不应报错：%v", res.err)
		}
		if len(res.text) > maxPluginToolResultBytes+len(pluginToolTruncatedMarker) {
			t.Fatalf("结果长度 = %d，超过上限+marker（%d）", len(res.text),
				maxPluginToolResultBytes+len(pluginToolTruncatedMarker))
		}
		if !strings.HasSuffix(res.text, pluginToolTruncatedMarker) {
			t.Fatalf("超长结果缺少截断 marker：尾部 = %q", res.text[max(0, len(res.text)-80):])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超长结果没有回到模型")
	}
	if n := m.surfaceAgentHub().pendingToolCalls(); n != 0 {
		t.Fatalf("等待表没清空：%d", n)
	}
	close(gate.release)
	waitSurfaceTerminal(t, buf, runID, 10*time.Second)
}
