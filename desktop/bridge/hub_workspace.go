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
			if e.Path == "" || isHubHomePath(e.Path) {
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
		if ws == nil || ws.kind == runtimeHubAgent || isHubHomePath(path) {
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
		abs, err := hubAbsExisting(root)
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
	path, err := hubCreateWorkspacePath(req.Parent, req.Name, roots)
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
	if o.m.wsReg != nil {
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
	}
	return agenthub.WorkspaceInfo{Path: path, Name: name, UpdatedAt: time.Now().UTC()}, nil
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
		if v, ok := row["updated_at"].(time.Time); ok {
			info.UpdatedAt = v
		}
		out = append(out, info)
	}
	return out, nil
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
	info := agenthub.SessionInfo{ID: id, Workspace: path, Title: req.Title, UpdatedAt: time.Now().UTC()}
	if info.Title == "" {
		info.Title = "新会话"
	}
	if bs != nil {
		info.Running = bs.running.Load()
	}
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
	bs := o.m.get(path, sid)
	if bs == nil {
		// 会话在盘上但工作区还没装配（重启后第一次派发）：装配一次再找，
		// 不自动建会话 —— 不存在的 session 仍然报 NotFound。
		o.m.runtime(path)
		bs = o.m.get(path, sid)
	}
	if bs == nil {
		return agenthub.DispatchResult{}, fmt.Errorf("%w: session %q", agenthub.ErrNotFound, sid)
	}
	if err := bs.s.Ask(ctx, core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: text})); err != nil {
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
	o.m.mu.Lock()
	ws := o.m.workspaces[clean]
	o.m.mu.Unlock()
	if ws != nil && ws.kind != runtimeHubAgent && !isHubHomePath(clean) {
		return clean, nil
	}
	// 已登记但还没装配 runtime（刚建好 / 重启后未打开）：允许，但目录必须在。
	if o.m.wsReg != nil && !isHubHomePath(clean) {
		for _, e := range o.m.wsReg.list() {
			if e.Path != clean {
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
	parentReal, err := hubAbsExisting(parent)
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
		return "", err
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
	clean := filepath.Clean(abs)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		clean = real
	}
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	cfgDir := filepath.Join(home, ".go-code")
	if clean == cfgDir || strings.HasPrefix(clean, cfgDir+string(os.PathSeparator)) {
		for _, sub := range []string{"plugins", "runtime"} {
			dir := filepath.Join(cfgDir, sub)
			if clean == dir || strings.HasPrefix(clean, dir+string(os.PathSeparator)) {
				return false
			}
		}
		return true
	}
	for _, name := range []string{".ssh", ".aws", ".gnupg", ".netrc", ".kube"} {
		base := filepath.Join(home, name)
		if clean == base || strings.HasPrefix(clean, base+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}
