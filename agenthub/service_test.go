package agenthub

// P2-02：WorkspaceService —— 工具层的校验与派发语义（假 WorkspaceOps）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
		{TaskRunning, "", true, ""},
		{TaskCompleted, "", true, "此前已完成"},
		{TaskDelivered, "", true, "此前已回推"},
		{TaskStatus("some_new_status"), "URL /secret", false, ""},
		{TaskCanceled, "目标侧去重命中", false, "目标侧去重命中"},
		{TaskInterrupted, "进程重启：没有运行记录", false, "进程重启：没有运行记录"},
		{TaskBlockedBudget, "预算超限", false, "预算超限"},
		{TaskDeclined, "目标拒绝", false, "目标拒绝"},
		{TaskFailed, "/secret/path: permission denied", false, "派发失败，详见 task_status"},
	}
	if sendResultAccepted(nil) {
		t.Fatal("nil task must not be accepted")
	}
	for _, c := range cases {
		task := &Task{Status: c.status, Error: c.error}
		if got := sendResultAccepted(task); got != c.accepted {
			t.Errorf("%s accepted = %v, want %v", c.status, got, c.accepted)
		}
		gotMsg := sendResultMessage(task)
		if c.message != "" {
			if !strings.Contains(gotMsg, c.message) {
				t.Errorf("%s message = %q, want contains %q", c.status, gotMsg, c.message)
			}
		} else if gotMsg != "" {
			t.Errorf("%s message = %q, want empty", c.status, gotMsg)
		}
	}
}

// batchOps 在 fakeWorkspaceOps 上补两个可选扩展（命令通道 + 建账前附件校验），
// 用来证明 WorkspaceService 在**没有**这些扩展时仍然工作（兼容旧替身）。
type batchOps struct {
	*fakeWorkspaceOps
	running    bool
	pending    int
	commands   []CommandSessionReq
	commandErr error
	checked    []AttachmentReq
	checkErr   error
	checkFn    func(AttachmentReq) ([]AttachmentInfo, error)
}

func (b *batchOps) SessionStatus(context.Context, string, string) (SessionInfo, error) {
	return SessionInfo{ID: "s1", Workspace: "/w", Running: b.running, Pending: b.pending}, nil
}

func (b *batchOps) CommandSession(_ context.Context, req CommandSessionReq) error {
	if b.commandErr != nil {
		return b.commandErr
	}
	b.commands = append(b.commands, req)
	return nil
}

func (b *batchOps) CheckAttachments(_ context.Context, req AttachmentReq) ([]AttachmentInfo, error) {
	b.checked = append(b.checked, req)
	if b.checkFn != nil {
		return b.checkFn(req)
	}
	if b.checkErr != nil {
		return nil, b.checkErr
	}
	infos := make([]AttachmentInfo, 0, len(req.Files)+len(req.Images))
	for _, p := range req.Files {
		infos = append(infos, AttachmentInfo{Kind: AttachmentKindFile, Path: p, Name: filepath.Base(p), Bytes: 11})
	}
	for _, p := range req.Images {
		infos = append(infos, AttachmentInfo{Kind: AttachmentKindImage, Path: p, Name: filepath.Base(p), Bytes: 22})
	}
	return infos, nil
}

// 场景4：一批里的命令/文件/图片各自的白名单与互斥规则。
func TestWorkspaceServiceSendBatchValidation(t *testing.T) {
	ctx := context.Background()
	ops := &batchOps{fakeWorkspaceOps: &fakeWorkspaceOps{}}
	svc := NewWorkspaceService(ops, "ag_1")
	svc.NewTaskID = func() string { return "task_x" }

	many := func(prefix string) []string {
		out := make([]string, 0, MaxSessionAttachments+1)
		for i := 0; i <= MaxSessionAttachments; i++ {
			out = append(out, fmt.Sprintf("/tmp/%s-%d", prefix, i))
		}
		return out
	}
	cases := []struct {
		name string
		in   SendInput
	}{
		{"empty workspace", SendInput{SessionID: "s1", Text: "x"}},
		{"empty session", SendInput{Workspace: "/w", Text: "x"}},
		{"all empty", SendInput{Workspace: "/w", SessionID: "s1", Text: "   "}},
		{"unknown command", SendInput{Workspace: "/w", SessionID: "s1", Command: "nope"}},
		{"clear with text", SendInput{Workspace: "/w", SessionID: "s1", Command: CommandClear, Text: "x"}},
		{"clear with file", SendInput{Workspace: "/w", SessionID: "s1", Command: CommandClear, Files: []string{"/tmp/a.md"}}},
		{"clear with image", SendInput{Workspace: "/w", SessionID: "s1", Command: CommandClear, Images: []string{"/tmp/a.png"}}},
		{"skills without load", SendInput{Workspace: "/w", SessionID: "s1", Command: CommandSkills, CommandArgs: "pdf"}},
		{"skills two names", SendInput{Workspace: "/w", SessionID: "s1", Command: CommandSkills, CommandArgs: "load a b"}},
		{"compact with args", SendInput{Workspace: "/w", SessionID: "s1", Command: CommandCompact, CommandArgs: "x"}},
		{"args without command", SendInput{Workspace: "/w", SessionID: "s1", Text: "x", CommandArgs: "load pdf"}},
		{"relative file", SendInput{Workspace: "/w", SessionID: "s1", Files: []string{"a.md"}}},
		{"home alias file", SendInput{Workspace: "/w", SessionID: "s1", Files: []string{"~/.ssh/id_rsa"}}},
		{"too many files", SendInput{Workspace: "/w", SessionID: "s1", Files: many("f")}},
		{"too many images", SendInput{Workspace: "/w", SessionID: "s1", Images: many("i")}},
	}
	for _, c := range cases {
		if _, err := svc.SendBatch(ctx, c.in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s = %v, want ErrInvalid", c.name, err)
		}
	}
	if len(ops.dispatched) != 0 || len(ops.commands) != 0 || len(ops.checked) != 0 {
		t.Fatalf("validation must not reach ops: dispatched=%d commands=%d checked=%d",
			len(ops.dispatched), len(ops.commands), len(ops.checked))
	}
}

// 场景4：只有命令 → 不建账（task_id 为空、status=command_only），直接进 CommandSession。
func TestWorkspaceServiceSendBatchCommandOnly(t *testing.T) {
	ctx := context.Background()
	ops := &batchOps{fakeWorkspaceOps: &fakeWorkspaceOps{}}
	st := NewMemoryStore()
	q := NewQueue(NewLedger(st), ops)
	svc := &WorkspaceService{Ops: ops, AgentID: "ag_1", Queue: q}

	res, err := svc.SendBatch(ctx, SendInput{Workspace: " /w ", SessionID: " s1 ", Command: CommandCompact})
	if err != nil {
		t.Fatalf("command-only: %v", err)
	}
	if res.TaskID != "" || res.Status != StatusCommandOnly || !res.Accepted || res.Queued {
		t.Fatalf("command-only result = %+v; want empty task_id + command_only", res)
	}
	if res.Command == nil || !res.Command.Delivered || res.Command.Name != CommandCompact || res.Command.Args != "" {
		t.Fatalf("command receipt = %+v", res.Command)
	}
	if len(ops.commands) != 1 || ops.commands[0].Command != CommandCompact ||
		ops.commands[0].Workspace != "/w" || ops.commands[0].SessionID != "s1" || ops.commands[0].AgentID != "ag_1" {
		t.Fatalf("command session req = %+v", ops.commands)
	}
	if len(ops.dispatched) != 0 {
		t.Fatalf("command-only must not Dispatch: %+v", ops.dispatched)
	}
	// 不建 Task：账本里必须一条都没有（绝不编造一次性 task_id）。
	if tasks, err := st.ListTasks(ctx, TaskFilter{}); err != nil || len(tasks) != 0 {
		t.Fatalf("command-only created tasks = %+v, %v", tasks, err)
	}

	// skills 的 args 原样转给会话命令通道。
	if _, err := svc.SendBatch(ctx, SendInput{Workspace: "/w", SessionID: "s1", Command: CommandSkills, CommandArgs: "load pdf"}); err != nil {
		t.Fatalf("skills command: %v", err)
	}
	if ops.commands[len(ops.commands)-1].Args != "load pdf" {
		t.Fatalf("skills args = %q", ops.commands[len(ops.commands)-1].Args)
	}

	// Ops 没实现命令通道 → 明确 ErrInvalid（不是静默当成文本派发）。
	plain := NewWorkspaceService(&fakeWorkspaceOps{}, "ag_1")
	if _, err := plain.SendBatch(ctx, SendInput{Workspace: "/w", SessionID: "s1", Command: CommandCompact}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("command-only without CommandSession = %v, want ErrInvalid", err)
	}
}

// 场景4：命令 + 正文同批 → 命令存进 Task，泵送时先投命令再投正文（同一批）。
func TestWorkspaceServiceSendBatchCommandWithText(t *testing.T) {
	ctx := context.Background()
	target := TaskTarget{Kind: TargetWorkspace, Workspace: "/w", SessionID: "s1"}

	// 目标忙：queued，命令尚未投递。
	ops := &batchOps{fakeWorkspaceOps: &fakeWorkspaceOps{}, running: true}
	st := NewMemoryStore()
	q := NewQueue(NewLedger(st), ops)
	svc := &WorkspaceService{Ops: ops, AgentID: "ag_1", Queue: q}

	res, err := svc.SendBatch(ctx, SendInput{Workspace: "/w", SessionID: "s1", Text: " 做对比表 ", Command: CommandCompact})
	if err != nil {
		t.Fatalf("queued send: %v", err)
	}
	if !res.Queued || res.Status != string(TaskQueued) || res.TaskID == "" {
		t.Fatalf("queued result = %+v", res)
	}
	if res.Command == nil || res.Command.Delivered {
		t.Fatalf("queued command receipt = %+v; delivered must be false", res.Command)
	}
	queued, err := NewLedger(st).Replay(ctx, TaskID(res.TaskID))
	if err != nil || queued.Command != CommandCompact || queued.Instruction != "做对比表" {
		t.Fatalf("queued task = %+v, %v", queued, err)
	}

	// 目标空闲 → 泵送：DispatchReq 带命令，正文仍在。
	ops.running = false
	if _, err := q.Pump(ctx, target); err != nil {
		t.Fatalf("pump: %v", err)
	}
	if len(ops.dispatched) != 1 {
		t.Fatalf("dispatch calls = %d", len(ops.dispatched))
	}
	if got := ops.dispatched[0]; got.Command != CommandCompact || got.Text != "做对比表" || got.CommandArgs != "" {
		t.Fatalf("pumped dispatch req = %+v", got)
	}

	// 目标空闲时直接派发：命令已投递（delivered=true）。
	ops2 := &batchOps{fakeWorkspaceOps: &fakeWorkspaceOps{}}
	svc2 := &WorkspaceService{Ops: ops2, AgentID: "ag_1", Queue: NewQueue(NewLedger(NewMemoryStore()), ops2)}
	res2, err := svc2.SendBatch(ctx, SendInput{Workspace: "/w", SessionID: "s1", Text: "正文", Command: CommandCompact})
	if err != nil {
		t.Fatalf("idle send: %v", err)
	}
	if res2.Status != string(TaskDispatched) || res2.Command == nil || !res2.Command.Delivered {
		t.Fatalf("idle result = %+v / %+v", res2, res2.Command)
	}
}

// 场景4：标题在拼 refs 之前取；附件引用只拼路径，账本不留内容。
func TestWorkspaceServiceTaskTitleAndAttachments(t *testing.T) {
	ctx := context.Background()
	ops := &batchOps{fakeWorkspaceOps: &fakeWorkspaceOps{}, running: true}
	st := NewMemoryStore()
	q := NewQueue(NewLedger(st), ops)
	svc := &WorkspaceService{Ops: ops, AgentID: "ag_1", Queue: q}

	res, err := svc.SendBatch(ctx, SendInput{
		Workspace: "/w", SessionID: "s1", Text: "第一行\n第二行",
		Files: []string{"/a/x.md", "/a/x.md", "/a/y z.md"}, Images: []string{"/a/pic.png"},
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	task, err := NewLedger(st).Replay(ctx, TaskID(res.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "第一行" {
		t.Fatalf("title = %q; must be taken before file refs are appended", task.Title)
	}
	if task.Instruction != "第一行\n第二行\n@/a/x.md\n@/a/y%20z.md" {
		t.Fatalf("instruction = %q", task.Instruction)
	}
	if len(task.Attachments) != 3 {
		t.Fatalf("attachments = %+v", task.Attachments)
	}
	if a := task.Attachments[0]; a.Kind != AttachmentKindFile || a.Path != "/a/x.md" || a.Name != "x.md" || a.Bytes != 11 {
		t.Fatalf("file attachment = %+v", a)
	}
	if a := task.Attachments[2]; a.Kind != AttachmentKindImage || a.Path != "/a/pic.png" {
		t.Fatalf("image attachment = %+v", a)
	}

	// 只有文件 → 正文只有引用行，标题给占位。
	resFiles, err := svc.SendBatch(ctx, SendInput{Workspace: "/w", SessionID: "s1", Files: []string{"/a/only.md"}})
	if err != nil {
		t.Fatalf("files-only: %v", err)
	}
	taskFiles, _ := NewLedger(st).Replay(ctx, TaskID(resFiles.TaskID))
	if taskFiles.Title != SessionFileOnlyTitle || taskFiles.Instruction != "@/a/only.md" {
		t.Fatalf("files-only task = %+v", taskFiles)
	}

	// 只有图片 → 正文为空也合法（附件撑着任务），标题给占位。
	resImages, err := svc.SendBatch(ctx, SendInput{Workspace: "/w", SessionID: "s1", Images: []string{"/a/only.png"}})
	if err != nil {
		t.Fatalf("images-only: %v", err)
	}
	taskImages, _ := NewLedger(st).Replay(ctx, TaskID(resImages.TaskID))
	if taskImages.Title != SessionImageOnlyTitle || taskImages.Instruction != "" || len(taskImages.Attachments) != 1 {
		t.Fatalf("images-only task = %+v", taskImages)
	}

	// 执行边界拒绝附件 → 不落账、不派发。
	ops.checkErr = fmt.Errorf("%w: 不在 policy.workspace_roots 内", ErrForbidden)
	before, _ := st.ListTasks(ctx, TaskFilter{})
	if _, err := svc.SendBatch(ctx, SendInput{Workspace: "/w", SessionID: "s1", Files: []string{"/etc/passwd"}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("attachment check error = %v, want ErrForbidden", err)
	}
	after, _ := st.ListTasks(ctx, TaskFilter{})
	if len(after) != len(before) {
		t.Fatalf("rejected attachments created a task: %d -> %d", len(before), len(after))
	}
}

// 场景4：账本/任务事件里绝不能出现 data URL 或图片字节。
func TestLedgerTaskEventsCarryNoImagePayload(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ops := &batchOps{fakeWorkspaceOps: &fakeWorkspaceOps{}, running: true}
	q := NewQueue(NewLedger(st), ops)
	svc := &WorkspaceService{Ops: ops, AgentID: "ag_1", Queue: q}
	res, err := svc.SendBatch(ctx, SendInput{
		Workspace: "/w", SessionID: "s1", Text: "看三张图",
		Images: []string{"/a/1.png", "/a/2.png", "/a/3.png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.ReadTaskEvents(ctx, TaskID(res.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 100*1024 {
		t.Fatalf("task events = %d bytes, want < 100KB", len(raw))
	}
	for _, needle := range []string{"base64", "data:image", "iVBORw0KGgo"} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("task events contain %q: %s", needle, raw)
		}
	}
	task, err := st.LoadTask(ctx, TaskID(res.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	taskJSON, _ := json.Marshal(task)
	if strings.Contains(string(taskJSON), "base64") || strings.Contains(string(taskJSON), "data:image") {
		t.Fatalf("task snapshot contains image payload: %s", taskJSON)
	}
	if len(task.Attachments) != 3 || task.Attachments[1].Path != "/a/2.png" {
		t.Fatalf("attachments = %+v", task.Attachments)
	}
}

// 场景4：AppendFileRefs 的口径（%20、去重、正文已有引用、空正文）。
func TestAppendFileRefs(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		files []string
		want  string
	}{
		{"single", "看看", []string{"/ws/a.md"}, "看看\n@/ws/a.md"},
		{"space escaped", "看看", []string{"/ws/a b.md"}, "看看\n@/ws/a%20b.md"},
		{"dedupe", "看看", []string{"/ws/a.md", "/ws/a.md"}, "看看\n@/ws/a.md"},
		{"already referenced", "看看\n@/ws/a.md", []string{"/ws/a.md"}, "看看\n@/ws/a.md"},
		{"already referenced escaped", "看看\n@/ws/a%20b.md", []string{"/ws/a b.md"}, "看看\n@/ws/a%20b.md"},
		{"token boundary", "@/ws/a.md.bak", []string{"/ws/a.md"}, "@/ws/a.md.bak\n@/ws/a.md"},
		{"empty text", "   ", []string{"/ws/a.md", "/ws/b.md"}, "@/ws/a.md\n@/ws/b.md"},
		{"trailing newline", "看看\n", []string{"/ws/a.md"}, "看看\n@/ws/a.md"},
		{"no files", "看看", nil, "看看"},
	}
	for _, c := range cases {
		if got := AppendFileRefs(c.text, c.files); got != c.want {
			t.Errorf("%s = %q, want %q", c.name, got, c.want)
		}
	}
}

// 兼容：Ops 没实现 AttachmentChecker（旧替身/只读实现）时附件仍能派发，
// 账本至少落路径/名称/字节数（路径校验由执行边界在派发时做）。
func TestWorkspaceServiceAttachmentsWithoutChecker(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	file := filepath.Join(dir, "a.md")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := &fakeWorkspaceOps{}
	svc := NewWorkspaceService(ops, "ag_1")
	svc.NewTaskID = func() string { return "task_nc" }
	res, err := svc.SendBatch(ctx, SendInput{Workspace: "/w", SessionID: "s1", Files: []string{file}})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.TaskID != "task_nc" || len(ops.dispatched) != 1 {
		t.Fatalf("result = %+v dispatched = %+v", res, ops.dispatched)
	}
	if got := ops.dispatched[0]; got.Text != "@"+file || len(got.Files) != 1 || got.Files[0] != file {
		t.Fatalf("dispatch req = %+v", got)
	}

	st := NewMemoryStore()
	svc2 := &WorkspaceService{Ops: ops, AgentID: "ag_1", Queue: NewQueue(NewLedger(st), ops)}
	res2, err := svc2.SendBatch(ctx, SendInput{Workspace: "/w", SessionID: "s1", Text: "看图", Images: []string{"/w/i.png"}})
	if err != nil {
		t.Fatalf("queued send: %v", err)
	}
	task, err := NewLedger(st).Replay(ctx, TaskID(res2.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Attachments) != 1 || task.Attachments[0].Kind != AttachmentKindImage ||
		task.Attachments[0].Name != "i.png" || task.Attachments[0].Bytes != 0 {
		t.Fatalf("fallback attachment = %+v", task.Attachments)
	}
}
