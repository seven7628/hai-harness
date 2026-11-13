package main

// P2-02：Hub 工作区/会话工具 —— 参数解析、Service 透传、Policy 可见性。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/tools"
)

type toolFakeOps struct {
	emptyLists  bool
	dispatchErr error
	dispatched  []agenthub.DispatchReq
	created     []agenthub.CreateWorkspaceReq
	createdSes  []agenthub.CreateSessionReq
}

func (f *toolFakeOps) ListWorkspaces(context.Context) ([]agenthub.WorkspaceInfo, error) {
	if f.emptyLists {
		return nil, nil
	}
	return []agenthub.WorkspaceInfo{{Path: "/w", Name: "w"}}, nil
}

func (f *toolFakeOps) CreateWorkspace(_ context.Context, req agenthub.CreateWorkspaceReq) (agenthub.WorkspaceInfo, error) {
	f.created = append(f.created, req)
	return agenthub.WorkspaceInfo{Path: req.Parent + "/" + req.Name, Name: req.Name}, nil
}

func (f *toolFakeOps) ListSessions(context.Context, string) ([]agenthub.SessionInfo, error) {
	return []agenthub.SessionInfo{{ID: "s1", Workspace: "/w", Title: "会话", Running: true}}, nil
}

func (f *toolFakeOps) SessionStatus(context.Context, string, string) (agenthub.SessionInfo, error) {
	return agenthub.SessionInfo{ID: "s1", Workspace: "/w"}, nil
}

func (f *toolFakeOps) CreateSession(_ context.Context, req agenthub.CreateSessionReq) (agenthub.SessionInfo, error) {
	f.createdSes = append(f.createdSes, req)
	return agenthub.SessionInfo{ID: "s-new", Workspace: req.Workspace, Title: req.Title}, nil
}

func (f *toolFakeOps) Dispatch(_ context.Context, req agenthub.DispatchReq) (agenthub.DispatchResult, error) {
	if f.dispatchErr != nil {
		return agenthub.DispatchResult{}, f.dispatchErr
	}
	f.dispatched = append(f.dispatched, req)
	return agenthub.DispatchResult{SessionID: req.SessionID, Accepted: true}, nil
}

func (f *toolFakeOps) InterruptSession(context.Context, string) error { return nil }

func hubWorkspaceToolEngine(t *testing.T, ops agenthub.WorkspaceOps) (*tools.Engine, *agenthub.WorkspaceService) {
	t.Helper()
	svc := agenthub.NewWorkspaceService(ops, "ag_tools")
	svc.NewTaskID = func() string { return "task_fixed" }
	eng := tools.NewToolEngine()
	registerHubWorkspaceTools(eng, svc, nil)
	return eng, svc
}

func callHubTool(t *testing.T, eng *tools.Engine, name, args string) (string, error) {
	t.Helper()
	tl, err := eng.GetTool(context.Background(), name)
	if err != nil {
		t.Fatalf("GetTool(%s): %v", name, err)
	}
	return tl.Call(context.Background(), name, args)
}

func TestHubWorkspaceTools(t *testing.T) {
	ops := &toolFakeOps{}
	eng, _ := hubWorkspaceToolEngine(t, ops)

	out, err := callHubTool(t, eng, "workspace_list", `{}`)
	if err != nil {
		t.Fatalf("workspace_list: %v", err)
	}
	var ws struct {
		Workspaces []agenthub.WorkspaceInfo `json:"workspaces"`
	}
	if json.Unmarshal([]byte(out), &ws) != nil || len(ws.Workspaces) != 1 || ws.Workspaces[0].Path != "/w" {
		t.Fatalf("workspace_list out = %s", out)
	}

	out, err = callHubTool(t, eng, "workspace_create", `{"parent":" /root ","name":" proj "}`)
	if err != nil {
		t.Fatalf("workspace_create: %v", err)
	}
	if !strings.Contains(out, `"name":"proj"`) {
		t.Fatalf("workspace_create out = %s", out)
	}
	if len(ops.created) != 1 || ops.created[0].Parent != "/root" || ops.created[0].Name != "proj" ||
		ops.created[0].AgentID != "ag_tools" {
		t.Fatalf("create req = %+v", ops.created)
	}
	// parent 可省略（默认第一个 root）：工具层接受、ops 决定最终父目录。
	if _, err := callHubTool(t, eng, "workspace_create", `{"name":"auto"}`); err != nil {
		t.Fatalf("workspace_create without parent: %v", err)
	}
	pub, _ := eng.GetTool(context.Background(), "workspace_create")
	if err := pub.ValidParams(context.Background(), "workspace_create", `{"name":""}`); err == nil {
		t.Fatal("workspace_create must require name")
	}

	out, err = callHubTool(t, eng, "session_list", `{"workspace":"/w"}`)
	if err != nil {
		t.Fatalf("session_list: %v", err)
	}
	if !strings.Contains(out, `"id":"s1"`) {
		t.Fatalf("session_list out = %s", out)
	}
	// 空 workspace 在工具层就被拦（不落到 Service/ops）。
	tl, _ := eng.GetTool(context.Background(), "session_list")
	if err := tl.ValidParams(context.Background(), "session_list", `{"workspace":""}`); err == nil {
		t.Fatal("session_list must require workspace")
	}

	out, err = callHubTool(t, eng, "session_create", `{"workspace":"/w","title":" 首个 "}`)
	if err != nil {
		t.Fatalf("session_create: %v", err)
	}
	if !strings.Contains(out, `"id":"s-new"`) || len(ops.createdSes) != 1 || ops.createdSes[0].Title != "首个" {
		t.Fatalf("session_create out = %s req = %+v", out, ops.createdSes)
	}

	out, err = callHubTool(t, eng, "session_send", `{"workspace":"/w","session_id":"s1","text":" 做对比表 ","client_msg_id":" cm1 "}`)
	if err != nil {
		t.Fatalf("session_send: %v", err)
	}
	var sent struct {
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
		Accepted  bool   `json:"accepted"`
	}
	if json.Unmarshal([]byte(out), &sent) != nil || sent.TaskID != "task_fixed" || sent.SessionID != "s1" || !sent.Accepted {
		t.Fatalf("session_send out = %s", out)
	}
	if len(ops.dispatched) != 1 || ops.dispatched[0].Text != "做对比表" || ops.dispatched[0].ClientMsgID != " cm1 " {
		t.Fatalf("dispatch req = %+v", ops.dispatched)
	}
	if _, err := callHubTool(t, eng, "session_send", `{"workspace":"/w","session_id":"s1","text":"  "}`); err == nil {
		t.Fatal("session_send empty text must be rejected")
	}
	// ops 错误原样上抛（工具层不吞错、不重试）。
	ops.dispatchErr = errors.New("工作区未登记")
	if _, err := callHubTool(t, eng, "session_send", `{"workspace":"/w","session_id":"s1","text":"x"}`); err == nil ||
		!strings.Contains(err.Error(), "工作区未登记") {
		t.Fatalf("dispatch error = %v", err)
	}
}

// readonly 预设：list 可见、create/send 被撤下；disable 精确名同样生效。
func TestHubWorkspaceToolsPolicy(t *testing.T) {
	ops := &toolFakeOps{}
	eng, _ := hubWorkspaceToolEngine(t, ops)
	hidden := applyHubToolPolicy(context.Background(), eng, agenthub.ToolPolicy{Preset: agenthub.ToolPresetReadonly})
	for _, name := range []string{"workspace_create", "session_create", "session_send"} {
		if _, err := eng.GetTool(context.Background(), name); err == nil || !strings.Contains(err.Error(), "disabled by policy") {
			t.Errorf("readonly must hide %s (hidden=%v): %v", name, hidden, err)
		}
	}
	for _, name := range []string{"workspace_list", "session_list"} {
		if _, err := eng.GetTool(context.Background(), name); err != nil {
			t.Errorf("readonly must keep %s: %v", name, err)
		}
	}

	eng2, _ := hubWorkspaceToolEngine(t, ops)
	applyHubToolPolicy(context.Background(), eng2, agenthub.ToolPolicy{Disable: []string{"workspace_list"}})
	if _, err := eng2.GetTool(context.Background(), "workspace_list"); err == nil || !strings.Contains(err.Error(), "disabled by policy") {
		t.Fatalf("disable=workspace_list must hide it: %v", err)
	}
}

// 空列表必须是 []（不是 null），时间字段不能冒出 0001-01-01。
func TestHubWorkspaceToolsEmptyLists(t *testing.T) {
	eng, _ := hubWorkspaceToolEngine(t, &toolFakeOps{emptyLists: true})
	out, err := callHubTool(t, eng, "workspace_list", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"workspaces":[]}` {
		t.Fatalf("workspace_list empty = %s", out)
	}
	if strings.Contains(out, "0001-01-01") {
		t.Fatalf("zero updated_at leaked: %s", out)
	}
	sl, _ := eng.GetTool(context.Background(), "session_list")
	out, err = sl.Call(context.Background(), "session_list", `{"workspace":"/w"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "0001-01-01") {
		t.Fatalf("zero updated_at leaked: %s", out)
	}
}

// 场景4：toolFakeOps 的命令通道 + 忙闲可调（只用于新协议测试，旧测试仍用裸 fake）。
type toolBatchFakeOps struct {
	*toolFakeOps
	running  bool
	commands []agenthub.CommandSessionReq
}

func (f *toolBatchFakeOps) SessionStatus(context.Context, string, string) (agenthub.SessionInfo, error) {
	return agenthub.SessionInfo{ID: "s1", Workspace: "/w", Running: f.running}, nil
}

func (f *toolBatchFakeOps) CommandSession(_ context.Context, req agenthub.CommandSessionReq) error {
	f.commands = append(f.commands, req)
	return nil
}

// 旧的 {workspace,session_id,text} 调用：返回的键与值一个字都不能变。
func TestHubSessionSendToolLegacyShape(t *testing.T) {
	eng, _ := hubWorkspaceToolEngine(t, &toolFakeOps{})
	tl, err := eng.GetTool(context.Background(), "session_send")
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.Call(context.Background(), "session_send",
		`{"workspace":"/w","session_id":"s1","text":"做对比表","client_msg_id":"cm1"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"accepted":true,"message":"","queued":false,"session_id":"s1","status":"","task_id":"task_fixed"}`
	if out != want {
		t.Fatalf("legacy session_send out = %s, want %s", out, want)
	}
}

// 场景4：一批的参数校验（至少一个、命令白名单、clear 互斥、绝对路径、数量上限）。
func TestHubSessionSendToolBatchParams(t *testing.T) {
	eng, _ := hubWorkspaceToolEngine(t, &toolFakeOps{})
	tl, err := eng.GetTool(context.Background(), "session_send")
	if err != nil {
		t.Fatal(err)
	}
	many := make([]string, 0, agenthub.MaxSessionAttachments+1)
	for i := 0; i <= agenthub.MaxSessionAttachments; i++ {
		many = append(many, fmt.Sprintf(`"/w/f-%d.md"`, i))
	}
	bad := []string{
		`{"workspace":"/w","session_id":"s1","text":"  "}`,
		`{"workspace":"/w","text":"x"}`,
		`{"workspace":"/w","session_id":"s1","command":"nope"}`,
		`{"workspace":"/w","session_id":"s1","command":"clear","text":"x"}`,
		`{"workspace":"/w","session_id":"s1","command":"clear","images":["/w/a.png"]}`,
		`{"workspace":"/w","session_id":"s1","command":"skills","command_args":"pdf"}`,
		`{"workspace":"/w","session_id":"s1","command":"compact","command_args":"x"}`,
		`{"workspace":"/w","session_id":"s1","files":["a.md"]}`,
		`{"workspace":"/w","session_id":"s1","files":["~/.ssh/id_rsa"]}`,
		`{"workspace":"/w","session_id":"s1","files":[` + strings.Join(many, ",") + `]}`,
	}
	for _, args := range bad {
		if err := tl.ValidParams(context.Background(), "session_send", args); err == nil {
			t.Errorf("ValidParams(%s) must fail", args)
		}
	}
	good := []string{
		`{"workspace":"/w","session_id":"s1","text":"x"}`,
		`{"workspace":"/w","session_id":"s1","text":"","images":["/w/a.png"]}`,
		`{"workspace":"/w","session_id":"s1","text":"","files":["/w/a.md"]}`,
		`{"workspace":"/w","session_id":"s1","command":"compact"}`,
		`{"workspace":"/w","session_id":"s1","command":"clear"}`,
		`{"workspace":"/w","session_id":"s1","command":"skills","command_args":"load pdf"}`,
		`{"workspace":"/w","session_id":"s1","text":"正文","command":"compact","files":["/w/a.md"],"images":["/w/a.png"]}`,
	}
	for _, args := range good {
		if err := tl.ValidParams(context.Background(), "session_send", args); err != nil {
			t.Errorf("ValidParams(%s) = %v", args, err)
		}
	}
}

// 场景4：只有命令 → command_only + 空 task_id（不编造一次性 id、不建 Task）。
func TestHubSessionSendToolCommandOnly(t *testing.T) {
	ops := &toolBatchFakeOps{toolFakeOps: &toolFakeOps{}}
	eng, _ := hubWorkspaceToolEngine(t, ops)
	out, err := callHubTool(t, eng, "session_send", `{"workspace":"/w","session_id":"s1","command":"compact"}`)
	if err != nil {
		t.Fatalf("command-only: %v", err)
	}
	var got struct {
		TaskID  string `json:"task_id"`
		Status  string `json:"status"`
		Queued  bool   `json:"queued"`
		Command *struct {
			Name      string `json:"name"`
			Args      string `json:"args"`
			Delivered bool   `json:"delivered"`
		} `json:"command"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.TaskID != "" || got.Status != agenthub.StatusCommandOnly || got.Queued {
		t.Fatalf("command-only out = %s", out)
	}
	if got.Command == nil || got.Command.Name != "compact" || !got.Command.Delivered {
		t.Fatalf("command receipt = %s", out)
	}
	if len(ops.commands) != 1 || ops.commands[0].Command != "compact" || len(ops.dispatched) != 0 {
		t.Fatalf("commands = %+v dispatched = %+v", ops.commands, ops.dispatched)
	}
}

// 场景4：命令 + 正文走队列 → queued=true 时 command.delivered=false（命令还没进目标）。
func TestHubSessionSendToolQueuedCommandNotDelivered(t *testing.T) {
	ops := &toolBatchFakeOps{toolFakeOps: &toolFakeOps{}, running: true}
	svc := &agenthub.WorkspaceService{
		Ops: ops, AgentID: "ag_tools",
		Queue: agenthub.NewQueue(agenthub.NewLedger(agenthub.NewMemoryStore()), ops),
	}
	eng := tools.NewToolEngine()
	registerHubWorkspaceTools(eng, svc, nil)
	out, err := callHubTool(t, eng, "session_send", `{"workspace":"/w","session_id":"s1","text":"正文","command":"compact"}`)
	if err != nil {
		t.Fatalf("queued send: %v", err)
	}
	var got struct {
		TaskID  string `json:"task_id"`
		Queued  bool   `json:"queued"`
		Command *struct {
			Delivered bool `json:"delivered"`
		} `json:"command"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Queued || got.TaskID == "" || got.Command == nil || got.Command.Delivered {
		t.Fatalf("queued out = %s; want queued with command.delivered=false", out)
	}
}
