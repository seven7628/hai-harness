package main

// 测试包缺的公共小工具与最小 manager 装配。
//
// 这些 helper 在历史里只存在于**未入库**的文件中（.gitignore 忽略了
// desktop/app/scripts，而 bridge 侧也是同一种事故），于是 `go test
// ./desktop/bridge/` 在任何提交上都编译不过 —— 不是某次改动引入的。
//
// 这里只提供最小实现：一个并发安全的 buffer（桥把 JSON 行写进它，测试再读回来）、
// 一个逐行找 JSON 的扫描器、一个命令构造器，以及**隔离 HOME** 的 manager。

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/session"
	"github.com/seven7628/hai-harness/subagent"
)

// syncBuf 并发安全的 bytes.Buffer：桥在自己的 goroutine 里写，测试在另一个里读。
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuf) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// findFirst 在逐行 JSON 的流里找第一条满足 pred 的对象（找不到返回 nil）。
// 不是 JSON 的行（日志等）直接跳过 —— 它们本来就会混在同一个流里。
func findFirst(s string, pred func(map[string]any) bool) map[string]any {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if pred(m) {
			return m
		}
	}
	return nil
}

// cmd 构造一条命令（命令名在 V2 协议里是类型化的 Command）。
func cmd(id int, name string, payload map[string]any) command {
	return command{Id: id, Type: Command(name), Payload: payload}
}

// helperSeq 给每条测试命令/会话一个进程内唯一 id：同一个测试里连开两个会话时，
// 复用固定 id 会让 findFirst 读到上一次的响应（interrupt_no_cascade 就是两开）。
var helperSeq atomic.Int64

// newSessionID 走 new_session 命令真开一个会话，并返回它的 id。
// 之所以不直接 `m.create(...)`：新会话的 blocks/checkpoint 恢复路径本身就是要测的
// 一部分（restore 协议、live 快照），绕过 dispatch 等于把被测代码换掉。
func newSessionID(t *testing.T, m *manager, buf *syncBuf, ws string) string {
	t.Helper()
	n := helperSeq.Add(1)
	sid := "sess-test-" + strconv.FormatInt(n, 10)
	cid := int(n)
	m.dispatch(cmd(cid, "new_session", map[string]any{"workspace": ws, "id": sid}))
	resp := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(cid)
	})
	if resp == nil || resp["ok"] != true {
		t.Fatalf("new_session: %+v", resp)
	}
	return sid
}

// waitTreeSettled 等一棵目录安静下来：全树 (文件数 + 总字节 + 最新 mtime) 连续
// quiet 不变即返回，最多等 max。用于「异步写入 + 立刻删目录」这种竞态。
func waitTreeSettled(root string, quiet, max time.Duration) {
	deadline := time.Now().Add(max)
	var last int64 = -1
	var lastMod time.Time
	stableSince := time.Time{}
	for time.Now().Before(deadline) {
		files, size := 0, int64(0)
		newest := time.Time{}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			files++
			info, err := d.Info()
			if err != nil {
				return nil
			}
			size += info.Size()
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
		sig := size<<16 | int64(files)
		if sig == last && newest.Equal(lastMod) {
			if stableSince.IsZero() {
				stableSince = time.Now()
			} else if time.Since(stableSince) >= quiet {
				return
			}
		} else {
			stableSince = time.Time{}
			last, lastMod = sig, newest
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// helperHome testManager 给本次测试分配的 HOME。waitStoreSettled 需要它：
// 存储根在会话创建时就绑定了当时的 HOME，测试中途再改 HOME 不会移动已打开的文件。
var helperHome string

// waitStoreSettled 等会话记录落盘安静下来（size/mtime 连续 120ms 不变）。
//
// 会话 Shutdown 后仍有异步写入在飞，测试结束就 RemoveAll 会和它们竞态
// （表现为随机 "directory not empty" 或记录只写了一半）。这里不猜时长，
// 而是看文件本身安静了没有；记录始终没出现（空会话）就直接返回。
func waitStoreSettled(t *testing.T, ws, sid string) {
	t.Helper()
	_ = ws
	name := sid + ".record.json"
	appearBy := time.Now().Add(2 * time.Second)
	var size int64 = -1
	var mod time.Time
	var quiet time.Time
	for time.Now().Before(appearBy.Add(3 * time.Second)) {
		path := ""
		_ = filepath.WalkDir(helperHome, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != name {
				return nil
			}
			path = p
			return fs.SkipAll
		})
		if path == "" {
			if time.Now().After(appearBy) {
				return // 空会话根本没有记录可等
			}
			time.Sleep(20 * time.Millisecond)
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			return
		}
		if fi.Size() != size || !fi.ModTime().Equal(mod) {
			size, mod, quiet = fi.Size(), fi.ModTime(), time.Time{}
		} else if quiet.IsZero() {
			quiet = time.Now()
		} else if time.Since(quiet) >= 120*time.Millisecond {
			return // 连续 120ms 没动 → 写完了
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// testManager 返回一个把 HOME 指到临时目录的 manager（所有落盘都进 t.TempDir，
// 不碰真实 ~/.go-code），以及收集 stdout 事件的 buffer。
// 用完由 t.Cleanup 关掉所有会话，避免测试结束时 goroutine 还在跑。
func testManager(t *testing.T) (*manager, *syncBuf) {
	t.Helper()
	home := t.TempDir()
	helperHome = home
	t.Setenv("HOME", home)
	// 手机端会尝试监听端口，测试里一律关掉
	t.Setenv("HAI_MOBILE", "off")

	buf := &syncBuf{}
	m := &manager{
		workspaces: map[string]*workspaceRuntime{},
		provCfg:    loadProviderConfig(),
		out:        newOut(buf),
		metrics:    NewFileMetricsStore(filepath.Join(home, "metrics.jsonl")),
		memPrefs:   newMemoryPrefStore(),
		hub:        session.NewHub(meshNodeID()),
		wsReg:      newWorkspaceRegistry(filepath.Join(home, "workspaces.json")),
	}
	t.Cleanup(func() {
		// 会话可能还挂着 goroutine：统一收掉，避免相互污染
		defer func() { _ = recover() }()
		m.shutdownAll()
		// 落盘是异步的（reducer commit / 增量写 / 备份）。不等它安静下来，
		// t.TempDir 的 RemoveAll 就会与迟到写入竞态 —— 表现为随机
		// "directory not empty"，且只在大套件里偶发（单跑常常是绿的）。
		waitTreeSettled(home, 300*time.Millisecond, 3*time.Second)
	})
	return m, buf
}

// benchSessionID 基准语料用的会话 id（形如真实会话，避免代码里出现测试专用分支）。
const benchSessionID = "bench-corpus-session"

// readLines 把 stdout 流拆成逐行 JSON 对象（非 JSON 行——日志、进度——跳过）。
// 需要「第 N 条响应 / 响应之后新到了什么」的测试用它切片，配合 lastOK。
func readLines(buf *syncBuf) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

// lastResp 取**最后一条**命令响应，返回 (整行, data)。命令可能同步回多行
// （先 ack 后结果），所以按 event_type 过滤而不是按位置。
func lastResp(t *testing.T, buf *syncBuf) (map[string]any, map[string]any) {
	t.Helper()
	all := readLines(buf)
	for i := len(all) - 1; i >= 0; i-- {
		if all[i]["event_type"] != "command_response" {
			continue
		}
		data, _ := all[i]["data"].(map[string]any)
		if data == nil {
			data = map[string]any{}
		}
		return all[i], data
	}
	t.Fatalf("没有命令响应：%s", buf.String())
	return nil, nil
}

// waitBridgeString 等 stdout 里出现满足 pred 的片段（桥的事件由别的 goroutine 写）。
// 异步事件（task_started / task_result_delivered）无法同步断言，只能等。
func waitBridgeString(t *testing.T, buf *syncBuf, pred func(string) bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred(buf.String()) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时，最后的输出：%s", buf.String())
}

// writeMCPConfig 写项目层 MCP 配置（{ws}/.go-code/settings.json 的 mcpServers 段）。
// 内容用原始 JSON 串：测试要复现「文件里本来是什么」，包括未知字段与旧值。
func writeMCPConfig(t *testing.T, path, servers string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"mcpServers": ` + servers + `}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// jsonFile 读一个 JSON 文件成通用 map（断言落盘结果用）。
func jsonFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s 不是合法 JSON: %v", path, err)
	}
	return m
}

// mcpProjects 走 mcp_projects 命令取项目视图，按 workspace 路径建索引。
// 命令行里的数据形状（[]mcpProjectInfo 的 JSON）是**对外契约**，所以这里按 JSON 读，
// 不绑到 Go 结构体上——契约改了测试该红。
func mcpProjects(t *testing.T, m *manager, buf *syncBuf, id int, wss []string) map[string]map[string]any {
	t.Helper()
	// payload 必须按 JSON 解出来的形状给（[]any）：生产的 strSlice 只认这一种，
	// 直接塞 []string 会静默得到空列表 —— 那会让「头丢了」的断言假失败。
	wsAny := make([]any, len(wss))
	for i, w := range wss {
		wsAny[i] = w
	}
	buf.Reset()
	m.dispatch(cmd(id, "mcp_projects", map[string]any{"workspaces": wsAny}))
	resp, data := lastResp(t, buf)
	if resp["ok"] != true {
		t.Fatalf("mcp_projects: %+v", resp)
	}
	list, _ := data["projects"].([]any)
	out := make(map[string]map[string]any, len(list))
	for _, it := range list {
		row, _ := it.(map[string]any)
		if p, _ := row["path"].(string); p != "" {
			out[p] = row
		}
	}
	return out
}

// mcpProjectServers 把项目视图里的 servers 数组按 name 建索引。
func mcpProjectServers(t *testing.T, proj map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, _ := proj["servers"].([]any)
	for _, it := range list {
		row, _ := it.(map[string]any)
		if n, _ := row["name"].(string); n != "" {
			out[n] = row
		}
	}
	return out
}

// mustApply 应用一条事件，失败即 Fatal（无宿主注解的路径）。
func mustApply(t *testing.T, r *ViewReducer, ev events.Event) {
	t.Helper()
	if err := r.Apply(ev, EventMeta{}); err != nil {
		t.Fatalf("apply %s: %v", ev.Type(), err)
	}
}

// blockByKind 取视图中某类块（kind: tool / agent / assistant / ...）。
func blockByKind(t *testing.T, cp *Checkpoint, kind string) []Block {
	t.Helper()
	var out []Block
	for _, b := range cp.Blocks {
		if b.Kind == kind {
			out = append(out, b)
		}
	}
	return out
}

// evToolStart / evToolEnd / evAgentStart 构造事件。
// 只填 reducer 真的读的字段：EventType 由生产者设置，reducer 按具体类型分派，
// 测试里留零值即可（真日志的字段集合由 incremental_marshal 等测试覆盖）。
func evToolStart(t *testing.T, runID, id, name, args string, ts time.Time) *events.ToolStart {
	t.Helper()
	return &events.ToolStart{Id: id, Name: name, RunId: runID, Arguments: args, Timestamp: ts}
}

func evToolEnd(t *testing.T, runID, id, name, result string, isErr bool, diff *core.FileDiff, ts time.Time) *events.ToolResponse {
	t.Helper()
	return &events.ToolResponse{Id: id, Name: name, RunId: runID, Result: result, IsError: isErr, Diff: diff, Timestamp: ts}
}

func evAgentStart(t *testing.T, runID, parentRunID, name, taskID string, ts time.Time) *events.AgentStart {
	t.Helper()
	e := &events.AgentStart{RunId: runID, ParentRunId: parentRunID, Name: name, TaskId: taskID, Timestamp: ts}
	if parentRunID != "" {
		e.Depth = 1
	}
	return e
}

// taskStatus 取后台任务当前状态（找不到 → 空串，与「还没起来」区分不开时由调用点断言）。
func taskStatus(bs *bridgeSession, taskID string) subagent.TaskStatus {
	for _, ti := range bs.s.Registry().List() {
		if ti.ID == taskID {
			return ti.Status
		}
	}
	return ""
}

// bridgeMockProvider 按预设轮次作答的主 loop 假 provider：
// 每一轮是一个函数（可发 ToolCall、可发文本），第 N 次 Stream 用第 N 个。
// 比真 provider 稳（无网络、无时序），但走的是**同一个** AgentLoop 分派路径。
type bridgeMockProvider struct {
	rounds []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error
	n      int
}

func (p *bridgeMockProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	if p.n >= len(p.rounds) {
		return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
	}
	fn := p.rounds[p.n]
	p.n++
	return fn(req, handler)
}
