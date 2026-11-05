package main

// hub_commands.go：Agent Hub 的智能体 CRUD 命令（方案 §7.1.2，P1-03）。
//
// 命令面很薄：校验 → 领域包（agenthub）保存 → 事件。**不**在这里做运行态收尾
//（停会话/取消任务/重建 loop）—— 那是 P1-04 起的会话生命周期，CRUD 只保证
//「磁盘上的 profile 是什么」。存储根 `<appData>/hub`（= ~/.go-code/hub）。
//
// 校验口径（§5.1，全部同步返回错误、不静默）：
//   - provider 必须已配置且未删除（settings.json provider 段，见 providerExists）；
//   - name 1–32 字符且不区分大小写唯一（ValidateAgentNameUnique）；
//   - compress_threshold 等字段由 AgentProfile.Normalize 钳位/补默认；
//   - 总数上限 MaxAgents（软删的不占名额）。
//
// 版本：create 落 version=1；update 先把**旧** profile 追加进
// agents/<id>/profile_versions.jsonl 再 version++（§5.1，保留最近 50 版）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
)

// hubAgentStatus 智能体状态点（§7.1.5 hub_agent_status）。CRUD 只发 idle/deleted，
// running/waiting/error 由运行态（P1-04+）发。
const (
	hubStatusIdle    = "idle"
	hubStatusDeleted = "deleted"
)

// dispatchHub 处理 hub_* 命令；不是 hub 命令时返回 false（调用方继续走既有 switch）。
func (m *manager) dispatchHub(c command) bool {
	if c.Type == CmdHubOpen {
		m.ensureHubRecovery()
	}
	switch c.Type {
	case CmdHubAgentCreate:
		m.hubAgentCreate(c)
	case CmdHubAgentUpdate:
		m.hubAgentUpdate(c)
	case CmdHubAgentGet:
		m.hubAgentGet(c)
	case CmdHubAgentList:
		m.hubAgentList(c)
	case CmdHubAgentDelete:
		m.hubAgentDelete(c)
	case CmdHubAgentRestore:
		m.hubAgentRestore(c)
	case CmdHubAgentVersions:
		m.hubAgentVersions(c)
	case CmdHubAgentRollback:
		m.hubAgentRollback(c)
	case CmdHubPromptPreview:
		m.hubPromptPreview(c)
	case CmdHubAgentMemory:
		m.hubAgentMemory(c)
	case CmdHubAgentMemoryWrite:
		m.hubAgentMemoryWrite(c)
	case CmdHubOpen:
		m.hubOpen(c)
	case CmdHubSend:
		m.hubSend(c)
	case CmdHubHistory:
		m.hubHistory(c)
	case CmdHubTaskList:
		m.hubTaskList(c)
	case CmdHubTaskGet:
		m.hubTaskGet(c)
	case CmdHubTaskCancel:
		m.hubTaskCancel(c)
	case CmdHubTaskRetry:
		m.hubTaskRetry(c)
	case CmdHubNeedList:
		m.hubNeedList(c)
	case CmdHubNeedResolve:
		m.hubNeedResolve(c)
	case CmdHubClear:
		m.hubClear(c)
	case CmdHubGroupList:
		m.hubGroupList(c)
	case CmdHubGroupGet:
		m.hubGroupGet(c)
	case CmdHubGroupCreate:
		m.hubGroupCreate(c)
	case CmdHubGroupUpdate:
		m.hubGroupUpdate(c)
	case CmdHubGroupDelete:
		m.hubGroupDelete(c)
	case CmdHubGroupMemberAdd:
		m.hubGroupMemberAdd(c)
	case CmdHubGroupMemberRemove:
		m.hubGroupMemberRemove(c)
	case CmdHubGroupHistory:
		m.hubGroupHistory(c)
	case CmdHubGroupActivity:
		m.hubGroupActivity(c)
	default:
		return false
	}
	return true
}

// hubStore 懒开存储（首次用到 hub 命令时创建 ~/.go-code/hub）。
// 打开失败（schema 过新/目录不可写）不缓存：修好之后下一次命令能自愈。
func (m *manager) hubStore() (*agenthub.FileStore, error) {
	m.mu.Lock()
	if m.hubFS != nil {
		st := m.hubFS
		m.mu.Unlock()
		return st, nil
	}
	m.mu.Unlock()

	st, err := agenthub.OpenFileStore(filepath.Join(appDataDir(), "hub"))
	if err != nil {
		return nil, fmt.Errorf("打开 Hub 存储失败: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hubFS != nil { // 并发首次打开：用先到的那个，丢掉自己这份
		return m.hubFS, nil
	}
	m.hubFS = st
	return st, nil
}

// decodeHubProfile 把命令 payload 的 JSON 对象解成 AgentProfile（含 patch 的部分字段）。
func decodeHubProfile(v any) (agenthub.AgentProfile, error) {
	var p agenthub.AgentProfile
	if v == nil {
		return p, fmt.Errorf("缺少 profile")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return p, fmt.Errorf("profile 不可序列化: %w", err)
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("profile 格式错误: %w", err)
	}
	return p, nil
}

// validateHubProvider provider 必须存在且未删除（§5.1；不要求已填 key ——
// 无 key 的 profile 允许保存，派发/运行时由 §6.2.4 报「配置失效，去设置」）。
// 空值单独说：那是「没填」，不是「填错了」。
func (m *manager) validateHubProvider(provider string) error {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return fmt.Errorf("provider 不能为空")
	}
	if !m.provCfg.providerExists(provider) {
		return fmt.Errorf("provider %q 未配置或已删除，请先在设置里配置", provider)
	}
	return nil
}

// liveAgents 非软删的智能体（总数上限与重名校验都只看它们：
// 删掉的智能体不该永久占名额，也不该阻止同名重建）。
func liveAgents(all []*agenthub.AgentProfile) []*agenthub.AgentProfile {
	out := make([]*agenthub.AgentProfile, 0, len(all))
	for _, p := range all {
		if p != nil && !p.Deleted {
			out = append(out, p)
		}
	}
	return out
}

// hubAgentCreate 新建智能体：分配 id → 校验 → 落盘 version=1 → 事件。
func (m *manager) hubAgentCreate(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	p, err := decodeHubProfile(c.Payload["profile"])
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	if err := m.validateHubProvider(p.Provider); err != nil {
		m.respCode(c, false, err.Error(), "provider_not_found", nil)
		return
	}
	all, err := st.ListAgents(ctx)
	if err != nil {
		m.resp(c, false, "读取智能体列表失败: "+err.Error(), nil)
		return
	}
	live := liveAgents(all)
	if err := agenthub.ValidateAgentCount(live); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	// create 的契约是「生成 id」（§7.1.2）：**忽略**客户端传来的 id。
	// 否则一条带 stale id 的重复提交会静默覆盖已有智能体（version 归 1、不归档、
	// 记忆与 DM 记录仍挂在同一 id 上）—— 实测可完整替换旧 profile（P1-03 复核）。
	p.ID = ""
	id, err := st.AllocateID(ctx, agenthub.PrefixAgent)
	if err != nil {
		m.resp(c, false, "分配智能体 ID 失败: "+err.Error(), nil)
		return
	}
	p.ID = agenthub.AgentID(id)
	if err := p.Normalize(); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	if err := agenthub.ValidateAgentNameUnique(p.Name, live, p.ID); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	now := time.Now().UTC()
	p.Version = 1
	p.Deleted = false
	p.CreatedAt, p.UpdatedAt = now, now
	if err := st.SaveAgent(ctx, &p); err != nil {
		m.resp(c, false, "保存智能体失败: "+err.Error(), nil)
		return
	}
	m.emitHubAgentStatus(string(p.ID), hubStatusIdle)
	m.resp(c, true, "", map[string]any{"agent": p})
}

// hubAgentUpdate 局部更新：patch 只覆盖给出的字段 → 校验 → 旧版本留档 → version++。
func (m *manager) hubAgentUpdate(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.AgentID(str(c.Payload, "agent_id"))
	if id == "" {
		m.resp(c, false, "agent_id 不能为空", nil)
		return
	}
	cur, err := st.LoadAgent(ctx, id)
	if err != nil {
		m.resp(c, false, "读取智能体失败: "+err.Error(), nil)
		return
	}
	// patch 合并：把 JSON 解进**深拷贝**，未给的字段保持原值（json 零值不会覆盖）。
	// 必须深拷贝而不是 `next := *cur`：切片字段共享底层数组，json 解复用数组写元素，
	// 会在归档前把 cur（旧版本）一起改掉（P1-03 复核实测：patch 只给 1 个 disable 项，
	// 归档里的旧数组变成 [新项, 旧2, 旧3]）。
	next := *agenthub.CloneAgentProfile(cur)
	if raw, ok := c.Payload["patch"]; ok && raw != nil {
		if err := mergeJSONInto(raw, &next); err != nil {
			m.resp(c, false, "patch 格式错误: "+err.Error(), nil)
			return
		}
	}
	next.ID = cur.ID // id 不可改（§5.1）
	// deleted 只能经 delete 命令改：patch 里带着它要么静默软删、要么绕过回收站复活。
	next.Deleted = cur.Deleted
	if strings.TrimSpace(next.Provider) == "" {
		next.Provider = cur.Provider
	}
	if err := m.validateHubProvider(next.Provider); err != nil {
		m.respCode(c, false, err.Error(), "provider_not_found", nil)
		return
	}
	all, err := st.ListAgents(ctx)
	if err != nil {
		m.resp(c, false, "读取智能体列表失败: "+err.Error(), nil)
		return
	}
	if err := next.Normalize(); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	// provider/model 是原子对（P1-10 B-1）：只改 provider 时旧 model 可能不存在于
	// 新 provider（openai/deepseek-v4-pro 这种组合会让 loop 装配出无效模型）。
	// 不静默改写调用方给的 model，明确报错让其重新选择。
	if !strings.EqualFold(next.Provider, cur.Provider) && !m.provCfg.hasModelIn(next.Provider, next.Model) {
		m.respCode(c, false, fmt.Sprintf("模型 %q 不属于 provider %q，请重新选择", next.Model, next.Provider), "unknown_model", nil)
		return
	}
	if !strings.EqualFold(next.Name, cur.Name) {
		if err := agenthub.ValidateAgentNameUnique(next.Name, liveAgents(all), cur.ID); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
	}
	// 旧版本先留档：写归档失败就别改当前版本，否则「回滚」会缺一档。
	if err := st.AppendProfileVersion(ctx, cur); err != nil {
		m.resp(c, false, "保存历史版本失败: "+err.Error(), nil)
		return
	}
	next.Version = cur.Version + 1
	next.UpdatedAt = time.Now().UTC()
	next.CreatedAt = cur.CreatedAt
	if err := st.SaveAgent(ctx, &next); err != nil {
		m.resp(c, false, "保存智能体失败: "+err.Error(), nil)
		return
	}
	// 人格/记忆开关/阈值都进 loop：改完就把已装配会话的 loop 换掉（下一个 Run 生效）。
	// 不重建的话，「关闭记忆」这类改动要等到重启或下一次写记忆才生效 —— 面板说关了、
	// 模型照旧看得到（§6.11.2）。
	data := map[string]any{"agent": next, "effective": "next_run"}
	// 资料页改 provider/model/effort：已装配会话的 bs.* 是会话创建时固定的，
	// 不同步的话 applyHubSessionLoop 会继续用旧值（P1-10 评审 B4）。
	m.syncHubSessionProfile(string(next.ID), cur, &next)
	// 工具开关（preset/disable/allow/MCP）是 engine 级可见性，必须重新应用；
	// bash_roots / policy 进了装配期沙箱，只能等下次会话（如实返回 next_session）。
	if hubEngineNeedsRebuild(cur, &next) {
		data["effective"] = "next_session"
		data["warning"] = "bash_roots / policy 变更需要重建会话后生效"
	} else if err := m.refreshHubEngines(string(next.ID), &next); err != nil {
		data["effective"] = "next_session"
		data["warning"] = "重新应用工具策略失败，重建会话后生效: " + err.Error()
	}
	if err := m.refreshHubLoops(string(next.ID)); err != nil {
		data["effective"] = "next_session"
		data["warning"] = "重建会话 loop 失败，重启后生效: " + err.Error()
	}
	// 有 run 正在跑时不能发 idle：前端把 hub_agent_status 映射成运行环，会把正在跑的
	// 智能体显示成空闲（P1-07 复核 N9）。收尾由 agent_end 自己负责。
	if !m.hubAgentRunning(string(next.ID)) {
		m.emitHubAgentStatus(string(next.ID), hubStatusIdle)
	}
	m.resp(c, true, "", data)
}

// hubAgentGet 返回完整 profile（运行态/今日用量是 P1-04+ 的字段，这里不编造）。
func (m *manager) hubAgentGet(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.AgentID(str(c.Payload, "agent_id"))
	if id == "" {
		m.resp(c, false, "agent_id 不能为空", nil)
		return
	}
	p, err := st.LoadAgent(ctx, id)
	if err != nil {
		m.resp(c, false, "读取智能体失败: "+err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"agent": p})
}

// hubAgentList 轻量列表（左栏）。默认不含软删；include_deleted=true 时全给（设置页恢复用）。
func (m *manager) hubAgentList(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	all, err := st.ListAgents(ctx)
	if err != nil {
		m.resp(c, false, "读取智能体列表失败: "+err.Error(), nil)
		return
	}
	includeDeleted := c.Payload["include_deleted"] == true
	out := make([]*agenthub.AgentProfile, 0, len(all))
	for _, p := range all {
		if p == nil {
			continue
		}
		if p.Deleted && !includeDeleted {
			continue
		}
		out = append(out, p)
	}
	// 左栏预览/未读（§6.4）：DM 索引维护 last_message，不读整个 transcript。
	// 与 agents 并列返回（不塞进 profile —— profile 是配置，预览是投影）。
	dm := map[string]map[string]any{}
	for _, p := range out {
		unread, last, derr := st.DMUnread(ctx, p.ID)
		if derr != nil {
			continue
		}
		item := map[string]any{"unread": unread}
		if last != nil {
			item["last_seq"] = last.Seq
			item["last_text"] = truncateRunes(strings.Join(strings.Fields(last.Text), " "), hubPreviewRunes)
			item["last_sender"] = last.Sender.Kind
			item["last_at"] = last.CreatedAt
		}
		dm[string(p.ID)] = item
	}
	m.resp(c, true, "", map[string]any{"agents": out, "dm": dm})
}

// hubPreviewRunes 左栏预览的最大字数（一条摘要，不整段塞给列表）。
const hubPreviewRunes = 80

// truncateRunes 按**字符**截断（不是字节）：中文预览不能切半个字。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// hubAgentDelete 删除：默认软删（DM/群记录与记忆保留），purge=true 才物理删目录。
func (m *manager) hubAgentDelete(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.AgentID(str(c.Payload, "agent_id"))
	if id == "" {
		m.resp(c, false, "agent_id 不能为空", nil)
		return
	}
	cur, err := st.LoadAgent(ctx, id)
	purge := c.Payload["purge"] == true
	// purge 幂等：目录已经没了就算成功（双击/重试不该弹「not found」）。
	if err != nil && purge && errors.Is(err, agenthub.ErrNotFound) {
		m.resp(c, true, "", map[string]any{"agent_id": string(id), "purged": true})
		return
	}
	if err != nil {
		m.resp(c, false, "读取智能体失败: "+err.Error(), nil)
		return
	}
	if purge {
		if err := st.DeleteAgent(ctx, id); err != nil {
			m.resp(c, false, "删除智能体目录失败: "+err.Error(), nil)
			return
		}
	} else {
		if cur.Deleted { // 幂等：已经在回收站，不重复加版本
			m.resp(c, true, "", map[string]any{"agent_id": string(id), "purged": false})
			return
		}
		if err := st.AppendProfileVersion(ctx, cur); err != nil {
			m.resp(c, false, "保存历史版本失败: "+err.Error(), nil)
			return
		}
		cur.Deleted = true
		cur.Version++
		cur.UpdatedAt = time.Now().UTC()
		if err := st.SaveAgent(ctx, cur); err != nil {
			m.resp(c, false, "保存智能体失败: "+err.Error(), nil)
			return
		}
	}
	m.emitHubAgentStatus(string(id), hubStatusDeleted)
	m.resp(c, true, "", map[string]any{"agent_id": string(id), "purged": purge})
}

// hubAgentRestore 从回收站恢复（§5.2：软删保留记录，可从设置里恢复）。
//
// 为什么必须单独一条命令：update 的 patch 被禁止改 deleted（否则能绕过 delete 静默
// 软删/复活），于是「恢复」需要明确入口 —— 否则 §5.2 的「可恢复」只在数据层成立。
func (m *manager) hubAgentRestore(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.AgentID(str(c.Payload, "agent_id"))
	if id == "" {
		m.resp(c, false, "agent_id 不能为空", nil)
		return
	}
	cur, err := st.LoadAgent(ctx, id)
	if err != nil {
		m.resp(c, false, "读取智能体失败: "+err.Error(), nil)
		return
	}
	// 幂等：没删过就是成功（UI 连点不该报错）。
	if !cur.Deleted {
		m.resp(c, true, "", map[string]any{"agent": cur})
		return
	}
	// 同名冲突：回收站里的名字可能已被新智能体占用（软删不占名额）。
	all, err := st.ListAgents(ctx)
	if err != nil {
		m.resp(c, false, "读取智能体列表失败: "+err.Error(), nil)
		return
	}
	if err := agenthub.ValidateAgentNameUnique(cur.Name, liveAgents(all), cur.ID); err != nil {
		m.resp(c, false, "名字已被占用，先改名再恢复: "+err.Error(), nil)
		return
	}
	if err := st.AppendProfileVersion(ctx, cur); err != nil {
		m.resp(c, false, "保存历史版本失败: "+err.Error(), nil)
		return
	}
	next := *agenthub.CloneAgentProfile(cur)
	next.Deleted = false
	next.Version = cur.Version + 1
	next.UpdatedAt = time.Now().UTC()
	if err := st.SaveAgent(ctx, &next); err != nil {
		m.resp(c, false, "保存智能体失败: "+err.Error(), nil)
		return
	}
	m.emitHubAgentStatus(string(next.ID), hubStatusIdle)
	m.resp(c, true, "", map[string]any{"agent": next})
}

// hubAgentMemory 记忆面板：不带 path = 列清单，带 path = 读内容（不存在时 exists=false）。
//
// 记忆放在 agents/<id>/memory（不在沙箱放开的 home 里，§9.2）：面板读写走这条命令，
// 智能体的文件工具碰不到自己的记忆文件，只有注入进系统提示词的索引/正文可见。
func (m *manager) hubAgentMemory(c command) {
	agentID := str(c.Payload, "agent_id")
	if agentID == "" {
		m.resp(c, false, "agent_id 不能为空", nil)
		return
	}
	p, _, err := m.hubLoadAgent(agentID)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	root := filepath.Join(appDataDir(), "hub")
	rel := strings.TrimSpace(str(c.Payload, "path"))
	if rel == "" {
		entries, err := agenthub.ListMemory(root, p.ID)
		if err != nil {
			m.resp(c, false, "读取记忆目录失败: "+err.Error(), nil)
			return
		}
		dir, _ := agenthub.MemoryAgentDir(root, p.ID)
		m.resp(c, true, "", map[string]any{"entries": entries, "dir": dir})
		return
	}
	content, err := agenthub.ReadMemoryFile(root, p.ID, rel)
	if errors.Is(err, agenthub.ErrNotFound) {
		// 新文件：ok=true + exists=false（面板据此进「新建」而不是报错）。
		m.resp(c, true, "", map[string]any{"path": rel, "content": "", "exists": false})
		return
	}
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"path": rel, "content": content, "exists": true})
}

// hubAgentMemoryWrite 写记忆文件：写前备份 → 原子落盘 → 重建该智能体的 loop。
//
// 生效时机（§6.9 / 验收项）：记忆索引是在**装配 loop 时**注入 system prompt 的，
// 所以写完必须重建 loop；已经在跑的 Run 用旧 loop 收尾（不打断中间状态）。
func (m *manager) hubAgentMemoryWrite(c command) {
	agentID := str(c.Payload, "agent_id")
	if agentID == "" {
		m.resp(c, false, "agent_id 不能为空", nil)
		return
	}
	rel := strings.TrimSpace(str(c.Payload, "path"))
	if rel == "" {
		m.resp(c, false, "path 不能为空", nil)
		return
	}
	content, ok := c.Payload["content"].(string)
	if !ok {
		m.resp(c, false, "content 必须是字符串", nil)
		return
	}
	p, _, err := m.hubLoadAgent(agentID)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	root := filepath.Join(appDataDir(), "hub")
	res, err := agenthub.WriteMemoryFile(root, p.ID, rel, content)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	// 重建失败不回滚写入（文件已经落盘）：把错误带回去让面板提示「重启后生效」。
	rebuildErr := m.refreshHubLoops(agentID)
	data := map[string]any{
		"path": res.Path, "backup": res.Backup, "bytes": res.Bytes,
		"effective": "next_run", // 已有的 Run 用旧 loop 收尾
	}
	if rebuildErr != nil {
		data["effective"] = "next_session"
		data["warning"] = "重建会话 loop 失败，重启后生效: " + rebuildErr.Error()
	}
	m.resp(c, true, "", data)
}

// hubAgentVersions 历史版本清单 + 当前版本（设置页「人格历史」）。
//
// 只回摘要不回整份 profile：最多 50 版，带上 Instructions/Rules 会让响应膨胀到几百 KB，
// 而列表只需要「第几版、什么时候、叫什么、用哪个模型」。回滚用 version 号引回快照。
func (m *manager) hubAgentVersions(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.AgentID(str(c.Payload, "agent_id"))
	if id == "" {
		m.resp(c, false, "agent_id 不能为空", nil)
		return
	}
	cur, err := st.LoadAgent(ctx, id)
	if err != nil {
		m.resp(c, false, "读取智能体失败: "+err.Error(), nil)
		return
	}
	limit := intNum(c.Payload, "limit", 20)
	versions, err := st.ListProfileVersions(ctx, id, limit)
	if err != nil {
		m.resp(c, false, "读取历史版本失败: "+err.Error(), nil)
		return
	}
	items := make([]map[string]any, 0, len(versions))
	for i := len(versions) - 1; i >= 0; i-- { // 新的在前：UI 从头往下看
		v := versions[i]
		if v.Profile == nil || v.Profile.Name == "" {
			continue // 坏行/空 profile（历史文件被手工改过）：列表跳过，不阻断
		}
		items = append(items, map[string]any{
			"version":  v.Version,
			"saved_at": v.SavedAt,
			"name":     v.Profile.Name,
			"role":     v.Profile.Role,
			"provider": v.Profile.Provider,
			"model":    v.Profile.Model,
			"deleted":  v.Profile.Deleted,
		})
	}
	m.resp(c, true, "", map[string]any{"agent_id": string(id), "current": cur.Version, "versions": items})
}

// hubAgentRollback 回到某个历史版本（§5.1「人格改坏了回滚」）。
//
// 语义是「把那一版的内容落成一个**新版本**」，不是把 version 号改回去：
//   - 当前版本先留档，回滚本身也可再回滚（不会因为一次误点丢两版）；
//   - version 单调递增，DM 记录/任务引用不受影响（它们只认 agent_id）。
func (m *manager) hubAgentRollback(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.AgentID(str(c.Payload, "agent_id"))
	if id == "" {
		m.resp(c, false, "agent_id 不能为空", nil)
		return
	}
	// 协议里 JSON 解出来是 float64，脚本/测试常给 int；非整数（2.9）必须拒绝而不是
	// 静默截断成 2 然后真的回滚（P1-06 复核 N9）。
	version := intNum(c.Payload, "version", 0)
	if f, ok := c.Payload["version"].(float64); ok && f != math.Trunc(f) {
		version = 0
	}
	if version <= 0 {
		m.resp(c, false, "version 必须是正整数", nil)
		return
	}
	cur, err := st.LoadAgent(ctx, id)
	if err != nil {
		m.resp(c, false, "读取智能体失败: "+err.Error(), nil)
		return
	}
	if cur.Deleted {
		m.resp(c, false, "智能体已删除，先恢复再回滚", nil)
		return
	}
	if version == cur.Version {
		m.resp(c, false, "这已经是当前版本，无需回滚", nil)
		return
	}
	all, err := st.ListProfileVersions(ctx, id, 0)
	if err != nil {
		m.resp(c, false, "读取历史版本失败: "+err.Error(), nil)
		return
	}
	var snap *agenthub.ProfileVersion
	for i := range all {
		if all[i].Version == version {
			snap = &all[i] // 版本号单调递增，命中即唯一
			break
		}
	}
	if snap == nil || snap.Profile == nil {
		m.resp(c, false, fmt.Sprintf("找不到第 %d 版（可能已被裁剪，最多保留 %d 版）", version, agenthub.MaxProfileVersions), nil)
		return
	}
	// 深拷贝：归档的切片不能与将保存的 profile 共享底层数组。
	next := *agenthub.CloneAgentProfile(snap.Profile)
	next.ID = cur.ID
	next.CreatedAt = cur.CreatedAt
	next.Deleted = cur.Deleted // 回滚不改回收站状态（软删恢复有自己的命令）
	if strings.TrimSpace(next.Provider) == "" {
		next.Provider = cur.Provider
	}
	if err := m.validateHubProvider(next.Provider); err != nil {
		m.respCode(c, false, err.Error(), "provider_not_found", nil)
		return
	}
	if err := next.Normalize(); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	if !strings.EqualFold(next.Name, cur.Name) {
		agents, aerr := st.ListAgents(ctx)
		if aerr != nil {
			m.resp(c, false, "读取智能体列表失败: "+aerr.Error(), nil)
			return
		}
		if err := agenthub.ValidateAgentNameUnique(next.Name, liveAgents(agents), cur.ID); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
	}
	if err := st.AppendProfileVersion(ctx, cur); err != nil {
		m.resp(c, false, "保存历史版本失败: "+err.Error(), nil)
		return
	}
	next.Version = cur.Version + 1
	next.UpdatedAt = time.Now().UTC()
	if err := st.SaveAgent(ctx, &next); err != nil {
		m.resp(c, false, "保存智能体失败: "+err.Error(), nil)
		return
	}
	// 人格变了 → 已装配的会话按新 profile 重建 loop（下一个 Run 生效）。
	data := map[string]any{"agent": next, "restored_from": version, "effective": "next_run"}
	// 回滚同样可能改 provider/model/effort 与工具策略：与 update 走同一套同步。
	m.syncHubSessionProfile(string(next.ID), cur, &next)
	if hubEngineNeedsRebuild(cur, &next) {
		data["effective"] = "next_session"
		data["warning"] = "bash_roots / policy 变更需要重建会话后生效"
	} else if err := m.refreshHubEngines(string(next.ID), &next); err != nil {
		data["effective"] = "next_session"
		data["warning"] = "重新应用工具策略失败，重建会话后生效: " + err.Error()
	}
	if err := m.refreshHubLoops(string(next.ID)); err != nil {
		data["effective"] = "next_session"
		data["warning"] = "重建会话 loop 失败，重启后生效: " + err.Error()
	}
	if !m.hubAgentRunning(string(next.ID)) {
		m.emitHubAgentStatus(string(next.ID), hubStatusIdle)
	}
	m.resp(c, true, "", data)
}

// hubPromptPreview 系统提示词分层预览（设置页「提示词」页签）。
//
// 与真正下发的提示词同源：L1–L3 走 agenthub.RenderLayers，L4 记忆走装配用的同一个
// hubMemoryInjection。L5 技能清单与工具 schema 由内核在请求期拼装，**不复制**到这里
// （复制就会变成第二份事实源），在 note 里写明。
func (m *manager) hubPromptPreview(c command) {
	p, _, err := m.hubLoadAgent(str(c.Payload, "agent_id"))
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	home := hubAgentHome(string(p.ID))
	layers := agenthub.RenderLayers(agenthub.PromptSpec{Agent: p})
	// 内核在系统提示词尾部还会追加「## Current Workspace」与 home 里的 AGENTS.md：
	// 不预览这两段，「用户看到的 = 模型看到的」就不成立（P1-06 复核实测差 94 字节，
	// 写 AGENTS.md 后差更多）。顺序与 agents.composeSystemPrompt 一致。
	var env strings.Builder
	fmt.Fprintf(&env, "## Current Workspace\n- %s", home)
	if body, err := os.ReadFile(filepath.Join(home, "AGENTS.md")); err == nil && len(body) > 0 {
		env.WriteString("\n\n")
		env.Write(body)
	}
	layers = append(layers, agenthub.PromptLayer{Key: "environment", Title: "L3 · 环境与 AGENTS.md", Text: env.String()})
	if inj := m.hubMemoryInjection(p, home); strings.TrimSpace(inj) != "" {
		layers = append(layers, agenthub.PromptLayer{Key: "memory", Title: "L4 · 记忆", Text: inj})
	}
	total := 0
	for _, layer := range layers {
		total += len(layer.Text)
	}
	m.resp(c, true, "", map[string]any{
		"agent_id":    string(p.ID),
		"layers":      layers,
		"total_bytes": total,
		"note":        "L0 对 Hub 已关闭（D17）；L5 技能清单与工具 schema 在请求期由内核装配，不在预览内。",
	})
}

// emitHubAgentStatus 状态点事件（左栏状态点/头像动效，§7.1.5）。
func (m *manager) emitHubAgentStatus(agentID, status string) {
	if m.out == nil || agentID == "" {
		return
	}
	m.out.writeLine(map[string]any{
		"event_type": "hub_agent_status",
		"agent_id":   agentID,
		"status":     status,
	})
}

// mergeJSONInto 把 JSON 值合并进结构体（只覆盖 payload 里出现的字段）。
func mergeJSONInto(v any, dst any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

// hubTarget 解析 DM 目标（§7.1.1 的 target 形状 + 兼容顶层 agent_id）。
// 群组是 P3，这里只认 dm；其它 kind 明确报错而不是静默当成 DM。
func hubTarget(payload map[string]any) (kind string, agentID string, err error) {
	kind = "dm"
	target, _ := payload["target"].(map[string]any)
	if target != nil {
		if k, _ := target["kind"].(string); k != "" {
			kind = k
		}
		agentID, _ = target["agent_id"].(string)
	}
	if agentID == "" {
		agentID, _ = payload["agent_id"].(string)
	}
	if kind != "dm" {
		return kind, "", fmt.Errorf("暂只支持 kind=dm（群组在 P3）")
	}
	if strings.TrimSpace(agentID) == "" {
		return kind, "", fmt.Errorf("agent_id 不能为空")
	}
	return kind, agentID, nil
}

// hubLoadAgent 读 profile 并拒绝已软删的（UI 上删除的智能体不能再聊）。
func (m *manager) hubLoadAgent(agentID string) (*agenthub.AgentProfile, *agenthub.FileStore, error) {
	st, err := m.hubStore()
	if err != nil {
		return nil, nil, err
	}
	p, err := st.LoadAgent(context.Background(), agenthub.AgentID(agentID))
	if err != nil {
		return nil, nil, fmt.Errorf("读取智能体失败: %w", err)
	}
	if p.Deleted {
		return nil, nil, fmt.Errorf("智能体已删除")
	}
	return p, st, nil
}

// hubOpen 打开 DM：返回最近一页 + 会话 ID + 游标，并推进未读。
//
// **不**在这里创建会话（§6.4：第一次发消息才懒创建）：只是打开看历史时不该起 loop、
// 不该读 provider key。session_id 是确定性 ID，前端可以先拿到它。
func (m *manager) hubOpen(c command) {
	ctx := context.Background()
	_, agentID, err := hubTarget(c.Payload)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	p, st, err := m.hubLoadAgent(agentID)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	beforeSeq := int64(numOr(c.Payload, "before_seq", 0))
	limit := int(numOr(c.Payload, "limit", hubDMPageSize))
	msgs, hasMore, err := m.hubDMPage(ctx, st, agenthub.AgentID(agentID), beforeSeq, limit)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	// 打开 = 人看到这一页 → 推进已读到最后一条（只前进）。
	// mark_read=false 供后台预热/预取用（§7.1.1 的 hub_mark_read 语义）。
	markRead := true
	if v, ok := c.Payload["mark_read"].(bool); ok {
		markRead = v
	}
	if markRead && len(msgs) > 0 {
		if err := st.SaveDMCursor(ctx, agenthub.AgentID(agentID), msgs[len(msgs)-1].Seq); err != nil {
			m.resp(c, false, "保存已读位置失败: "+err.Error(), nil)
			return
		}
	}
	cur, err := st.LoadDMCursor(ctx, agenthub.AgentID(agentID))
	if err != nil {
		m.resp(c, false, "读取已读位置失败: "+err.Error(), nil)
		return
	}
	// 只读路径：不建运行时（没有就说明还没聊过，exists=false）。
	var exists bool
	if ws := m.hubRuntimeIfExists(agentID); ws != nil {
		ws.mu.Lock()
		exists = ws.sessions[hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeDM})] != nil
		ws.mu.Unlock()
	}
	sid := hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeDM})
	m.resp(c, true, "", map[string]any{
		"target":     map[string]any{"kind": "dm", "agent_id": agentID},
		"agent":      p,
		"session_id": sid,
		"messages":   msgs,
		"has_more":   hasMore,
		"cursor":     cur,
		"exists":     exists,
	})
}

// hubHistory 向上翻页（seq 游标，不用 offset）。不推进已读 —— 翻历史不等于「已看到最新」。
func (m *manager) hubHistory(c command) {
	ctx := context.Background()
	agentID := str(c.Payload, "agent_id")
	if agentID == "" {
		_, agentID, _ = hubTarget(c.Payload)
	}
	if agentID == "" {
		m.resp(c, false, "agent_id 不能为空", nil)
		return
	}
	if _, _, err := m.hubLoadAgent(agentID); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	beforeSeq := int64(numOr(c.Payload, "before_seq", 0))
	limit := int(numOr(c.Payload, "limit", hubDMPageSize))
	msgs, hasMore, err := m.hubDMPage(ctx, st, agenthub.AgentID(agentID), beforeSeq, limit)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"agent_id": agentID, "messages": msgs, "has_more": hasMore})
}

// hubDMPage 取一页并算出 has_more（多读一条判断，不额外全量扫描）。
func (m *manager) hubDMPage(ctx context.Context, st *agenthub.FileStore, agentID agenthub.AgentID, beforeSeq int64, limit int) ([]agenthub.Message, bool, error) {
	if limit <= 0 {
		limit = hubDMPageSize
	}
	msgs, err := st.ReadDirectMessages(ctx, agentID, beforeSeq, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("读取 DM 失败: %w", err)
	}
	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs, hasMore, nil
}

// hubSend 人发一条 DM：索引落账 → 确保会话 → Ask + wake。
//
// client_msg_id 幂等（§7.1.3）：断线重连重发同一条不会重复入模型上下文，
// 直接回原来的 seq。
func (m *manager) hubSend(c command) {
	ctx := context.Background()
	_, agentID, err := hubTarget(c.Payload)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	text := strings.TrimSpace(str(c.Payload, "text"))
	clientMsgID := str(c.Payload, "client_msg_id")
	if text == "" {
		m.resp(c, false, "text 不能为空", nil)
		return
	}
	if _, st, err := m.hubLoadAgent(agentID); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	} else if clientMsgID != "" {
		if seq, dup, _ := st.FindDirectMessageByClientID(ctx, agenthub.AgentID(agentID), clientMsgID); dup {
			m.resp(c, true, "", map[string]any{"seq": seq, "deduplicated": true,
				"session_id": hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeDM})})
			return
		}
	}
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	msg := &agenthub.Message{
		Kind:   agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
		Text:   text,
	}
	if clientMsgID != "" {
		msg.Refs = map[string]any{"client_msg_id": clientMsgID}
	}
	// 先把消息交给会话（Ask 只入队，失败会立刻返回），成功后才落索引：
	// 反过来的话 Ask 失败会留下「索引里有、上下文里没有」的消息，重试又被
	// client_msg_id 幂等挡掉 —— 用户看到已发送，模型永远收不到（P1-04 复核）。
	if err := bs.s.Ask(ctx, core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: text})); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	seq, err := m.hubStoreSeq(func(st *agenthub.FileStore) (int64, error) {
		return st.AppendDirectMessage(ctx, agenthub.AgentID(agentID), msg)
	})
	if err != nil {
		m.resp(c, false, "保存消息失败: "+err.Error(), nil)
		return
	}
	// 人发出去的消息 = 人已经看到这里。
	_ = m.hubStoreCursor(agentID, seq)
	m.emitHubMessage(agentID, seq, msg, clientMsgID)
	bs.lastActive.Store(time.Now().UnixMilli())
	bs.wakeRun()
	m.resp(c, true, "", map[string]any{"seq": seq, "session_id": bs.id})
}

// hubStoreSeq 在 hubStore 上执行一次写（避免每个调用点重复 if 展开）。
func (m *manager) hubStoreSeq(fn func(*agenthub.FileStore) (int64, error)) (int64, error) {
	st, err := m.hubStore()
	if err != nil {
		return 0, err
	}
	return fn(st)
}

func (m *manager) hubStoreCursor(agentID string, seq int64) error {
	st, err := m.hubStore()
	if err != nil {
		return err
	}
	return st.SaveDMCursor(context.Background(), agenthub.AgentID(agentID), seq)
}

// hubDMPageSize DM 默认每页条数。
const hubDMPageSize = 50

// numOr 读数字字段（JSON 解出来是 float64；调用方也可能直接给 int）。
func numOr(payload map[string]any, key string, def int64) int64 {
	switch v := payload[key].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	default:
		return def
	}
}
