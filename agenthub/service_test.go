package agenthub

// P2-02：WorkspaceService —— 工具层的校验与派发语义（假 WorkspaceOps）。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeWorkspaceOps struct {
	listed      int
	created     []CreateWorkspaceReq
	sessions    []SessionInfo
	createdSes  []CreateSessionReq
	dispatched  []DispatchReq
	dispatchErr error
	duplicate   bool
	workspaces  []WorkspaceInfo
}

func (f *fakeWorkspaceOps) ListWorkspaces(context.Context) ([]WorkspaceInfo, error) {
	f.listed++
	return f.workspaces, nil
}

func (f *fakeWorkspaceOps) CreateWorkspace(_ context.Context, req CreateWorkspaceReq) (WorkspaceInfo, error) {
	f.created = append(f.created, req)
	return WorkspaceInfo{Path: req.Parent + "/" + req.Name, Name: req.Name}, nil
}

func (f *fakeWorkspaceOps) ListSessions(_ context.Context, workspace string) ([]SessionInfo, error) {
	f.sessions = []SessionInfo{{ID: "s1", Workspace: workspace, Title: "会话"}}
	return f.sessions, nil
}

func (f *fakeWorkspaceOps) SessionStatus(context.Context, string, string) (SessionInfo, error) {
	return SessionInfo{ID: "s1", Workspace: "/w"}, nil
}

func (f *fakeWorkspaceOps) CreateSession(_ context.Context, req CreateSessionReq) (SessionInfo, error) {
	f.createdSes = append(f.createdSes, req)
	return SessionInfo{ID: "s-new", Workspace: req.Workspace, Title: req.Title}, nil
}

func (f *fakeWorkspaceOps) Dispatch(_ context.Context, req DispatchReq) (DispatchResult, error) {
	if f.dispatchErr != nil {
		return DispatchResult{}, f.dispatchErr
	}
	f.dispatched = append(f.dispatched, req)
	msg := ""
	if f.duplicate {
		msg = DispatchDuplicateMessage
	}
	return DispatchResult{SessionID: req.SessionID, Accepted: true, Message: msg}, nil
}

func (f *fakeWorkspaceOps) InterruptSession(context.Context, string) error { return nil }

func TestWorkspaceServiceSend(t *testing.T) {
	ops := &fakeWorkspaceOps{}
	svc := NewWorkspaceService(ops, "ag_1")
	svc.NewTaskID = func() string { return "task_fixed" }

	res, err := svc.Send(context.Background(), " /w ", " s1 ", "  做一份对比表  ", " cm1 ")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.TaskID != "task_fixed" || res.SessionID != "s1" || !res.Accepted {
		t.Fatalf("send result = %+v", res)
	}
	if len(ops.dispatched) != 1 {
		t.Fatalf("dispatch calls = %d", len(ops.dispatched))
	}
	got := ops.dispatched[0]
	if got.AgentID != "ag_1" || got.Workspace != "/w" || got.SessionID != "s1" ||
		got.Text != "做一份对比表" || got.ClientMsgID != " cm1 " { // 幂等键原样透传，不 TrimSpace
		t.Fatalf("dispatch req = %+v", got)
	}
	// 默认 id：非空、前缀、两次不同（一次性 id）。
	svc.NewTaskID = nil
	a, _ := svc.Send(context.Background(), "/w", "s1", "x", "")
	b, _ := svc.Send(context.Background(), "/w", "s1", "x", "")
	if !strings.HasPrefix(a.TaskID, "task_") || a.TaskID == "" || a.TaskID == b.TaskID {
		t.Fatalf("default task ids = %q / %q", a.TaskID, b.TaskID)
	}
}

func TestWorkspaceServiceSendQueueDuplicateReason(t *testing.T) {
	ctx := context.Background()
	ops := &fakeWorkspaceOps{duplicate: true}
	st := NewMemoryStore()
	q := NewQueue(NewLedger(st), ops)
	svc := &WorkspaceService{Ops: ops, AgentID: "ag_1", Queue: q}

	res, err := svc.Send(ctx, "/w", "s1", "做一份对比表", "c-dup")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Status != string(TaskCanceled) || res.Message == "" || res.Accepted {
		t.Fatalf("duplicate result = %+v; want canceled/non-accepted with reason", res)
	}
	if !strings.Contains(res.Message, "去重") {
		t.Fatalf("duplicate reason = %q, want 去重", res.Message)
	}
}

func TestWorkspaceServiceValidation(t *testing.T) {
	ops := &fakeWorkspaceOps{}
	svc := NewWorkspaceService(ops, "ag_1")

	cases := []struct {
		name string
		call func() error
	}{
		{"send empty workspace", func() error { _, err := svc.Send(context.Background(), "  ", "s1", "x", ""); return err }},
		{"send empty session", func() error { _, err := svc.Send(context.Background(), "/w", " ", "x", ""); return err }},
		{"send empty text", func() error { _, err := svc.Send(context.Background(), "/w", "s1", "  ", ""); return err }},
		{"create empty name", func() error { _, err := svc.CreateWorkspace(context.Background(), "/w", " "); return err }},
		{"list empty workspace", func() error { _, err := svc.ListSessions(context.Background(), " "); return err }},
		{"create session empty workspace", func() error { _, err := svc.CreateSession(context.Background(), "", "t"); return err }},
	}
	for _, c := range cases {
		if err := c.call(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", c.name, err)
		}
	}
	if len(ops.dispatched) != 0 || len(ops.created) != 0 || len(ops.createdSes) != 0 {
		t.Fatalf("validation must not reach ops: %+v", ops)
	}
	// 没有 ops 也要给出明确错误，而不是 panic。
	empty := NewWorkspaceService(nil, "ag_1")
	if _, err := empty.Send(context.Background(), "/w", "s1", "x", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil ops = %v", err)
	}
}

func TestWorkspaceServicePassThrough(t *testing.T) {
	ops := &fakeWorkspaceOps{workspaces: []WorkspaceInfo{{Path: "/w", Name: "w"}}}
	svc := NewWorkspaceService(ops, "ag_1")

	ws, err := svc.ListWorkspaces(context.Background())
	if err != nil || len(ws) != 1 || ws[0].Path != "/w" {
		t.Fatalf("list = %+v, %v", ws, err)
	}
	created, err := svc.CreateWorkspace(context.Background(), " /root ", " proj ")
	if err != nil || created.Name != "proj" {
		t.Fatalf("create = %+v, %v", created, err)
	}
	if len(ops.created) != 1 || ops.created[0].Parent != "/root" || ops.created[0].Name != "proj" || ops.created[0].AgentID != "ag_1" {
		t.Fatalf("create req = %+v", ops.created)
	}
	if _, err := svc.ListSessions(context.Background(), "/w"); err != nil {
		t.Fatal(err)
	}
	ses, err := svc.CreateSession(context.Background(), " /w ", " 标题 ")
	if err != nil || ses.ID != "s-new" {
		t.Fatalf("create session = %+v, %v", ses, err)
	}
	if len(ops.createdSes) != 1 || ops.createdSes[0].Workspace != "/w" || ops.createdSes[0].Title != "标题" || ops.createdSes[0].AgentID != "ag_1" {
		t.Fatalf("create session req = %+v", ops.createdSes)
	}
	// ops 错误原样向上传（工具层不吞错）。
	ops.dispatchErr = errors.New("工作区未登记")
	if _, err := svc.Send(context.Background(), "/w", "s1", "x", ""); err == nil || !strings.Contains(err.Error(), "工作区未登记") {
		t.Fatalf("dispatch error = %v", err)
	}
}

// P2.5-07 N2/N3/N4：结果文案与 accepted 的状态白名单。
func TestSendResultSemantics(t *testing.T) {
	cases := []struct {
		status   TaskStatus
		error    string
		accepted bool
		message  string
	}{
		{TaskDispatched, "", true, ""},
		{TaskQueued, "", true, ""},
		{TaskCompleted, "", true, ""},
		{TaskCanceled, "目标侧去重命中", false, "目标侧去重命中"},
		{TaskInterrupted, "进程重启：没有运行记录", false, "进程重启：没有运行记录"},
		{TaskBlockedBudget, "预算超限", false, "预算超限"},
		{TaskDeclined, "目标拒绝", false, "目标拒绝"},
		{TaskFailed, "/secret/path: permission denied", false, "派发失败，详见 task_status"},
	}
	for _, c := range cases {
		task := &Task{Status: c.status, Error: c.error}
		if got := sendResultAccepted(task); got != c.accepted {
			t.Errorf("%s accepted = %v, want %v", c.status, got, c.accepted)
		}
		if got := sendResultMessage(task); got != c.message {
			t.Errorf("%s message = %q, want %q", c.status, got, c.message)
		}
	}
}
