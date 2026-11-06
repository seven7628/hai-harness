package agenthub

// service.go：Hub 对「工作区/会话」的服务层（方案 §6.6 / §7.5）。
//
// 工具层（desktop/bridge/hub_workspace_tools.go）只做参数解析与 JSON 渲染，
// 真正的校验/派发都走这里：可以在 agenthub 单测里用假 WorkspaceOps 跑起来，
// 不必起 bridge。P2-03 的任务账本会在这里绑定 task_id（现在先返回一次性 id）。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// SendResult session_send 的返回：task_id 供后续 task_status/task_result 引用。
type SendResult struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	Accepted  bool   `json:"accepted"`
	Status    string `json:"status,omitempty"` // queued | dispatched（P2-03 账本状态）
	Queued    bool   `json:"queued,omitempty"` // true = 目标忙，任务在 Hub 队列里
	Message   string `json:"message,omitempty"`
}

// WorkspaceService 是工具层的能力面（组合 WorkspaceOps，注入 AgentID）。
type WorkspaceService struct {
	Ops     WorkspaceOps
	AgentID AgentID
	// Queue 非空 = 任务走账本/空闲闸门（P2-03）；nil = 直接派发（P2-02 兼容）。
	Queue *Queue
	// Guard 非空 = 建账前校验预算/并发/去重/链长（P2-07）。
	Guard *Guard
	// Origin 任务来源（DM / 群）；零值按 DM + AgentID 处理。
	Origin TaskOrigin
	// NewTaskID 可注入（无 Queue 时的一次性 id）；nil = 进程内随机 task_<hex>。
	NewTaskID func() string
}

func NewWorkspaceService(ops WorkspaceOps, agentID AgentID) *WorkspaceService {
	return &WorkspaceService{Ops: ops, AgentID: agentID}
}

func (s *WorkspaceService) ops() (WorkspaceOps, error) {
	if s == nil || s.Ops == nil {
		return nil, fmt.Errorf("%w: workspace ops 未配置", ErrInvalid)
	}
	return s.Ops, nil
}

// ListWorkspaces 列出可派发的项目工作区。
func (s *WorkspaceService) ListWorkspaces(ctx context.Context) ([]WorkspaceInfo, error) {
	ops, err := s.ops()
	if err != nil {
		return nil, err
	}
	return ops.ListWorkspaces(ctx)
}

// CreateWorkspace 在 workspace_roots 下新建工作区并登记。
func (s *WorkspaceService) CreateWorkspace(ctx context.Context, parent, name string) (WorkspaceInfo, error) {
	ops, err := s.ops()
	if err != nil {
		return WorkspaceInfo{}, err
	}
	parent = strings.TrimSpace(parent)
	name = strings.TrimSpace(name)
	if name == "" {
		return WorkspaceInfo{}, fmt.Errorf("%w: name 不能为空", ErrInvalid)
	}
	return ops.CreateWorkspace(ctx, CreateWorkspaceReq{AgentID: s.AgentID, Parent: parent, Name: name})
}

// ListSessions 列出工作区里的会话（调用前应先用 workspace_list 找到工作区）。
func (s *WorkspaceService) ListSessions(ctx context.Context, workspace string) ([]SessionInfo, error) {
	ops, err := s.ops()
	if err != nil {
		return nil, err
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return nil, fmt.Errorf("%w: workspace 不能为空（先 workspace_list 再 session_list）", ErrInvalid)
	}
	return ops.ListSessions(ctx, workspace)
}

// CreateSession 在工作区里开一个项目会话。
func (s *WorkspaceService) CreateSession(ctx context.Context, workspace, title string) (SessionInfo, error) {
	ops, err := s.ops()
	if err != nil {
		return SessionInfo{}, err
	}
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return SessionInfo{}, fmt.Errorf("%w: workspace 不能为空（先 workspace_list 再 session_create）", ErrInvalid)
	}
	return ops.CreateSession(ctx, CreateSessionReq{
		AgentID: s.AgentID, Workspace: workspace, Title: strings.TrimSpace(title),
	})
}

// Send 把任务派给项目会话。目标必须是已登记项目工作区里的会话（WorkspaceOps 负责校验）。
func (s *WorkspaceService) Send(ctx context.Context, workspace, sessionID, text, clientMsgID string) (SendResult, error) {
	ops, err := s.ops()
	if err != nil {
		return SendResult{}, err
	}
	workspace = strings.TrimSpace(workspace)
	sessionID = strings.TrimSpace(sessionID)
	text = strings.TrimSpace(text)
	if workspace == "" {
		return SendResult{}, fmt.Errorf("%w: workspace 不能为空（先 session_list 再 session_send）", ErrInvalid)
	}
	if sessionID == "" {
		return SendResult{}, fmt.Errorf("%w: session_id 不能为空（先 session_list 再 session_send）", ErrInvalid)
	}
	if text == "" {
		return SendResult{}, fmt.Errorf("%w: text 不能为空", ErrInvalid)
	}
	if s.Queue != nil {
		origin := s.Origin
		if origin.Kind == "" {
			origin = TaskOrigin{Kind: OriginDM, AgentID: s.AgentID}
		}
		if origin.AgentID == "" {
			origin.AgentID = s.AgentID
		}
		t := &Task{
			Origin:      origin,
			Target:      TaskTarget{Kind: TargetWorkspace, Workspace: workspace, SessionID: sessionID},
			Title:       firstLine(text),
			Instruction: text,
			Callback:    CallbackAgentEnd,
			ClientMsgID: clientMsgID, // 原样透传：同一 id 重试返回同一账本任务
		}
		created, err := s.Queue.SubmitGuarded(ctx, t, s.Guard)
		if err != nil {
			return SendResult{}, err
		}
		return SendResult{
			TaskID:    string(created.ID),
			SessionID: created.Target.SessionID,
			Accepted:  true,
			Status:    string(created.Status),
			Queued:    created.Status == TaskQueued,
		}, nil
	}
	res, err := ops.Dispatch(ctx, DispatchReq{
		AgentID: s.AgentID, Workspace: workspace, SessionID: sessionID,
		Text: text, ClientMsgID: clientMsgID, // 原样透传：幂等键的空白也是键的一部分
	})
	if err != nil {
		return SendResult{}, err
	}
	taskID := ""
	if s.NewTaskID != nil {
		taskID = s.NewTaskID()
	} else {
		taskID = defaultTaskID()
	}
	return SendResult{
		TaskID: taskID, SessionID: res.SessionID, Accepted: res.Accepted, Message: res.Message,
	}, nil
}

// firstLine 取任务标题（首行，截断到 80 rune）。
func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	r := []rune(strings.TrimSpace(text))
	if len(r) > 80 {
		r = r[:80]
	}
	return string(r)
}

// defaultTaskID 生成一次性任务 id（P2-03 起改由账本分配并持久化）。
func defaultTaskID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 常量 fallback 会让两次派发拿到同一个 task_id；用纳秒兜底保持唯一。
		return fmt.Sprintf("task_%d", time.Now().UnixNano())
	}
	return "task_" + hex.EncodeToString(b[:])
}
