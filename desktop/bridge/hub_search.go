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
//   - scope=all 时遍历未删除的群 + DM，逐会话取一页，合并后按 (created_at, id) 倒序，
//     全局截断 limit，并把**逐会话续页游标**放进 next_before.conversations。
//
// 为什么 next_before 是逐会话 before_seq 而不是全局 (created_at,id)：
// 全局时间戳游标要求下一页把每个会话从头扫一遍再丢掉「比游标新」的命中；单个会话
// 命中数超过 单会话上限/翻页上限 时，翻页会走完额度却一条都发不出来，has_more 还会
// 变 false（P4-04b-1 复审 B1）。逐会话 before_seq 让每个会话**只向前走**，不发出去
// 就不推进，且天然绕开「msg_<seq> 全局不唯一」的同刻碰撞（B1/N1）。
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
	// hubSearchPerConvMax 单会话单次最多取多少条命中（多取一条判断「还有更旧」）。
	// 取全局上限同一个数：单会话被截断时它已经贡献了 >= limit 条命中，这些命中整体
	// 都比被截掉的新，所以全局前 limit 条不会漏；续页游标负责把更旧的取回来。
	hubSearchPerConvMax = hubSearchMaxLimit
	// hubSearchSnippetRadius 片段在命中词两侧各保留多少个 rune。
	hubSearchSnippetRadius = 32
)

// hubSearchCursor 是翻页游标。
//
//   - Conversations 是 scope=all 的权威续页信息：key = "g:<group_id>" / "d:<agent_id>"，
//     value = 该会话下一次的 before_seq（会话内 seq 严格单调）。
//     **map 里的键 = 还有更旧命中的会话；不在键里的会话已经取完，下一页直接跳过。**
//     value 可以是 0：该会话本轮一条都没发出去（被全局 limit 挤掉），下页仍从最新取。
//   - Conversations 整个缺失 = 第一页（兼容旧游标/客户端不传游标）。
//   - CreatedAt/ID/Seq 保留给 UI 展示与 scope=group/dm 的 seq 快速路径；scope=all
//     不用它们做过滤（msg_<seq> 只在一个会话内唯一，跨会话同刻会碰撞）。
type hubSearchCursor struct {
	CreatedAt     time.Time        `json:"created_at"`
	ID            string           `json:"id"`
	Seq           int64            `json:"seq,omitempty"`
	Conversations map[string]int64 `json:"conversations,omitempty"`
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

// key 是续页游标里的会话键（见 hubSearchCursor.Conversations）。
func (cv hubSearchConversation) key() string {
	return hubSearchConvKey(cv.scope, string(cv.groupID), string(cv.agentID))
}

// hubSearchPlan 记录本轮某个会话的取数参数：全局截断之后要靠它推导续页游标
// （「取到了多少」与「实际发出了多少」的差 = 还有没有没发出去的命中）。
type hubSearchPlan struct {
	conv      hubSearchConversation
	beforeSeq int64 // 本轮使用的 before_seq（0 = 从最新开始）
	collected int   // 本轮收集到的命中数（<= limit）
	truncated bool  // 该会话还有比本轮更旧的命中
}

func hubSearchConvKey(scope, groupID, agentID string) string {
	if scope == "group" {
		return "g:" + groupID
	}
	return "d:" + agentID
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
	plans := make([]hubSearchPlan, 0, len(convs))
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
		// 每个会话只按自己的 before_seq 向更旧走。
		// 游标里带 conversations 时，**缺失 key = 该会话已经取完**（跳过，绝不从最新重扫）；
		// 只有完全没有 conversations（第一页/旧格式游标）才从最新开始。
		beforeSeq := int64(0)
		skip := false
		if before != nil {
			switch {
			case scope != "all" && before.Seq > 0:
				beforeSeq = before.Seq // 单会话翻页：seq 就是精确的 before_seq（老游标也兼容）
			case before.Conversations != nil:
				v, ok := before.Conversations[cv.key()]
				if ok {
					beforeSeq = v
				} else {
					skip = true
				}
			}
		}
		if skip {
			continue
		}
		msgs, more, serr := searchConversationPage(fetch, beforeSeq, limit)
		if serr != nil {
			m.resp(c, false, fmt.Sprintf("搜索 %s 失败: %v", cv.name, serr), nil)
			return
		}
		plans = append(plans, hubSearchPlan{conv: cv, beforeSeq: beforeSeq, collected: len(msgs), truncated: more})
		for _, msg := range msgs {
			all = append(all, buildHubSearchHit(cv, msg, query, names))
		}
	}
	sortHubSearchHits(all)
	if len(all) > limit {
		all = all[:limit]
	}
	nextConvs := hubSearchNextConversations(plans, all)
	var next *hubSearchCursor
	if len(nextConvs) > 0 && len(all) > 0 {
		last := all[len(all)-1]
		next = &hubSearchCursor{
			CreatedAt: last.CreatedAt, ID: last.MessageID, Seq: last.Seq,
			Conversations: nextConvs,
		}
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

// hubSearchNextConversations 由「本轮每个会话取到多少」+「全局页实际发出多少」
// 推导下一轮的逐会话续页游标：
//
//   - 发出了 >0 条：推进到该会话**最旧一条已发出命中的 seq**。下一次取 seq 严格更小
//     的命中，同页没发出去/更旧的命中仍然够得着（不丢、不重）；
//   - 一条没发出但该会话还有命中（被全局 limit 挤掉）：**保留**本轮 before_seq，
//     下页重取同一批（推进就会跳过它们）；
//   - 该会话的命中已全部发出且没有更旧的：从 map 里删掉（不再宣称 has_more）。
//
// 返回空 map = 没有更旧的结果。has_more 严格等价于「map 非空」。
func hubSearchNextConversations(plans []hubSearchPlan, page []hubSearchHit) map[string]int64 {
	type emitted struct {
		count  int
		oldest int64 // 已发出命中里最小的 seq
	}
	byKey := make(map[string]emitted, len(plans))
	for _, h := range page {
		k := hubSearchConvKey(h.Scope, h.GroupID, h.AgentID)
		e := byKey[k]
		e.count++
		if e.oldest == 0 || h.Seq < e.oldest {
			e.oldest = h.Seq
		}
		byKey[k] = e
	}
	next := make(map[string]int64, len(plans))
	for _, p := range plans {
		e := byKey[p.conv.key()]
		if e.count == p.collected && !p.truncated {
			continue // 本会话的命中都发出去了，而且没有更旧的
		}
		if e.count > 0 {
			next[p.conv.key()] = e.oldest
			continue
		}
		next[p.conv.key()] = p.beforeSeq
	}
	return next
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

// searchConversationPage 取某个会话的一页命中（beforeSeq=0 表示从最新开始）。
//
// store 返回的是「seq < beforeSeq 的最新 limit 条」，顺序旧→新；这里多取一条
// （per+1）用来判断该会话还有没有更旧的命中，并把多出来的那条丢掉（它属于下一页）。
func searchConversationPage(fetch func(beforeSeq int64, limit int) ([]agenthub.Message, error), beforeSeq int64, limit int) ([]agenthub.Message, bool, error) {
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

// parseHubSearchCursor 解析 before 游标。
//
//   - conversations 非空：scope=all 的权威续页信息，created_at/id 可缺省（只用于展示/兼容）；
//   - conversations 缺失：回落到旧格式（created_at + id + seq 必填其一），
//     解析成「第一页」——旧游标继续翻页会从头开始，不会错位。
//
// 格式错误显式报错，不静默当成第一页（否则翻页会原地打转）。
func parseHubSearchCursor(v any) (*hubSearchCursor, error) {
	if v == nil {
		return nil, nil
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("before 必须是 {created_at,id,conversations} 对象")
	}
	if len(raw) == 0 {
		return nil, nil
	}
	convs := parseHubSearchConversations(raw["conversations"])
	atRaw := strings.TrimSpace(str(raw, "created_at"))
	id := strings.TrimSpace(str(raw, "id"))
	if atRaw == "" && id == "" && len(convs) == 0 {
		return nil, nil // 空游标 = 第一页
	}
	cur := &hubSearchCursor{ID: id, Seq: numOr(raw, "seq", 0), Conversations: convs}
	if atRaw != "" {
		at, err := time.Parse(time.RFC3339Nano, atRaw)
		if err != nil {
			if at2, err2 := time.Parse(time.RFC3339, atRaw); err2 == nil {
				at = at2
			} else {
				return nil, fmt.Errorf("before.created_at 不是 RFC3339: %q", atRaw)
			}
		}
		cur.CreatedAt = at.UTC()
	}
	if id == "" && len(convs) == 0 {
		return nil, fmt.Errorf("before.id 不能为空")
	}
	return cur, nil
}

// parseHubSearchConversations 解析逐会话续页游标（key → before_seq）。
// 非法 key、非数字/非正数 value 一律丢弃：value<=0 等于「从最新开始」，不构成续页信息。
func parseHubSearchConversations(v any) map[string]int64 {
	out := map[string]int64{}
	add := func(k string, n int64) {
		key := strings.TrimSpace(k)
		if key == "" || (!strings.HasPrefix(key, "g:") && !strings.HasPrefix(key, "d:")) {
			return
		}
		if n < 0 {
			return
		}
		// 0 是合法续页值：该会话本轮一条都没发出去，下一页仍从最新取（不推进）。
		out[key] = n
	}
	// JSON 解码给 map[string]any（数字是 float64）；同进程调用方可能直接传
	// map[string]int64/int，两种都要认（测试直接用 Go map 构造 payload）。
	switch raw := v.(type) {
	case map[string]any:
		for k, val := range raw {
			switch t := val.(type) {
			case float64:
				add(k, int64(t))
			case int:
				add(k, int64(t))
			case int64:
				add(k, t)
			}
		}
	case map[string]int64:
		for k, n := range raw {
			add(k, n)
		}
	case map[string]int:
		for k, n := range raw {
			add(k, int64(n))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
