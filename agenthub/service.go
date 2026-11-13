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
	"os"
	"path/filepath"
	"strings"
	"time"
)

// session_send 的命令名（生产环境只有这三条内置命令，没有自定义命令）。
const (
	CommandCompact = "compact"
	CommandClear   = "clear"
	CommandSkills  = "skills"

	// StatusCommandOnly 只有命令、没有正文/附件时的返回状态：不建账本任务，
	// 因此没有 task_id 可给（绝不编造一次性 id 让模型误以为有任务在跑）。
	StatusCommandOnly = "command_only"

	// MaxSessionAttachments 单批 files / images 各自的数量上限（与消息附件同一口径）。
	MaxSessionAttachments = MaxMessageAttachments
	// MaxSessionImageBytes 单批图片 data URL 的总量上限（3MiB，与引擎内嵌图片闸门
	// 及消息附件同一口径）。
	MaxSessionImageBytes = MaxAttachmentImageBytes

	// SessionImageOnlyTitle / SessionFileOnlyTitle：正文为空时任务标题的占位
	// （与 Hub DM 纯图/纯文件消息的占位同口径）。
	SessionImageOnlyTitle = "[图片]"
	SessionFileOnlyTitle  = "[文件]"
)

// SendResult session_send 的返回：task_id 供后续 task_status/task_result 引用。
type SendResult struct {
	TaskID    string `json:"task_id"`
	SessionID string `json:"session_id"`
	Accepted  bool   `json:"accepted"`
	Status    string `json:"status,omitempty"` // queued | dispatched | command_only
	Queued    bool   `json:"queued,omitempty"` // true = 目标忙，任务在 Hub 队列里
	Message   string `json:"message,omitempty"`
	// Command 非空 = 本批带了一条 SDK 命令；Delivered=false 表示命令还没进目标
	// 会话（queued 时任务仍在队列里，泵送时才执行）。
	Command *SendCommand `json:"command,omitempty"`
}

// SendCommand 是批内命令的回执（name/args + 是否已真正投递）。
type SendCommand struct {
	Name      string `json:"name"`
	Args      string `json:"args,omitempty"`
	Delivered bool   `json:"delivered"`
}

// SendInput 是 session_send 的一批输入：正文 / 命令 / 文件引用 / 图片。
//
// 一批 = 一次投递：命令先进目标会话的 inbox，正文随后入队（FIFO），目标会话在
// 同一轮里先消费命令、再看正文。只有命令（无正文/附件）时不是任务，走
// CommandSession 直接投递（见 SendBatch）。
type SendInput struct {
	Workspace   string
	SessionID   string
	Text        string
	Command     string
	CommandArgs string
	Files       []string // 绝对路径：只拼 `@路径` 到正文，不读内容
	Images      []string // 绝对路径：派发时读成 image 内容块
	ClientMsgID string
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

// Send 是 session_send 的旧签名（只发正文）：等价于 SendBatch 里只给 text。
// 保留它是为了不破坏既有调用方与测试；新代码用 SendBatch。
func (s *WorkspaceService) Send(ctx context.Context, workspace, sessionID, text, clientMsgID string) (SendResult, error) {
	return s.SendBatch(ctx, SendInput{
		Workspace: workspace, SessionID: sessionID, Text: text, ClientMsgID: clientMsgID,
	})
}

// SendBatch 把一批输入（正文/命令/文件引用/图片）派给项目会话。
//
// 三条分支：
//   - 只有命令（无正文/附件）→ 不是任务：绕过队列与账本，直接 CommandSession；
//     返回 status=command_only、task_id 为空（不编造一次性 id）；
//   - 有正文/附件 → 走既有 queue/账本（目标忙则 queued，泵送时命令先于正文投递）；
//   - 无 Queue（P2-02 兼容）→ 直接 Dispatch，命令与正文同一次投递。
//
// 附件与命令的校验都发生在**建账之前**：附件由执行边界（WorkspaceOps 的可选
// AttachmentChecker）校验路径白名单/图片闸门，失败即不落账、不派发、不占去重表。
func (s *WorkspaceService) SendBatch(ctx context.Context, in SendInput) (SendResult, error) {
	ops, err := s.ops()
	if err != nil {
		return SendResult{}, err
	}
	in, err = in.normalized()
	if err != nil {
		return SendResult{}, err
	}
	hasBody := in.Text != "" || len(in.Files) > 0 || len(in.Images) > 0
	if in.Command != "" && !hasBody {
		return s.sendCommandOnly(ctx, ops, in)
	}
	attachments, err := s.checkAttachments(ctx, ops, in)
	if err != nil {
		return SendResult{}, err
	}
	// 文件引用用**执行边界归一后**的真实路径：账本、正文、以及后续泵送时
	// DispatchReq.Files 三处必须是同一串路径。否则 `/tmp` 与 `/private/tmp`
	// 这类软链接差异会让泵送时再拼一遍同一文件的另一个写法（重复引用）。
	filePaths := attachmentPaths(attachments, AttachmentKindFile)
	if len(filePaths) == 0 {
		filePaths = in.Files
	}
	// 标题必须在拼文件引用**之前**取：否则只有附件时标题会变成一串 `@/path`。
	title := sessionTaskTitle(in.Text, filePaths, in.Images)
	instruction := AppendFileRefs(in.Text, filePaths)
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
			Target:      TaskTarget{Kind: TargetWorkspace, Workspace: in.Workspace, SessionID: in.SessionID},
			Title:       title,
			Instruction: instruction,
			Command:     in.Command,
			CommandArgs: in.CommandArgs,
			Attachments: attachments,
			Callback:    CallbackAgentEnd,
			ClientMsgID: in.ClientMsgID, // 原样透传：同一 id 重试返回同一账本任务
		}
		created, err := s.Queue.SubmitGuarded(ctx, t, s.Guard)
		if err != nil {
			return SendResult{}, err
		}
		return SendResult{
			TaskID:    string(created.ID),
			SessionID: created.Target.SessionID,
			Accepted:  sendResultAccepted(created),
			Status:    string(created.Status),
			Queued:    created.Status == TaskQueued,
			Message:   sendResultMessage(created),
			Command:   sendCommandResult(created.Command, created.CommandArgs, commandDelivered(created)),
		}, nil
	}
	res, err := ops.Dispatch(ctx, DispatchReq{
		AgentID: s.AgentID, Workspace: in.Workspace, SessionID: in.SessionID,
		Text: instruction, ClientMsgID: in.ClientMsgID, // 原样透传：幂等键的空白也是键的一部分
		Command: in.Command, CommandArgs: in.CommandArgs,
		Files: filePaths, Images: in.Images,
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
		Command: sendCommandResult(in.Command, in.CommandArgs, in.Command != ""),
	}, nil
}

// sendCommandOnly 只发命令：不建账、不过队列。
func (s *WorkspaceService) sendCommandOnly(ctx context.Context, ops WorkspaceOps, in SendInput) (SendResult, error) {
	cs, ok := ops.(CommandSessionOps)
	if !ok {
		return SendResult{}, fmt.Errorf("%w: 只发命令需要目标工作区支持 CommandSession", ErrInvalid)
	}
	if err := cs.CommandSession(ctx, CommandSessionReq{
		AgentID: s.AgentID, Workspace: in.Workspace, SessionID: in.SessionID,
		Command: in.Command, Args: in.CommandArgs,
	}); err != nil {
		return SendResult{}, err
	}
	return SendResult{
		SessionID: in.SessionID,
		Accepted:  true,
		Status:    StatusCommandOnly,
		Command:   sendCommandResult(in.Command, in.CommandArgs, true),
	}, nil
}

// checkAttachments 在建账前把附件路径交给执行边界校验，并落成账本可存的引用。
func (s *WorkspaceService) checkAttachments(ctx context.Context, ops WorkspaceOps, in SendInput) ([]TaskAttachment, error) {
	if len(in.Files) == 0 && len(in.Images) == 0 {
		return nil, nil
	}
	if checker, ok := ops.(AttachmentChecker); ok {
		infos, err := checker.CheckAttachments(ctx, AttachmentReq{
			AgentID: s.AgentID, Workspace: in.Workspace, Files: in.Files, Images: in.Images,
		})
		if err != nil {
			return nil, err
		}
		out := make([]TaskAttachment, 0, len(infos))
		for _, a := range infos {
			out = append(out, TaskAttachment{Kind: a.Kind, Path: a.Path, Name: a.Name, Bytes: a.Bytes})
		}
		return out, nil
	}
	// 没有执行边界可用（测试替身/无 policy 的实现）：至少落路径/名称/字节数，
	// 路径本身仍由 Dispatch 侧校验。
	out := make([]TaskAttachment, 0, len(in.Files)+len(in.Images))
	for _, p := range in.Files {
		out = append(out, taskAttachmentFor(AttachmentKindFile, p))
	}
	for _, p := range in.Images {
		out = append(out, taskAttachmentFor(AttachmentKindImage, p))
	}
	return out, nil
}

// taskAttachmentFor 构造一条附件引用（字节数取文件实际大小，失败记 0 = 未知）。
func taskAttachmentFor(kind, path string) TaskAttachment {
	a := TaskAttachment{Kind: kind, Path: path, Name: filepath.Base(path)}
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		a.Bytes = st.Size()
	}
	return a
}

// sendCommandResult 命令回执（无命令返回 nil：旧的纯文本调用返回键/值完全不变）。
func sendCommandResult(command, args string, delivered bool) *SendCommand {
	if command == "" {
		return nil
	}
	return &SendCommand{Name: command, Args: args, Delivered: delivered}
}

// commandDelivered 命令是否真的进了目标会话：只有 dispatched 之后的状态才算。
// queued（目标忙）/canceled/failed 都不算 —— 别让模型以为 clear/compact 已经生效。
func commandDelivered(t *Task) bool {
	if t == nil || t.Command == "" {
		return false
	}
	switch t.Status {
	case TaskDispatched, TaskRunning, TaskCompleted, TaskDelivered:
		return true
	}
	return false
}

// normalized 规范化一批输入并做与执行边界无关的校验（幂等键原样保留）。
func (in SendInput) normalized() (SendInput, error) {
	in.Workspace = strings.TrimSpace(in.Workspace)
	in.SessionID = strings.TrimSpace(in.SessionID)
	in.Text = strings.TrimSpace(in.Text)
	in.Command = strings.TrimSpace(in.Command)
	in.CommandArgs = strings.TrimSpace(in.CommandArgs)
	if in.Workspace == "" {
		return in, fmt.Errorf("%w: workspace 不能为空（先 session_list 再 session_send）", ErrInvalid)
	}
	if in.SessionID == "" {
		return in, fmt.Errorf("%w: session_id 不能为空（先 session_list 再 session_send）", ErrInvalid)
	}
	if err := ValidateSessionCommand(in.Command, in.CommandArgs); err != nil {
		return in, err
	}
	files, err := CleanAttachmentPaths("files", in.Files)
	if err != nil {
		return in, err
	}
	images, err := CleanAttachmentPaths("images", in.Images)
	if err != nil {
		return in, err
	}
	in.Files, in.Images = files, images
	if in.Command == "" && in.Text == "" && len(files) == 0 && len(images) == 0 {
		return in, fmt.Errorf("%w: text/command/files/images 至少要给一个", ErrInvalid)
	}
	// clear 会清空整个上下文（含同一批的正文/附件）：同批出现是自相矛盾的请求。
	if in.Command == CommandClear && (in.Text != "" || len(files) > 0 || len(images) > 0) {
		return in, fmt.Errorf("%w: command=clear 不能与 text/files/images 同批（clear 会清空同批正文）", ErrInvalid)
	}
	return in, nil
}

// ValidateSessionCommand 校验 session_send 的 command/command_args。
//
// 生产只有三条内置命令（自定义命令由宿主注册，工具层不暴露名字 —— 让模型自由
// 拼 command 会把会话状态改成不可预测的形状）。skills 的形态固定为
// `command_args="load <name>"`（Session.Command 原样转发 args）。
func ValidateSessionCommand(command, args string) error {
	command = strings.TrimSpace(command)
	args = strings.TrimSpace(args)
	switch command {
	case "":
		if args != "" {
			return fmt.Errorf("%w: command_args 必须与 command 一起给", ErrInvalid)
		}
		return nil
	case CommandCompact, CommandClear:
		if args != "" {
			return fmt.Errorf("%w: command=%s 不接受 command_args", ErrInvalid, command)
		}
		return nil
	case CommandSkills:
		rest, ok := strings.CutPrefix(args, "load ")
		if !ok || strings.TrimSpace(rest) == "" {
			return fmt.Errorf("%w: command=skills 需要 command_args=\"load <name>\"", ErrInvalid)
		}
		if len(strings.Fields(rest)) != 1 {
			return fmt.Errorf("%w: skills 一次只加载一个技能（command_args=\"load <name>\"）", ErrInvalid)
		}
		return nil
	default:
		return fmt.Errorf("%w: 未知 command %q（可选 compact/clear/skills）", ErrInvalid, command)
	}
}

// CleanAttachmentPaths 规范一批附件路径：去空白、去重（保序）、必须是绝对路径、
// 数量不超过 MaxSessionAttachments。相对路径一律拒绝 —— 它的基准不明确，
// 解析到哪算「允许的目录」由执行边界决定，工具层不该替它猜。
func CleanAttachmentPaths(kind string, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, raw := range paths {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("%w: %s 只接受绝对路径，%q 不是", ErrInvalid, kind, p)
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) > MaxSessionAttachments {
		return nil, fmt.Errorf("%w: %s 最多 %d 个，收到 %d 个", ErrInvalid, kind, MaxSessionAttachments, len(out))
	}
	return out, nil
}

// sessionTaskTitle 任务标题：正文首行；没有正文时按附件类型给占位。
func sessionTaskTitle(text string, files, images []string) string {
	if title := firstLine(text); title != "" {
		return title
	}
	if len(images) > 0 {
		return SessionImageOnlyTitle
	}
	if len(files) > 0 {
		return SessionFileOnlyTitle
	}
	return ""
}

// AppendFileRefs 把文件路径拼成 `@路径`（每行一个）：空格转 %20（`@` token 以空白
// 为界，与前端 Hub 附件条同口径）、同一路径只拼一次、正文里已有的引用不再重复拼。
// 只拼引用，**不读文件内容**（目标会话自己按需 read_file）。
func AppendFileRefs(text string, files []string) string {
	if len(files) == 0 {
		return text
	}
	seen := make(map[string]bool, len(files))
	refs := make([]string, 0, len(files))
	for _, raw := range files {
		p := strings.TrimSpace(raw)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		token := "@" + strings.ReplaceAll(p, " ", "%20")
		if hasFileRef(text, token) {
			continue
		}
		refs = append(refs, token)
	}
	if len(refs) == 0 {
		return text
	}
	joined := strings.Join(refs, "\n")
	if strings.TrimSpace(text) == "" {
		return joined // 空正文只留引用行，不留前导空行
	}
	if strings.HasSuffix(text, "\n") {
		return text + joined
	}
	return text + "\n" + joined
}

// hasFileRef 正文里是否已有该引用（按 token 边界判：`/a/b.go` 不该命中 `/a/b.go.bak`）。
func hasFileRef(text, token string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], token)
		if j < 0 {
			return false
		}
		k := i + j + len(token)
		if k >= len(text) || refTokenBoundary(text[k]) {
			return true
		}
		i += j + 1
	}
}

// refTokenBoundary `@路径` token 的右边界（空白即结束）。
func refTokenBoundary(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
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

// sendResultAccepted 只有「真的投递/仍在队列」才是 accepted；取消/失败/预算阻断/
// 拒绝/中断都不是。避免模型把 accepted=true + status=canceled 读成已投递（P2.5-07 N3）。
func sendResultAccepted(t *Task) bool {
	if t == nil {
		return false
	}
	// 显式 allow-list：新增终态/未知状态默认「未投递」，避免重演
	// 「accepted=true + status=canceled」把模型带偏（P2.5-07 N5）。
	switch t.Status {
	case TaskQueued, TaskDispatched, TaskRunning, TaskCompleted, TaskDelivered:
		return true
	}
	return false
}

// sendResultMessage 回传模型可读的原因；failed 可能含 OS/路径细节，只给指向
// task_status 的固定文案，不把内部错误原样塞进上下文（P2.5-07 N4）。
func sendResultMessage(t *Task) string {
	if t == nil {
		return ""
	}
	switch t.Status {
	case TaskCanceled, TaskInterrupted, TaskBlockedBudget, TaskDeclined:
		return t.Error
	case TaskFailed:
		return "派发失败，详见 task_status"
	case TaskCompleted:
		return "该 client_msg_id 的任务此前已完成，不会重跑；结果见 task_status"
	case TaskDelivered:
		return "该 client_msg_id 的任务此前已回推结果，不会重跑；结果见 task_status"
	}
	return ""
}
