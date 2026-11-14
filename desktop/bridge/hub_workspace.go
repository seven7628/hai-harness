package main

// hub_workspace.go：agenthub.WorkspaceOps 的 bridge 实现（方案 §3.2 / §6.6 / §9.2）。
//
// 这个文件是 Hub 智能体**唯一**能碰工作区的入口。所有路径参数必须在这里做
// 根目录白名单 + 敏感目录 + 符号链接归一 —— 领域包（agenthub）刻意不提供任何
// 路径旁路，工具层也不能直接调 m.runtime/m.create。
//
// 信任边界（§9.1）：智能体只能派任务，不能直接读写工作区文件；落盘由工作区
// 主会话执行。所以这里不提供写文件/执行命令的接口。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// hubWorkspaceOps 是 WorkspaceOps 的实现（无状态，按调用传 AgentID 查 policy）。
type hubWorkspaceOps struct {
	m *manager
}

func newHubWorkspaceOps(m *manager) *hubWorkspaceOps { return &hubWorkspaceOps{m: m} }

var _ agenthub.WorkspaceOps = (*hubWorkspaceOps)(nil)

// ListWorkspaces 列「已登记的项目工作区」（不含 Hub home：那里不是工作区）。
//
// 真相源是 workspaces.json（workspace_registry）：只列内存里的 m.workspaces 会漏掉
// 「刚建好、还没装配 runtime」和「重启后还没打开」的工作区。两处合并去重，
// 内存里的标题优先（用户可能刚改过名）。
func (o *hubWorkspaceOps) ListWorkspaces(ctx context.Context) ([]agenthub.WorkspaceInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	byPath := map[string]agenthub.WorkspaceInfo{}
	if o.m.wsReg != nil {
		for _, e := range o.m.wsReg.list() {
			// 插件虚拟 home 绝不作为 Hub 派发目标（09 §4.4 第 3 条）：它没有项目上下文，
			// 派进去就等于「智能体拿插件的 home 当仓库改」。
			if e.Path == "" || isHubHomePath(e.Path) || isPluginHomePath(e.Path) || hubSensitivePath(e.Path) {
				continue
			}
			name := e.Title
			if name == "" {
				name = filepath.Base(e.Path)
			}
			byPath[e.Path] = agenthub.WorkspaceInfo{Path: e.Path, Name: name}
		}
	}
	o.m.mu.Lock()
	for path, ws := range o.m.workspaces {
		if ws == nil || ws.kind == runtimeHubAgent || isHubHomePath(path) || isPluginHomePath(path) || hubSensitivePath(path) {
			continue
		}
		if _, ok := byPath[path]; !ok {
			byPath[path] = agenthub.WorkspaceInfo{Path: path, Name: filepath.Base(path)}
		}
	}
	o.m.mu.Unlock()
	out := make([]agenthub.WorkspaceInfo, 0, len(byPath))
	for _, info := range byPath {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// workspaceRoots 取智能体允许的根（Profile.Policy.WorkspaceRoots，已归一化）。
func (o *hubWorkspaceOps) workspaceRoots(agentID agenthub.AgentID) ([]string, error) {
	if agentID == "" {
		return nil, fmt.Errorf("%w: agent_id 不能为空", agenthub.ErrInvalid)
	}
	p, err := o.m.hubStore()
	if err != nil {
		return nil, err
	}
	prof, err := p.LoadAgent(context.Background(), agentID)
	if err != nil {
		return nil, fmt.Errorf("读取智能体失败: %w", err)
	}
	if prof.Deleted {
		return nil, fmt.Errorf("智能体已删除")
	}
	roots := make([]string, 0, len(prof.Policy.WorkspaceRoots))
	for _, root := range prof.Policy.WorkspaceRoots {
		abs, err := hubAbsExisting(hubExpandHome(root))
		if err != nil {
			continue // 不存在的根在运行期就等于不可用：跳过而不是让整个动作失败
		}
		roots = append(roots, abs)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("智能体未配置 policy.workspace_roots（§9.2 要求显式白名单）")
	}
	return roots, nil
}

// CreateWorkspace 在 workspace_roots 下新建目录（已存在 → 拒绝，不接管用户目录）。
func (o *hubWorkspaceOps) CreateWorkspace(ctx context.Context, req agenthub.CreateWorkspaceReq) (agenthub.WorkspaceInfo, error) {
	if err := ctx.Err(); err != nil {
		return agenthub.WorkspaceInfo{}, err
	}
	roots, err := o.workspaceRoots(req.AgentID)
	if err != nil {
		return agenthub.WorkspaceInfo{}, err
	}
	parent := strings.TrimSpace(req.Parent)
	if parent == "" {
		// parent 省略 = 用第一个白名单根（模型只需知道 name；roots 不出现在工具输出里）。
		parent = roots[0]
	}
	path, err := hubCreateWorkspacePath(parent, req.Name, roots)
	if err != nil {
		return agenthub.WorkspaceInfo{}, err
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return agenthub.WorkspaceInfo{}, fmt.Errorf("%w: %s 已存在（不接管已有目录）", agenthub.ErrConflict, path)
		}
		return agenthub.WorkspaceInfo{}, err
	}
	name := filepath.Base(path)
	if o.m.wsReg == nil {
		// 没有 registry 时「创建成功」是假的：目录建了但不可列出、不可派任务。
		_ = os.Remove(path)
		return agenthub.WorkspaceInfo{}, fmt.Errorf("%w: 工作区登记未初始化", agenthub.ErrInvalid)
	}
	// 落 workspaces.json：不登记的工作区重启后既不显示也无法派任务。
	e, err := o.m.wsReg.add(path)
	if err != nil {
		// 目录是刚 mkdir 的空目录，回滚掉比留一个「建了但不可用」的路径更干净。
		_ = os.Remove(path)
		return agenthub.WorkspaceInfo{}, fmt.Errorf("登记工作区失败: %w", err)
	}
	if e.Title != "" {
		name = e.Title
	}
	now := time.Now().UTC()
	return agenthub.WorkspaceInfo{Path: path, Name: name, UpdatedAt: &now}, nil
}

// ListSessions 只允许已登记的工作区（不存在/未登记 → ErrNotFound，顺带阻止 m.runtime 造目录）。
func (o *hubWorkspaceOps) ListSessions(ctx context.Context, workspace string) ([]agenthub.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := o.knownWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	rows := o.m.listSessions(path)
	out := make([]agenthub.SessionInfo, 0, len(rows))
	for _, row := range rows {
		info := agenthub.SessionInfo{Workspace: path}
		if v, ok := row["id"].(string); ok {
			info.ID = v
		}
		if v, ok := row["title"].(string); ok {
			info.Title = v
		}
		if v, ok := row["running"].(bool); ok {
			info.Running = v
		}
		if bs := o.m.get(path, info.ID); bs != nil {
			info.Pending = bs.s.PendingCount()
		}
		switch v := row["updated_at"].(type) {
		case time.Time:
			info.UpdatedAt = &v
		case int64: // listSessions 的 updated_at 是毫秒时间戳（不是 time.Time）
			if v > 0 {
				t := time.UnixMilli(v).UTC()
				info.UpdatedAt = &t
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// hubDispatchMessage 构造派发消息，块序固定：hub_task header → 正文 text → 图片块。
//
// provider 按文本送出（模型看得到 task_id）；UI/标题只认 text 块，用户气泡里只有正文。
// 图片只在这一层出现（data URL 随消息进模型上下文），**绝不进账本/任务事件**。
// 正文为空但带图时补占位正文：目标会话的快照/标题链路把「没有 text 块」当空消息。
// images 用变参：老调用点（只发正文）保持 `hubDispatchMessage(text, header)` 不变。
func hubDispatchMessage(text, header string, images ...core.Content) core.Message {
	msg := core.NewUserMessage()
	if header != "" {
		msg.Content = append(msg.Content, core.Content{Type: core.ContentTypeHubTask, Content: header})
	}
	if text == "" && len(images) > 0 {
		text = hubImageOnlyText
	}
	if text != "" {
		msg.Content = append(msg.Content, core.Content{Type: core.ContentTypeText, Content: text})
	}
	msg.Content = append(msg.Content, images...)
	return msg
}

// SessionStatus 单会话空闲闸门探针（P2-03 队列用）。
func (o *hubWorkspaceOps) SessionStatus(ctx context.Context, workspace, sessionID string) (agenthub.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return agenthub.SessionInfo{}, err
	}
	path, err := o.knownWorkspace(workspace)
	if err != nil {
		return agenthub.SessionInfo{}, err
	}
	bs := o.m.get(path, sessionID)
	if bs == nil {
		return agenthub.SessionInfo{}, fmt.Errorf("%w: session %q", agenthub.ErrNotFound, sessionID)
	}
	return agenthub.SessionInfo{
		ID: sessionID, Workspace: path,
		// 统一忙判据（含 hubDispatchPending 覆盖的 takeBatch→AgentStart 窗口：已 Ask
		// 但根 run 未起，此时 running 还是 false、Pending 已清空，只看两者会误判空闲，
		// P2-03 B1）与群轮闩（hubGroupBusy：群任务已投但 AgentEnd 未到，P3b-03 R3）。
		Running: hubSessionBusy(bs),
		Pending: bs.s.PendingCount(),
	}, nil
}

// hubQueueFor 懒开 Hub 派发队列（账本 + 本 manager 的 WorkspaceOps）。
func (m *manager) hubQueueFor() (*agenthub.Queue, error) {
	st, err := m.hubStore()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hubQueue == nil {
		m.hubQueue = agenthub.NewQueue(agenthub.NewLedger(st), newHubWorkspaceOps(m))
	}
	return m.hubQueue, nil
}

// pumpHubQueue 根 run 结束后推进队列（见 bridgeSession.emit 尾部）。
// 同步路径保留给测试/诊断；生产走 scheduleHubPump（可取消、可等待）。
// pumpHubQueue 同步泵一次。只能在非 Ops 回调路径调用：Queue.Pump 会持
// 目标锁并调 ops.Dispatch/SessionStatus，回调内反向调用同目标会自锁。
func (m *manager) pumpHubQueue(workspace, sessionID string) {
	if m == nil {
		return
	}
	ctx, ok := m.hubBackgroundContext()
	if !ok {
		return
	}
	m.pumpHubQueueCtx(ctx, workspace, sessionID)
}

func (m *manager) pumpHubQueueCtx(ctx context.Context, workspace, sessionID string) {
	if ctx == nil || ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	q := m.hubQueue
	m.mu.Unlock()
	if q == nil || q.QueuedCount(workspace, sessionID) == 0 {
		return // 没用过 Hub 派发/没有排队任务：不碰存储、不阻塞事件
	}
	_, _ = q.Pump(ctx, agenthub.TaskTarget{
		Kind: agenthub.TargetWorkspace, Workspace: workspace, SessionID: sessionID,
	})
}

// scheduleHubPump 是生产入口：登记到 Hub 后台 WaitGroup 后再派发。
func (m *manager) scheduleHubPump(workspace, sessionID string) {
	m.spawnHubBackground(func(ctx context.Context) {
		m.pumpHubQueueCtx(ctx, workspace, sessionID)
	})
}

// CreateSession 在已登记工作区里开会话（标题由首条消息生成，这里只给 id）。
func (o *hubWorkspaceOps) CreateSession(ctx context.Context, req agenthub.CreateSessionReq) (agenthub.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return agenthub.SessionInfo{}, err
	}
	path, err := o.knownWorkspace(req.Workspace)
	if err != nil {
		return agenthub.SessionInfo{}, err
	}
	id := o.m.genID()
	bs, err := o.m.create(path, id, "")
	if err != nil {
		return agenthub.SessionInfo{}, err
	}
	now := time.Now().UTC()
	info := agenthub.SessionInfo{ID: id, Workspace: path, UpdatedAt: &now}
	if bs != nil {
		info.Running = bs.running.Load()
	}
	// 标题统一 trim + 80 rune（与 session_rename 一致）；空标题落 defaultSessionTitle，
	// 否则 create 回显「新会话」而 list 显示 New Chat（P2-02 复审 B1 残留）。
	title := []rune(strings.TrimSpace(req.Title))
	if len(title) == 0 {
		title = []rune(defaultSessionTitle)
	}
	if len(title) > 80 {
		title = title[:80]
	}
	info.Title = string(title)
	// 标题必须走现有 session_manual_titles（titles.json）机制：只回显不落盘
	// 会让 session_create 说「已建 评审标题」而 session_list 显示 New Chat（P2-02 B1）。
	sessionManualTitles.set(workspaceKey(path), id, info.Title)
	return info, nil
}

// Dispatch 把一条任务消息送进工作区会话（Ask 只入队；运行由会话自己的 runLoop 推进）。
//
// 一批可以同时带：命令（compact/clear/skills）+ 正文 + 文件引用 + 图片。
// 投递顺序 = 命令 → 正文（同一批 FIFO：命令先进目标会话 inbox，正文随后）。
// 所有校验（命令白名单、clear 同批矛盾、附件路径白名单、图片格式/体积）都在
// **占 dedup 之前**完成 —— 坏输入必须能原样重试，不能留下「从未投递」的去重占位。
func (o *hubWorkspaceOps) Dispatch(ctx context.Context, req agenthub.DispatchReq) (agenthub.DispatchResult, error) {
	if err := ctx.Err(); err != nil {
		return agenthub.DispatchResult{}, err
	}
	path, err := o.knownWorkspace(req.Workspace)
	if err != nil {
		return agenthub.DispatchResult{}, err
	}
	sid := strings.TrimSpace(req.SessionID)
	if sid == "" {
		return agenthub.DispatchResult{}, fmt.Errorf("%w: session_id 不能为空", agenthub.ErrInvalid)
	}
	if isHubHomePath(path) {
		// 双保险：knownWorkspace 已挡，但这里错了就是「把工作区任务派进智能体自己」。
		return agenthub.DispatchResult{}, fmt.Errorf("%w: Hub home 不是工作区", agenthub.ErrInvalid)
	}
	command := strings.TrimSpace(req.Command)
	commandArgs := strings.TrimSpace(req.CommandArgs)
	if err := agenthub.ValidateSessionCommand(command, commandArgs); err != nil {
		return agenthub.DispatchResult{}, err
	}
	text := strings.TrimSpace(req.Text)
	files := hubCleanPaths(req.Files)
	images := hubCleanPaths(req.Images)
	// clear 会清空整个上下文：同批再塞正文/附件等于「先清空再投递」，语义自相矛盾。
	if command == agenthub.CommandClear && (text != "" || len(files) > 0 || len(images) > 0) {
		return agenthub.DispatchResult{}, fmt.Errorf(
			"%w: command=clear 不能与 text/files/images 同批（clear 会清空同批正文）", agenthub.ErrInvalid)
	}
	// Dispatch 必须真的有一条消息要投（正文/文件引用/图片）：只有命令的批次走
	// CommandSession（选项窄接口），不要用空消息 Ask 出一个「没有内容」的回合。
	if text == "" && len(files) == 0 && len(images) == 0 {
		return agenthub.DispatchResult{}, fmt.Errorf(
			"%w: Dispatch 需要 text/files/images 至少一个（只发命令用 CommandSession）", agenthub.ErrInvalid)
	}
	// 注意：m.get 自己会按路径装配 runtime，所以「先 get 再 runtime 再 get」没有意义；
	// 而磁盘上的 session 不会自动回灌 ws.sessions。重启后要派任务，需要先让 UI
	// workspace_list/session_list 打开该工作区（P2-03 任务账本会补会话恢复）。
	bs := o.m.get(path, sid)
	if bs == nil {
		return agenthub.DispatchResult{}, fmt.Errorf("%w: session %q", agenthub.ErrNotFound, sid)
	}
	// header 校验必须在去重占位之前：坏 header 返回 ErrInvalid 时不能留下
	// client_msg_id 占位，否则同 id 的合法重试会被误判 duplicate（P2-04 复审 NB-1）。
	if strings.TrimSpace(req.TaskHeader) != "" {
		if _, err := agenthub.ParseTaskHeader(req.TaskHeader); err != nil {
			return agenthub.DispatchResult{}, fmt.Errorf("%w: 无效的 hub_task header: %v", agenthub.ErrInvalid, err)
		}
	}
	// 附件同样在占位之前：路径越界/图片超限要能原样重试（同坏 header 的口径）。
	attachments, err := o.hubPrepareAttachments(req.AgentID, path, files, images)
	if err != nil {
		return agenthub.DispatchResult{}, err
	}
	body := agenthub.AppendFileRefs(text, files)
	// client_msg_id 幂等（进程内）：同一 (workspace, session, id) 只投递一次。
	// 跨重启的幂等由 P2-03 账本/目标侧去重负责；这里先保证「超时用同一个 id 重试」
	// 不会把同一任务执行两遍（P2-02 评审 B2）。
	dedupKey := ""
	if req.ClientMsgID != "" {
		dedupKey = path + "\x00" + sid + "\x00" + req.ClientMsgID
		o.m.mu.Lock()
		if _, seen := o.m.hubMsgDedup[dedupKey]; seen {
			o.m.mu.Unlock()
			return agenthub.DispatchResult{SessionID: sid, Accepted: true, Message: agenthub.DispatchDuplicateMessage}, nil
		}
		if o.m.hubMsgDedup == nil {
			o.m.hubMsgDedup = map[string]string{}
		}
		o.m.hubMsgDedup[dedupKey] = sid // 先占位：并发同 id 的第二个请求直接返回
		o.m.mu.Unlock()
	}
	msg := hubDispatchMessage(body, req.TaskHeader, attachments.blocks...)
	// 先置 pending 再 Ask：若 runLoop 已有 wake token，Ask 后可能立刻
	// AgentStart→AgentEnd，晚置位会把闸门永久 latch（P2-03 复审 NB-1）。
	bs.hubDispatchPending.Store(true)
	// 命令先入 inbox（纯 enqueue，立即返回），正文随后：同一批里 FIFO 命令在前，
	// 目标会话在注入段先执行 compact/clear/skills、再看正文。
	// 命令失败 = 这一批没投递：回滚 pending 与去重占位，让调用方拿到原始错误。
	// 已知边界：命令成功但随后的 Ask 失败时，命令已经进了 inbox（无法撤回），
	// 回滚的只是 pending/dedup —— 正文仍可安全重试。
	if command != "" {
		if err := bs.s.Command(ctx, command, commandArgs); err != nil {
			bs.hubDispatchPending.Store(false)
			o.hubRollbackDedup(dedupKey)
			return agenthub.DispatchResult{}, err
		}
	}
	if err := bs.s.Ask(ctx, msg); err != nil {
		bs.hubDispatchPending.Store(false)
		o.hubRollbackDedup(dedupKey)
		return agenthub.DispatchResult{}, err
	}
	bs.lastActive.Store(time.Now().UnixMilli())
	bs.wakeRun()
	return agenthub.DispatchResult{SessionID: sid, Accepted: true}, nil
}

// hubRollbackDedup 撤销本批的 client_msg_id 占位（空键 = 本次没有占位）。
func (o *hubWorkspaceOps) hubRollbackDedup(key string) {
	if key == "" {
		return
	}
	o.m.mu.Lock()
	delete(o.m.hubMsgDedup, key)
	o.m.mu.Unlock()
}

// CommandSession 只发一条命令（无正文/附件）：不建账、不过队列。
//
// 命令是**会话操作**而不是任务：没有正文要谁做、没有 AgentEnd 可回推。走账本会
// 凭空造一条永远结算不了的任务并强占空闲闸门，所以这里直接进目标会话的 inbox。
func (o *hubWorkspaceOps) CommandSession(ctx context.Context, req agenthub.CommandSessionReq) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := o.knownWorkspace(req.Workspace)
	if err != nil {
		return err
	}
	sid := strings.TrimSpace(req.SessionID)
	if sid == "" {
		return fmt.Errorf("%w: session_id 不能为空", agenthub.ErrInvalid)
	}
	if isHubHomePath(path) {
		return fmt.Errorf("%w: Hub home 不是工作区", agenthub.ErrInvalid)
	}
	command := strings.TrimSpace(req.Command)
	args := strings.TrimSpace(req.Args)
	if command == "" {
		return fmt.Errorf("%w: command 不能为空", agenthub.ErrInvalid)
	}
	if err := agenthub.ValidateSessionCommand(command, args); err != nil {
		return err
	}
	bs := o.m.get(path, sid)
	if bs == nil {
		return fmt.Errorf("%w: session %q", agenthub.ErrNotFound, sid)
	}
	if err := bs.s.Command(ctx, command, args); err != nil {
		return err
	}
	bs.lastActive.Store(time.Now().UnixMilli())
	bs.wakeRun()
	return nil
}

// CheckAttachments 在建账前校验附件（WorkspaceOps 的可选扩展，见 agenthub.AttachmentChecker）。
//
// 为什么要图片也在这里读一遍：格式/单边/体积闸门只有读了文件才知道。留到派发时
// 才发现「第 3 张图超限」会先建出一条 failed 任务并占住 client_msg_id 去重表。
//
// workspace 来自模型 JSON（session_send），必须先过与 Dispatch 同一道登记检查：
// hubPrepareAttachments 会把 workspace 加进附件白名单根，未登记路径（尤其 "/"）
// 等于允许按任意根读盘；图片还会被整份读入/解码（存在/大小/类型 oracle + DoS）。
func (o *hubWorkspaceOps) CheckAttachments(ctx context.Context, req agenthub.AttachmentReq) ([]agenthub.AttachmentInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := o.knownWorkspace(req.Workspace)
	if err != nil {
		return nil, err
	}
	if len(req.Files) == 0 && len(req.Images) == 0 {
		return nil, nil
	}
	prep, err := o.hubPrepareAttachments(req.AgentID, path, hubCleanPaths(req.Files), hubCleanPaths(req.Images))
	if err != nil {
		return nil, err
	}
	out := make([]agenthub.AttachmentInfo, 0, len(prep.infos))
	for _, a := range prep.infos {
		out = append(out, agenthub.AttachmentInfo{Kind: a.Kind, Path: a.Path, Name: a.Name, Bytes: a.Bytes})
	}
	return out, nil
}

// hubDispatchAttachments 一次派发的附件装配结果：账本引用 + 模型图片块。
type hubDispatchAttachments struct {
	infos  []agenthub.TaskAttachment
	blocks []core.Content
}

// hubPrepareAttachments 校验附件路径（白名单/敏感目录/软链归一）并把图片读成内容块。
// 返回值只含路径/名称/字节数 —— data URL 留在 blocks 里，不进账本。
func (o *hubWorkspaceOps) hubPrepareAttachments(agentID agenthub.AgentID, workspace string, files, images []string) (hubDispatchAttachments, error) {
	if len(files) == 0 && len(images) == 0 {
		return hubDispatchAttachments{}, nil
	}
	if len(files) > agenthub.MaxSessionAttachments || len(images) > agenthub.MaxSessionAttachments {
		return hubDispatchAttachments{}, fmt.Errorf(
			"%w: 单批 files/images 各最多 %d 个（files=%d, images=%d）",
			agenthub.ErrInvalid, agenthub.MaxSessionAttachments, len(files), len(images))
	}
	roots, err := o.hubAttachmentRoots(agentID, workspace)
	if err != nil {
		return hubDispatchAttachments{}, err
	}
	out := hubDispatchAttachments{infos: make([]agenthub.TaskAttachment, 0, len(files)+len(images))}
	imagePaths := make([]string, 0, len(images))
	for _, raw := range files {
		info, err := hubCheckAttachmentPath(raw, agenthub.AttachmentKindFile, roots)
		if err != nil {
			return hubDispatchAttachments{}, err
		}
		out.infos = append(out.infos, info)
	}
	for _, raw := range images {
		info, err := hubCheckAttachmentPath(raw, agenthub.AttachmentKindImage, roots)
		if err != nil {
			return hubDispatchAttachments{}, err
		}
		out.infos = append(out.infos, info)
		imagePaths = append(imagePaths, info.Path)
	}
	blocks, err := hubLoadAttachmentImages(imagePaths)
	if err != nil {
		return hubDispatchAttachments{}, err
	}
	out.blocks = blocks
	return out, nil
}

// hubCheckAttachmentPath 校验一个附件路径，返回账本可存的引用（真实路径）。
func hubCheckAttachmentPath(raw, kind string, roots []string) (agenthub.TaskAttachment, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return agenthub.TaskAttachment{}, fmt.Errorf("%w: %s 路径不能为空", agenthub.ErrInvalid, kind)
	}
	// 相对路径没有明确基准（解析到哪算「允许目录」由实现猜），~ 也不展开：
	// 两种写法在模型侧都常见，所以错误文案要说清楚该怎么写。
	if !filepath.IsAbs(raw) {
		return agenthub.TaskAttachment{}, fmt.Errorf("%w: %s 只接受绝对路径，%q 不是（~ 也不展开）", agenthub.ErrInvalid, kind, raw)
	}
	// 软链接在这里归一：逃逸出白名单的目标会被下面的 hubPathWithin 判掉。
	real := hubCanonicalPath(raw)
	if isHubHomePath(real) {
		return agenthub.TaskAttachment{}, fmt.Errorf("%w: %s 不能指向 Hub home（智能体自己的家目录）", agenthub.ErrForbidden, kind)
	}
	if hubSensitivePath(real) {
		return agenthub.TaskAttachment{}, fmt.Errorf("%w: %s 命中敏感目录（~/.go-code、~/.ssh 等）", agenthub.ErrForbidden, kind)
	}
	inside := false
	for _, root := range roots {
		if hubPathWithin(real, root) {
			inside = true
			break
		}
	}
	if !inside {
		return agenthub.TaskAttachment{}, fmt.Errorf(
			"%w: %s %q 不在 policy.workspace_roots / tools.bash_roots 内（软链接逃逸同判）", agenthub.ErrForbidden, kind, raw)
	}
	st, err := os.Lstat(real)
	if err != nil {
		return agenthub.TaskAttachment{}, fmt.Errorf("%w: %s 不存在或不可读: %v", agenthub.ErrInvalid, kind, raw)
	}
	if !st.Mode().IsRegular() {
		return agenthub.TaskAttachment{}, fmt.Errorf("%w: %s 不是普通文件: %q", agenthub.ErrInvalid, kind, raw)
	}
	return agenthub.TaskAttachment{Kind: kind, Path: real, Name: filepath.Base(real), Bytes: st.Size()}, nil
}

// hubAttachmentRoots 计算附件允许的根：policy.workspace_roots ∪ tools.bash_roots ∪ 目标工作区。
// 三项任一命中即可（bash_roots 本来就是「智能体可读的目录」）。
func (o *hubWorkspaceOps) hubAttachmentRoots(agentID agenthub.AgentID, workspace string) ([]string, error) {
	if strings.TrimSpace(string(agentID)) == "" {
		return nil, fmt.Errorf("%w: agent_id 不能为空（附件路径要按智能体 policy 校验）", agenthub.ErrInvalid)
	}
	st, err := o.m.hubStore()
	if err != nil {
		return nil, err
	}
	prof, err := st.LoadAgent(context.Background(), agentID)
	if err != nil {
		return nil, fmt.Errorf("读取智能体失败: %w", err)
	}
	if prof == nil || prof.Deleted {
		return nil, fmt.Errorf("%w: 智能体 %s 不可用", agenthub.ErrNotFound, agentID)
	}
	roots := make([]string, 0, len(prof.Policy.WorkspaceRoots)+len(prof.Tools.BashRoots)+1)
	seen := make(map[string]bool, cap(roots))
	add := func(p string) {
		if strings.TrimSpace(p) == "" {
			return
		}
		real, err := hubAbsExisting(hubExpandHome(p))
		if err != nil || seen[real] {
			return
		}
		seen[real] = true
		roots = append(roots, real)
	}
	for _, p := range prof.Policy.WorkspaceRoots {
		add(p)
	}
	for _, p := range prof.Tools.BashRoots {
		add(p)
	}
	add(workspace)
	if len(roots) == 0 {
		return nil, fmt.Errorf("%w: 智能体未配置 policy.workspace_roots（附件路径必须落在显式白名单内）", agenthub.ErrForbidden)
	}
	return roots, nil
}

// hubLoadAttachmentImages 把图片路径读成模型内容块（格式/单边/体积闸门复用 tools/builtin）。
//
// 复用方式：走 builtin 的 read_file 工具 + tools.ImageSink，而不是在这里再写一份
// 解码/切片/重编码。loadImageForModel 是未导出的，但它的**行为**就是 read_file 的
// 图片分支（png/jpeg/gif 可解码、单边 >8192px 切片、超体积降采样 JPEG），两套判据
// 分开写必然漂移（那次 390x9697 截图事故就是这么来的）。这里只负责把 blocks 收集
// 起来、并把**总量**卡在 MaxSessionImageBytes（单张闸门由 read_file 自己管）。
func hubLoadAttachmentImages(paths []string) ([]core.Content, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	// root 只影响相对路径解析；这里全是绝对路径，取第一个文件的目录即可。
	var readTool tools.Tool
	for _, tl := range builtin.NewFileTools(filepath.Dir(paths[0])).Tools() {
		if tl.Name() == "read_file" {
			readTool = tl
			break
		}
	}
	if readTool == nil {
		return nil, fmt.Errorf("%w: 图片读取通路不可用（read_file 未注册）", agenthub.ErrInvalid)
	}
	blocks := make([]core.Content, 0, len(paths))
	var total int64
	for _, p := range paths {
		args, err := json.Marshal(map[string]string{"path": p})
		if err != nil {
			return nil, err
		}
		sink := &tools.ImageSink{}
		if _, err := readTool.Call(tools.WithImageSink(context.Background(), sink), "read_file", string(args)); err != nil {
			return nil, fmt.Errorf("%w: 读取图片失败: %v", agenthub.ErrInvalid, err)
		}
		imgs := sink.Images()
		if len(imgs) == 0 {
			return nil, fmt.Errorf("%w: %s 不是可解码的图片（png/jpeg/gif）", agenthub.ErrInvalid, filepath.Base(p))
		}
		for _, im := range imgs {
			total += int64(len(im.Content))
		}
		// 逐张累计，超了立刻失败（不做「先全部读入再判」的白烧内存）。
		if total > agenthub.MaxSessionImageBytes {
			return nil, fmt.Errorf("%w: 图片总量超过 %d 字节（%d 张图的 data URL 之后仍超限）",
				agenthub.ErrInvalid, agenthub.MaxSessionImageBytes, len(paths))
		}
		blocks = append(blocks, imgs...)
	}
	return blocks, nil
}

// hubCleanPaths 去空白、去重、保序（工具层/服务层已校验，这里是派发前的最后一道）。
func hubCleanPaths(paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, raw := range paths {
		p := strings.TrimSpace(raw)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// AgentDispatch 把智能体目标任务投进目标的群会话（P3b-03，target.kind=agent）。
//
// 判忙与投递必须在**一次**调用里完成：探测（SessionStatus 式）与 Ask 分成两次，
// 中间可能被一次普通群唤醒插队 —— 那一轮的 hub_task 就被顶到下一批，空闲闸门失效。
// 忙（hubGroupBusy 闩已被占 / 会话在跑 / 有积压）→ Accepted=false，什么都没投，
// 任务由 Queue 留在队列里（P3b-04 在目标回合结束后泵）。
func (o *hubWorkspaceOps) AgentDispatch(ctx context.Context, req agenthub.AgentDispatchReq) (agenthub.AgentDispatchResult, error) {
	if err := ctx.Err(); err != nil {
		return agenthub.AgentDispatchResult{}, err
	}
	target := req.Target
	if target.Kind != agenthub.TargetAgent {
		return agenthub.AgentDispatchResult{}, fmt.Errorf("%w: AgentDispatch 只接受 agent 目标，得到 %q", agenthub.ErrInvalid, target.Kind)
	}
	gid, to := target.GroupID, target.AgentID
	if gid == "" || to == "" {
		return agenthub.AgentDispatchResult{}, fmt.Errorf("%w: agent 目标需要 group_id/agent_id", agenthub.ErrInvalid)
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return agenthub.AgentDispatchResult{}, fmt.Errorf("%w: text 不能为空", agenthub.ErrInvalid)
	}
	// header 先校验：坏 header 不能以「已投递」的形式进入会话（P2-04 NB-1 同口径）。
	if strings.TrimSpace(req.TaskHeader) != "" {
		if _, err := agenthub.ParseTaskHeader(req.TaskHeader); err != nil {
			return agenthub.AgentDispatchResult{}, fmt.Errorf("%w: 无效的 hub_task header: %v", agenthub.ErrInvalid, err)
		}
	}
	st, err := o.m.hubStore()
	if err != nil {
		return agenthub.AgentDispatchResult{}, err
	}
	g, err := st.LoadGroup(ctx, gid)
	if err != nil {
		return agenthub.AgentDispatchResult{}, err
	}
	if g == nil || g.Deleted {
		return agenthub.AgentDispatchResult{}, fmt.Errorf("%w: 群组 %s 不存在", agenthub.ErrNotFound, gid)
	}
	if !groupHasMember(g, to) {
		return agenthub.AgentDispatchResult{}, fmt.Errorf("%w: %s 不是群 %s 的成员", agenthub.ErrInvalid, to, gid)
	}
	if req.OriginAgent != "" && !groupHasMember(g, req.OriginAgent) {
		// 派发瞬间发起者已被移出群：任务不能再落地（成员校验在工具层也做，这里是二道闸）。
		return agenthub.AgentDispatchResult{}, fmt.Errorf("%w: 发起者 %s 已不在群 %s", agenthub.ErrInvalid, req.OriginAgent, gid)
	}
	if p, _, perr := o.m.hubLoadAgent(string(to)); perr != nil || p == nil {
		return agenthub.AgentDispatchResult{}, fmt.Errorf("%w: 目标智能体 %s 不可用", agenthub.ErrNotFound, to)
	}
	// 懒装配群会话：同一个 (group, agent) 只有一个会话，与 @ 唤醒/group_say 共用。
	bs, err := o.m.hubGroupSession(string(to), string(gid))
	if err != nil {
		return agenthub.AgentDispatchResult{}, err
	}
	// 判忙 + 占闩：CAS 失败 = 已有群轮/任务在跑，什么都不投。
	if !bs.hubGroupBusy.CompareAndSwap(false, true) {
		return agenthub.AgentDispatchResult{SessionID: bs.id, Accepted: false}, nil
	}
	seq := bs.hubGroupSeq.Add(1)
	bs.hubGroupOwner.Store(seq)
	ok := false
	defer func() {
		if !ok {
			bs.clearHubGroupBusy(seq) // 没投出去：立即还闩，别把这个智能体永久标忙
		}
	}()
	// 占闩后二次确认：running/pending 还说明有未收尾的回合（闩和运行态短暂不一致时
	// 也不能把第二个 hub_task 塞进同一批）。闩本身已经被本次占住，用去掉闩的判据。
	if hubSessionRoundInFlight(bs) || bs.s.PendingCount() > 0 {
		return agenthub.AgentDispatchResult{SessionID: bs.id, Accepted: false}, nil
	}
	// 进程内幂等（同一 (会话, client_msg_id) 只投一次）；跨重启靠账本状态。
	dedupKey := ""
	if req.ClientMsgID != "" {
		dedupKey = "agent\x00" + bs.id + "\x00" + req.ClientMsgID
		o.m.mu.Lock()
		_, seen := o.m.hubMsgDedup[dedupKey]
		if !seen {
			if o.m.hubMsgDedup == nil {
				o.m.hubMsgDedup = map[string]string{}
			}
			o.m.hubMsgDedup[dedupKey] = bs.id
		}
		o.m.mu.Unlock()
		if seen {
			return agenthub.AgentDispatchResult{
				SessionID: bs.id, Accepted: true, Message: agenthub.DispatchDuplicateMessage,
			}, nil
		}
	}
	msg := hubDispatchMessage(text, req.TaskHeader)
	// 与工作区派发同口径：Ask 成功到根 AgentStart 之间的窗口用 hubDispatchPending
	// 表达忙（AgentStart/AgentEnd 会清）——这段 running 还是 false，只看运行态会把
	// 「已投递、未开跑」读成空闲（P2-03 B1 / P3b-03 R3）。
	bs.hubDispatchPending.Store(true)
	if err := bs.s.Ask(ctx, msg); err != nil {
		bs.hubDispatchPending.Store(false)
		if dedupKey != "" {
			o.m.mu.Lock()
			delete(o.m.hubMsgDedup, dedupKey)
			o.m.mu.Unlock()
		}
		return agenthub.AgentDispatchResult{}, err
	}
	ok = true
	bs.lastActive.Store(time.Now().UnixMilli())
	bs.wakeRun()
	return agenthub.AgentDispatchResult{SessionID: bs.id, Accepted: true}, nil
}

// InterruptSession 中断某个工作区会话的当前 run（跨工作区按 sid 查）。
func (o *hubWorkspaceOps) InterruptSession(ctx context.Context, sid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return fmt.Errorf("%w: session_id 不能为空", agenthub.ErrInvalid)
	}
	o.m.mu.Lock()
	paths := make([]string, 0, len(o.m.workspaces))
	for path := range o.m.workspaces {
		paths = append(paths, path)
	}
	o.m.mu.Unlock()
	for _, path := range paths {
		if bs := o.m.get(path, sid); bs != nil {
			bs.s.Interrupt()
			return nil
		}
	}
	return fmt.Errorf("%w: session %q", agenthub.ErrNotFound, sid)
}

// knownWorkspace 校验一个已登记的工作区路径（不接受任意目录：那等于任意读写）。
func (o *hubWorkspaceOps) knownWorkspace(path string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return "", fmt.Errorf("%w: workspace 不能为空", agenthub.ErrInvalid)
	}
	// 09 §4.4 第 3 条：插件虚拟 home 硬拒绝（unknownWorkspace 的语义是「不是工作区」，
	// 这里用 ErrForbidden 而不是 ErrNotFound —— 路径形状合法、只是永远不许派任务）。
	if isPluginHomePath(clean) {
		return "", fmt.Errorf("%w: workspace %q 是插件虚拟 Project 的 home，不是项目工作区", agenthub.ErrForbidden, clean)
	}
	if isHubHomePath(clean) || hubSensitivePath(clean) {
		// registry 里的路径是用户显式加的（workspace_add/sync 不查敏感目录），
		// 但 §9.2 的敏感目录是硬线：登记过也不许派任务（P2-01 复审 N5）。
		return "", fmt.Errorf("%w: workspace %q 是敏感目录或 Hub home", agenthub.ErrForbidden, clean)
	}
	o.m.mu.Lock()
	ws := o.m.workspaces[clean]
	matchedKey := clean
	if ws == nil {
		// m.workspaces 的 key 是调用方原样字符串（可能带尾斜杠/..）：再按归一比较一次，
		// 否则「用 ./proj/ 打开过的 runtime」永远匹配不上。命中时必须返回**原键**，
		// 下游 m.get 才能找到同一个 runtime（返回 clean 会另装配一个，P2-01 复审 N6）。
		for key, cand := range o.m.workspaces {
			if filepath.Clean(key) == clean {
				ws = cand
				matchedKey = key
				break
			}
		}
	}
	o.m.mu.Unlock()
	if ws != nil && ws.kind != runtimeHubAgent {
		return matchedKey, nil
	}
	// 已登记但还没装配 runtime（刚建好 / 重启后未打开）：允许，但目录必须在。
	if o.m.wsReg != nil {
		for _, e := range o.m.wsReg.list() {
			if e.Path != clean || isHubHomePath(e.Path) {
				continue
			}
			if st, err := os.Stat(clean); err == nil && st.IsDir() {
				return clean, nil
			}
			break
		}
	}
	return "", fmt.Errorf("%w: workspace %q 未登记", agenthub.ErrNotFound, clean)
}

// hubExpandHome 展开 ~ / ~/xxx（policy.workspace_roots 的文档默认值就是 ~/hai-workspaces）。
// 不展开的话 filepath.Abs("~/ws") 会变成 <cwd>/~/ws，EvalSymlinks 失败后被静默跳过，
// 最后报「未配置 workspace_roots」——配了但说没配（P2-01 复审 N1）。
func hubExpandHome(p string) string {
	p = strings.TrimSpace(p)
	if p != "~" && !strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// hubAbsExisting 把路径解析成绝对真实路径（必须存在）。
func hubAbsExisting(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(real), nil
}

// hubCreateWorkspacePath 计算「新建工作区」的最终路径（§9.2）。
//
// 规则：
//   - Parent 必须存在，且解析符号链接后仍落在某个允许根内（根本身或其子目录）；
//   - Name 是单段目录名：不允许分隔符、`.`/`..`、NUL；
//   - 结果不得已存在（不接管目录），且不得命中敏感目录（~/.go-code、~/.ssh 等）。
func hubCreateWorkspacePath(parent, name string, roots []string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: 工作区名不能为空或 . / ..", agenthub.ErrInvalid)
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: 工作区名不能包含路径分隔符", agenthub.ErrInvalid)
	}
	if len([]rune(name)) > 64 {
		return "", fmt.Errorf("%w: 工作区名过长", agenthub.ErrInvalid)
	}
	parentReal, err := hubAbsExisting(hubExpandHome(parent))
	if err != nil {
		return "", fmt.Errorf("%w: 父目录不可用: %v", agenthub.ErrInvalid, err)
	}
	if hubSensitivePath(parentReal) {
		return "", fmt.Errorf("%w: 敏感目录不能作为工作区根", agenthub.ErrForbidden)
	}
	inside := false
	for _, root := range roots {
		rootReal, err := hubAbsExisting(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootReal, parentReal)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..") {
			inside = true
			break
		}
	}
	if !inside {
		return "", fmt.Errorf("%w: 父目录不在 policy.workspace_roots 内", agenthub.ErrForbidden)
	}
	target := filepath.Join(parentReal, name)
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("%w: %s 已存在（不接管已有目录）", agenthub.ErrConflict, target)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("%w: 目标路径不可用: %v", agenthub.ErrInvalid, err)
	}
	if hubSensitivePath(target) {
		return "", fmt.Errorf("%w: 目标命中敏感目录", agenthub.ErrForbidden)
	}
	return target, nil
}

// hubSensitivePath 敏感目录判定（与 agents.sensitivePath / Electron isSensitiveFsPath 同口径）。
//
// 为什么在 bridge 复制一份：那是 agents 包内的未导出函数，而 WorkspaceOps 的路径
// 校验必须在**执行前**发生（不能等 agents 的引用展开）。两边口径要一起改。
func hubSensitivePath(abs string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	// 两边都归一：macOS 上 $HOME 可能是 /var/folders/...，而调用方给的是 EvalSymlinks
	// 之后的 /private/var/...（前缀比较会漏判）。
	clean := hubCanonicalPath(abs)
	home = hubCanonicalPath(home)
	cfgDir := filepath.Join(home, ".go-code")
	if hubPathWithin(clean, cfgDir) {
		for _, sub := range []string{"plugins", "runtime"} {
			if hubPathWithin(clean, filepath.Join(cfgDir, sub)) {
				return false
			}
		}
		return true
	}
	for _, name := range []string{".ssh", ".aws", ".gnupg", ".netrc", ".kube"} {
		if hubPathWithin(clean, filepath.Join(home, name)) {
			return true
		}
	}
	return false
}

// hubCanonicalPath 能解析就解析真实路径，不存在则保持 Clean（父级仍可比较）。
func hubCanonicalPath(p string) string {
	clean := filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real
	}
	if dir, base := filepath.Split(clean); dir != "" {
		if realDir, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
			return filepath.Join(realDir, base)
		}
	}
	return clean
}

// hubPathWithin 判断 p 是 base 本身还是其后代。
//
// **不能用字符串前缀比较**：macOS APFS 默认大小写不敏感，~/.SSH 与 ~/.ssh 是同一个
// inode，但字符串不相等 → 敏感目录检查会被绕过（P2-01 复审 B1）。这里逐级祖先做
// os.SameFile（一并覆盖符号链接/firmlink）；base 不存在时退回大小写折叠比较。
func hubPathWithin(p, base string) bool {
	if info, err := os.Stat(base); err == nil {
		for cur := filepath.Clean(p); ; {
			if ci, err := os.Stat(cur); err == nil && os.SameFile(ci, info) {
				return true
			}
			parent := filepath.Dir(cur)
			if parent == cur {
				return false
			}
			cur = parent
		}
	}
	pFold := strings.ToLower(filepath.Clean(p))
	bFold := strings.ToLower(filepath.Clean(base))
	return pFold == bFold || strings.HasPrefix(pFold, bFold+string(os.PathSeparator))
}
