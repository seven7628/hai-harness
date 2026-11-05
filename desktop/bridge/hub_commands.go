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
	"path/filepath"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// hubAgentStatus 智能体状态点（§7.1.5 hub_agent_status）。CRUD 只发 idle/deleted，
// running/waiting/error 由运行态（P1-04+）发。
const (
	hubStatusIdle    = "idle"
	hubStatusDeleted = "deleted"
)

// dispatchHub 处理 hub_* 命令；不是 hub 命令时返回 false（调用方继续走既有 switch）。
func (m *manager) dispatchHub(c command) bool {
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

// validateHubProvider provider 必须已配置且未删除（§5.1）。
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
	// loop 重建属于会话生命周期（P1-04）：此刻没有在跑的 loop，改了就是「下一个 Run 生效」。
	m.emitHubAgentStatus(string(next.ID), hubStatusIdle)
	m.resp(c, true, "", map[string]any{"agent": next})
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
	m.resp(c, true, "", map[string]any{"agents": out})
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
