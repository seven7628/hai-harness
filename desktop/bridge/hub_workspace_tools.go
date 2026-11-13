package main

// hub_workspace_tools.go：Hub 智能体的工作区/会话工具（方案 §6.6 / §7.5，P2-02）。
//
// 五个工具是同一个能力面的不同粒度：
//   workspace_list  → 找到目标工作区（必须先做）
//   workspace_create→ 在 policy.workspace_roots 下新建并登记
//   session_list    → 找到目标项目会话（send 前必须先做）
//   session_create  → 在工作区里开新会话
//   session_send    → 把任务派进已存在的项目会话
//
// 工具层只解析参数 + 渲染 JSON；校验/路径安全在 agenthub.WorkspaceService 与
// bridge 的 hubWorkspaceOps（唯一执行入口，智能体不能直接读写工作区文件）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/tools"
)

func registerHubWorkspaceTools(eng *tools.Engine, svc *agenthub.WorkspaceService, alert func(code, message string)) {
	if eng == nil || svc == nil {
		return
	}
	send := newHubSessionSendTool(svc)
	send.alert = alert
	for _, tl := range []tools.Tool{
		newHubWorkspaceListTool(svc),
		newHubWorkspaceCreateTool(svc),
		newHubSessionListTool(svc),
		newHubSessionCreateTool(svc),
		send,
	} {
		eng.RegisterTool(context.Background(), tl)
	}
}

type hubWorkspaceListTool struct {
	tools.BaseTool
	svc *agenthub.WorkspaceService
}

func newHubWorkspaceListTool(svc *agenthub.WorkspaceService) *hubWorkspaceListTool {
	return &hubWorkspaceListTool{
		BaseTool: tools.BaseTool{
			Name_: "workspace_list",
			Description_: "List the project workspaces registered on this machine (path + name). " +
				"Call this first to find a real workspace path for session_list / session_send. " +
				"This agent's own home is not a workspace.",
			Params_:      tools.Obj(map[string]any{}),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		svc: svc,
	}
}

func (t *hubWorkspaceListTool) ValidParams(_ context.Context, _, arguments string) error {
	if arguments == "" || arguments == "null" {
		return nil
	}
	var a map[string]any
	return json.Unmarshal([]byte(arguments), &a)
}

func (t *hubWorkspaceListTool) Call(ctx context.Context, _, _ string) (string, error) {
	list, err := t.svc.ListWorkspaces(ctx)
	if err != nil {
		return "", err
	}
	if list == nil {
		list = []agenthub.WorkspaceInfo{}
	}
	return marshalHubToolResult(map[string]any{"workspaces": list})
}

type hubWorkspaceCreateTool struct {
	tools.BaseTool
	svc *agenthub.WorkspaceService
}

func newHubWorkspaceCreateTool(svc *agenthub.WorkspaceService) *hubWorkspaceCreateTool {
	return &hubWorkspaceCreateTool{
		BaseTool: tools.BaseTool{
			Name_: "workspace_create",
			Description_: "Create a new project workspace directory under this agent's policy.workspace_roots, then register it in workspaces.json. " +
				"parent is optional (defaults to the first policy.workspace_roots entry) and must be an existing allowed root or a subdirectory of one; " +
				"name is a single directory name (no separators or '..'); an existing directory is rejected instead of being taken over.",
			Params_: tools.Obj(map[string]any{
				"parent": tools.Str("Optional existing parent directory inside policy.workspace_roots (default: first allowed root)"),
				"name":   tools.Str("New directory name (single path segment, max 64 chars)"),
			}, "name"),
			CanParallel_: false,
			ReadOnly_:    false,
		},
		svc: svc,
	}
}

type hubWorkspaceCreateArgs struct {
	Parent string `json:"parent"`
	Name   string `json:"name"`
}

func (t *hubWorkspaceCreateTool) ValidParams(_ context.Context, _, arguments string) error {
	var a hubWorkspaceCreateArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if a.Name == "" {
		return fmt.Errorf("workspace_create: name 不能为空")
	}
	return nil
}

func (t *hubWorkspaceCreateTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a hubWorkspaceCreateArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	info, err := t.svc.CreateWorkspace(ctx, a.Parent, a.Name)
	if err != nil {
		return "", err
	}
	return marshalHubToolResult(map[string]any{"workspace": info})
}

type hubSessionListTool struct {
	tools.BaseTool
	svc *agenthub.WorkspaceService
}

func newHubSessionListTool(svc *agenthub.WorkspaceService) *hubSessionListTool {
	return &hubSessionListTool{
		BaseTool: tools.BaseTool{
			Name_: "session_list",
			Description_: "List sessions in a project workspace (id, title, running). " +
				"Call workspace_list first to get a valid workspace path, then use this to find the target session for session_send. " +
				"Hub agent home is not a valid workspace.",
			Params_: tools.Obj(map[string]any{
				"workspace": tools.Str("Workspace path from workspace_list"),
			}, "workspace"),
			CanParallel_: true,
			ReadOnly_:    true,
		},
		svc: svc,
	}
}

type hubSessionListArgs struct {
	Workspace string `json:"workspace"`
}

func (t *hubSessionListTool) ValidParams(_ context.Context, _, arguments string) error {
	var a hubSessionListArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if a.Workspace == "" {
		return fmt.Errorf("session_list: workspace 不能为空（先 workspace_list）")
	}
	return nil
}

func (t *hubSessionListTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a hubSessionListArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	list, err := t.svc.ListSessions(ctx, a.Workspace)
	if err != nil {
		return "", err
	}
	if list == nil {
		list = []agenthub.SessionInfo{}
	}
	return marshalHubToolResult(map[string]any{"sessions": list})
}

type hubSessionCreateTool struct {
	tools.BaseTool
	svc *agenthub.WorkspaceService
}

func newHubSessionCreateTool(svc *agenthub.WorkspaceService) *hubSessionCreateTool {
	return &hubSessionCreateTool{
		BaseTool: tools.BaseTool{
			Name_: "session_create",
			Description_: "Create a new session in a project workspace. The session uses the current global active provider/model, " +
				"but this tool never changes the global provider configuration. Call workspace_list first for a valid workspace path.",
			Params_: tools.Obj(map[string]any{
				"workspace": tools.Str("Workspace path from workspace_list"),
				"title":     tools.Str("Optional session title"),
			}, "workspace"),
			CanParallel_: false,
			ReadOnly_:    false,
		},
		svc: svc,
	}
}

type hubSessionCreateArgs struct {
	Workspace string `json:"workspace"`
	Title     string `json:"title"`
}

func (t *hubSessionCreateTool) ValidParams(_ context.Context, _, arguments string) error {
	var a hubSessionCreateArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if a.Workspace == "" {
		return fmt.Errorf("session_create: workspace 不能为空（先 workspace_list）")
	}
	return nil
}

func (t *hubSessionCreateTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a hubSessionCreateArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	info, err := t.svc.CreateSession(ctx, a.Workspace, a.Title)
	if err != nil {
		return "", err
	}
	return marshalHubToolResult(map[string]any{"session": info})
}

type hubSessionSendTool struct {
	tools.BaseTool
	svc *agenthub.WorkspaceService
	// alert 护栏拒绝时发 hub_alert（熔断要可见，不能只回给模型）。
	alert func(code, message string)
}

func newHubSessionSendTool(svc *agenthub.WorkspaceService) *hubSessionSendTool {
	return &hubSessionSendTool{
		BaseTool: tools.BaseTool{
			Name_: "session_send",
			Description_: "Send one batch to an existing project session: text, a slash-command (compact|clear|skills), file references and/or images. " +
				"First call workspace_list and session_list to find the target; the target must be a session inside a registered project workspace " +
				"(never this agent's own home). At least one of text/command/files/images is required. " +
				"command=compact compresses the context, clear resets it (cannot be combined with text/files/images), skills needs command_args=\"load <name>\". " +
				"The command runs in the SAME batch right before the text (FIFO), so clear/compact happen before the project session reads the message. " +
				"files are ONLY `@path` references appended to the text (absolute paths, <=10, content is not read here; the project session reads them itself). " +
				"images are absolute paths that are read and embedded as image blocks (<=10, <=3MiB total); never put data URLs in text. " +
				"Returns task_id (ledger id) and session_id; queued=true means the target is busy and the task waits in the Hub queue (it has NOT started yet, " +
				"command.delivered=false). command-only calls (no text/files/images) return status=command_only with an empty task_id: the command is " +
				"enqueued directly and no task is created. accepted=false + status=canceled/failed/interrupted/blocked_budget/declined means the task " +
				"was NOT delivered; read message and call task_status before retrying. This only dispatches — it does not read or write workspace files directly.",
			Params_: tools.Obj(map[string]any{
				"workspace":     tools.Str("Workspace path from workspace_list"),
				"session_id":    tools.Str("Session id from session_list"),
				"text":          tools.Str("Task text for the project session (may be empty when files/images/command carry the request)"),
				"command":       tools.Str("Optional built-in command to run in the target session before the text: compact | clear | skills"),
				"command_args":  tools.Str("Arguments for command: only skills uses this, in the form \"load <name>\""),
				"files":         tools.ArrayStr("Optional absolute file paths to reference as `@path` in the text (max 10). Only the reference is appended; the file content is not sent"),
				"images":        tools.ArrayStr("Optional absolute image paths to embed for the project session (max 10, png/jpeg/gif, <=3MiB total)"),
				"client_msg_id": tools.Str("Optional idempotency key: a retry with the same id returns the same logical task; failed/canceled/interrupted retries get a NEW task_id (new attempt), completed ones are not re-run"),
			}, "workspace", "session_id"),
			CanParallel_: false,
			ReadOnly_:    false,
		},
		svc: svc,
	}
}

// hubSessionSendArgs 是 session_send 的一批参数（text/command/files/images 至少一个）。
type hubSessionSendArgs struct {
	Workspace   string   `json:"workspace"`
	SessionID   string   `json:"session_id"`
	Text        string   `json:"text"`
	Command     string   `json:"command"`
	CommandArgs string   `json:"command_args"`
	Files       []string `json:"files"`
	Images      []string `json:"images"`
	ClientMsgID string   `json:"client_msg_id"`
}

// input 转成服务层输入（幂等键原样透传，其余字段由服务层 TrimSpace/规范化）。
func (a hubSessionSendArgs) input() agenthub.SendInput {
	return agenthub.SendInput{
		Workspace: a.Workspace, SessionID: a.SessionID, Text: a.Text,
		Command: a.Command, CommandArgs: a.CommandArgs,
		Files: a.Files, Images: a.Images, ClientMsgID: a.ClientMsgID,
	}
}

// validateSessionSendArgs 工具层校验：命令白名单/同批矛盾/附件数量与绝对路径。
// 服务层会再校验一遍（工具层先给出可恢复的明确拒绝，别让模型一路调到执行边界）。
func validateSessionSendArgs(a hubSessionSendArgs) error {
	if strings.TrimSpace(a.Workspace) == "" || strings.TrimSpace(a.SessionID) == "" {
		return fmt.Errorf("session_send: workspace 与 session_id 不能为空（先 workspace_list / session_list）")
	}
	if err := agenthub.ValidateSessionCommand(a.Command, a.CommandArgs); err != nil {
		return fmt.Errorf("session_send: %v", err)
	}
	files, err := agenthub.CleanAttachmentPaths("files", a.Files)
	if err != nil {
		return fmt.Errorf("session_send: %v", err)
	}
	images, err := agenthub.CleanAttachmentPaths("images", a.Images)
	if err != nil {
		return fmt.Errorf("session_send: %v", err)
	}
	text := strings.TrimSpace(a.Text)
	command := strings.TrimSpace(a.Command)
	// clear 会清空上下文（含同一批的正文/附件）：同批出现是自相矛盾的请求。
	if command == agenthub.CommandClear && (text != "" || len(files) > 0 || len(images) > 0) {
		return fmt.Errorf("session_send: command=clear 不能与 text/files/images 同批（clear 会清空同批正文）")
	}
	if command == "" && text == "" && len(files) == 0 && len(images) == 0 {
		return fmt.Errorf("session_send: text/command/files/images 至少要给一个")
	}
	return nil
}

func (t *hubSessionSendTool) ValidParams(_ context.Context, _, arguments string) error {
	var a hubSessionSendArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	return validateSessionSendArgs(a)
}

func (t *hubSessionSendTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a hubSessionSendArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	res, err := t.svc.SendBatch(ctx, a.input())
	if err != nil {
		if t.alert != nil {
			code := "dispatch_denied"
			switch {
			case errors.Is(err, agenthub.ErrBudgetExceeded):
				code = "blocked_budget"
			case errors.Is(err, agenthub.ErrTooConcurrent):
				code = "concurrency_limit"
			case errors.Is(err, agenthub.ErrDuplicateTask):
				code = "duplicate_task"
			case errors.Is(err, agenthub.ErrChainTooDeep):
				code = "chain_too_deep"
			}
			t.alert(code, err.Error())
		}
		return "", err
	}
	out := map[string]any{
		"task_id": res.TaskID, "session_id": res.SessionID,
		"accepted": res.Accepted, "status": res.Status, "queued": res.Queued,
		"message": res.Message,
	}
	// 只有带命令的批次才多这个键：旧的纯文本调用返回的键/值与之前完全一致。
	if res.Command != nil {
		out["command"] = res.Command
	}
	return marshalHubToolResult(out)
}

func marshalHubToolResult(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
