package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/internal/officekit"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/provider/anthropic"
	"github.com/seven7628/hai-harness/provider/responses"
	goruntime "github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

// syncBuf 线程安全 buffer：run 循环 goroutine（emit 写）与测试 goroutine（readLines 读）
// 并发访问时无数据竞争（wake 类命令如 load_skill 会触发后台 run）。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
func (s *syncBuf) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}
func (s *syncBuf) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

// Take 原子地取出当前全部内容并清空（读-清在**同一次加锁**内完成）。
//
// 为什么必须原子：readLines 曾用 String() + Reset() 两条独立加锁的调用读取，
// 两者之间到达的 event（run goroutine 仍在 emit）会被随之而来的 Reset 抹掉、
// 永久丢弃 —— 之后任何轮询都再也等不到它，表现为 waitEvent 随机超时
// （实测 18 轮 1 次失败；main_test.go 的 TestSandboxGate* 系列最易命中）。
func (s *syncBuf) Take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.b.String()
	s.b.Reset()
	return out
}

// testPythonRuntime 构造隔离的受管 Python 运行时实例（Root 落临时目录，不碰真实
// ~/.go-code/runtime/python）。仅用于「工具是否注册」这类装配断言，不会触发 Ensure
// （工具注册只读元数据）；真执行路径由 tools/builtin 的 run_python 测试覆盖。
func testPythonRuntime(t *testing.T) *goruntime.Runtime {
	t.Helper()
	return goruntime.New(goruntime.Config{Root: filepath.Join(t.TempDir(), "python")})
}

// testManager：provCfg 默认配置构造 manager（构建 loop 不触发网络；runLoop 只在 ask/wake 后才发 LLM）
func testManager(t *testing.T) (*manager, *syncBuf) {
	t.Helper()
	return testManagerWithDataDir(t, t.TempDir())
}

// testManagerWithDataDir 与 testManager 同一套隔离（临时 HOME / 办公组件按未装 / 统一收尾），
// 但 APP 数据目录由调用方给 —— 「重启」类用例要用**同一个** appDataDir 先后起两个 manager，
// 否则测的就不是重启（磁盘上什么都没留下）。
func testManagerWithDataDir(t *testing.T, dataDir string) (*manager, *syncBuf) {
	t.Helper()
	// 隔离真实用户配置、插件和 Seatbelt 相关宿主状态；bridge 单测不应读取 ~/.go-code。
	// HOME 之外还要隔离 USERPROFILE：os.UserHomeDir 在 Windows 上读它，只设 HOME 时
	// p404aWriteUserHooks / 边界测试可能写到真实 %USERPROFILE%\.go-code（破坏性）。
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	appDataDirOverride = dataDir
	t.Cleanup(func() { appDataDirOverride = "" })
	buf := &syncBuf{}
	m := &manager{
		workspaces: map[string]*workspaceRuntime{},
		provCfg:    loadProviderConfig(),
		out:        newOut(buf),
	}
	// 办公组件默认按「未安装」起步（R08d）：Prober 的零值选项会探测**本机**
	//（PATH + /Applications），装了 LibreOffice 的开发机上预览会真的起 soffice ——
	// 那会把「预览通路」测成「这台机器装没装 LO」，而且一次冷启就能吃掉用例的等待窗口。
	// 需要 kit 的用例自己 installOfficeState（office_test.go），其余一律走近似路径。
	installOfficeState(m, noKitOptions(t), officekit.RenderImages)
	// 测试结束前统一停掉所有会话（不关 workspace：ws.plugins/mcp 关闭会误伤
	// injectBrowserWS 等 fake 插件）。后台 run goroutine（401 重试等）在 TempDir
	// cleanup 时仍可能 emit → syncMode 同步写盘 → directory not empty flaky。
	t.Cleanup(func() {
		// Hub 智能体运行态不在 m.workspaces 里（刻意分开登记），单独收尾：
		// 否则 DM 会话的 run goroutine 会在 TempDir 清理后继续写盘（directory not empty）。
		m.shutdownHubRuntimes()
		// 插件虚拟 Project 同样不在 m.workspaces 里（pluginRuntimes），单独收尾：
		// 否则虚拟 home/sessions 里的 record 提交与 block 回收会在 TempDir 清理后继续写。
		m.closePluginRuntimes()
		m.mu.Lock()
		paths := make([]string, 0, len(m.workspaces))
		for wsPath := range m.workspaces {
			paths = append(paths, wsPath)
		}
		m.mu.Unlock()
		for _, wsPath := range paths {
			ws := m.workspaces[wsPath]
			if ws == nil {
				continue
			}
			ws.mu.Lock()
			items := make([]*bridgeSession, 0, len(ws.sessions))
			for _, bs := range ws.sessions {
				items = append(items, bs)
			}
			ws.mu.Unlock()
			for _, bs := range items {
				stopSession(bs) // 停 run goroutine + coordinator drain，不碰插件
			}
			// MCP 配置监听必须停：否则测试目录（t.TempDir）被清理时 watcher 会收到事件、
			// 触发 apply → m.runtime 把运行态复活并在已删目录里建目录（cleanup flaky）。
			ws.stopMCPWatch()
		}
	})
	return m, buf
}

// readLines 原子地取出并清空 buffer 的全部事件行（经 syncBuf.Take 同一次加锁完成）。
// 切勿拆成 readAllLines + Reset 两步：中间到达的 event 会被 Reset 抹掉、永久丢失。
func readLines(buf *syncBuf) []map[string]any {
	return parseLines(buf.Take())
}

// readAllLines 读取但**不清空**（需要反复轮询同一批已到达事件时用）。
func readAllLines(buf *syncBuf) []map[string]any {
	return parseLines(buf.String())
}

func parseLines(raw string) []map[string]any {
	var lines []map[string]any
	for _, l := range strings.Split(raw, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			lines = append(lines, m)
		}
	}
	return lines
}

func parseJSONLines(s string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(s, "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}
func onlyEvent(lines []map[string]any, typ events.EventType) []map[string]any {
	out := make([]map[string]any, 0)
	for _, line := range lines {
		if line["event_type"] == string(typ) {
			out = append(out, line)
		}
	}
	return out
}

func commandResponses(lines []map[string]any) []map[string]any {
	var out []map[string]any
	for _, line := range lines {
		if line["event_type"] == "command_response" {
			out = append(out, line)
		}
	}
	return out
}

func firstCommandResponse(lines []map[string]any) map[string]any {
	responses := commandResponses(lines)
	if len(responses) == 0 {
		return nil
	}
	return responses[0]
}

func cmd(id int, typ string, payload map[string]any) command {
	return command{Id: id, Type: Command(typ), Payload: payload}
}

// newSessionID：dispatch new_session 并从响应取 session_id
func newSessionID(t *testing.T, m *manager, buf *syncBuf, ws string) string {
	t.Helper()
	m.dispatch(cmd(0, "new_session", map[string]any{"workspace": ws}))
	for _, l := range readLines(buf) {
		if l["ok"] == true {
			if d, ok := l["data"].(map[string]any); ok {
				if sid, ok := d["session_id"].(string); ok {
					return sid
				}
			}
		}
	}
	t.Fatal("new_session 响应无 session_id")
	return ""
}

// runtime / create / get 的工作区隔离
func TestWorkspaceRuntimeIsolation(t *testing.T) {
	wsA, wsB := t.TempDir(), t.TempDir()
	m, _ := testManager(t)

	sA, err := m.create(wsA, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	sB, err := m.create(wsB, "s1", "") // 同名 id、不同 workspace → 各自独立
	if err != nil {
		t.Fatal(err)
	}
	if sA == sB {
		t.Fatal("同名会话跨 workspace 应独立")
	}

	if got := m.runtime(wsA).storeDir; got != wsSessionsDir(wsA) {
		t.Fatalf("storeDir = %s", got)
	}

	again, _ := m.create(wsA, "s1", "")
	if again != sA {
		t.Fatal("进程内活跃会话二次 create 应复用而非重建")
	}

	if got := m.get(wsA, "s1"); got != sA {
		t.Fatal("get(wsA) 应命中 sA")
	}
	if got := m.get(wsB, "s1"); got != sB {
		t.Fatal("get(wsB) 应命中 sB")
	}
	if got := m.get(wsA, "nope"); got != nil {
		t.Fatal("不存在的会话应返回 nil")
	}
}

// list 按工作区隔离
func TestListSessionsPerWorkspace(t *testing.T) {
	wsA, wsB := t.TempDir(), t.TempDir()
	m, _ := testManager(t)
	for _, id := range []string{"a-1", "a-2"} {
		if _, err := m.create(wsA, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.create(wsB, "b-1", ""); err != nil {
		t.Fatal(err)
	}

	if got := len(m.listSessions(wsA)); got != 2 {
		t.Fatalf("wsA 应有 2 会话，got %d", got)
	}
	if got := len(m.listSessions(wsB)); got != 1 {
		t.Fatalf("wsB 应有 1 会话，got %d", got)
	}
}

// ask_batch 命令：客户端暂存队列一次性批量推送多条用户输入（逐条入 inbox、下一轮一起成批消费）。
func TestAskBatchCommand(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)

	respByID := func(id int) map[string]any {
		for _, l := range readLines(buf) {
			if l["id"] == float64(id) {
				return l
			}
		}
		return nil
	}

	// 缺 messages → 报错
	m.dispatch(cmd(1, "ask_batch", map[string]any{"workspace": ws, "session_id": sid}))
	if r := respByID(1); r == nil || r["ok"] != false {
		t.Fatalf("缺 messages 应报错: %+v", r)
	}
	// messages 非数组 → 报错
	m.dispatch(cmd(2, "ask_batch", map[string]any{"workspace": ws, "session_id": sid, "messages": "nope"}))
	if r := respByID(2); r == nil || r["ok"] != false {
		t.Fatalf("messages 非数组应报错: %+v", r)
	}
	// 会话不存在 → 报错
	m.dispatch(cmd(3, "ask_batch", map[string]any{"workspace": ws, "session_id": "nope", "messages": []any{map[string]any{"content": []any{map[string]any{"type": "text", "content": "x"}}}}}))
	if r := respByID(3); r == nil || r["ok"] != false || !strings.Contains(str(r, "error"), "session not found") {
		t.Fatalf("无此会话应报错: %+v", r)
	}

	// 成功：多条统一入队（空白过滤），含普通文本 + 文本+图片附件混排
	m.dispatch(cmd(4, "ask_batch", map[string]any{
		"workspace": ws, "session_id": sid,
		"messages": []any{
			map[string]any{"content": []any{map[string]any{"type": "text", "content": "hello"}}},
			map[string]any{"content": []any{map[string]any{"type": "text", "content": "  world  "}, map[string]any{"type": "image", "content": "data:image/png;base64,xxx", "mime_type": "image/png"}}},
			map[string]any{"content": []any{map[string]any{"type": "text", "content": "   "}}}, // 纯空白 → 跳过
		},
	}))
	if r := respByID(4); r == nil || r["ok"] != true {
		t.Fatalf("ask_batch 应 ok: %+v", r)
	}
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session 应存在")
	}
	type gotBlock struct {
		typ, content, mime string
	}
	var got []gotBlock
	for _, mm := range bs.s.History() {
		if mm.Role != core.User {
			continue
		}
		for _, cc := range mm.Content {
			if cc.Type == core.ContentTypeCommand || cc.Type == core.ContentTypeTaskResult {
				continue
			}
			got = append(got, gotBlock{typ: cc.Type, content: cc.Content, mime: cc.MimeType})
		}
	}
	want := []gotBlock{
		{"text", "hello", ""},
		{"text", "  world  ", ""},                           // 内容块原样保留（trim 由客户端负责）
		{"image", "data:image/png;base64,xxx", "image/png"}, // 图片块保留 + mime
	}
	if len(got) != len(want) {
		t.Fatalf("入队内容块 = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("入队内容块 = %#v, want %#v", got, want)
		}
	}
}

// dispatch 路由 + 响应回填 workspace + shutdown 只关本工作区
func TestDispatchMultiWorkspace(t *testing.T) {
	wsA, wsB := t.TempDir(), t.TempDir()
	m, buf := testManager(t)

	idA := newSessionID(t, m, buf, wsA)
	idB := newSessionID(t, m, buf, wsB)
	if idA == idB {
		t.Fatal("不同 workspace 的会话 id 应不同")
	}

	// list 响应带 workspace 且只回本工作区会话
	m.dispatch(cmd(0, "list", map[string]any{"workspace": wsA}))
	lines := readLines(buf)
	if len(lines) != 1 {
		t.Fatalf("list 应 1 条响应，got %d", len(lines))
	}
	if lines[0]["workspace"] != wsA {
		t.Fatalf("响应 workspace = %v", lines[0]["workspace"])
	}
	data := lines[0]["data"].(map[string]any)
	if n := len(data["sessions"].([]any)); n != 1 {
		t.Fatalf("wsA list 应 1 会话，got %d", n)
	}

	// 缺 workspace → 报错
	m.dispatch(cmd(0, "new_session", map[string]any{}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("缺 workspace 应返回错误")
	}

	// 会话级命令路由到所属 workspace（switch_effort 不触发 LLM，确定性）
	m.dispatch(cmd(0, "switch_effort", map[string]any{"workspace": wsA, "session_id": idA, "effort": "high"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatal("同 workspace 会话命令应成功")
	}
	m.dispatch(cmd(0, "switch_effort", map[string]any{"workspace": wsB, "session_id": idA, "effort": "high"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("跨 workspace 用 idA 应 not found")
	}

	// shutdown 只关 wsA；wsB 会话不受影响
	m.dispatch(cmd(0, "shutdown", map[string]any{"workspace": wsA}))
	readLines(buf)
	if m.get(wsA, idA).ctx.Err() == nil {
		t.Fatal("wsA 会话应已取消")
	}
	if m.get(wsB, idB).ctx.Err() != nil {
		t.Fatal("wsB 会话不应受影响")
	}
}

// emit 注入 session_id + workspace（事件自包含）
func TestEmitStampsWorkspace(t *testing.T) {
	wsA := t.TempDir()
	m, buf := testManager(t)
	bs, err := m.create(wsA, "s1", "")
	if err != nil {
		t.Fatal(err)
	}

	bs.emit(&events.AgentStart{RunId: "r-1", Content: "任务", Timestamp: time.Now(), EventType: events.AgentStartType})
	lines := onlyEvent(readLines(buf), events.AgentStartType)
	if len(lines) != 1 {
		t.Fatalf("应 1 条事件，got %d", len(lines))
	}
	if lines[0]["session_id"] != "s1" {
		t.Fatalf("session_id = %v", lines[0]["session_id"])
	}
	if lines[0]["workspace"] != wsA {
		t.Fatalf("workspace = %v", lines[0]["workspace"])
	}
	if lines[0]["run_id"] != "r-1" {
		t.Fatalf("run_id = %v", lines[0]["run_id"])
	}
}

// emit 旁路落盘事件日志：**时间线事件**的文件行 = stdout 行（字节一致）。
// 注意流式增量（content_chunk/reasoning_chunk）只走 stdout、不落盘——
// 见 TestEmitFiltersStreamingChunks 与 logEventLine 注释。
func TestEmitLogsEvents(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离 APP 级存储（~/.go-code/…）
	wsA := t.TempDir()
	m, buf := testManager(t)
	bs, err := m.create(wsA, "s1", "")
	if err != nil {
		t.Fatal(err)
	}

	bs.emit(&events.AgentStart{RunId: "r-1", Content: "任务", Timestamp: time.Now(), EventType: events.AgentStartType})
	bs.emit(&events.AgentEnd{RunId: "r-1", Index: 1, Content: "完成", Timestamp: time.Now(), FinishReason: "end", EventType: events.AgentEndType})

	allLines := readAllLines(buf)
	buf.Reset()
	stdoutLines := append(onlyEvent(allLines, events.AgentStartType), onlyEvent(allLines, events.AgentEndType)...)
	if len(stdoutLines) != 2 {
		t.Fatalf("stdout 应 2 行，got %d", len(stdoutLines))
	}
	data, err := os.ReadFile(filepath.Join(wsEventsDir(wsA), "s1.jsonl"))
	if err != nil {
		t.Fatalf("事件日志文件不存在: %v", err)
	}
	logLines := onlyEvent(parseJSONLines(string(data)), events.AgentStartType)
	logLines = append(logLines, onlyEvent(parseJSONLines(string(data)), events.AgentEndType)...)
	if len(logLines) != 2 {
		t.Fatalf("事件日志应 2 行，got %d", len(logLines))
	}
	if logLines[0]["event_type"] != stdoutLines[0]["event_type"] || logLines[1]["event_type"] != stdoutLines[1]["event_type"] {
		t.Fatal("事件日志行与 stdout 行不一致")
	}
}

// 流式增量不落盘：content_chunk / reasoning_chunk 走 stdout（前端实时渲染）
// 但不写事件日志——可重建（llm_end 携带最终文本），实测占日志体积 78.5%。
func TestEmitFiltersStreamingChunks(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离 APP 级存储
	wsA := t.TempDir()
	m, buf := testManager(t)
	bs, err := m.create(wsA, "s-chunk", "")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	bs.emit(&events.AgentStart{RunId: "r-1", Content: "任务", Timestamp: now, EventType: events.AgentStartType})
	bs.emit(&events.ContentChunk{RunId: "r-1", RequestId: "req-1", Index: 0, Content: "你好", Timestamp: now, EventType: events.ContentChunkType})
	bs.emit(&events.ReasoningChunk{RunId: "r-1", RequestId: "req-1", Index: 0, Content: "思考", Timestamp: now, EventType: events.ReasoningChunkType})
	bs.emit(&events.LLMEnd{RunId: "r-1", RequestId: "req-1", Model: "m", Content: "你好", Timestamp: now, EventType: events.LLMEndType})
	bs.emit(&events.AgentEnd{RunId: "r-1", Index: 1, Content: "完成", Timestamp: now, FinishReason: "end", EventType: events.AgentEndType})

	// stdout（实时转发）：chunk 必须保留，否则前端无法流式渲染
	stdout := readLines(buf) // stdout 行已解析为 map（与既有测试同口径）
	if n := len(onlyEvent(stdout, events.ContentChunkType)); n != 1 {
		t.Fatalf("stdout 应保留 content_chunk 1 条，got %d", n)
	}
	if n := len(onlyEvent(stdout, events.ReasoningChunkType)); n != 1 {
		t.Fatalf("stdout 应保留 reasoning_chunk 1 条，got %d", n)
	}

	// 事件日志（落盘）：chunk 被过滤，时间线事件保留
	data, err := os.ReadFile(filepath.Join(wsEventsDir(wsA), "s-chunk.jsonl"))
	if err != nil {
		t.Fatalf("事件日志文件不存在: %v", err)
	}
	logged := parseJSONLines(string(data))
	if n := len(onlyEvent(logged, events.ContentChunkType)); n != 0 {
		t.Fatalf("事件日志不应含 content_chunk，got %d", n)
	}
	if n := len(onlyEvent(logged, events.ReasoningChunkType)); n != 0 {
		t.Fatalf("事件日志不应含 reasoning_chunk，got %d", n)
	}
	for _, want := range []events.EventType{events.AgentStartType, events.LLMEndType, events.AgentEndType} {
		if n := len(onlyEvent(logged, want)); n != 1 {
			t.Fatalf("事件日志应保留 %s 1 条，got %d", want, n)
		}
	}
}

// isStreamingChunkType：按事件类型判定（不扫序列化行）。
// 回归点：事件行 JSON key 按字母序排列，长 chunk 会把 event_type 推到很后面——
// 若按行首子串匹配会漏判（chunk 照样落盘），故必须用 typed event 判定。
func TestIsStreamingChunkType(t *testing.T) {
	chunks := []events.EventType{events.ContentChunkType, events.ReasoningChunkType}
	for _, ty := range chunks {
		if !isStreamingChunkType(ty) {
			t.Fatalf("%s 应判定为流式增量（不落盘）", ty)
		}
	}
	timeline := []events.EventType{
		events.AgentStartType, events.AgentEndType, events.LLMStartType, events.LLMEndType,
		events.ToolRunStartType, events.ToolRunEndType, events.CompressStartType, events.CompressEndType,
		events.TaskStartedType, events.TaskEndType, events.AskUserQuestionType,
	}
	for _, ty := range timeline {
		if isStreamingChunkType(ty) {
			t.Fatalf("%s 是时间线事件，必须落盘", ty)
		}
	}
}

// buildSnapshot：事件日志聚合为有序快照数组（保留 event_type 与字段、去掉 session_id）；
// 无日志/空日志/全坏行 → (nil, false)
func TestBuildSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离 APP 级存储
	wsA := t.TempDir()
	m, _ := testManager(t)
	evFile := filepath.Join(wsEventsDir(wsA), "s1.jsonl")
	if err := os.MkdirAll(filepath.Dir(evFile), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"event_type":"agent_start","session_id":"s1","workspace":"` + wsA + `","run_id":"r1","content":"任务"}`,
		`{"event_type":"llm_start","session_id":"s1","workspace":"` + wsA + `","model":"x"}`,
		`garbage-not-json`,
		`{"session_id":"s1","run_id":"r1"}`,
	}
	if err := os.WriteFile(evFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, ok := m.buildSnapshot(wsA, "s1")
	if !ok {
		t.Fatal("有事件日志应返回 true")
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Events) != 2 {
		t.Fatalf("应 2 条有效事件（保留顺序），got %d: %v", len(decoded.Events), decoded.Events)
	}
	if decoded.Events[0]["event_type"] != "agent_start" || decoded.Events[1]["event_type"] != "llm_start" {
		t.Fatalf("event_type 顺序/保留异常: %v", decoded.Events)
	}
	if _, has := decoded.Events[0]["session_id"]; has {
		t.Fatal("session_id 应从快照事件中剔除")
	}
	if decoded.Events[0]["run_id"] != "r1" {
		t.Fatalf("其余字段应保留: %v", decoded.Events[0])
	}

	if _, ok := m.buildSnapshot(wsA, "nope"); ok {
		t.Fatal("无日志应返回 false")
	}
	if err := os.WriteFile(evFile, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.buildSnapshot(wsA, "s1"); ok {
		t.Fatal("空日志应返回 false")
	}
}

// new_session 冷启动恢复（有事件日志）→ 单行 command_response 带 restored:"snapshot"
// 与有序快照 events（不再逐行重放 stdout；不带 messages）
func TestNewSessionRestoresSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离 APP 级存储
	wsA := t.TempDir()
	m, buf := testManager(t)
	evFile := filepath.Join(wsEventsDir(wsA), "sess1.jsonl")
	if err := os.MkdirAll(filepath.Dir(evFile), 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"event_type":"agent_start","session_id":"sess1","workspace":"` + wsA + `","run_id":"r1"}`,
		`{"event_type":"llm_start","session_id":"sess1","workspace":"` + wsA + `","model":"x"}`,
		`{"event_type":"llm_end","session_id":"sess1","workspace":"` + wsA + `","usage":{"Input":100,"Output":20}}`,
	}
	if err := os.WriteFile(evFile, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.dispatch(cmd(0, "new_session", map[string]any{"workspace": wsA, "id": "sess1"}))

	// 环境噪音容忍：seatbelt 降级时 create 会先写一条 sandbox_status，仅取 command_response 行断言
	var resp map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err == nil && m["event_type"] == "command_response" {
			if resp != nil {
				t.Fatal("应恰 1 条 command_response，got 多条")
			}
			resp = m
		}
	}
	if resp == nil {
		t.Fatal("无 command_response 响应")
	}
	if resp["ok"] != true {
		t.Fatalf("响应异常: %v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	// 懒迁移：无 record 但有 events → 重放生成 checkpoint 并落盘（新正确行为）
	if data["restored"] != "checkpoint" {
		t.Fatalf("restored = %v, want checkpoint（懒迁移）", data["restored"])
	}
	vc, _ := data["view_checkpoint"].(map[string]any)
	blocks, _ := vc["blocks"].([]any)
	if len(blocks) == 0 {
		t.Fatalf("checkpoint blocks 不应为空（3 条事件应生成视图）")
	}
	if _, has := data["messages"]; has {
		t.Fatal("checkpoint 路径不应带 messages")
	}
	if data["session_id"] != "sess1" {
		t.Fatalf("session_id = %v", data["session_id"])
	}
}

// new_session warm 重载（bridge 存活 + renderer 重载丢 view）：进程内已有会话 →
// 仍聚合事件日志为快照（修复 ctx/usage/turns/cost 重建），单行响应带 restored:"snapshot"
func TestNewSessionSnapshotOnWarmReload(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离 APP 级存储
	wsA := t.TempDir()
	m, buf := testManager(t)
	if _, err := m.create(wsA, "sess1", ""); err != nil {
		t.Fatal(err)
	}
	bs := m.get(wsA, "sess1")
	if bs == nil {
		t.Fatal("session should exist in-process")
	}
	bs.emit(&events.AgentStart{RunId: "r1", Content: "任务", EventType: events.AgentStartType})
	ev := filepath.Join(wsEventsDir(wsA), "sess1.jsonl")
	if b, err := os.ReadFile(ev); err != nil || !strings.Contains(string(b), "agent_start") {
		t.Fatalf("事件应已落日志: %q err=%v", b, err)
	}

	buf.Reset()
	m.dispatch(cmd(0, "new_session", map[string]any{"workspace": wsA, "id": "sess1"}))

	raw := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(raw) != 1 {
		t.Fatalf("warm reload 应单行响应（快照内嵌），got %d 行: %v", len(raw), raw)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(raw[0]), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["event_type"] != "command_response" || resp["ok"] != true {
		t.Fatalf("响应异常: %v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data["restored"] != "snapshot" {
		t.Fatalf("warm reload 应带 restored=snapshot，got %+v", data)
	}
	snap, _ := data["snapshot"].(map[string]any)
	evs, _ := snap["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("快照应含 1 条事件，got %d", len(evs))
	}
	if _, has := data["messages"]; has {
		t.Fatal("warm reload snapshot 路径不应带 messages")
	}
}

// new_session 无事件日志且无历史消息（全新会话）→ 无 restored/snapshot/messages（空视图）
func TestNewSessionFallbackMessages(t *testing.T) {
	wsA := t.TempDir()
	m, buf := testManager(t)

	m.dispatch(cmd(0, "new_session", map[string]any{"workspace": wsA}))
	var resp map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err == nil && m["event_type"] == "command_response" {
			resp = m
		}
	}
	if resp == nil {
		t.Fatal("无 command_response 响应")
	}
	data, _ := resp["data"].(map[string]any)
	if _, has := data["restored"]; has {
		t.Fatal("无事件日志且无消息不应带 restored")
	}
	if _, has := data["snapshot"]; has {
		t.Fatal("无事件日志不应带 snapshot")
	}
	if _, has := data["messages"]; has {
		t.Fatal("无恢复内容不应带 messages")
	}
}

// list updated_at 降序（相同 → id 降序）；条目带 updated_at/title
func TestListSessionsUpdatedAtSort(t *testing.T) {
	wsA := t.TempDir()
	m, buf := testManager(t)
	for _, id := range []string{"s-a", "s-b", "s-c"} {
		m.dispatch(cmd(0, "new_session", map[string]any{"workspace": wsA, "id": id}))
		readLines(buf)
	}

	m.dispatch(cmd(0, "list", map[string]any{"workspace": wsA}))
	lines := readLines(buf)
	if len(lines) != 1 {
		t.Fatalf("list 应 1 响应，got %d", len(lines))
	}
	data, _ := lines[0]["data"].(map[string]any)
	sessions, _ := data["sessions"].([]any)
	if len(sessions) != 3 {
		t.Fatalf("应 3 会话，got %d", len(sessions))
	}
	// 新创建 lastActive 更大（毫秒递增场景）→ 双保险按 updated_at/id 降序：s-c, s-b, s-a
	first, _ := sessions[0].(map[string]any)
	last, _ := sessions[2].(map[string]any)
	if first["id"] != "s-c" {
		t.Fatalf("updated_at 降序应 s-c 第一，got %v", first["id"])
	}
	if last["id"] != "s-a" {
		t.Fatalf("最旧应 s-a 最后，got %v", last["id"])
	}
	for _, s := range sessions {
		sm, _ := s.(map[string]any)
		if _, has := sm["updated_at"]; !has {
			t.Fatal("会话条目应带 updated_at")
		}
		if _, has := sm["title"]; !has {
			t.Fatal("会话条目应带 title")
		}
	}
}

// 主会话引擎按 persona profile 装配工具：code = 全工具（含 bash/agent_*/plan_submit），
// work = 文件(无 bash)+todo（不派子 agent / 不规划）
func TestSessionEngineRegistersAllTools(t *testing.T) {
	assertNoUserPrices(t)
	t.Setenv("HOME", t.TempDir()) // 封闭：不读真实 settings.json（否则污染全局 userPriceTable，跨用例串价）
	reg := subagent.NewRegistry()
	bg := func() context.Context { return context.Background() }
	sk, err := skills.NewRegistry(t.TempDir()) // 空技能目录：空注册表（工具注册不受影响）
	if err != nil {
		t.Fatal(err)
	}

	code, _ := newSessionEngine(t.TempDir(), "", nil, loadProviderConfig(), "deepseek", loadProviderConfig().defaultModel(), string(provider.ReasoningEffortLevelLow), bg, reg, events.NewQuestionWaiter(0), sk, buildSubagentRegistry(t.TempDir()), nil, nil, profiles["code"], nil, nil, testPythonRuntime(t), nil, nil)
	for _, name := range []string{
		"read_file", "write_file", "edit_file", "grep", "bash", // 文件工具
		"run_python",              // 办公执行通道（受管 Python 运行时）
		"todo_add", "todo_update", // 会话级待办
		"agent_spawn", "agent_send", "agent_interrupt", "TaskList", "TaskOutput", // 子 agent 家族（无 agent_wait：结果自动回传）
		"subagent_explore", // Explore 子 agent：专职问题分析
		"git_worktree",     // Git Worktree：Code 模式并行开发隔离目录
		"load_skill",       // 技能渐进披露激活
	} {
		if _, err := code.GetTool(context.Background(), name); err != nil {
			t.Fatalf("code profile: 工具 %s 未注册: %v", name, err)
		}
	}

	// work 也注册 run_python（办公执行通道不是 code 专属；对 work 而言它是唯一执行通道）
	work, _ := newSessionEngine(t.TempDir(), "", nil, loadProviderConfig(), "deepseek", loadProviderConfig().defaultModel(), string(provider.ReasoningEffortLevelLow), bg, reg, events.NewQuestionWaiter(0), sk, buildSubagentRegistry(t.TempDir()), nil, nil, profiles["work"], nil, nil, testPythonRuntime(t), nil, nil)
	for _, name := range []string{"read_file", "write_file", "edit_file", "grep", "run_python", "todo_add", "todo_update", "load_skill"} {
		if _, err := work.GetTool(context.Background(), name); err != nil {
			t.Fatalf("work profile: 工具 %s 未注册: %v", name, err)
		}
	}
	for _, name := range []string{"bash", "agent_spawn", "subagent_explore", "git_worktree", "plan_submit"} {
		if _, err := work.GetTool(context.Background(), name); err == nil {
			t.Fatalf("work profile: %s 不应注册（办公助理安全边界）", name)
		}
	}

	// 未装配受管运行时（pyRT=nil，如 SDK 层用法）→ 不注册 run_python：
	// 注册一个必然失败的工具只会让模型白耗轮次。
	noRT, _ := newSessionEngine(t.TempDir(), "", nil, loadProviderConfig(), "deepseek", loadProviderConfig().defaultModel(), string(provider.ReasoningEffortLevelLow), bg, reg, events.NewQuestionWaiter(0), sk, buildSubagentRegistry(t.TempDir()), nil, nil, profiles["code"], nil, nil, nil, nil, nil)
	if _, err := noRT.GetTool(context.Background(), "run_python"); err == nil {
		t.Fatal("未装配受管运行时（pyRT=nil）时 run_python 不应注册")
	}
}

// TestExploreToolEngineRestricted 守卫 Explore 子 agent 的受限工具集：read_file/grep/glob/bash
// + load_skill（技能激活；2026-08-30 起 Explore 可用技能），不含 write/edit/agent_* 等落写或
// 调度工具（专职问题分析，不落写变更）。
func TestExploreToolEngineRestricted(t *testing.T) {
	sk, err := skills.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("skills registry: %v", err)
	}
	eng := exploreToolEngine(t.TempDir(), sk, nil)
	for _, name := range []string{"read_file", "grep", "glob", "bash", "load_skill"} {
		if _, err := eng.GetTool(context.Background(), name); err != nil {
			t.Fatalf("explore toolset should include %s: %v", name, err)
		}
	}
	for _, name := range []string{"write_file", "edit_file", "agent_spawn", "todo_add", "git_worktree"} {
		if _, err := eng.GetTool(context.Background(), name); err == nil {
			t.Fatalf("explore toolset must NOT include %s（专职分析工具集）", name)
		}
	}
}

// plan_submit 已移除：计划/文档使用 write_file，任务拆分使用 todo。
func TestPlanSubmitNotRegistered(t *testing.T) {
	assertNoUserPrices(t)
	t.Setenv("HOME", t.TempDir()) // 封闭：不读真实 settings.json（否则污染全局 userPriceTable，跨用例串价）
	sk, err := skills.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	code, _ := newSessionEngine(t.TempDir(), "", nil, loadProviderConfig(), "deepseek", loadProviderConfig().defaultModel(), string(provider.ReasoningEffortLevelLow), func() context.Context { return context.Background() }, subagent.NewRegistry(), events.NewQuestionWaiter(0), sk, buildSubagentRegistry(t.TempDir()), nil, nil, profiles["code"], nil, nil, nil, nil, nil)
	if _, err := code.GetTool(context.Background(), "plan_submit"); err == nil {
		t.Fatal("plan_submit must not be registered")
	}
}

// TestExecSandboxForWorkPersonaRunPython 端到端接线：装配了受管运行时的 work 会话必须拿到
// **真 Seatbelt 后端**（不是 nil/NoSandbox）。这是「run_python 的工作区文件边界」在 work
// persona 下唯一能生效的路径；只断言 needsExecSandbox 不够 —— 装配链上任何一处按 withBash
// 短路都会让断言通过而真机失效。
func TestExecSandboxForWorkPersonaRunPython(t *testing.T) {
	assertNoUserPrices(t)
	if !sandbox.Available() {
		t.Skip("sandbox-exec 不可用（非 macOS / 受限环境）")
	}
	t.Setenv("HOME", t.TempDir()) // loadSandboxSettings 默认 seatbelt
	appDataDirOverride = t.TempDir()
	t.Cleanup(func() { appDataDirOverride = "" })
	m := &manager{workspaces: map[string]*workspaceRuntime{}, provCfg: loadProviderConfig(), out: newOut(&strings.Builder{})}
	ws := t.TempDir()
	sbx := m.execSandbox(ws, "s1", profiles["work"], nil, testPythonRuntime(t))
	if sbx == nil {
		t.Fatal("work + 受管运行时：应构造沙箱（否则 run_python 越界写不受阻）")
	}
	// 必须真沙箱：越界写被拦（NoSandbox 下会成功）
	out, err := sbx.Run(context.Background(), sandbox.ExecSpec{
		Command: "/bin/sh -c 'echo x > /etc/go-code-exec-sandbox-probe'",
		Cwd:     ws,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("沙箱执行不应返回 Go error: %v", err)
	}
	if !sandbox.IsSeatbeltBlocked(out) {
		t.Fatalf("work 的 run_python 后端应是真沙箱（越界写被拦），got %q", out)
	}
	if _, statErr := os.Stat("/etc/go-code-exec-sandbox-probe"); statErr == nil {
		_ = os.Remove("/etc/go-code-exec-sandbox-probe")
		t.Fatal("越界文件竟然被创建")
	}
	// 无人依赖运行时（pyRT=nil）且无 bash → 无需沙箱
	if m.execSandbox(ws, "s1", profiles["work"], nil, nil) != nil {
		t.Fatal("work + 无运行时：没有执行通道，不应构造沙箱")
	}
}

// TestNeedsExecSandbox run_python 是执行通道：work persona（无 bash）也必须拿到沙箱后端。
// 2026-09 前的装配只按 withBash 判定 → work 的 run_python 静默落到 NoSandbox，工具描述
// 承诺的「工作区外写入被拒」当场失效（且没有任何报错）。
func TestNeedsExecSandbox(t *testing.T) {
	work := profiles["work"]
	code := profiles["code"]
	if work.withBash {
		t.Fatal("前提：work 无 bash")
	}
	rt := testPythonRuntime(t)

	// 未装配运行时：work 确实没有执行通道（无需沙箱）
	if needsExecSandbox(work, nil) {
		t.Fatal("work + 无运行时：没有执行通道，不应构造沙箱")
	}
	// 装配运行时：work 有 run_python → 必须构造沙箱
	if !needsExecSandbox(work, rt) {
		t.Fatal("work + 有 run_python：执行通道存在，必须构造沙箱（否则文件边界承诺失效）")
	}
	// code：无论如何都要（bash 在）
	if !needsExecSandbox(code, nil) || !needsExecSandbox(code, rt) {
		t.Fatal("code：有 bash，始终需要沙箱")
	}
}

// TestWorkPersonaPromptAndBinaryHintMentionRunPython 回归（2026-09）：work persona 的
// 系统提示词与 read_file 的二进制拒绝提示此前都**不提 run_python**——办公能力（受管
// Python + docx/xlsx/pptx/pdf 技能）其实已随包就位，模型却只能回「本会话不支持这类
// 格式，请导出成 txt」（TECH_PLAN §3.6：提示必须指向真实存在的能力）。两处文案都要
// 点名 run_python，且引导顺序是 load_skill（拿流程）→ run_python（执行）。
func TestWorkPersonaPromptAndBinaryHintMentionRunPython(t *testing.T) {
	// 1) 系统提示词：work 会话真实注入的就是这里（profiles["work"] → prompt.DesktopWork，
	//    见 buildLoop）；断言常量而非副本，改装配不改文案的回归也能被挡住。
	wp := profiles["work"].systemPrompt
	for _, s := range []string{"run_python", "load_skill"} {
		if !strings.Contains(wp, s) {
			t.Fatalf("work 提示词未提 %s（唯一执行通道/技能入口）: %s", s, wp)
		}
	}

	// 2) 二进制提示：办公四格式各给对应技能 + 库名。
	for _, c := range []struct{ file, skill, lib string }{
		{"报告.docx", "docx", "python-docx"},
		{"数据.xlsx", "xlsx", "openpyxl"},
		{"幻灯片.pptx", "pptx", "python-pptx"},
		{"论文.pdf", "pdf", "pypdf"},
	} {
		h := workBinaryHint(c.file, true)
		if !strings.Contains(h, "run_python") || !strings.Contains(h, "load_skill") {
			t.Fatalf("%s: 提示必须同时给出 load_skill 与 run_python: %s", c.file, h)
		}
		if !strings.Contains(h, `load_skill("`+c.skill+`")`) || !strings.Contains(h, c.lib) {
			t.Fatalf("%s: 应点名技能 %s 与库 %s: %s", c.file, c.skill, c.lib, h)
		}
		if strings.Contains(strings.ToLower(h), "bash") {
			t.Fatalf("%s: work 会话没有 bash，提示不得指向它: %s", c.file, h)
		}
		if strings.Contains(h, "not supported") || strings.Contains(h, "no managed Python") {
			t.Fatalf("%s: 有 run_python 通道，不得宣布不支持: %s", c.file, h)
		}
		// 引导顺序：同一句里先 load_skill 后 run_python（先拿 gotcha/流程，再执行）。
		ordered := false
		for _, sent := range strings.Split(h, ". ") {
			li, ri := strings.Index(sent, "load_skill"), strings.Index(sent, "run_python")
			ordered = ordered || (li >= 0 && ri > li)
		}
		if !ordered {
			t.Fatalf("%s: 引导顺序应为 load_skill → run_python: %s", c.file, h)
		}
	}

	// 3) 没有随包技能的格式（扩展名大小写不敏感）：仍先指向 run_python（脚本/标准库），
	//    确实无通道才如实告知——不把「无通道」提前套到办公格式上，也不反过来假装有技能。
	zipHint := workBinaryHint("archive.ZIP", true)
	if !strings.Contains(zipHint, "run_python") {
		t.Fatalf("无技能格式也应先指向 run_python: %s", zipHint)
	}
	if strings.Contains(zipHint, `load_skill("`) {
		t.Fatalf("无技能格式不得编造技能名: %s", zipHint)
	}

	// 4) 未装配受管运行时（pyRT=nil → run_python 不注册，见 newSessionEngine）：反过来
	//    不得提 run_python/load_skill，否则又变回「指向不存在的工具」。
	noRT := workBinaryHint("报告.docx", false)
	if strings.Contains(noRT, "run_python") || strings.Contains(noRT, "load_skill") {
		t.Fatalf("无 run_python 的装配下不得提它: %s", noRT)
	}
	if !strings.Contains(noRT, "txt") {
		t.Fatalf("真没有通道时应给出可行的替代（导出文本）: %s", noRT)
	}
}

// 模式审批门控：只读工具恒 Auto；变更工具按模式（manual 全审 / accept-edits 写自动 bash 审）
func TestParseModeApprovalGating(t *testing.T) {
	manual := parseMode("manual")
	if manual.Approve(core.ToolCall{Name: "edit_file"}) != agents.ApprovalRequired {
		t.Fatal("manual: edit_file 应审批")
	}
	if manual.Approve(core.ToolCall{Name: "write_file"}) != agents.ApprovalRequired {
		t.Fatal("manual: write_file 应审批")
	}
	if manual.Approve(core.ToolCall{Name: "bash"}) != agents.ApprovalRequired {
		t.Fatal("manual: bash 应审批")
	}
	if manual.Approve(core.ToolCall{Name: "run_python"}) != agents.ApprovalRequired {
		t.Fatal("manual: run_python 应审批（执行类工具，能写文件）")
	}
	if manual.Approve(core.ToolCall{Name: "read_file"}) != agents.ApprovalAuto {
		t.Fatal("manual: read_file 应自动")
	}
	if manual.Approve(core.ToolCall{Name: "todo_add"}) != agents.ApprovalAuto {
		t.Fatal("manual: todo_add 应自动")
	}

	accept := parseMode("accept-edits")
	if accept.Approve(core.ToolCall{Name: "write_file"}) != agents.ApprovalAuto {
		t.Fatal("accept-edits: write_file 应自动")
	}
	if accept.Approve(core.ToolCall{Name: "bash"}) != agents.ApprovalRequired {
		t.Fatal("accept-edits: bash 应审批")
	}
	if accept.Approve(core.ToolCall{Name: "run_python"}) != agents.ApprovalRequired {
		t.Fatal("accept-edits: run_python 应审批（与 bash 同组：不限写文件，执行类都需确认）")
	}
	if accept.Approve(core.ToolCall{Name: "read_file"}) != agents.ApprovalAuto {
		t.Fatal("accept-edits: read_file 应自动")
	}
}

// 客户端三模式（2026-08-15 重构；2026-08-17 auto 改为全自动）：
// hitl=用户确认（write/edit/bash 审批）、auto=全自动（write/edit/bash 均不审批，
// bash 工具层静态高危拦截/沙箱仍生效）、full-access=完全访问（全部自动放行）
func TestParseModeThreeModes(t *testing.T) {
	assertNoUserPrices(t)
	hitl := parseMode("hitl")
	if hitl.Approve(core.ToolCall{Name: "write_file"}) != agents.ApprovalRequired {
		t.Fatal("hitl: write_file 应审批")
	}
	if hitl.Approve(core.ToolCall{Name: "edit_file"}) != agents.ApprovalRequired {
		t.Fatal("hitl: edit_file 应审批")
	}
	if hitl.Approve(core.ToolCall{Name: "bash"}) != agents.ApprovalRequired {
		t.Fatal("hitl: bash 应审批")
	}
	// 审批绕过防线：run_python 能写文件/执行命令，漏列即等于开一个后门。
	if hitl.Approve(core.ToolCall{Name: "run_python"}) != agents.ApprovalRequired {
		t.Fatal("hitl: run_python 应审批（防审批绕过）")
	}
	if hitl.Approve(core.ToolCall{Name: "read_file"}) != agents.ApprovalAuto {
		t.Fatal("hitl: read_file 应自动")
	}
	if hitl.Approve(core.ToolCall{Name: "todo_add"}) != agents.ApprovalAuto {
		t.Fatal("hitl: todo_add 应自动")
	}

	auto := parseMode("auto")
	if auto.Name != "auto" {
		t.Fatalf("auto 模式名应为 auto, got %s", auto.Name)
	}
	// 2026-08-17：auto 改为全自动放行（此前 bash 每次执行弹审批 → 用户无感知即"卡住"）
	for _, name := range []string{"write_file", "edit_file", "bash", "run_python", "read_file"} {
		if auto.Approve(core.ToolCall{Name: name}) != agents.ApprovalAuto {
			t.Fatalf("auto: %s 应自动放行", name)
		}
	}

	fa := parseMode("full-access")
	if fa.Name != "full-access" {
		t.Fatalf("full-access 模式名应为 full-access, got %s", fa.Name)
	}
	for _, name := range []string{"write_file", "edit_file", "bash", "run_python", "read_file"} {
		if fa.Approve(core.ToolCall{Name: name}) != agents.ApprovalAuto {
			t.Fatalf("full-access: %s 应自动放行", name)
		}
	}
}

// emit llm_end：记录一条指标（metrics 文件）+ 事件打 cost_usd（价表在宿主）
func TestEmitRecordsMetrics(t *testing.T) {
	assertNoUserPrices(t)
	wsA := t.TempDir()
	m, buf := testManager(t)
	m.metrics = NewFileMetricsStore(filepath.Join(t.TempDir(), "metrics.jsonl"))
	bs, err := m.create(wsA, "s1", "")
	if err != nil {
		t.Fatal(err)
	}

	bs.emit(&events.LLMEnd{Model: "deepseek-v4-flash", Usage: &core.Usage{Input: 1000, CacheRead: 900, Output: 100}, Timestamp: time.Now(), EventType: events.LLMEndType})
	// 压缩调用同样记录指标（成本随 compress_end 的 Usage；此前压缩成本在指标层缺失）
	bs.emit(&events.CompressEnd{Before: 100, After: 20, Reason: "automatic", Model: "deepseek-v4-flash",
		Usage: &core.Usage{Input: 2000, Output: 300, Cost: core.Cost{Input: 280, Output: 84, Total: 364}}, EventType: events.CompressEndType})

	// 指标落两行（llm_end + compress_end）
	rep, err := m.metrics.Aggregate(MetricsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Input != 1000+2000 || rep.Total.CacheRead != 900 {
		t.Fatalf("metrics total = %+v", rep.Total)
	}
	if len(rep.ByModel) != 1 || rep.ByModel[0].CostUsd <= 0 {
		t.Fatalf("bymodel = %+v", rep.ByModel)
	}
	// 事件打 cost_usd（非零；llm_end 一行）
	lines := onlyEvent(readLines(buf), events.LLMEndType)
	if len(lines) != 1 {
		t.Fatalf("应 1 行事件, got %d", len(lines))
	}
	if c, ok := lines[0]["cost_usd"].(float64); !ok || c <= 0 {
		t.Fatalf("llm_end 应带 cost_usd, got %v", lines[0]["cost_usd"])
	}
}

// metrics 命令：返回聚合报告（Total + ByDay/ByModel/ByWorkspace）
func TestMetricsCommand(t *testing.T) {
	wsA := t.TempDir()
	m, buf := testManager(t)
	m.metrics = NewFileMetricsStore(filepath.Join(t.TempDir(), "metrics.jsonl"))
	_ = m.metrics.Record(MetricsEntry{Timestamp: time.Now(), Workspace: wsA, SessionID: "s1", Model: "deepseek-v4-flash", Usage: core.Usage{Input: 500, Output: 50}})

	m.dispatch(cmd(0, "metrics", map[string]any{}))
	lines := readLines(buf)
	if len(lines) != 1 {
		t.Fatalf("应 1 响应, got %d", len(lines))
	}
	if lines[0]["ok"] != true {
		t.Fatalf("metrics 应成功: %v", lines[0])
	}
	data := lines[0]["data"].(map[string]any)
	report, ok := data["report"].(map[string]any)
	if !ok {
		t.Fatalf("响应应带 report: %v", data)
	}
	total := report["Total"].(map[string]any)
	if total["Input"] != float64(500) || total["CostUsd"] == nil {
		t.Fatalf("report total = %v", total)
	}
	if byDay, ok := report["ByDay"].([]any); !ok || len(byDay) != 1 {
		t.Fatalf("report byday = %v", report["ByDay"])
	}
}

// switch_persona：重建 loop（persona 字段更新；work profile 生效）；未知 persona 报错
func TestSwitchPersona(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	bs, err := m.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if bs.persona != "code" {
		t.Fatalf("默认 persona 应为 code, got %s", bs.persona)
	}

	m.dispatch(cmd(0, "switch_persona", map[string]any{"workspace": ws, "session_id": "s1", "persona": "work"}))
	lines := commandResponses(readLines(buf))
	if len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("switch_persona 应成功: %v", lines[0])
	}
	if bs.persona != "work" {
		t.Fatalf("persona 应切为 work, got %s", bs.persona)
	}
	if bs.loop == nil {
		t.Fatal("切换后 loop 不应为 nil")
	}

	m.dispatch(cmd(0, "switch_persona", map[string]any{"workspace": ws, "session_id": "s1", "persona": "nope"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("未知 persona 应报错")
	}
}

// switch_model：价表校验 + SetModel；未知模型报错
func TestSwitchModel(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	bs, err := m.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}

	m.dispatch(cmd(0, "switch_model", map[string]any{"workspace": ws, "session_id": "s1", "model": "deepseek-chat"}))
	lines := commandResponses(readLines(buf))
	if len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("switch_model 应成功: %v", lines[0])
	}
	if bs.model != "deepseek-chat" {
		t.Fatalf("model 应切为 deepseek-chat, got %s", bs.model)
	}

	m.dispatch(cmd(0, "switch_model", map[string]any{"workspace": ws, "session_id": "s1", "model": "nope"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("未知模型（价表无）应报错")
	}
}

// file_preview：读工作区内文件 + 越界拒绝
func TestFilePreview(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)

	m.dispatch(cmd(0, "file_preview", map[string]any{"workspace": ws, "path": "a.txt"}))
	lines := readLines(buf)
	if lines[0]["ok"] != true {
		t.Fatalf("file_preview 应成功: %v", lines[0])
	}
	data := lines[0]["data"].(map[string]any)
	if data["content"] != "line1\nline2\n" || data["lines"] != float64(2) {
		t.Fatalf("file_preview 内容 = %v", data)
	}

	// 越界路径拒绝
	m.dispatch(cmd(0, "file_preview", map[string]any{"workspace": ws, "path": "../evil.txt"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("越界路径应拒绝")
	}
	// 文件不存在
	m.dispatch(cmd(0, "file_preview", map[string]any{"workspace": ws, "path": "missing.txt"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("文件不存在应报错")
	}
}

// TestFilePreviewAbsolutePath 统一 open-file：绝对路径（工作区外）可读，且全量不截断。
func TestFilePreviewAbsolutePath(t *testing.T) {
	outside := t.TempDir()                           // 工作区外目录
	big := strings.Repeat("line of content\n", 3000) // 3000 行 > 旧 2000 截断阈值
	if err := os.WriteFile(filepath.Join(outside, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)
	ws := t.TempDir()

	// 绝对路径 + 工作区（工作区外文件）
	m.dispatch(cmd(0, "file_preview", map[string]any{"workspace": ws, "path": filepath.Join(outside, "big.txt")}))
	lines := readLines(buf)
	if len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("工作区外绝对路径应成功: %v", lines[0])
	}
	data := lines[0]["data"].(map[string]any)
	if data["content"] != big {
		t.Fatalf("全量内容不应截断: got %d chars, want %d", len(data["content"].(string)), len(big))
	}
	if data["truncated"] != false {
		t.Fatalf("truncated 应为 false: %v", data["truncated"])
	}
	if data["lines"] != float64(3000) {
		t.Fatalf("lines 应为 3000: %v", data["lines"])
	}

	// 无 workspace（无会话场景）+ 绝对路径：仍可读（相对路径才需要 workspace）
	m.dispatch(cmd(0, "file_preview", map[string]any{"path": filepath.Join(outside, "big.txt")}))
	lines = readLines(buf)
	if len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("无 workspace 的绝对路径应成功: %v", lines[0])
	}
	// 无 workspace + 相对路径 → 拒绝
	m.dispatch(cmd(0, "file_preview", map[string]any{"path": "rel.txt"}))
	lines = readLines(buf)
	if len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatalf("无 workspace 的相对路径应拒绝: %v", lines[0])
	}
}

// xlsx_preview：路径口径与 file_preview 完全一致（同一 resolveWorkspacePath），
// 拒绝理由不因「看的是表格」而变；无 workspace + 相对路径拒、绝对路径放行。
//
// 这里只测**同步的校验路径**（不真跑转换）：真转换需要受管运行时 + openpyxl，
// 属慢测试，见 tools/builtin/xlsx_preview_real_test.go。
func TestXlsxPreviewPathGuards(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "book.xlsx"), []byte("not a real xlsx"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)

	// path 为空
	m.dispatch(cmd(0, "xlsx_preview", map[string]any{"workspace": ws, "path": ""}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("path 为空应报错")
	}
	// 越界（.. 逃逸）
	m.dispatch(cmd(0, "xlsx_preview", map[string]any{"workspace": ws, "path": "../evil.xlsx"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("越界路径应拒绝")
	}
	// 无 workspace + 相对路径 → 拒绝（与 file_preview 同口径）
	m.dispatch(cmd(0, "xlsx_preview", map[string]any{"path": "rel.xlsx"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("无 workspace 的相对路径应拒绝")
	}
	// ~/.go-code 受保护目录：拒绝（settings.json 里可能含 api_key）
	home, err := os.UserHomeDir()
	if err == nil {
		m.dispatch(cmd(0, "xlsx_preview", map[string]any{"path": filepath.Join(home, ".go-code", "secret.xlsx")}))
		if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
			t.Fatal("~/.go-code 下的路径应拒绝")
		}
	}
	// 目录而非文件
	m.dispatch(cmd(0, "xlsx_preview", map[string]any{"workspace": ws, "path": "."}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("目录应拒绝（不是 xlsx 文件）")
	}
	// 文件不存在
	m.dispatch(cmd(0, "xlsx_preview", map[string]any{"workspace": ws, "path": "missing.xlsx"}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("文件不存在应报错")
	}
}

// xlsx_preview 的合法路径会**异步**转换（首次调用可能先 bootstrap 运行时，故不能阻塞
// dispatch）：本用例只验证「命令被受理且不会同步返回错误」——真产物见慢测试。
// 用不存在的 xlsx（内容非法）会让转换失败，但失败也是异步回，不阻塞主循环。
func TestXlsxPreviewAsyncDispatch(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "book.xlsx"), []byte("PK\x03\x04 not really a workbook"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, _ := testManager(t)
	done := make(chan struct{})
	go func() {
		m.dispatch(cmd(0, "xlsx_preview", map[string]any{"workspace": ws, "path": "book.xlsx"}))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("xlsx_preview 的校验与派发不应阻塞（转换必须异步）")
	}
}

// answer_question 命令：用户回答 ask_user 提问批次（batch_id 为等待器锚，
// answers 为逐题回答）；未知 batch_id 报错
func TestAnswerQuestionCommand(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	bs, err := m.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}

	// 注册一个待答提问批次（等价于 ask_user 工具阻塞在 waiter 上）
	wait := bs.questions.Begin("q1")
	m.dispatch(cmd(0, "answer_question", map[string]any{"workspace": ws, "session_id": "s1", "batch_id": "q1",
		"answers": []any{map[string]any{"question_id": "q1-1", "answer": "选A"}, map[string]any{"question_id": "q1-2", "answer": "只改一处"}}}))
	responses := commandResponses(readLines(buf))
	if len(responses) == 0 || responses[0]["ok"] != true {
		t.Fatal("answer_question 应成功")
	}
	ans, err := wait(context.Background())
	if err != nil || len(ans) != 2 || ans[0].Answer != "选A" || ans[1].Answer != "只改一处" {
		t.Fatalf("等待器应收到整批回答 %+v, err=%v", ans, err)
	}

	// 未知 batch_id → 报错
	m.dispatch(cmd(0, "answer_question", map[string]any{"workspace": ws, "session_id": "s1", "batch_id": "nope",
		"answers": []any{map[string]any{"question_id": "x", "answer": "y"}}}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != false {
		t.Fatal("未知 batch_id 应报错")
	}
}

// waitSkillResult 轮询等 load_skill 的命令结果：返回 (命令是否被接受, command_result 文本)。
// command_result 在 RunStream 入口（injectCommands）发出、早于后续 LLM 尝试——可确定性等。
// 顺带校验 dispatch 未产出 ok:false 响应（会话不存在 / Command 入队失败）。
func waitSkillResult(t *testing.T, buf *syncBuf, name string, timeout time.Duration) (bool, string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, l := range readLines(buf) {
			if l["event_type"] == "command_response" && l["ok"] != true {
				return false, ""
			}
			if l["event_type"] == "command_result" && l["name"] == name {
				if r, ok := l["result"].(string); ok {
					return true, r
				}
				if e, ok := l["error"].(string); ok {
					return true, e
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout waiting command_result")
	return false, ""
}

// Skills 命令族（2026-08-14）：清单（含启用标志）/ 开关（真过滤 + 持久化）/ 详情（预览）/
// load_skill（SDK 命令框架 skills load，禁用技能拒绝）。命令消息剥离后无用户输入 → 不触发 LLM。
func TestSkillsCommands(t *testing.T) {
	ws := t.TempDir()
	// 建工作区技能（分层注册表规范目录：{ws}/.go-code/skills）
	skillDir := filepath.Join(ws, ".go-code", "skills", "code-review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: code-review\ndescription: 代码评审\n---\n按规范逐条审查变更。\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)

	// 1) skills 清单
	m.dispatch(cmd(0, "skills", map[string]any{"workspace": ws}))
	lines := readLines(buf)
	if len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("skills 应成功: %v", lines)
	}
	arr := lines[0]["data"].(map[string]any)["skills"].([]any)
	if len(arr) != 1 {
		t.Fatalf("want 1 skill, got %v", arr)
	}
	s0 := arr[0].(map[string]any)
	if s0["name"] != "code-review" || s0["enabled"] != true {
		t.Fatalf("skill meta wrong: %v", s0)
	}

	// 2) skill_toggle 禁用 → 清单 enabled=false + skills.json 持久化
	m.dispatch(cmd(0, "skill_toggle", map[string]any{"workspace": ws, "name": "code-review", "enabled": false}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatal("skill_toggle 应成功")
	}
	m.dispatch(cmd(0, "skills", map[string]any{"workspace": ws}))
	lines = readLines(buf)
	s0 = lines[0]["data"].(map[string]any)["skills"].([]any)[0].(map[string]any)
	if s0["enabled"] != false {
		t.Fatalf("禁用后 enabled 应为 false: %v", s0)
	}
	if b, err := os.ReadFile(filepath.Join(ws, ".go-code", "skills.json")); err != nil || !strings.Contains(string(b), "code-review") {
		t.Fatalf("skills.json 应持久化禁用集: %q %v", b, err)
	}

	// 3) skill_get 详情（预览：禁用技能也可取）
	m.dispatch(cmd(0, "skill_get", map[string]any{"workspace": ws, "name": "code-review"}))
	lines = readLines(buf)
	if len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("skill_get 应成功: %v", lines)
	}
	gi := lines[0]["data"].(map[string]any)
	if !strings.Contains(gi["instructions"].(string), "逐条审查") {
		t.Fatalf("skill_get 应含指令: %v", gi)
	}

	// 4) load_skill：会话 + 命令框架加载。命令消费后 run 仍会尝试一轮 LLM（空 token → 401 重试），
	// 但 CommandResult 在 RunStream 入口（injectCommands）发出、早于 LLM——可确定性断言；
	// 结尾 shutdownWorkspace 停掉两个会话的 run 循环（避免 goroutine 在 TempDir 清理期间写 store）。
	// 4a) 禁用技能 → CommandResult error 含 disabled
	if _, err := m.create(ws, "s1", ""); err != nil {
		t.Fatal(err)
	}
	m.dispatch(cmd(0, "load_skill", map[string]any{"workspace": ws, "session_id": "s1", "name": "code-review"}))
	ok, cr := waitSkillResult(t, buf, "skills", 5*time.Second)
	if !ok {
		t.Fatal("load_skill 命令应被接受")
	}
	if !strings.Contains(cr, "disabled") {
		t.Fatalf("禁用技能 load 应报 disabled, got %q", cr)
	}

	// 4b) 重新启用 + 新会话 → CommandResult loaded into context
	m.dispatch(cmd(0, "skill_toggle", map[string]any{"workspace": ws, "name": "code-review", "enabled": true}))
	if _, err := m.create(ws, "s2", ""); err != nil {
		t.Fatal(err)
	}
	m.dispatch(cmd(0, "load_skill", map[string]any{"workspace": ws, "session_id": "s2", "name": "code-review"}))
	ok, cr = waitSkillResult(t, buf, "skills", 5*time.Second)
	if !ok {
		t.Fatal("load_skill（启用）命令应被接受")
	}
	if !strings.Contains(cr, "loaded") {
		t.Fatalf("启用技能 load 应 loaded, got %q", cr)
	}

	m.shutdownWorkspace(ws) // 停掉 s1/s2 的 run 循环（401 重试止损 + 落盘收尾）
}

// TestCommandSlashDispatch 斜杠命令（/xxx）：/技能名→skills load（CommandResult loaded）；
// /clear→清空上下文（CommandResult cleared，run 在 LLM 前 Stop 无需 provider）；
// 未知命令→同步错误响应。各命令用独立会话（skills load 后 run 会继续 LLM 401 重试，锁不跨会话）。
func TestCommandSlashDispatch(t *testing.T) {
	ws := t.TempDir()
	skillDir := filepath.Join(ws, ".go-code", "skills", "code-review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: code-review\ndescription: 评审\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)
	for _, id := range []string{"s1", "s2", "s3"} {
		if _, err := m.create(ws, id, ""); err != nil {
			t.Fatal(err)
		}
	}

	// /code-review → skills load → CommandResult "loaded"（CommandResult 早于 LLM 发出）
	m.dispatch(cmd(0, "command", map[string]any{"workspace": ws, "session_id": "s1", "line": "/code-review"}))
	ok, cr := waitSkillResult(t, buf, "skills", 5*time.Second)
	if !ok {
		t.Fatal("/code-review 命令应被接受")
	}
	if !strings.Contains(cr, "loaded") {
		t.Fatalf("/code-review 应加载技能, got %q", cr)
	}

	// /clear → CommandResult "context cleared"（run 在 LLM 前 Stop，无 401 重试）
	m.dispatch(cmd(0, "command", map[string]any{"workspace": ws, "session_id": "s2", "line": "/clear"}))
	ok, cr = waitSkillResult(t, buf, "clear", 5*time.Second)
	if !ok {
		t.Fatal("/clear 命令应被接受")
	}
	if !strings.Contains(cr, "cleared") {
		t.Fatalf("/clear 应返回 context cleared, got %q", cr)
	}

	// /nope → 未识别命令：按普通用户文本回退进上下文（问题十一约定），不再报错死局
	m.dispatch(cmd(0, "command", map[string]any{"workspace": ws, "session_id": "s3", "line": "/nope"}))
	// 过滤 command_response（并发 agent_start/end 事件可能先到，不能假设第一行是响应）
	var respLine map[string]any
	for _, l := range readLines(buf) {
		if l["event_type"] == "command_response" {
			respLine = l
			break
		}
	}
	if respLine == nil || respLine["ok"] != true {
		t.Fatalf("/nope 应回退为文本（ok:true）, got %v", respLine)
	}
	if bs3 := m.get(ws, "s3"); bs3 != nil {
		found := false
		for _, msg := range bs3.s.History() {
			for _, c := range msg.Content {
				if c.Type == core.ContentTypeText && strings.Contains(c.Content, "/nope") {
					found = true
				}
			}
		}
		if !found {
			t.Fatal("/nope 原文应作为用户消息进入会话历史")
		}
	}

	m.shutdownWorkspace(ws) // 停掉 s1 的 401 重试 run
}

// TestCommandReloadSkills /reload_skills：往 .go-code/skills 新增技能后无需重启，命令重扫注册表并立即生效。
func TestCommandReloadSkills(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	if _, err := m.create(ws, "s1", ""); err != nil {
		t.Fatal(err)
	}
	// 运行态已建好（此时注册表为空）；模拟「之后才把技能放进 .go-code/skills」
	skillDir := filepath.Join(ws, ".go-code", "skills", "new-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: new-skill\ndescription: 新增技能\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// /reload_skills → 重扫 + CommandResult（无 SDK run 触发，无 401 重试）
	m.dispatch(cmd(0, "command", map[string]any{"workspace": ws, "session_id": "s1", "line": "/reload_skills"}))
	ok, cr := waitSkillResult(t, buf, "reload_skills", 5*time.Second)
	if !ok {
		t.Fatal("/reload_skills 命令应被接受")
	}
	if !strings.Contains(cr, "已重载") || !strings.Contains(cr, "1") {
		t.Fatalf("/reload_skills 结果异常, got %q", cr)
	}

	// 重载后 /new-skill 应能加载（证明注册表已刷新到会话 loop）
	m.dispatch(cmd(0, "command", map[string]any{"workspace": ws, "session_id": "s1", "line": "/new-skill"}))
	ok, cr = waitSkillResult(t, buf, "skills", 5*time.Second)
	if !ok || !strings.Contains(cr, "loaded") {
		t.Fatalf("/new-skill 应能加载, got %q", cr)
	}

	m.shutdownWorkspace(ws) // 停掉 skills load 后的 401 重试 run
}

// TestSamplingDefaults 验证采样默认（宿主决策 2026-08）：DeepSeek 显式
// top_p=0.95 / temperature=1.0（命名常量）；OpenAI 保持厂商默认（nil）。
func TestSamplingDefaults(t *testing.T) {
	ds := &providerConfig{active: "deepseek"}
	temp, topP := ds.samplingDefaults()
	if temp == nil || *temp != deepseekDefaultTemperature {
		t.Errorf("deepseek temperature = %v, want %v", temp, deepseekDefaultTemperature)
	}
	if topP == nil || *topP != deepseekDefaultTopP {
		t.Errorf("deepseek top_p = %v, want %v", topP, deepseekDefaultTopP)
	}

	oa := &providerConfig{active: "openai"}
	if temp, topP := oa.samplingDefaults(); temp != nil || topP != nil {
		t.Errorf("openai samplingDefaults = (%v, %v), want (nil, nil)", temp, topP)
	}
}

// TestMaxTokensFor 每模型输出上限：显式配置优先 → 已知模型官方默认 → defaultMaxTokens 兜底。
func TestMaxTokensFor(t *testing.T) {
	assertNoUserPrices(t)
	pc := &providerConfig{
		active:    "deepseek",
		maxTokens: map[string]map[string]int64{"deepseek": {"deepseek-v4-flash-vision-exp": 200000}},
	}
	// 显式配置优先（覆盖 known 默认 8192）
	if got := pc.maxTokensFor("deepseek-v4-flash-vision-exp"); got != 200000 {
		t.Errorf("explicit max_tokens = %d, want 200000", got)
	}
	// 未显式配置 → known 默认表
	if got := pc.maxTokensFor("deepseek-chat"); got != 8192 {
		t.Errorf("known default deepseek-chat = %d, want 8192", got)
	}
	// 视觉模型默认输出上限
	defaultPC := &providerConfig{active: "deepseek", maxTokens: map[string]map[string]int64{"deepseek": {}}}
	if got := defaultPC.maxTokensFor("deepseek-v4-flash-vision-exp"); got != 8192 {
		t.Errorf("known default vision model = %d, want 8192", got)
	}
	// 未知模型 → 兜底
	if got := pc.maxTokensFor("nope-model"); got != defaultMaxTokens {
		t.Errorf("fallback = %d, want %d", got, defaultMaxTokens)
	}

	// 默认配置（loadProviderConfig）：DeepSeek 视觉模型默认 384k（注册表真实值）；openai gpt-4o 16384。
	// HOME 指向空临时目录：不读用户真实 ~/.go-code/settings.json（测试封闭）。
	t.Setenv("HOME", t.TempDir())
	fresh := loadProviderConfig()
	fresh.active = "deepseek"
	if got := fresh.maxTokensFor("deepseek-v4-flash-vision-exp"); got != 384000 {
		t.Errorf("default deepseek vision max_tokens = %d, want 384000", got)
	}
	fresh.active = "openai"
	if got := fresh.maxTokensFor("gpt-4o"); got != 16384 {
		t.Errorf("default gpt-4o = %d, want 16384", got)
	}
	// 窗口：DeepSeek 视觉模型默认 1M；OpenCode 兼容的旧 Flash 默认 1M。
	fresh.active = "deepseek"
	if got := fresh.windowFor("deepseek-v4-flash-vision-exp"); got != 1000000 {
		t.Errorf("default deepseek vision window = %d, want 1000000", got)
	}
	// setMaxTokens 不影响默认表（JSON 派生；深拷贝隔离）
	fresh.setMaxTokens("deepseek", map[string]int64{"deepseek-v4-flash-vision-exp": 12345})
	_, defaultMax, _ := builtinFiveDefaultTables()
	pc2 := &providerConfig{active: "deepseek", maxTokens: defaultMax}
	if got := pc2.maxTokensFor("deepseek-v4-flash-vision-exp"); got != 384000 {
		t.Errorf("默认表 polluted = %d, want 384000", got)
	}
}

// TestOpencodeProviderConfig OpenCode Go provider 注册：默认 baseURL / 模型窗口 / max_tokens /
// setActive / defaultModel / 采样默认（厂商默认，不套 DeepSeek 采样）/ key env 兜底。
func TestOpencodeProviderConfig(t *testing.T) {
	assertNoUserPrices(t)
	t.Setenv("HOME", t.TempDir()) // 封闭：不读用户真实 ~/.go-code/settings.json
	t.Setenv("OPENCODE_API_KEY", "")
	fresh := loadProviderConfig()

	// 默认 baseURL 指向 OpenCode Go 网关
	if got := fresh.baseURL["opencode"]; got != "https://opencode.ai/zen/go/v1" {
		t.Errorf("opencode baseURL = %q, want https://opencode.ai/zen/go/v1", got)
	}
	// setActive 白名单放行 opencode
	if err := fresh.setActive("opencode"); err != nil {
		t.Fatalf("setActive(opencode) = %v", err)
	}
	// 默认模型 = 配置列表排序首个（deepseek-v4-flash）
	if got := fresh.defaultModel(); got != "deepseek-v4-flash" {
		t.Errorf("opencode defaultModel = %q, want deepseek-v4-flash", got)
	}
	// 窗口 / 输出上限：deepseek-v4-flash 1M / 384k（known 默认）
	if got := fresh.windowFor("deepseek-v4-flash"); got != 1000000 {
		t.Errorf("opencode deepseek-v4-flash window = %d, want 1000000", got)
	}
	if got := fresh.maxTokensFor("deepseek-v4-flash"); got != 384000 {
		t.Errorf("opencode deepseek-v4-flash max_tokens = %d, want 384000", got)
	}
	// 未知模型回退默认窗口 / 输出上限
	if got := fresh.windowFor("nope-model"); got != defaultContextWindow {
		t.Errorf("opencode unknown window = %d, want %d", got, defaultContextWindow)
	}
	if got := fresh.maxTokensFor("nope-model"); got != defaultMaxTokens {
		t.Errorf("opencode unknown max_tokens = %d, want %d", got, defaultMaxTokens)
	}
	// 采样默认：opencode 保持厂商默认（不套 DeepSeek top_p/temperature）
	if temp, topP := fresh.samplingDefaults(); temp != nil || topP != nil {
		t.Errorf("opencode samplingDefaults = (%v, %v), want (nil, nil)", temp, topP)
	}
	// 非法 provider 名拒绝（setActive 校验格式/保留键）
	if err := fresh.setActive("active"); err == nil {
		t.Error("setActive(active) 应拒绝（保留键）")
	}
	// key env 兜底
	pc := &providerConfig{apiKey: map[string]string{}}
	t.Setenv("OPENCODE_API_KEY", " sk-go-123 ")
	if got := pc.keyLocked("opencode"); got != "sk-go-123" {
		t.Errorf("opencode keyLocked = %q, want sk-go-123", got)
	}
}

// TestKimiZhipuProviderConfig Kimi / Zhipu 内置 provider 注册：默认 baseURL / 模型窗口 /
// defaultModel / 采样默认（厂商默认）/ key env 兜底；Zhipu 的 compat.BasePath=/v4 修正
// /api/paas/v4 版本根（否则 chat completions 会拼出 /v4/v1 错误路径）。
func TestKimiZhipuProviderConfig(t *testing.T) {
	assertNoUserPrices(t)
	t.Setenv("HOME", t.TempDir()) // 封闭：不读用户真实 ~/.go-code/settings.json
	t.Setenv("KIMI_API_KEY", "")
	t.Setenv("ZHIPUAI_API_KEY", "")
	fresh := loadProviderConfig()

	// 默认 baseURL
	if got := fresh.baseURL["kimi"]; got != "https://api.moonshot.cn/v1" {
		t.Errorf("kimi baseURL = %q, want https://api.moonshot.cn/v1", got)
	}
	if got := fresh.baseURL["zhipu"]; got != "https://open.bigmodel.cn/api/paas/v4" {
		t.Errorf("zhipu baseURL = %q, want https://open.bigmodel.cn/api/paas/v4", got)
	}

	// Zhipu compat：BasePath=/v4（版本根修正）；保留完整 chat-completions 能力
	zc := fresh.compat["zhipu"]
	if zc.BasePath != "/v4" {
		t.Errorf("zhipu compat.BasePath = %q, want /v4", zc.BasePath)
	}

	// defaultModel（排序首个；默认表从 providers.json 派生，zhipu 含 glm-4.5/4.5-air 等）
	for _, c := range []struct{ prov, want string }{
		{"kimi", "kimi-k2-thinking"}, // 排序首个（k2-thinking 字母序 < k2.5/k2.6）
		{"zhipu", "glm-4.5"},
	} {
		if err := fresh.setActive(c.prov); err != nil {
			t.Fatalf("setActive(%s) = %v", c.prov, err)
		}
		if got := fresh.defaultModel(); got != c.want {
			t.Errorf("%s defaultModel = %q, want %q", c.prov, got, c.want)
		}
	}

	// 窗口 / 输出上限（known 默认；默认表已对齐注册表真实窗口）
	fresh.setActive("kimi")
	if got := fresh.windowFor("kimi-k2.6"); got != 262144 {
		t.Errorf("kimi kimi-k2.6 window = %d, want 262144", got)
	}
	if got := fresh.maxTokensFor("kimi-k2.6"); got != 262144 {
		t.Errorf("kimi kimi-k2.6 max_tokens = %d, want 262144", got)
	}
	fresh.setActive("zhipu")
	if got := fresh.windowFor("glm-5.2"); got != 1000000 {
		t.Errorf("zhipu glm-5.2 window = %d, want 1000000", got)
	}

	// 采样默认：Kimi/Zhipu 保持厂商默认（不套 DeepSeek top_p/temperature）
	for _, prov := range []string{"kimi", "zhipu"} {
		fresh.setActive(prov)
		if temp, topP := fresh.samplingDefaults(); temp != nil || topP != nil {
			t.Errorf("%s samplingDefaults = (%v, %v), want (nil, nil)", prov, temp, topP)
		}
	}

	// key env 兜底
	pc := &providerConfig{apiKey: map[string]string{}}
	t.Setenv("KIMI_API_KEY", "sk-kimi-1")
	if got := pc.keyLocked("kimi"); got != "sk-kimi-1" {
		t.Errorf("kimi keyLocked = %q, want sk-kimi-1", got)
	}
	pc2 := &providerConfig{apiKey: map[string]string{}}
	t.Setenv("ZHIPUAI_API_KEY", "sk-zhipu-1")
	if got := pc2.keyLocked("zhipu"); got != "sk-zhipu-1" {
		t.Errorf("zhipu keyLocked = %q, want sk-zhipu-1", got)
	}
}

// TestIsOpenCodeGateway 判定当前活跃 provider 是否走 OpenCode Go 网关（baseURL 含
// opencode.ai）：true → buildLoop 强制推理内容回传（网关全部思考模型要求，缺失 400）。
func TestIsOpenCodeGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 封闭
	pc := &providerConfig{
		active:  "opencode",
		baseURL: map[string]string{"opencode": "https://opencode.ai/zen/go/v1"},
	}
	if !pc.isOpenCodeGateway() {
		t.Error("opencode 网关应判定为 true（强制回传推理内容）")
	}
	pc.active = "deepseek"
	pc.baseURL["deepseek"] = "https://api.deepseek.com/v1"
	if pc.isOpenCodeGateway() {
		t.Error("deepseek 直连不应判定为 opencode 网关")
	}
	// 自定义 provider 但 baseURL 非 opencode 域名 → false
	pc.active = "selfhost"
	pc.baseURL["selfhost"] = "http://127.0.0.1:11434/v1"
	if pc.isOpenCodeGateway() {
		t.Error("非 opencode 域名不应判定为网关")
	}
	// 未知活跃 provider（baseURL 缺省）→ false 安全兜底
	pc.active = "ghost"
	if pc.isOpenCodeGateway() {
		t.Error("无 baseURL 的活跃 provider 应回 false")
	}
}

// TestProtocolSelection OpenAI 接入协议选择：默认 chat_completions → OpenAIProvider；
// responses（未实现 2026-08）→ 占位 provider，Stream 报「暂未支持」；setProtocol 只接受合法值。
func TestProtocolSelection(t *testing.T) {
	pc := &providerConfig{
		baseURL:  map[string]string{"openai": "https://api.openai.com/v1"},
		apiKey:   map[string]string{},
		protocol: map[string]string{"openai": protocolChatCompletions},
	}
	// 默认 chat_completions → OpenAIProvider
	if _, ok := pc.newProvider("openai").(*provider.OpenAIProvider); !ok {
		t.Fatal("chat_completions 应构造 OpenAIProvider")
	}
	// responses 已实现（T6）→ 返回 ResponsesProvider
	pc.setProtocol("openai", protocolResponses)
	p := pc.newProvider("openai")
	if _, ok := p.(*responses.ResponsesProvider); !ok {
		t.Fatalf("responses 应返回 ResponsesProvider, got %T", p)
	}
	// anthropic 已实现（T5）→ 返回 AnthropicProvider（不再占位）
	pc.setProtocol("openai", protocolAnthropic)
	if got := pc.protocol["openai"]; got != protocolAnthropic {
		t.Fatalf("anthropic protocol = %q, want %q", got, protocolAnthropic)
	}
	if _, ok := pc.newProvider("openai").(*anthropic.AnthropicProvider); !ok {
		t.Fatalf("anthropic 应返回 AnthropicProvider, got %T", pc.newProvider("openai"))
	}
	// 未知协议 → 归 chat_completions
	pc.setProtocol("openai", "bogus")
	if pc.protocol["openai"] != protocolChatCompletions {
		t.Fatalf("unknown protocol = %q, want chat_completions", pc.protocol["openai"])
	}
	// 空协议不改动现状（no-op）
	pc.setProtocol("openai", protocolResponses)
	pc.setProtocol("openai", "")
	if pc.protocol["openai"] != protocolResponses {
		t.Fatalf("empty protocol should no-op, got %q", pc.protocol["openai"])
	}
	// 非法 provider 名不改动（格式校验）
	pc.setProtocol("bad name", protocolResponses)
	if _, ok := pc.protocol["bad name"]; ok {
		t.Fatal("invalid provider name protocol should not be stored")
	}
}

// TestProtocolBackwardCompat 旧 settings.json（无 protocol 字段）不破坏现有行为：
// 协议回退 chat_completions，newProvider 照常构造 OpenAIProvider（改动前后行为一致）。
func TestProtocolBackwardCompat(t *testing.T) {
	assertNoUserPrices(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 旧格式：只有 base_url/models/max_tokens，无 protocol 键
	old := `{"provider":{"active":"deepseek",` +
		`"deepseek":{"base_url":"https://api.deepseek.com/v1","models":{"deepseek-v4-flash":1000000},"max_tokens":{"deepseek-v4-flash":384000}},` +
		`"openai":{"base_url":"https://api.openai.com/v1","models":{"gpt-4o":128000},"max_tokens":{"gpt-4o":8192}},` +
		`"opencode":{"base_url":"https://opencode.ai/zen/go/v1","models":{"deepseek-v4-flash":1000000},"max_tokens":{"deepseek-v4-flash":384000}}}}`
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := loadProviderConfig()
	for _, p := range []string{"deepseek", "openai", "opencode"} {
		if got := pc.protocol[p]; got != protocolChatCompletions {
			t.Fatalf("provider %s protocol = %q, want chat_completions（旧 settings 无 protocol 应回退默认）", p, got)
		}
		if _, ok := pc.newProvider(p).(*provider.OpenAIProvider); !ok {
			t.Fatalf("provider %s 应构造 OpenAIProvider（旧 settings 不破坏现有行为）", p)
		}
	}
}

// TestCustomProvider 用户自定义 provider：settings.json 任意 key 加载、格式校验放行、
// key env 兜底（GOCODE_KEY_<UPPER>）、newProvider 用自定义 baseURL。
func TestCustomProvider(t *testing.T) {
	assertNoUserPrices(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".go-code"), 0o755)
	cfg := `{"provider":{"active":"deepseek",` +
		`"deepseek":{"base_url":"https://api.deepseek.com/v1"},` +
		`"local-vllm":{"base_url":"http://localhost:8000/v1","protocol":"chat_completions","models":{"qwen3-32b":128000},"max_tokens":{"qwen3-32b":8192}}}}`
	if err := os.WriteFile(filepath.Join(home, ".go-code", "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := loadProviderConfig()

	if got := pc.baseURL["local-vllm"]; got != "http://localhost:8000/v1" {
		t.Fatalf("custom baseURL = %q, want http://localhost:8000/v1", got)
	}
	if got := pc.models["local-vllm"]["qwen3-32b"]; got != 128000 {
		t.Fatalf("custom model window = %d, want 128000", got)
	}
	if pc.protocol["local-vllm"] != protocolChatCompletions {
		t.Fatalf("custom protocol = %q, want chat_completions", pc.protocol["local-vllm"])
	}
	// 切到自定义 provider + 构造 OpenAIProvider（自定义 baseURL 生效）
	if err := pc.setActive("local-vllm"); err != nil {
		t.Fatalf("setActive(custom) = %v", err)
	}
	if _, ok := pc.newProvider("local-vllm").(*provider.OpenAIProvider); !ok {
		t.Fatal("custom 应构造 OpenAIProvider")
	}
	if got := pc.defaultModel(); got != "qwen3-32b" {
		t.Fatalf("custom defaultModel = %q, want qwen3-32b（排序首个）", got)
	}
	// key env 兜底 GOCODE_KEY_<UPPER(name)>
	t.Setenv("GOCODE_KEY_LOCAL-VLLM", " sk-local ")
	if got := pc.keyLocked("local-vllm"); got != "sk-local" {
		t.Fatalf("custom keyLocked = %q, want sk-local", got)
	}
	// 命名校验：保留键 / 空格 / 数字开头拒绝；合法自定义名放行
	for _, bad := range []string{"active", "bad name", "1abc", ""} {
		if err := pc.setActive(bad); err == nil {
			t.Fatalf("setActive(%q) 应拒绝", bad)
		}
	}
	if err := pc.setActive("ok-name_2"); err != nil {
		t.Fatalf("setActive(合法自定义名) 应通过: %v", err)
	}
}

func TestCustomProviderCompatCapabilities(t *testing.T) {
	assertNoUserPrices(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".go-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"provider":{"active":"pipixia","pipixia":{"base_url":"https://api.example.test","protocol":"chat_completions","openai_compat":{"minimal":true,"base_path":"/v1"},"models":{"gpt-5.4":256000},"max_tokens":{"gpt-5.4":128000}}}}`
	if err := os.WriteFile(filepath.Join(home, ".go-code", "settings.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	pc := loadProviderConfig()
	got := pc.compat["pipixia"]
	if !got.Minimal || got.BasePath != "/v1" {
		t.Fatalf("compat = %+v", got)
	}
	if _, ok := pc.newProvider("pipixia").(*provider.OpenAIProvider); !ok {
		t.Fatal("minimal compat custom provider should build OpenAIProvider")
	}

	m, buf := testManager(t)
	m.dispatch(cmd(77, "set_provider", map[string]any{
		"provider":      "pipixia",
		"base_url":      "https://api.example.test",
		"openai_compat": map[string]any{"minimal": true, "base_path": "/v1"},
	}))
	if !lastRespOK(t, buf, 77) {
		t.Fatal("set_provider compat not ok")
	}
	if got := m.provCfg.compat["pipixia"]; !got.Minimal || got.BasePath != "/v1" {
		t.Fatalf("hot compat = %+v", got)
	}
}

// 直达 providerConfig（不依赖 settings.json 落盘时序），rebuildAll 后窗口/输出上限即时生效。
func TestSetProviderModelsMaxTokens(t *testing.T) {
	assertNoUserPrices(t)
	t.Setenv("HOME", t.TempDir()) // 封闭：不读用户真实 ~/.go-code/settings.json
	m, buf := testManager(t)
	m.dispatch(cmd(1, "set_provider", map[string]any{
		"provider":   "deepseek",
		"models":     map[string]any{"deepseek-v4-flash": 500000, "deepseek-chat": 64000},
		"max_tokens": map[string]any{"deepseek-v4-flash": 123456},
	}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("set_provider 未 ok")
	}
	if got := m.provCfg.windowFor("deepseek-v4-flash"); got != 500000 {
		t.Errorf("windowFor(flash) = %d, want 500000", got)
	}
	if got := m.provCfg.maxTokensFor("deepseek-v4-flash"); got != 123456 {
		t.Errorf("maxTokensFor(flash) = %d, want 123456", got)
	}
	if got := m.provCfg.maxTokensFor("deepseek-chat"); got != 8192 {
		t.Errorf("maxTokensFor(chat) = %d, want known default 8192", got)
	}
}

// list_provider_presets：预设模型内置价（models_cost）随响应下发 —— 前端打开设置
// 即能回显内置价，无需先「拉取模型列表」（本地注册表读取，无网络依赖）。回归：
// 此前内置价只在 list_models（依赖网络 + 用户点击）的 capabilities 里带，重启后
// 设置页模型价格显示为空/0，直到手动拉取才出现。
func TestListProviderPresetsIncludesModelCosts(t *testing.T) {
	assertNoUserPrices(t)
	t.Setenv("HOME", t.TempDir()) // 封闭
	m, buf := testManager(t)
	m.dispatch(cmd(5, "list_provider_presets", map[string]any{}))
	var data map[string]any
	for _, l := range readLines(buf) {
		if l["id"] == float64(5) && l["ok"] == true {
			data = l["data"].(map[string]any)
		}
	}
	if data == nil {
		t.Fatal("list_provider_presets 无响应")
	}
	presets, ok := data["presets"].([]any)
	if !ok || len(presets) == 0 {
		t.Fatalf("presets 缺失: %v", data["presets"])
	}
	costs, ok := data["models_cost"].(map[string]any)
	if !ok {
		t.Fatalf("models_cost 缺失: %v", data)
	}
	// deepseek 预设的 flash 应带内置价（0.15/0.003/0.6 按当前 providers.json）
	ds, ok := costs["deepseek"].(map[string]any)
	if !ok {
		t.Fatalf("models_cost 缺 deepseek: %v", costs)
	}
	flash, ok := ds["deepseek-v4-flash"].(map[string]any)
	if !ok {
		t.Fatalf("models_cost 缺 deepseek/deepseek-v4-flash: %v", ds)
	}
	if flash["input"] == nil || flash["output"] == nil {
		t.Fatalf("deepseek-v4-flash cost 字段缺失: %v", flash)
	}
	// 值也要对（不只非 nil）—— 与 providers.json 的当前口径一致（0.15 / 0.6）
	if flash["input"] != 0.15 || flash["output"] != 0.6 {
		t.Fatalf("deepseek-v4-flash cost = input %v / output %v, want 0.15 / 0.6", flash["input"], flash["output"])
	}
}

// TestSetProviderPresetInit 用户从预设添加新 provider（如 anthropic）→ 自动补 base_url/protocol/models。
func TestSetProviderPresetInit(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 封闭
	m, buf := testManager(t)
	// 模拟前端：选 anthropic 预设，只传 provider + api_key（base_url/protocol/models 由 bridge 自动补）
	m.dispatch(cmd(2, "set_provider", map[string]any{
		"provider": "anthropic",
		"api_key":  "sk-ant-test",
	}))
	if !lastRespOK(t, buf, 2) {
		t.Fatal("set_provider(anthropic) 未 ok")
	}
	// base_url 自动补官方端点
	if got := m.provCfg.baseURL["anthropic"]; got != "https://api.anthropic.com" {
		t.Errorf("anthropic base_url = %q, want 官方端点", got)
	}
	// protocol 自动补 anthropic
	if got := m.provCfg.protocol["anthropic"]; got != protocolAnthropic {
		t.Errorf("anthropic protocol = %q, want anthropic", got)
	}
	// models 自动从注册表初始化（真实窗口）
	if got := m.provCfg.windowForIn("anthropic", "claude-sonnet-4-5"); got != 1000000 {
		t.Errorf("claude-sonnet-4-5 window = %d, want 1000000", got)
	}
	// 新 provider 可构造（协议分发 → AnthropicProvider）
	p := m.provCfg.newProvider("anthropic")
	if _, ok := p.(*anthropic.AnthropicProvider); !ok {
		t.Fatalf("anthropic newProvider = %T, want *AnthropicProvider", p)
	}
	// xai 预设 → responses 协议
	m.dispatch(cmd(3, "set_provider", map[string]any{"provider": "xai", "api_key": "xai-test"}))
	if got := m.provCfg.protocol["xai"]; got != protocolResponses {
		t.Errorf("xai protocol = %q, want responses", got)
	}
	if p := m.provCfg.newProvider("xai"); p == nil {
		t.Fatal("xai newProvider = nil")
	}
	// minimax 预设 → anthropic 协议（兼容端点）
	m.dispatch(cmd(4, "set_provider", map[string]any{"provider": "minimax", "api_key": "mm-test"}))
	if got := m.provCfg.protocol["minimax"]; got != protocolAnthropic {
		t.Errorf("minimax protocol = %q, want anthropic", got)
	}
	if got := m.provCfg.baseURL["minimax"]; got != "https://api.minimax.io/anthropic" {
		t.Errorf("minimax base_url = %q", got)
	}
}

// TestSetProviderSaveDoesNotJumpActive 回归：纯「保存 Provider 配置」（不带 active）
// 不得切换全局活跃 Provider——此前 set_provider 无条件 setActive(providerName)，
// 「编辑 GLM Provider 协议」保存后全局 active 跳到 GLM，新建会话默认 Provider 跟着变
// （多 Provider 场景请求串台）。仅 payload 显式带 active（「设为当前」）才切。
func TestSetProviderSaveDoesNotJumpActive(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 封闭
	m, buf := testManager(t)
	if got := m.provCfg.providerName(); got != "deepseek" {
		t.Fatalf("初始 active = %q, want deepseek", got)
	}

	// 保存 zhipu 配置（改协议/改 key）不带 active → active 保持 deepseek
	m.dispatch(cmd(1, "set_provider", map[string]any{
		"provider": "zhipu",
		"protocol": "anthropic",
		"api_key":  "zhipu-new-key",
	}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("set_provider(zhipu 保存) 应成功")
	}
	if got := m.provCfg.providerName(); got != "deepseek" {
		t.Fatalf("纯保存后 active 被切: got %q, want deepseek（不得跳变）", got)
	}
	// 配置本身已生效（协议/key 更新成功）
	if got := m.provCfg.protocol["zhipu"]; got != protocolAnthropic {
		t.Errorf("zhipu protocol = %q, want anthropic", got)
	}
	if got := m.provCfg.keyLocked("zhipu"); got != "zhipu-new-key" {
		t.Errorf("zhipu key = %q", got)
	}

	// 显式带 active（「设为当前」语义）→ 才切
	m.dispatch(cmd(2, "set_provider", map[string]any{
		"provider": "zhipu",
		"active":   "zhipu",
	}))
	if !lastRespOK(t, buf, 2) {
		t.Fatal("set_provider(带 active) 应成功")
	}
	if got := m.provCfg.providerName(); got != "zhipu" {
		t.Fatalf("带 active 应切到 zhipu, got %q", got)
	}
}

// TestSetProviderEmptyKeyClearsStale 显式传空 api_key → 清内存旧 key（回退 env/.env 兜底），
// 防止「面板保存未带 key → 内存旧 key 残留 → 新会话 401（****AGED 场景）」。
func TestSetProviderEmptyKeyClearsStale(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 封闭：不读用户真实 ~/.go-code/settings.json
	m, buf := testManager(t)

	// 先热设一个新 key
	m.dispatch(cmd(1, "set_provider", map[string]any{"provider": "deepseek", "api_key": "sk-new-1"}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("set_provider(带 key) 未 ok")
	}
	if got := m.provCfg.keyLocked("deepseek"); got != "sk-new-1" {
		t.Fatalf("keyLocked = %q, want sk-new-1", got)
	}

	// 再传空 api_key（payload 显式带空串）→ 内存旧 key 必须清除（回退 env 兜底；此时 env 无 → 空）
	m.dispatch(cmd(2, "set_provider", map[string]any{"provider": "deepseek", "api_key": ""}))
	if !lastRespOK(t, buf, 2) {
		t.Fatal("set_provider(空 key) 未 ok")
	}
	if got := m.provCfg.keyLocked("deepseek"); got != "" {
		t.Fatalf("空 key 后 keyLocked = %q, want \"\"（旧 key 不得残留）", got)
	}

	// 不传 api_key 字段（仅改其它配置）→ 内存值保持不变（不误清）
	m.dispatch(cmd(3, "set_provider", map[string]any{"provider": "deepseek", "base_url": "https://api.deepseek.com/v1"}))
	if !lastRespOK(t, buf, 3) {
		t.Fatal("set_provider(无 key 字段) 未 ok")
	}
	if got := m.provCfg.keyLocked("deepseek"); got != "" {
		t.Fatalf("无 key 字段 keyLocked = %q, want \"\"", got)
	}

	// 兜底路径：清空后 env DEEPSEEK_API_TOKEN 生效（keyLocked 回退）
	t.Setenv("DEEPSEEK_API_TOKEN", "sk-env-1")
	if got := m.provCfg.keyLocked("deepseek"); got != "sk-env-1" {
		t.Fatalf("清空后 env 兜底 keyLocked = %q, want sk-env-1", got)
	}
}

// TestSwitchModelInjectsProviderKey switch_model 可随 payload 带 api_key（BR-08）：
// key 不再持久化在 settings.json，由 Electron（keys.json / safeStorage）在切模型时按目标
// provider 注入。钉住三件事：
//
//	① 带 api_key → 该 provider 的 keyLocked 立刻生效（非活跃 provider 没有 env 兜底）；
//	② 不带 api_key → 不误清现有内存值（保留旧值）；
//	③ 注入必须发生在 loadSettings **之后** —— settings 已无 api_key，loadSettings 会清内存旧值，
//	   先注入就会被抹掉（带 settings.json 的分支正是这个顺序锚点）；清掉后回退 env 兜底。
func TestSwitchModelInjectsProviderKey(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	if _, err := m.create(ws, "s1", ""); err != nil {
		t.Fatal(err)
	}
	switchToZhipu := func(id int, key string, withKey bool) map[string]any {
		payload := map[string]any{"workspace": ws, "session_id": "s1", "model": "glm-5.3-flash", "provider": "zhipu"}
		if withKey {
			payload["api_key"] = key
		}
		m.dispatch(cmd(id, "switch_model", payload))
		if !lastRespOK(t, buf, id) {
			t.Fatalf("switch_model(zhipu) 应成功: %v", readAllLines(buf))
		}
		return payload
	}

	// ① 带 key：写入目标 provider 的内存 key（测试 HOME 里还没有 settings.json）
	switchToZhipu(1, "sk-zhipu-payload", true)
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-zhipu-payload" {
		t.Fatalf("带 api_key 的 switch_model 后 keyLocked = %q, want sk-zhipu-payload", got)
	}

	// ② 不带 api_key：内存值保留（不得误清）
	switchToZhipu(2, "", false)
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-zhipu-payload" {
		t.Fatalf("不带 api_key 的 switch_model 误清了内存 key: %q", got)
	}

	// ③ 有 settings.json（无 api_key，迁移后的正常形态）：loadSettings 会清内存 key，
	//    所以注入必须在其后 —— 否则 keyLocked 会是空的。
	cfgDir := filepath.Join(mustUserHome(t), ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{
		"schema_version": 2,
		"provider": {
			"active": "deepseek",
			"zhipu": {"base_url": "https://open.bigmodel.cn/api/paas/v4"}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	switchToZhipu(3, "sk-zhipu-payload-2", true)
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-zhipu-payload-2" {
		t.Fatalf("loadSettings 之后注入失败: keyLocked = %q, want sk-zhipu-payload-2（注入点必须在 loadSettings 之后）", got)
	}

	// ④ 再切一次且不带 key：注入值**保留**（不误清）——payload 不带 api_key 的语义是「不动」
	switchToZhipu(4, "", false)
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-zhipu-payload-2" {
		t.Fatalf("不带 api_key 的 switch_model 误清了注入 key: %q", got)
	}

	// ⑤ 显式空串 = 清除 → 回退 env 兜底
	t.Setenv("ZHIPUAI_API_KEY", "sk-zhipu-env")
	switchToZhipu(5, "", true) // 带 key 字段但为空串
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-zhipu-env" {
		t.Fatalf("清掉后 env 兜底 keyLocked = %q, want sk-zhipu-env", got)
	}
}

// TestSetProviderInjectedKeySurvivesReload set_provider 带 api_key 后，key 必须在内存里**留住**：
//   - settings.json 已无 api_key（BR-08 迁移后的正常形态）→ loadSettings 不得清掉注入值；
//   - reload_settings（切沙箱/浏览器引擎等顶层设置变更会发）不得清掉注入值；
//   - 再保存一次新 key 必须覆盖旧值（轮换生效）；
//   - 显式空 api_key = 清除（回退 env 兜底）。
//
// 回归背景：set_provider 里 setKey 曾排在 loadSettings 之前，settings 无 key 时会把它删掉 →
// 「面板里改了 key，实际还在用旧 key / 401」。
func TestSetProviderInjectedKeySurvivesReload(t *testing.T) {
	m, buf := testManager(t)
	cfgDir := filepath.Join(mustUserHome(t), ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{
		"schema_version": 2,
		"provider": {
			"active": "deepseek",
			"zhipu": {"base_url": "https://open.bigmodel.cn/api/paas/v4"}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	m.dispatch(cmd(1, "set_provider", map[string]any{"provider": "zhipu", "api_key": "sk-rotated-1"}))
	if !lastRespOK(t, buf, 1) {
		t.Fatalf("set_provider(带 key) 应成功: %v", readAllLines(buf))
	}
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-rotated-1" {
		t.Fatalf("保存后 keyLocked = %q, want sk-rotated-1（settings 无 api_key 时不得清掉注入值）", got)
	}

	// 顶层设置变更 → reload_settings（内部又跑一次 loadSettings）→ 注入值必须还在
	m.dispatch(cmd(2, "reload_settings", map[string]any{}))
	if !lastRespOK(t, buf, 2) {
		t.Fatalf("reload_settings 应成功: %v", readAllLines(buf))
	}
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-rotated-1" {
		t.Fatalf("reload_settings 后 keyLocked = %q, want sk-rotated-1（注入值不得被重载清掉）", got)
	}

	// 再保存（不带 key 字段）：仍是同一个 key
	m.dispatch(cmd(3, "set_provider", map[string]any{"provider": "zhipu", "protocol": "anthropic"}))
	if !lastRespOK(t, buf, 3) {
		t.Fatalf("set_provider(无 key) 应成功: %v", readAllLines(buf))
	}
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-rotated-1" {
		t.Fatalf("不带 key 的保存误清了 key: %q", got)
	}

	// 轮换：新 key 覆盖旧值
	m.dispatch(cmd(4, "set_provider", map[string]any{"provider": "zhipu", "api_key": "sk-rotated-2"}))
	if !lastRespOK(t, buf, 4) {
		t.Fatalf("set_provider(轮换) 应成功: %v", readAllLines(buf))
	}
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-rotated-2" {
		t.Fatalf("轮换后 keyLocked = %q, want sk-rotated-2", got)
	}

	// 显式空串 = 清除 → env 兜底
	t.Setenv("ZHIPUAI_API_KEY", "sk-zhipu-env")
	m.dispatch(cmd(5, "set_provider", map[string]any{"provider": "zhipu", "api_key": ""}))
	if !lastRespOK(t, buf, 5) {
		t.Fatalf("set_provider(空 key) 应成功: %v", readAllLines(buf))
	}
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-zhipu-env" {
		t.Fatalf("清 key 后 env 兜底 keyLocked = %q, want sk-zhipu-env", got)
	}
}

// TestInjectedKeyBeatsStaleSettingsPlaintext 回归（P5-09 复审 B1b）：settings 迁移未完成、
// provider 段里仍残留**旧明文** api_key 时，payload 注入的 key 必须永远胜出：
//   - set_provider 注入新 key 后，任何 loadSettings（reload_settings / 热重载）都不得把内存 key
//     顶回 settings 里的旧明文（旧 key 复活 → 401 / 轮换不生效）；
//   - 反向锚点：没有 payload 注入的 provider 仍按 settings 明文生效（迁移未完成的兜底保留）。
//
// 回归背景：loadSettings 曾无条件「settings 有 api_key 就覆盖内存 + 清注入标记」，
// 一次 reload_settings 就把刚保存的新 key 换回旧明文（实测 keyLocked = sk-OLD-plaintext）。
func TestInjectedKeyBeatsStaleSettingsPlaintext(t *testing.T) {
	m, buf := testManager(t)
	cfgDir := filepath.Join(mustUserHome(t), ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 迁移未完成的形态：settings 仍留旧明文（keys.json 里才是新 key，由 payload 下发）
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{
		"schema_version": 2,
		"provider": {
			"active": "deepseek",
			"zhipu": {"base_url": "https://open.bigmodel.cn/api/anthropic", "api_key": "sk-OLD-plaintext"},
			"opencode": {"base_url": "https://opencode.ai/zen/go/v1", "api_key": "sk-OLD-opencode"}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.provCfg.loadSettings() // 启动热载：迁移未完成时先吃 settings 明文（兜底）
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-OLD-plaintext" {
		t.Fatalf("前置：settings 明文应先生效，got %q", got)
	}

	// keys.json 里的最新 key 随 set_provider payload 下发
	m.dispatch(cmd(1, "set_provider", map[string]any{"provider": "zhipu", "api_key": "sk-NEW-injected"}))
	if !lastRespOK(t, buf, 1) {
		t.Fatalf("set_provider(带 key) 应成功: %v", readAllLines(buf))
	}
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-NEW-injected" {
		t.Fatalf("注入后 keyLocked = %q, want sk-NEW-injected", got)
	}

	// 顶层设置变更 → reload_settings（内部又跑一次 loadSettings）→ 旧明文不得复活
	m.dispatch(cmd(2, "reload_settings", map[string]any{}))
	if !lastRespOK(t, buf, 2) {
		t.Fatalf("reload_settings 应成功: %v", readAllLines(buf))
	}
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-NEW-injected" {
		t.Fatalf("reload_settings 后 keyLocked = %q, want sk-NEW-injected（settings 旧明文不得顶掉注入值）", got)
	}

	// 再直接跑一次 loadSettings（热重载/切 provider 都走它）：仍是新 key
	m.provCfg.loadSettings()
	if got := m.provCfg.keyLocked("zhipu"); got != "sk-NEW-injected" {
		t.Fatalf("再次 loadSettings 后 keyLocked = %q, want sk-NEW-injected", got)
	}

	// 反向锚点：没有注入值的 provider 仍读 settings 明文（兜底不能被一起删掉）
	if got := m.provCfg.keyLocked("opencode"); got != "sk-OLD-opencode" {
		t.Fatalf("无注入值的 provider 应按 settings 明文生效，got %q", got)
	}
}

// mustUserHome 取当前测试 HOME（testManager 已用 t.TempDir 隔离）。
func mustUserHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	return home
}

// TestLoadSettingsEmptyKeyClearsStale settings.json 里 provider 无 api_key →
// loadSettings 清内存旧 key（删除 key 后重启/热重载不留旧值 → 新会话 401）。
func TestLoadSettingsEmptyKeyClearsStale(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // 封闭：不读用户真实 ~/.go-code/settings.json
	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// settings.json：deepseek 无 api_key（用户删除 key）
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{
		"provider": {
			"active": "deepseek",
			"deepseek": {"base_url": "https://api.deepseek.com/anthropic", "protocol": "anthropic"}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// 直接构造 providerConfig（不经过 testManager，避免其覆盖 HOME），预置内存旧 key
	pc := &providerConfig{
		active:     "deepseek",
		apiKey:     map[string]string{"deepseek": "sk-old-stale"},
		baseURL:    map[string]string{"deepseek": "https://api.deepseek.com/anthropic"},
		protocol:   map[string]string{"deepseek": protocolAnthropic},
		models:     map[string]map[string]int64{},
		maxTokens:  map[string]map[string]int64{},
		compat:     map[string]provider.OpenAICompatCapabilities{},
		inputTypes: map[string]map[string][]string{},
		authType:   map[string]string{},
		registry:   provider.NewRegistry(),
	}
	pc.loadSettings()
	if got := pc.keyLocked("deepseek"); got != "" {
		t.Fatalf("settings 无 key 后 keyLocked = %q, want \"\"（旧 key 不得残留）", got)
	}

	// 反向：settings 有 key → 覆盖内存旧值
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{
		"provider": {
			"active": "deepseek",
			"deepseek": {"base_url": "https://api.deepseek.com/anthropic", "protocol": "anthropic", "api_key": "sk-new-2"}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pc.loadSettings()
	if got := pc.keyLocked("deepseek"); got != "sk-new-2" {
		t.Fatalf("settings 有 key 后 keyLocked = %q, want sk-new-2", got)
	}
}

// TestLoadSettingsUserPrices 每模型价表覆盖：settings.json provider[].prices 落盘 →
// loadSettings 写入全局 priceTable + 注册表（overrideRegistryLocked），costUsd 与
// registry.PriceFor 均返回用户价；模型名/全量价删除后回退内置价。
func TestLoadSettingsUserPrices(t *testing.T) {
	assertNoUserPrices(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 备份全局价表（测试污染隔离：本包其它测试依赖内置价）
	priceTableMu.Lock()
	savedUser := userPriceTable
	userPriceTable = map[string]map[string]provider.ModelPrice{}
	priceTableMu.Unlock()
	t.Cleanup(func() {
		priceTableMu.Lock()
		userPriceTable = savedUser
		priceTableMu.Unlock()
	})

	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{
		"provider": {
			"active": "opencode",
			"opencode": {"base_url": "https://opencode.ai/zen/go/v1", "models": {"deepseek-v4-flash": 1000000, "glm-5.2": 1000000},
			 "prices": {"deepseek-v4-flash": {"input": 0.05, "cache_read": 0.001, "output": 0.1}, "glm-5.2": {"input": 0.5, "output": 2.0}}}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pc := loadProviderConfig()
	pc.loadSettings() // 读取用户价 → 写 priceTable（loadProviderConfig 已 syncRegistry 一次，
	// 这里再载入后需要再 sync 一次让 override 拿到新价）
	pc.syncRegistry()

	// 宿主 costUsd 走用户价
	u := core.Usage{Input: 1_000_000, Output: 500_000}
	if got := costUsd("opencode", "deepseek-v4-flash", u); got != 0.05+0.05 {
		t.Fatalf("costUsd(用户价) = %v, want 0.10（input 0.05 + output 0.1×0.5M/1M）", got)
	}
	// 注册表价同步（FillUsageCost 走 registry.PriceFor）
	if p, ok := pc.registry.PriceFor("opencode", "deepseek-v4-flash"); !ok || p.Input != 0.05 || p.Output != 0.1 {
		t.Fatalf("registry.PriceFor(用户价) = %+v, %v; want input 0.05/output 0.1", p, ok)
	}
	// 未配置用户价的模型保持内置价（glm-5.2 内置 1.4/4.4；此处用户配了则覆盖验证上方）
	if p, ok := pc.registry.PriceFor("opencode", "glm-5.2"); !ok || p.Input != 0.5 {
		t.Fatalf("glm-5.2 用户价未生效: %+v, %v", p, ok)
	}

	// 清空某模型价格（前端删除 prices 条目后整表重写）→ 回退内置价
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{
		"provider": {
			"active": "opencode",
			"opencode": {"base_url": "https://opencode.ai/zen/go/v1", "models": {"deepseek-v4-flash": 1000000, "glm-5.2": 1000000},
			 "prices": {"glm-5.2": {"input": 0.5, "output": 2.0}}}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pc.loadSettings()
	pc.syncRegistry()
	if p, ok := pc.registry.PriceFor("opencode", "deepseek-v4-flash"); !ok || p.Input != 0.15 {
		t.Fatalf("删除价格条目后应回退内置价, got %+v ok=%v", p, ok)
	}
}

// TestTestConnection test_connection 命令：给定 baseURL（httptest 假 /v1/models）+ 假 key →
// GET /models 探活，返回模型数 + 列表；401 / 空 key → 明确失败。瞬态——不改 providerConfig。
func TestTestConnection(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 封闭：不读用户真实 ~/.go-code/settings.json

	t.Run("ok", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`))
		}))
		defer srv.Close()
		m, buf := testManager(t)
		m.dispatch(cmd(2, "test_connection", map[string]any{"provider": "deepseek", "base_url": srv.URL, "api_key": "sk-fake"}))
		found := false
		for _, l := range readLines(buf) {
			if l["id"] == float64(2) && l["ok"] == true {
				found = true
				d, _ := l["data"].(map[string]any)
				if got := d["count"]; got != float64(2) {
					t.Errorf("count = %v, want 2", got)
				}
				if len(d["models"].([]any)) != 2 {
					t.Errorf("models 长度 = %d, want 2", len(d["models"].([]any)))
				}
			}
		}
		if !found {
			t.Fatal("test_connection 未 ok")
		}
	})

	t.Run("401", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
		}))
		defer srv.Close()
		m, buf := testManager(t)
		m.dispatch(cmd(3, "test_connection", map[string]any{"base_url": srv.URL, "api_key": "bad"}))
		ok, hasErr := false, false
		for _, l := range readLines(buf) {
			if l["id"] == float64(3) {
				if l["ok"] == true {
					ok = true
				}
				if s, _ := l["error"].(string); s != "" {
					hasErr = true
				}
			}
		}
		if ok {
			t.Fatal("401 测试应失败")
		}
		if !hasErr {
			t.Error("401 应有非空 error")
		}
	})

	t.Run("no-key", func(t *testing.T) {
		m, buf := testManager(t)
		m.dispatch(cmd(4, "test_connection", map[string]any{"base_url": "https://api.example.com/v1"}))
		ok, hasErr := false, false
		for _, l := range readLines(buf) {
			if l["id"] == float64(4) {
				if l["ok"] == true {
					ok = true
				}
				if s, _ := l["error"].(string); s != "" {
					hasErr = true
				}
			}
		}
		if ok {
			t.Fatal("空 key 测试应失败")
		}
		if !hasErr {
			t.Error("空 key 应有非空 error")
		}
	})
}

// TestDeleteSession 验证 delete_session：删磁盘 sessions/events 日志 + 进程内会话摘除。
func TestDeleteSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离 APP 级存储
	m, buf := testManager(t)
	ws := t.TempDir()

	// 磁盘上已有会话（非进程内）：直接放两个文件模拟
	sid := "s-disk-1"
	sessionFile := filepath.Join(wsSessionsDir(ws), sid+".jsonl")
	eventFile := filepath.Join(wsEventsDir(ws), sid+".jsonl")
	_ = os.MkdirAll(filepath.Dir(sessionFile), 0o755)
	_ = os.MkdirAll(filepath.Dir(eventFile), 0o755)
	_ = os.WriteFile(sessionFile, []byte(`{"role":"user","content":"x"}`), 0o644)
	_ = os.WriteFile(eventFile, []byte(`{"event_type":"agent_start"}`), 0o644)

	// 进程内会话
	sid2 := newSessionID(t, m, buf, ws)

	m.dispatch(cmd(1, "delete_session", map[string]any{"workspace": ws, "session_id": sid}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("delete_session(s-disk-1) 未 ok")
	}
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Errorf("磁盘 session 文件未删: %v", err)
	}
	if _, err := os.Stat(eventFile); !os.IsNotExist(err) {
		t.Errorf("磁盘 event 文件未删: %v", err)
	}

	// 进程内会话也删除
	m.dispatch(cmd(2, "delete_session", map[string]any{"workspace": ws, "session_id": sid2}))
	if !lastRespOK(t, buf, 2) {
		t.Fatal("delete_session(sid2) 未 ok")
	}
	if wsrt := m.workspaces[ws]; wsrt != nil {
		wsrt.mu.Lock()
		_, still := wsrt.sessions[sid2]
		wsrt.mu.Unlock()
		if still {
			t.Errorf("进程内会话 %s 未摘除", sid2)
		}
	}
	// 列表不再含已删会话
	m.dispatch(cmd(3, "list", map[string]any{"workspace": ws}))
	lines := readLines(buf)
	var sessions []any
	for _, l := range lines {
		if l["id"] == float64(3) {
			if d, ok := l["data"].(map[string]any); ok {
				sessions = d["sessions"].([]any)
			}
		}
	}
	for _, s := range sessions {
		mm := s.(map[string]any)
		if mm["id"] == sid || mm["id"] == sid2 {
			t.Errorf("list 仍含已删会话 %v", mm["id"])
		}
	}
}

// TestDeleteWorkspace 验证 delete_workspace：删 harness 元数据（skills.json）、保留用户工作区
// 文件、停全部会话（session_closed）、运行态摘除。
func TestDeleteWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 隔离 APP 级存储
	m, buf := testManager(t)
	ws := t.TempDir()

	// 用户文件 + 元数据（harness 自有元数据 = skills.json；目录清空后应被移除）
	_ = os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main"), 0o644)
	_ = os.MkdirAll(filepath.Join(ws, ".go-code"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, ".go-code", "skills.json"), []byte(`{"disabled":["x"]}`), 0o600)
	_ = os.WriteFile(filepath.Join(wsSessionsDir(ws), "s-old.jsonl"), []byte(`{}`), 0o644)
	_ = newSessionID(t, m, buf, ws) // 进程内会话

	m.dispatch(cmd(1, "delete_workspace", map[string]any{"workspace": ws}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("delete_workspace 未 ok")
	}
	// skills.json 已删且目录已空 → 目录随之移除（若元数据未删，这里会残留）
	if _, err := os.Stat(filepath.Join(ws, ".go-code")); !os.IsNotExist(err) {
		t.Errorf(".go-code 元数据目录未清: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "main.go")); err != nil {
		t.Errorf("用户工作区文件被误删: %v", err)
	}
	if _, ok := m.workspaces[ws]; ok {
		t.Errorf("工作区运行态未摘除")
	}
}

// TestDeleteWorkspaceKeepsProjectConfig 删除工作区只删 harness 元数据（skills.json）：
// 用户手写的项目级配置（{.go-code}/settings.json，如 mcpServers —— 项目级 MCP 分层的
// 读取源）与其它用户数据必须保留（原实现 RemoveAll 整目录会静默删除用户配置）。
func TestDeleteWorkspaceKeepsProjectConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()

	meta := filepath.Join(ws, ".go-code")
	projCfg := `{"mcpServers":{"local":{"type":"stdio","command":"npx"}}}`
	_ = os.MkdirAll(filepath.Join(meta, "worktrees"), 0o755) // 用户数据（git worktree 基目录）
	_ = os.WriteFile(filepath.Join(meta, "settings.json"), []byte(projCfg), 0o600)
	_ = os.WriteFile(filepath.Join(meta, "skills.json"), []byte(`{"disabled":["x"]}`), 0o600)

	m.dispatch(cmd(1, "delete_workspace", map[string]any{"workspace": ws}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("delete_workspace 未 ok")
	}
	if _, err := os.Stat(filepath.Join(meta, "skills.json")); !os.IsNotExist(err) {
		t.Errorf("harness 元数据 skills.json 应删除: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(meta, "settings.json"))
	if err != nil {
		t.Fatalf("用户项目配置被误删: %v", err)
	}
	if string(got) != projCfg {
		t.Errorf("项目配置内容被改写: %q", got)
	}
	if fi, err := os.Stat(filepath.Join(meta, "worktrees")); err != nil || !fi.IsDir() {
		t.Errorf("用户数据 worktrees 应保留: %v", err)
	}
}

// lastRespOK 检查某命令 id 的响应是否 ok（readLines 后判定，方便断言语义）。
func lastRespOK(t *testing.T, buf *syncBuf, id int) bool {
	t.Helper()
	for _, l := range readLines(buf) {
		if l["id"] == float64(id) && l["event_type"] == "command_response" {
			return l["ok"] == true
		}
	}
	return false
}

// —— 长时运行异步化：promote_task 命令（docs/ASYNC_TOOLS_AND_PROMOTE.md Phase 5）——

// bridgeMockProvider 按脚本发射 LLM 事件（带请求消息捕获，供断言模型续跑轮收到的内容）。
type bridgeMockProvider struct {
	mu               sync.Mutex
	rounds           []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error
	calls            int
	round3TaskResult bool // round3 是否收到 task_result 消息（wake 续跑证明）
}

func (m *bridgeMockProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	m.mu.Lock()
	round := m.rounds[m.calls]
	if m.calls < len(m.rounds)-1 {
		m.calls++
	}
	m.mu.Unlock()
	return round(req, handler)
}

// bridgeBlockTool 可释放阻塞工具（promote_task 集成测试）。
type bridgeBlockTool struct {
	gate chan struct{}
	once sync.Once
}

func (b *bridgeBlockTool) Name() string                                      { return "block" }
func (b *bridgeBlockTool) Description() string                               { return "blocks until released" }
func (b *bridgeBlockTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (b *bridgeBlockTool) CanParallel() bool                                 { return true }
func (b *bridgeBlockTool) ValidParams(context.Context, string, string) error { return nil }
func (b *bridgeBlockTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (b *bridgeBlockTool) AfterCall(context.Context, core.ToolCall) {}
func (b *bridgeBlockTool) Release()                                 { b.once.Do(func() { close(b.gate) }) }
func (b *bridgeBlockTool) Call(ctx context.Context, name, arguments string) (string, error) {
	select {
	case <-b.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "block-done:" + name, nil
}

// waitBridgeString 轮询 stdout 缓冲直到条件满足（不 reset，累积匹配）。
func waitBridgeString(t *testing.T, buf *syncBuf, cond func(string) bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond(buf.String()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// waitStoreSettled 等会话 store 文件写入稳定（尺寸/mtime 连续 300ms 不变，或 3s 超时放行）。
// ask_batch/task_result 等入队语义：dispatch 返回后 loop/后台任务 goroutine 仍会异步
// persistHistory——测试若立即返回，t.TempDir 清理（RemoveAll）与迟到写入竞态，偶发
// "directory not empty" 清理失败（TestAskBatchWorkspaceRouting / TestInterruptTaskCommandSuccess）。
// 测试返回前调用本函数排空在途写入。
func waitStoreSettled(t *testing.T, ws, sid string) {
	t.Helper()
	path := filepath.Join(wsSessionsDir(ws), sid+".jsonl")
	type stamp struct {
		size int64
		mt   time.Time
	}
	var last stamp
	settledAt := time.Time{}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cur := stamp{-1, time.Time{}}
		if st, err := os.Stat(path); err == nil {
			cur = stamp{st.Size(), st.ModTime()}
		}
		if cur == last {
			if settledAt.IsZero() {
				settledAt = time.Now()
			} else if time.Since(settledAt) >= 300*time.Millisecond {
				return
			}
		} else {
			last = cur
			settledAt = time.Time{}
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// findFirst 扫描缓冲的 JSON 行，返回首个满足 match 的行 map。
func findFirst(s string, match func(map[string]any) bool) map[string]any {
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if match(m) {
			return m
		}
	}
	return nil
}

// countAgentEnd 统计缓冲里 agent_end 事件数。
func countAgentEnd(s string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["event_type"] == "agent_end" {
			n++
		}
	}
	return n
}

// TestPromoteTaskCommandErrors promote_task 错误路径：会话不存在 / 未知任务 id。
func TestPromoteTaskCommandErrors(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)

	// 会话不存在
	m.dispatch(cmd(0, "promote_task", map[string]any{"workspace": ws, "session_id": "nope", "task_id": "tooltask-1"}))
	got := findFirst(buf.String(), func(mm map[string]any) bool { return true })
	if got["ok"] != false || !strings.Contains(str(got, "error"), "session not found") {
		t.Fatalf("session-not-found: %+v", got)
	}
	buf.Reset()

	// 未知任务 id（会话存在但无运行中任务）
	sid := newSessionID(t, m, buf, ws)
	m.dispatch(cmd(1, "promote_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": "tooltask-999"}))
	got = findFirst(buf.String(), func(mm map[string]any) bool { return true })
	if got["ok"] != false || !strings.Contains(str(got, "error"), "task not found") {
		t.Fatalf("unknown-task: %+v", got)
	}
}

// TestPromoteTaskCommandSuccess 端到端：运行中长工具 → promote_task → 主 loop 拿占位继续 →
// release → 结果自动回传（task_result_delivered）→ wake 唤醒 run loop → 模型续跑收到结果。
func TestPromoteTaskCommandSuccess(t *testing.T) {
	// 等待预算 10s（原 3s）：干净 clone 首次全量跑时，其它包的编译与测试并行抢 CPU，
	// 3s 会让「promote → 占位 → task_result 回传」这种多段唤醒链偶发超时
	// （P0-09 的干净检出 gate 上实际踩到过一次）。断言内容不变，只放宽等待。
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	block := &bridgeBlockTool{gate: make(chan struct{})}
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), block)
	var mu sync.Mutex
	p := &bridgeMockProvider{}
	p.rounds = []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮1：调 block
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "block", Arguments: `{}`}},
			})
		},
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮2：看到占位，stop
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "moved to background"})
		},
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮3：收到 task_result 续跑
			for _, mm := range req.Messages {
				if mm.Role == core.User && len(mm.Content) > 0 && mm.Content[0].Type == core.ContentTypeTaskResult {
					mu.Lock()
					p.round3TaskResult = true
					mu.Unlock()
				}
			}
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "got result"})
		},
	}
	// 替换会话 loop（mock provider + block 工具）；bridgeSession.loop 字段保持原 loop（无碍）
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p), agents.WithToolEngine(eng), agents.WithMaxIterations(3)))

	// 触发一轮：block 工具运行中被 promote
	buf.Reset()
	m.dispatch(cmd(0, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "run"}))

	// 从 tool_run_start 事件取 task_id
	taskID := ""
	waitBridgeString(t, buf, func(s string) bool {
		ev := findFirst(s, func(mm map[string]any) bool {
			return mm["event_type"] == "tool_run_start" && str(mm, "task_id") != ""
		})
		if ev != nil {
			taskID = str(ev, "task_id")
		}
		return ev != nil
	}, 10*time.Second)

	// promote_task → ok:true
	buf.Reset()
	m.dispatch(cmd(1, "promote_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": taskID}))
	if resp := findFirst(buf.String(), func(mm map[string]any) bool { return true }); resp["ok"] != true {
		t.Fatalf("promote_task: %+v", resp)
	}

	// 主 run 拿到占位 → 下轮 stop → 第一次 agent_end
	waitBridgeString(t, buf, func(s string) bool { return countAgentEnd(s) >= 1 }, 10*time.Second)

	// release → 后台完成 → task_result_delivered 自动回传 + wake 唤醒 run loop
	block.Release()
	waitBridgeString(t, buf, func(s string) bool {
		return findFirst(s, func(mm map[string]any) bool { return mm["event_type"] == "task_result_delivered" }) != nil
	}, 10*time.Second)

	// wake 生效：run loop 再次运行，round3 模型收到 task_result 续跑（第二次 agent_end）
	waitBridgeString(t, buf, func(s string) bool { return countAgentEnd(s) >= 2 }, 10*time.Second)
	mu.Lock()
	gotResult := p.round3TaskResult
	mu.Unlock()
	if !gotResult {
		t.Fatal("round3 must receive the background task result (wakeRun must work)")
	}
}

// —— 客户端「停止」：interrupt 命令 → bridge → Session.Interrupt → ac.Abort 的即时性验证 ——

// abortAgentEnd 在缓冲里找 finish_reason=abort 的 agent_end（含 host 注入字段）。
func abortAgentEnd(s string) map[string]any {
	return findFirst(s, func(mm map[string]any) bool {
		return mm["event_type"] == "agent_end" && mm["finish_reason"] == string(core.FinishReasonAbort)
	})
}

// bridgeAbortStreamProvider 持续流式输出、只在 ctx 取消时解除阻塞的 provider
// （模拟模型一直吐字，用户点「停止」）。复现「中断必须立即打断流式调用」的桥接层场景。
type bridgeAbortStreamProvider struct {
	started chan struct{}
}

func (b *bridgeAbortStreamProvider) Stream(ctx context.Context, _ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	if err := handler(provider.LLMStartEvent{Model: "abort-stream"}); err != nil {
		return err
	}
	close(b.started)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
			if err := handler(provider.LLMContentDeltaEvent{Delta: "."}); err != nil {
				return err
			}
		}
	}
}

// TestBridgeInterruptStreamingImmediate 客户端 interrupt 命令是否立即打断 LLM 流式输出。
// 度量 dispatch("interrupt") → agent_end(finish_reason=abort) 的端到端延迟。
func TestBridgeInterruptStreamingImmediate(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	started := make(chan struct{})
	p := &bridgeAbortStreamProvider{started: started}
	eng := tools.NewToolEngine()
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p), agents.WithToolEngine(eng), agents.WithMaxIterations(3)))
	defer func() { _ = bs.s.Shutdown(context.Background()); bs.cancel() }() // 等运行收尾后再停 runLoop，避免 t.TempDir 清理竞态

	// 触发一轮：模型持续流式输出
	buf.Reset()
	m.dispatch(cmd(0, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "stream"}))
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}
	time.Sleep(30 * time.Millisecond) // 确认已在持续输出

	// 客户端「停止」：interrupt 命令
	buf.Reset() // 只统计 interrupt 之后的输出
	interruptAt := time.Now()
	m.dispatch(cmd(1, "interrupt", map[string]any{"workspace": ws, "session_id": sid}))

	// 等待 agent_end(finish_reason=abort) 发出
	waitBridgeString(t, buf, func(s string) bool { return abortAgentEnd(s) != nil }, 3*time.Second)
	latency := time.Since(interruptAt)
	if latency > 1*time.Second {
		t.Fatalf("interrupt was NOT immediate via bridge: latency=%v (expect <<1s)", latency)
	}
	t.Logf("bridge interrupt → agent_end(abort) latency: %v", latency)
}

// TestBridgeInterruptBlockingToolImmediate interrupt 是否能立即打断正在执行的工具调用
// （阻塞工具，模拟长 bash 命令）。度量 dispatch("interrupt") → agent_end(abort)。
func TestBridgeInterruptBlockingToolImmediate(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	block := &bridgeBlockTool{gate: make(chan struct{})} // 阻塞直到放行或 ctx 取消
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), block)
	p := &bridgeMockProvider{}
	p.rounds = []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮1：调阻塞工具
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "block", Arguments: `{}`}},
			})
		},
	}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p), agents.WithToolEngine(eng), agents.WithMaxIterations(3)))
	defer func() { _ = bs.s.Shutdown(context.Background()); bs.cancel() }() // 等运行收尾后再停 runLoop，避免 t.TempDir 清理竞态

	// 触发一轮：模型调用阻塞工具，工具进入执行中
	buf.Reset()
	m.dispatch(cmd(0, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "block"}))
	waitBridgeString(t, buf, func(s string) bool {
		return findFirst(s, func(mm map[string]any) bool { return mm["event_type"] == "tool_run_start" }) != nil
	}, 3*time.Second)

	// 客户端「停止」：interrupt 命令 → 工具应被 ctx 取消打断
	buf.Reset()
	interruptAt := time.Now()
	m.dispatch(cmd(1, "interrupt", map[string]any{"workspace": ws, "session_id": sid}))

	waitBridgeString(t, buf, func(s string) bool { return abortAgentEnd(s) != nil }, 3*time.Second)
	latency := time.Since(interruptAt)
	if latency > 1*time.Second {
		t.Fatalf("interrupt did NOT stop blocking tool immediately via bridge: latency=%v (expect <<1s)", latency)
	}
	t.Logf("bridge interrupt → agent_end(abort) during tool: %v", latency)
}

// TestSwitchModelKeepsOtherSessions 跨 Provider 切模型只影响目标会话：
// s1 切到 openai 模型后，s2 的 model/provider 必须保持不变（回归锚点：
// 此前客户端切模型会触发 set_provider → rebuildAll → 其它会话模型在目标
// Provider 缺失时被回退为默认模型 —— 模型串 Session 的服务端半环）。
func TestSwitchModelKeepsOtherSessions(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	s1, err := m.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := m.create(ws, "s2", "")
	if err != nil {
		t.Fatal(err)
	}

	// 记录切换前的全局默认（s1 切到 openai 后全局默认会变，不能拿它当 s2 的期望值）
	s2WantModel := m.provCfg.defaultModel()
	m.dispatch(cmd(0, "switch_model", map[string]any{"workspace": ws, "session_id": "s1", "model": "gpt-4o", "provider": "openai"}))
	if lines := commandResponses(readLines(buf)); len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("switch_model 应成功: %v", lines[0])
	}

	if s1.model != "gpt-4o" || s1.provider != "openai" {
		t.Fatalf("s1 应切为 openai/gpt-4o, got %s/%s", s1.provider, s1.model)
	}
	if s2.model != s2WantModel {
		t.Fatalf("s2 模型不应被改写: got %s, want %s", s2.model, s2WantModel)
	}
	if s2.provider != "deepseek" {
		t.Fatalf("s2 provider 不应被改写: got %s", s2.provider)
	}

	// 切回 s2 的 deepseek 模型同理不动 s1
	m.dispatch(cmd(0, "switch_model", map[string]any{"workspace": ws, "session_id": "s2", "model": "deepseek-chat", "provider": "deepseek"}))
	if lines := commandResponses(readLines(buf)); len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("switch_model 应成功: %v", lines[0])
	}
	if s1.model != "gpt-4o" || s1.provider != "openai" {
		t.Fatalf("s1 不应被 s2 的切换影响: got %s/%s", s1.provider, s1.model)
	}
	if s2.model != "deepseek-chat" {
		t.Fatalf("s2 应切为 deepseek-chat, got %s", s2.model)
	}
}

// TestAskBatchWakesIdleRun 问题十回归：Run 结束（idle）后到达的 ask_batch 必须唤醒
// runLoop 消费暂存消息并启动下一轮——「已推送但 harness 不继续执行」的卡死复现。
func TestAskBatchWakesIdleRun(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	var mu sync.Mutex
	p := &bridgeMockProvider{}
	p.rounds = []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮1：stop（Run1 结束）
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "run1 done"})
		},
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮2：应看到批量消息
			seen := false
			for _, mm := range req.Messages {
				if mm.Role == core.User && strings.Contains(mm.Content[0].Content, "BATCH-MSG") {
					seen = true
				}
			}
			mu.Lock()
			p.round3TaskResult = seen // 复用字段：轮2 是否看到批量消息
			mu.Unlock()
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "run2 done"})
		},
	}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p), agents.WithMaxIterations(3)))

	// Run1：ask → agent_end
	buf.Reset()
	m.dispatch(cmd(0, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "run"}))
	waitBridgeString(t, buf, func(s string) bool { return countAgentEnd(s) >= 1 }, 3*time.Second)

	// idle 后 ask_batch（对齐客户端 flushPending 的 payload 形状）
	buf.Reset()
	m.dispatch(cmd(1, "ask_batch", map[string]any{
		"workspace":  ws,
		"session_id": sid,
		"messages":   []any{map[string]any{"content": []any{map[string]any{"type": "text", "content": "BATCH-MSG"}}}},
	}))
	resp := findFirst(buf.String(), func(mm map[string]any) bool { return mm["event_type"] == "command_response" })
	t.Logf("[dbg] ask_batch resp: %+v", resp)
	t.Logf("[dbg] history len: %d", len(bs.s.History()))
	if resp == nil || resp["ok"] != true {
		t.Fatalf("ask_batch resp: %+v", resp)
	}

	// 必须唤醒 Run2 并消费消息
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countAgentEnd(buf.String()) >= 1 { // buf 已 Reset：只统计 Run2
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if countAgentEnd(buf.String()) < 1 {
		t.Fatalf("no run2 agent_end; full buf:\n%s", buf.String())
	}
	mu.Lock()
	seen := p.round3TaskResult
	mu.Unlock()
	if !seen {
		t.Fatal("run2 must receive the batched message")
	}
}

// TestRebuildAllKeepsExplicitProvider 回归：glm-5.3-flash 同时存在于 opencode 与 zhipu，
// 会话显式切到 zhipu（providerExplicit）后，触发 rebuildAll（set_provider 保存其它
// Provider）不得按模型名字母序把会话改写到 opencode——此前 o<z 导致请求打到错误网关
// （用户看到「选了 GLM Provider 却收到 Opencode 的 429」）。
func TestRebuildAllKeepsExplicitProvider(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)

	// 内置默认表里 opencode 与 zhipu 都含 glm-5.3-flash（字母序 opencode < zhipu）
	if !m.provCfg.hasModelIn("opencode", "glm-5.3-flash") || !m.provCfg.hasModelIn("zhipu", "glm-5.3-flash") {
		t.Fatal("前置：opencode/zhipu 都应含 glm-5.3-flash（否则测试无意义）")
	}

	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	// 用户显式切到 zhipu + glm-5.3-flash（Composer 跨 Provider 分组选中）
	buf.Reset()
	m.dispatch(cmd(0, "switch_model", map[string]any{"workspace": ws, "session_id": sid, "model": "glm-5.3-flash", "provider": "zhipu"}))
	if !lastRespOK(t, buf, 0) {
		t.Fatal("switch_model(zhipu/glm-5.3-flash) 应成功")
	}
	if bs.provider != "zhipu" || bs.model != "glm-5.3-flash" {
		t.Fatalf("会话应切为 zhipu/glm-5.3-flash, got %s/%s", bs.provider, bs.model)
	}

	// 保存其它 Provider 配置（如 deepseek 改 key）→ set_provider → rebuildAll
	buf.Reset()
	m.dispatch(cmd(1, "set_provider", map[string]any{"provider": "deepseek", "api_key": "sk-other"}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("set_provider 应成功")
	}
	// 回归锚点：rebuildAll 后会话仍归属 zhipu（不得被字母序改写到 opencode）
	if bs.provider != "zhipu" {
		t.Fatalf("rebuildAll 后会话 Provider 被改写: got %s, want zhipu（字母序 bug 回归）", bs.provider)
	}
	if bs.model != "glm-5.3-flash" {
		t.Fatalf("rebuildAll 后会话模型被改写: got %s", bs.model)
	}
}

// TestSwitchModelExplicitProviderNoSilentFallback 回归：用户显式指定 provider + 模型，
// 但该 provider 下（内存）暂时无此模型时，不得静默回退按模型名全局字母序解析——
// 此前会把 zhipu 的 glm-5.3-flash 猜成 opencode → 请求打到错误 Provider（401/429）。
// 现在：先重载 settings（刚落盘的 provider 配置）再查，仍无 → 显式报错 unknown_model。
func TestSwitchModelExplicitProviderNoSilentFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)

	// 场景：内存里 zhipu 无 glm-5.3-flash（用户刚 add provider，bridge 未同步），
	// 但 settings.json 刚落盘含它（electron provider:save 已写盘）→ 应重载后命中。
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), []byte(`{
		"provider": {
			"active": "opencode",
			"zhipu": {"base_url": "https://open.bigmodel.cn/api/anthropic", "protocol": "anthropic", "api_key": "zhipu-key", "models": {"glm-5.3-flash": 1000000}},
			"opencode": {"base_url": "https://opencode.ai/zen/go/v1", "protocol": "chat_completions", "api_key": "opencode-key", "models": {"glm-5.3-flash": 1000000}}
		}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.provCfg.loadSettings() // 模拟落盘已到但内存刚热载（含 zhipu）
	m.provCfg.syncRegistry()

	// 显式 zhipu + glm-5.3-flash → 必须成功且归属 zhipu（不落到 opencode）
	buf.Reset()
	m.dispatch(cmd(0, "switch_model", map[string]any{"workspace": ws, "session_id": sid, "model": "glm-5.3-flash", "provider": "zhipu"}))
	if !lastRespOK(t, buf, 0) {
		t.Fatalf("switch_model(zhipu) 应成功: %s", buf.String())
	}
	bs := m.get(ws, sid)
	if bs == nil || bs.provider != "zhipu" {
		t.Fatalf("会话应归属 zhipu, got %v", bs)
	}

	// 反向：显式 provider 下确实无此模型（settings 也没有）→ 报错而非静默回退
	buf.Reset()
	m.dispatch(cmd(1, "switch_model", map[string]any{"workspace": ws, "session_id": sid, "model": "glm-5.3-flash", "provider": "deepseek"}))
	ok := false
	for _, l := range readLines(buf) {
		if l["id"] == float64(1) && l["event_type"] == "command_response" {
			ok = l["ok"] == true
			if code, _ := l["code"].(string); code != "unknown_model" {
				t.Fatalf("应返回 unknown_model, got code=%v", l["code"])
			}
		}
	}
	if ok {
		t.Fatal("显式 provider 无此模型应失败（不得静默回退到其它 provider）")
	}
}

// TestNewSessionWithProvider 前端新建会话显式携带 provider（2026-09）→
// 新会话归属该 provider（而非全局 active 的旧值/字母序猜测）。
func TestNewSessionWithProvider(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	// 默认 active = deepseek；显式指定 zhipu 建会话
	m.dispatch(cmd(0, "new_session", map[string]any{"workspace": ws, "provider": "zhipu"}))
	var sid string
	for _, l := range readLines(buf) {
		if l["ok"] == true {
			if d, ok := l["data"].(map[string]any); ok {
				sid, _ = d["session_id"].(string)
			}
		}
	}
	if sid == "" {
		t.Fatal("new_session 无 session_id")
	}
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}
	if bs.provider != "zhipu" {
		t.Fatalf("新会话应归属显式指定的 zhipu, got %s", bs.provider)
	}
}

// TestAskBatchMidRunConsumed 问题十补充：Run 进行中到达的 ask_batch 在 stop 边界被
// 消费（插话路径），不依赖 wake。
func TestAskBatchMidRunConsumed(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	var mu sync.Mutex
	p := &bridgeMockProvider{}
	p.rounds = []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮1：sleep 给 ask_batch 留窗口 → 检查消息（成批消费则在轮1 上下文）
			time.Sleep(300 * time.Millisecond)
			if mockReqSawBATCH(req) {
				mu.Lock()
				p.round3TaskResult = true
				mu.Unlock()
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "consumed-in-round1"})
			}
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "waiting"})
		},
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮2：stop 边界插话消费 → 消息可见
			if mockReqSawBATCH(req) {
				mu.Lock()
				p.round3TaskResult = true
				mu.Unlock()
			}
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "consumed-in-round2"})
		},
	}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p), agents.WithMaxIterations(3)))

	buf.Reset()
	m.dispatch(cmd(0, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "run"}))
	// Run1 进行中（轮1 sleep 窗口内）推批量消息
	m.dispatch(cmd(1, "ask_batch", map[string]any{
		"workspace":  ws,
		"session_id": sid,
		"messages":   []any{map[string]any{"content": []any{map[string]any{"type": "text", "content": "BATCH-MSG"}}}},
	}))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seen := p.round3TaskResult
		mu.Unlock()
		if seen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	seen := p.round3TaskResult
	mu.Unlock()
	if !seen {
		t.Fatalf("batched message never reached the model; full buf:\n%s", buf.String())
	}
	waitStoreSettled(t, ws, sid) // 排空 run 异步落盘，避免 TempDir 清理竞态（同 TestAskBatchWorkspaceRouting）
}

// mockReqSawBATCH 检查请求消息里是否出现批量消息内容。
func mockReqSawBATCH(req *provider.StreamRequest) bool {
	for _, mm := range req.Messages {
		if mm.Role == core.User && strings.Contains(mm.Content[0].Content, "BATCH-MSG") {
			return true
		}
	}
	return false
}

// TestAskBatchAllSkippedFails 问题十回归：payload 全部被跳过（形状不识别/纯空）时
// 必须返回 ok:false——假成功会让客户端把消息标成「已推送」且永无消费路径。
func TestAskBatchAllSkippedFails(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)

	buf.Reset()
	m.dispatch(cmd(0, "ask_batch", map[string]any{
		"workspace":  ws,
		"session_id": sid,
		"messages":   []any{map[string]any{"content": []any{}}, map[string]any{"content": "not-an-array"}},
	}))
	resp := findFirst(buf.String(), func(mm map[string]any) bool { return mm["event_type"] == "command_response" })
	if resp == nil || resp["ok"] != false {
		t.Fatalf("all-skipped ask_batch must fail, got: %+v", resp)
	}
}

// TestSlashSkillWithArgsSplits 问题十一回归：/技能名 正文 → 名参切分——技能加载 +
// 正文作为普通用户消息进上下文。自带 mock provider（秒级收敛，不依赖真实 401 重试链）。
func TestSlashSkillWithArgsSplits(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	defer m.shutdownWorkspace(ws) // 等会话异步落盘收尾，避免 TempDir 清理竞态
	skillDir := filepath.Join(ws, ".go-code", "skills", "code-review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: code-review\ndescription: 评审\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.create(ws, "s1", ""); err != nil {
		t.Fatal(err)
	}
	bs := m.get(ws, "s1")
	if bs == nil {
		t.Fatal("s1 not found")
	}
	// mock provider：LLM 轮秒回，run 不被 401 重试占住
	mp := &bridgeMockProvider{}
	mp.rounds = []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}
	// WithSkills 必须带上：skills load 是内置命令，从 loop 配置取技能注册表
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(mp), agents.WithMaxIterations(3),
		agents.WithSkills(m.runtime(ws).skills)))

	m.dispatch(cmd(0, "command", map[string]any{"workspace": ws, "session_id": "s1", "line": "/code-review 帮我评审这个模块"}))
	ok, cr := waitSkillResult(t, buf, "skills", 10*time.Second)
	if !ok || !strings.Contains(cr, "loaded") {
		t.Fatalf("带正文的技能命令应正常加载, ok=%v cr=%q", ok, cr)
	}
	foundArg := false
	for _, msg := range bs.s.History() {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeText && strings.Contains(c.Content, "帮我评审这个模块") {
				foundArg = true
			}
		}
	}
	if !foundArg {
		t.Fatal("技能命令正文应作为用户消息进入会话历史")
	}

	// 未识别命令 → 文本回退：resp ok + 原文进 history（同会话验证闭环）
	m.dispatch(cmd(1, "command", map[string]any{"workspace": ws, "session_id": "s1", "line": "/nope-xyz 回退为文本"}))
	lines := readLines(buf)
	foundFallback := false
	for _, l := range lines {
		if l["event_type"] == "command_response" && l["ok"] == true {
			foundFallback = true
		}
	}
	if !foundFallback {
		t.Fatalf("未识别命令应 ok:true（文本回退）, got %v", lines)
	}
	foundText := false
	for _, msg := range bs.s.History() {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeText && strings.Contains(c.Content, "/nope-xyz 回退为文本") {
				foundText = true
			}
		}
	}
	if !foundText {
		t.Fatal("未识别命令原文应作为用户消息进入会话历史")
	}
}

// TestAskBatchWorkspaceRouting 问题十四回归：ask_batch 的 workspace 字段决定会话查找
// 归属——客户端（用户切走查看其他工作区后）后台会话 flush 必须携带会话归属 workspace，
// 否则 bridge 在错误 workspace 下查不到会话（session not found）。
func TestAskBatchWorkspaceRouting(t *testing.T) {
	ws := t.TempDir()
	other := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}
	p := &bridgeMockProvider{}
	p.rounds = []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
		},
	}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p), agents.WithMaxIterations(3)))

	// 正确归属 workspace → ok 且消息入队
	m.dispatch(cmd(0, "ask_batch", map[string]any{
		"workspace":  ws,
		"session_id": sid,
		"messages":   []any{map[string]any{"content": []any{map[string]any{"type": "text", "content": "MSG-WS-OK"}}}},
	}))
	lines := readLines(buf)
	found := false
	for _, l := range lines {
		if l["event_type"] == "command_response" && l["ok"] == true {
			found = true
		}
	}
	if !found {
		t.Fatalf("归属 workspace 的 ask_batch 应 ok:true, got %v", lines)
	}
	foundMsg := false
	for _, msg := range bs.s.History() {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeText && strings.Contains(c.Content, "MSG-WS-OK") {
				foundMsg = true
			}
		}
	}
	if !foundMsg {
		t.Fatal("消息应进入会话历史")
	}
	waitStoreSettled(t, ws, sid) // 排空 loop 异步落盘，避免 TempDir 清理竞态

	// 错位 workspace（会话不在该 workspace 下）→ 显式失败（客户端回滚为待发送）
	buf.Reset()
	m.dispatch(cmd(1, "ask_batch", map[string]any{
		"workspace":  other,
		"session_id": sid,
		"messages":   []any{map[string]any{"content": []any{map[string]any{"type": "text", "content": "MSG-WS-BAD"}}}},
	}))
	lines = readLines(buf)
	badOk := false
	for _, l := range lines {
		if l["event_type"] == "command_response" && l["ok"] == true {
			badOk = true
		}
	}
	if badOk {
		t.Fatal("错位 workspace 的 ask_batch 不应 ok:true（必须让客户端回滚）")
	}
}

// P1-10 N3：显式 provider 的模型表为空（本地 vLLM/Ollama 未拉列表）时允许指定模型；
// 表非空仍严格拒绝未知模型，provider 本身未配置也拒绝。
func TestAllowUnlistedModelOnlyForKnownEmptyProvider(t *testing.T) {
	pc := &providerConfig{
		baseURL:   map[string]string{"local": "http://127.0.0.1:8000/v1"},
		apiKey:    map[string]string{"local": "x"},
		models:    map[string]map[string]int64{"local": {}},
		maxTokens: map[string]map[string]int64{},
	}
	if !pc.allowUnlistedModel("local") {
		t.Fatal("known provider with empty model table must fail-open")
	}
	pc.models["local"]["known"] = 4096
	if pc.allowUnlistedModel("local") {
		t.Fatal("non-empty model table must be strict")
	}
	if pc.allowUnlistedModel("missing") {
		t.Fatal("unknown provider must not fail-open")
	}
}
