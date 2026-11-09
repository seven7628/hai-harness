package main

// hub_search.go：Hub 全历史搜索 v1（P4-04b-1，设计 docs/agent-hub/P4-04B-DESIGN.md §1）。
//
// 人类命令，**不给智能体注册工具**：agent 只能通过 group_history 的 query 搜自己参与
// 的群（走 agent-safe 的 Store.SearchMessages），拿不到跨群/跨 DM 的全局搜索面。
// 因此「人类专属」是结构性的（命令面 vs 工具面），不是靠运行时身份判断。
//
// 口径：
//   - 人类能看到 owner-only 正文（结果里带 owner_only 标记给 UI 打标）；
//   - routing_receipt / round_summary / approval_stub 默认排除（它们只是投影）；
//   - 大小写不敏感 substring，newest-first，不排相关度；
//   - scope=all 时遍历未删除的群 + DM，逐会话取「比游标更旧」的命中，合并后按
//     (created_at, id) 倒序，全局截断 limit 并给出 next_before 游标。
//
// 并发/锁：每个会话的读取各自进出 store 锁（SearchMessagesHuman/SearchDirectMessages），
// fan-out 期间不持全局 s.mu；匹配与排序都在锁外。v1 没有索引，成本随「会话数 ×
// 单会话日志长度」线性增长 —— 见 hub_search_bench_test.go 的 1 万条实测。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/seven7628/hai-harness/agenthub"
)

const (
	hubSearchDefaultLimit = 50
	hubSearchMaxLimit     = 200
	// hubSearchPerConvMax 单会话单次最多取多少条命中。取全局上限同一个数：
	// 单会话被截断时它已经贡献了 >= limit 条命中，这些命中整体都比被截掉的新，
	// 所以全局前 limit 条不会漏；翻页用游标继续。
	hubSearchPerConvMax = hubSearchMaxLimit
	// hubSearchPagesPerConv 带游标时单会话最多向更早翻几页（每页 per+1 条）。
	// 游标是新旧混合的 (created_at,id)，刚开始翻页时要跳过本页里比游标新的命中。
	hubSearchPagesPerConv = 8
	// hubSearchSnippetRadius 片段在命中词两侧各保留多少个 rune。
	hubSearchSnippetRadius = 32
)

// hubSearchCursor 是跨会话的全局游标：只按 (created_at, id) 比较，不依赖 seq
// （seq 只在单个会话内单调）。seq 用于 scope=group/dm 时的快速路径。
type hubSearchCursor struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
	Seq       int64     `json:"seq,omitempty"`
}

// hubSearchHit 是一条搜索结果（字段与设计 §1「结果与 UI」对齐）。
type hubSearchHit struct {
	Scope         string    `json:"scope"` // group | dm
	GroupID       string    `json:"group_id,omitempty"`
	AgentID       string    `json:"agent_id,omitempty"`
	Name          string    `json:"name"`
	Seq           int64     `json:"seq"`
	MessageID     string    `json:"message_id"`
	Snippet       string    `json:"snippet"`
	Sender        string    `json:"sender"`
	SenderKind    string    `json:"sender_kind,omitempty"`
	SenderAgentID string    `json:"sender_agent_id,omitempty"`
	Kind          string    `json:"kind"`
	CreatedAt     time.Time `json:"created_at"`
	OwnerOnly     bool      `json:"owner_only"`
}

// hubSearchConversation 是 fan-out 的一个目标（群或 DM）。
type hubSearchConversation struct {
	scope   string
	groupID agenthub.GroupID
	agentID agenthub.AgentID
	name    string
}

// hubSearch 是人类搜索命令入口（human-only：智能体没有对应工具，见文件头）。
func (m *manager) hubSearch(c command) {
	ctx := context.Background()
	query := strings.TrimSpace(str(c.Payload, "query"))
	if query == "" {
		m.resp(c, false, "query 不能为空", nil)
		return
	}
	scope := strings.TrimSpace(str(c.Payload, "scope"))
	if scope == "" {
		scope = "all"
	}
	switch scope {
	case "all", "group", "dm":
	default:
		m.resp(c, false, "scope 只能是 all / group / dm", nil)
		return
	}
	limit := int(numOr(c.Payload, "limit", hubSearchDefaultLimit))
	if limit <= 0 {
		limit = hubSearchDefaultLimit
	}
	if limit > hubSearchMaxLimit {
		limit = hubSearchMaxLimit
	}
	before, err := parseHubSearchCursor(c.Payload["before"])
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	convs, err := hubSearchConversations(ctx, st, scope, strings.TrimSpace(str(c.Payload, "group_id")), strings.TrimSpace(str(c.Payload, "agent_id")))
	if err != nil {
		m.resp(c, false, "搜索目标不可用: "+err.Error(), nil)
		return
	}
	names := hubAgentNameMap(ctx, st)

	all := make([]hubSearchHit, 0, limit*2)
	truncated := false
	for _, cv := range convs {
		if err := ctx.Err(); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		fetch := func(beforeSeq int64, n int) ([]agenthub.Message, error) {
			if cv.scope == "group" {
				return st.SearchMessagesHuman(ctx, cv.groupID, query, beforeSeq, n)
			}
			return st.SearchDirectMessages(ctx, cv.agentID, query, beforeSeq, n)
		}
		var msgs []agenthub.Message
		var more bool
		var serr error
		if scope != "all" && before != nil && before.Seq > 0 {
			// 单会话翻页：游标里的 seq 就是精确的 before_seq，一次读完（不用按时间走页）。
			msgs, more, serr = searchConversationOnce(fetch, before.Seq, limit)
		} else {
			msgs, more, serr = searchConversationPages(fetch, before, limit)
		}
		if serr != nil {
			m.resp(c, false, fmt.Sprintf("搜索 %s 失败: %v", cv.name, serr), nil)
			return
		}
		truncated = truncated || more
		for _, msg := range msgs {
			all = append(all, buildHubSearchHit(cv, msg, query, names))
		}
	}
	sortHubSearchHits(all)
	if len(all) > limit {
		all = all[:limit]
		truncated = true
	}
	var next *hubSearchCursor
	// 空页不再宣称「还有更旧」：UI 会把它当成可继续翻页，翻出来还是空页。
	if truncated && len(all) > 0 {
		last := all[len(all)-1]
		next = &hubSearchCursor{CreatedAt: last.CreatedAt, ID: last.MessageID, Seq: last.Seq}
	}
	m.resp(c, true, "", map[string]any{
		"query":       query,
		"scope":       scope,
		"limit":       limit,
		"hits":        all,
		"count":       len(all),
		"has_more":    next != nil,
		"next_before": next,
		"scanned":     len(convs),
	})
}

// hubSearchConversations 解析本次 fan-out 的目标列表；scope=group/dm 时只查一个会话。
// 已删除的群/智能体一律跳过（软删后仍能搜出内容等于「删除无效」）。
func hubSearchConversations(ctx context.Context, st *agenthub.FileStore, scope, groupID, agentID string) ([]hubSearchConversation, error) {
	switch scope {
	case "group":
		if groupID == "" {
			return nil, fmt.Errorf("%w: group_id 不能为空", agenthub.ErrInvalid)
		}
		g, err := st.LoadGroup(ctx, agenthub.GroupID(groupID))
		if err != nil {
			return nil, err
		}
		if g.Deleted {
			return nil, fmt.Errorf("%w: 群组已删除", agenthub.ErrNotFound)
		}
		return []hubSearchConversation{{scope: "group", groupID: g.ID, name: g.Name}}, nil
	case "dm":
		if agentID == "" {
			return nil, fmt.Errorf("%w: agent_id 不能为空", agenthub.ErrInvalid)
		}
		p, err := st.LoadAgent(ctx, agenthub.AgentID(agentID))
		if err != nil {
			return nil, err
		}
		if p.Deleted {
			return nil, fmt.Errorf("%w: 智能体已删除", agenthub.ErrNotFound)
		}
		return []hubSearchConversation{{scope: "dm", agentID: p.ID, name: p.Name}}, nil
	}
	groups, err := st.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]hubSearchConversation, 0, len(groups)+len(agents))
	for _, g := range groups {
		if g == nil || g.Deleted {
			continue
		}
		out = append(out, hubSearchConversation{scope: "group", groupID: g.ID, name: g.Name})
	}
	for _, p := range agents {
		if p == nil || p.Deleted {
			continue
		}
		out = append(out, hubSearchConversation{scope: "dm", agentID: p.ID, name: p.Name})
	}
	return out, nil
}

// searchConversationPages 在一个会话内收集「比游标更旧」的命中（最多 limit 条）。
//
// 无游标：只取最新一页（store 返回旧→新，多取一条判断是否还有更旧命中）。
// 有游标：从最新一页开始向更早翻，丢掉比游标新的命中。返回 more=true 表示该会话
// 还有没取完的更旧命中（被单会话上限/翻页上限截断）。
func searchConversationPages(fetch func(beforeSeq int64, limit int) ([]agenthub.Message, error), cursor *hubSearchCursor, limit int) ([]agenthub.Message, bool, error) {
	per := limit
	if per <= 0 || per > hubSearchPerConvMax {
		per = hubSearchPerConvMax
	}
	if cursor == nil {
		batch, err := fetch(0, per+1)
		if err != nil {
			return nil, false, err
		}
		if len(batch) > per {
			return batch[len(batch)-per:], true, nil
		}
		return batch, false, nil
	}

	kept := make([]agenthub.Message, 0, per+1)
	before := int64(0)
	more := false
	for page := 0; page < hubSearchPagesPerConv; page++ {
		batch, err := fetch(before, per+1)
		if err != nil {
			return nil, false, err
		}
		if len(batch) == 0 {
			break
		}
		for _, msg := range batch {
			if !hubSearchOlderThan(msg, *cursor) {
				continue
			}
			kept = append(kept, msg)
			if len(kept) > per {
				break
			}
		}
		if len(kept) > per {
			more = true
			break
		}
		if len(batch) <= per {
			break // 已经翻到该会话最早一条命中
		}
		before = batch[0].Seq
		if page == hubSearchPagesPerConv-1 {
			more = true // 翻页上限：保守认为还有更旧的命中
		}
	}
	// 同一会话里 seq 单调，按 seq 升序还原「旧→新」（跨页累积是倒序的）。
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Seq < kept[j].Seq })
	if len(kept) > per {
		kept = kept[len(kept)-per:]
		more = true
	}
	return kept, more, nil
}

// searchConversationOnce 是单会话翻页的快速路径：会话内 seq 严格单调，游标 seq
// 就是精确的 before_seq，多取一条判断是否还有更旧命中。
func searchConversationOnce(fetch func(beforeSeq int64, limit int) ([]agenthub.Message, error), beforeSeq int64, limit int) ([]agenthub.Message, bool, error) {
	per := limit
	if per <= 0 || per > hubSearchPerConvMax {
		per = hubSearchPerConvMax
	}
	batch, err := fetch(beforeSeq, per+1)
	if err != nil {
		return nil, false, err
	}
	if len(batch) > per {
		return batch[len(batch)-per:], true, nil
	}
	return batch, false, nil
}

// hubSearchOlderThan 是「比游标更旧」的唯一判据（与排序同一口径）。
func hubSearchOlderThan(m agenthub.Message, cursor hubSearchCursor) bool {
	if m.CreatedAt.Equal(cursor.CreatedAt) {
		return m.ID < cursor.ID
	}
	return m.CreatedAt.Before(cursor.CreatedAt)
}

// sortHubSearchHits 按 (created_at, id) 倒序；同刻用 scope/id/seq 兜底保证确定性。
func sortHubSearchHits(hits []hubSearchHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		if a.MessageID != b.MessageID {
			return a.MessageID > b.MessageID
		}
		if a.Scope != b.Scope {
			return a.Scope < b.Scope
		}
		if a.GroupID != b.GroupID {
			return a.GroupID < b.GroupID
		}
		if a.AgentID != b.AgentID {
			return a.AgentID < b.AgentID
		}
		return a.Seq > b.Seq
	})
}

func buildHubSearchHit(cv hubSearchConversation, msg agenthub.Message, query string, names map[agenthub.AgentID]string) hubSearchHit {
	sender, kind, agentID := hubSearchSenderName(msg, names)
	hit := hubSearchHit{
		Scope:         cv.scope,
		Name:          cv.name,
		Seq:           msg.Seq,
		MessageID:     msg.ID,
		Snippet:       hubSearchSnippet(msg.Text, query, hubSearchSnippetRadius),
		Sender:        sender,
		SenderKind:    kind,
		SenderAgentID: agentID,
		Kind:          string(msg.Kind),
		CreatedAt:     msg.CreatedAt,
		OwnerOnly:     msg.Visibility.EffectiveVisibility() == agenthub.VisibilityOwner,
	}
	if cv.scope == "group" {
		hit.GroupID = string(cv.groupID)
	} else {
		hit.AgentID = string(cv.agentID)
	}
	return hit
}

// hubSearchSenderName 把 sender 还原成可读名字（群/DM 共用）：
// agent → 名册里的显示名（查不到退回 id），human → 我，system → 系统。
func hubSearchSenderName(m agenthub.Message, names map[agenthub.AgentID]string) (name, kind, agentID string) {
	switch m.Sender.Kind {
	case agenthub.SenderAgent:
		id := m.Sender.AgentID
		name = strings.TrimSpace(names[id])
		if name == "" {
			name = string(id)
		}
		return name, string(agenthub.SenderAgent), string(id)
	case agenthub.SenderHuman:
		return "我", string(agenthub.SenderHuman), ""
	default:
		return "系统", string(agenthub.SenderSystem), ""
	}
}

// hubAgentNameMap 一次读全量名册：结果里的 sender 与 DM 会话名都要用。
// ListAgents 只读 profile（很小），比每条命中单独 LoadAgent 便宜且无锁竞争。
func hubAgentNameMap(ctx context.Context, st *agenthub.FileStore) map[agenthub.AgentID]string {
	out := map[agenthub.AgentID]string{}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		return out
	}
	for _, p := range agents {
		if p == nil {
			continue
		}
		out[p.ID] = p.Name
	}
	return out
}

// parseHubSearchCursor 解析 before 游标（created_at 必填、id 必填，seq 可选）。
// 缺省/空对象 = 第一页；格式错误显式报错，不静默当成第一页（否则翻页会原地打转）。
func parseHubSearchCursor(v any) (*hubSearchCursor, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("before 必须是 {created_at,id} 对象")
	}
	if len(raw) == 0 {
		return nil, nil
	}
	atRaw := strings.TrimSpace(str(raw, "created_at"))
	if atRaw == "" {
		return nil, fmt.Errorf("before.created_at 不能为空")
	}
	at, err := time.Parse(time.RFC3339Nano, atRaw)
	if err != nil {
		if at2, err2 := time.Parse(time.RFC3339, atRaw); err2 == nil {
			at = at2
		} else {
			return nil, fmt.Errorf("before.created_at 不是 RFC3339: %q", atRaw)
		}
	}
	id := strings.TrimSpace(str(raw, "id"))
	if id == "" {
		return nil, fmt.Errorf("before.id 不能为空")
	}
	return &hubSearchCursor{CreatedAt: at.UTC(), ID: id, Seq: numOr(raw, "seq", 0)}, nil
}

// hubSearchSnippet 生成以命中词为中心的片段：
//   - 按 rune 截断，永远不会切坏 UTF-8/汉字；
//   - 空白（含换行）折叠成单空格，结果行永远是一行；
//   - 省略号只在真的截断时加。
func hubSearchSnippet(text, query string, radius int) string {
	flat := collapseSearchSpace(text)
	if flat == "" {
		return ""
	}
	if radius <= 0 {
		radius = hubSearchSnippetRadius
	}
	src := []rune(flat)
	start := -1
	qr := []rune(strings.TrimSpace(strings.ToLower(query)))
	if len(qr) > 0 {
		hay := foldSearchRunes(src)
		for i := 0; i+len(qr) <= len(hay); i++ {
			match := true
			for k := range qr {
				if hay[i+k] != qr[k] {
					match = false
					break
				}
			}
			if match {
				start = i
				break
			}
		}
	}
	if start < 0 {
		// 没给 query 或没找到（理论上不会发生）：退回前缀，至少让人看到是哪条。
		to := radius * 2
		if to > len(src) {
			to = len(src)
		}
		out := string(src[:to])
		if to < len(src) {
			out += "…"
		}
		return out
	}
	from := start - radius
	if from < 0 {
		from = 0
	}
	to := start + len(qr) + radius
	if to > len(src) {
		to = len(src)
	}
	out := string(src[from:to])
	if from > 0 {
		out = "…" + out
	}
	if to < len(src) {
		out += "…"
	}
	return out
}

// foldSearchRunes 逐 rune 小写化：长度与原文严格一致，命中下标才能安全映射回原文
// （strings.ToLower 有少数特殊映射会改变长度，切出来就会错位）。
func foldSearchRunes(rs []rune) []rune {
	out := make([]rune, len(rs))
	for i, r := range rs {
		out[i] = unicode.ToLower(r)
	}
	return out
}

func collapseSearchSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
