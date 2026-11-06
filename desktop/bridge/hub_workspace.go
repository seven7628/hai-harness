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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
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
			if e.Path == "" || isHubHomePath(e.Path) || hubSensitivePath(e.Path) {
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
		if ws == nil || ws.kind == runtimeHubAgent || isHubHomePath(path) || hubSensitivePath(path) {
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

// hubDispatchMessage 构造派发消息：hub_task header 块在前、正文 text 块在后。
// provider 按文本送出（模型看得到 task_id）；UI/标题只认 text 块，用户气泡里只有正文。
func hubDispatchMessage(text, header string) core.Message {
	msg := core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: text})
	if header != "" {
		msg.Content = append([]core.Content{{Type: core.ContentTypeHubTask, Content: header}}, msg.Content...)
	}
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
		// hubDispatchPending 覆盖 takeBatch→AgentStart 的窗口：已 Ask 但根 run 未起，
		// 此时 running 还是 false、Pending 已清空，只看两者会误判空闲（P2-03 B1）。
		Running: bs.running.Load() || bs.hubDispatchPending.Load() || bs.hubSettling.Load(),
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

// Dispatch 把任务文本送进工作区会话（Ask 只入队；运行由会话自己的 runLoop 推进）。
func (o *hubWorkspaceOps) Dispatch(ctx context.Context, req agenthub.DispatchReq) (agenthub.DispatchResult, error) {
	if err := ctx.Err(); err != nil {
		return agenthub.DispatchResult{}, err
	}
	path, err := o.knownWorkspace(req.Workspace)
	if err != nil {
		return agenthub.DispatchResult{}, err
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return agenthub.DispatchResult{}, fmt.Errorf("%w: text 不能为空", agenthub.ErrInvalid)
	}
	sid := strings.TrimSpace(req.SessionID)
	if sid == "" {
		return agenthub.DispatchResult{}, fmt.Errorf("%w: session_id 不能为空", agenthub.ErrInvalid)
	}
	if isHubHomePath(path) {
		// 双保险：knownWorkspace 已挡，但这里错了就是「把工作区任务派进智能体自己」。
		return agenthub.DispatchResult{}, fmt.Errorf("%w: Hub home 不是工作区", agenthub.ErrInvalid)
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
	msg := hubDispatchMessage(text, req.TaskHeader)
	// 先置 pending 再 Ask：若 runLoop 已有 wake token，Ask 后可能立刻
	// AgentStart→AgentEnd，晚置位会把闸门永久 latch（P2-03 复审 NB-1）。
	bs.hubDispatchPending.Store(true)
	if err := bs.s.Ask(ctx, msg); err != nil {
		bs.hubDispatchPending.Store(false)
		if dedupKey != "" {
			o.m.mu.Lock()
			delete(o.m.hubMsgDedup, dedupKey)
			o.m.mu.Unlock()
		}
		return agenthub.DispatchResult{}, err
	}
	bs.lastActive.Store(time.Now().UnixMilli())
	bs.wakeRun()
	return agenthub.DispatchResult{SessionID: sid, Accepted: true}, nil
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
