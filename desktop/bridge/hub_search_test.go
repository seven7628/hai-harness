package main

// hub_search_test.go：hub_search（人类全历史搜索）命令层验收（P4-04b-1）。
//
// 覆盖：owner-only 对人类可见、系统噪声排除、scope=group/dm、scope=all 的游标翻页、
// 片段按 rune 截断、非法 payload、以及「智能体没有全局搜索工具」的结构性边界。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/tools"
)

type hubSearchResp struct {
	Query      string           `json:"query"`
	Scope      string           `json:"scope"`
	Limit      int              `json:"limit"`
	Hits       []hubSearchHit   `json:"hits"`
	Count      int              `json:"count"`
	HasMore    bool             `json:"has_more"`
	NextBefore *hubSearchCursor `json:"next_before"`
	Scanned    int              `json:"scanned"`
}

func hubSearchCmd(t *testing.T, m *manager, buf *syncBuf, id int, payload map[string]any) hubSearchResp {
	t.Helper()
	ok, errMsg, data := hubCmd(t, m, buf, id, "hub_search", payload)
	if !ok {
		t.Fatalf("hub_search(%v) failed: %s", payload, errMsg)
	}
	raw, _ := json.Marshal(data)
	var out hubSearchResp
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode hub_search: %v (%s)", err, raw)
	}
	if out.Hits == nil {
		out.Hits = []hubSearchHit{}
	}
	return out
}

func seedGroupSearch(t *testing.T, st *agenthub.FileStore, gid agenthub.GroupID, text, kind string, vis agenthub.Visibility, sender agenthub.Sender, at time.Time) agenthub.Message {
	t.Helper()
	msg := agenthub.Message{
		GroupID: gid, Kind: agenthub.MessageKind(kind), Visibility: vis,
		Sender: sender, Text: text, CreatedAt: at,
	}
	if _, err := agenthub.AppendGroupMessage(context.Background(), st, &msg); err != nil {
		t.Fatalf("append group message: %v", err)
	}
	return msg
}

func seedDM(t *testing.T, st *agenthub.FileStore, agentID agenthub.AgentID, text string, vis agenthub.Visibility, at time.Time) agenthub.Message {
	t.Helper()
	msg := agenthub.Message{
		Kind: agenthub.KindText, Visibility: vis,
		Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: text, CreatedAt: at,
	}
	if _, err := st.AppendDirectMessage(context.Background(), agentID, &msg); err != nil {
		t.Fatalf("append dm: %v", err)
	}
	return msg
}

func TestHubSearchHumanSeesOwnerOnlyAndSkipsNoise(t *testing.T) {
	m, buf := testManager(t)
	alice := createHubAgent(t, m, buf, 1, "Alice")
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_group_create", map[string]any{
		"name":    "搜索群",
		"members": []map[string]any{{"agent_id": string(alice.ID), "role": "owner"}},
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	g := groupFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)
	human := agenthub.Sender{Kind: agenthub.SenderHuman}
	seedGroupSearch(t, st, g.ID, "alpha 普通消息", "text", agenthub.VisibilityGroup, human, base.Add(1*time.Second))
	seedGroupSearch(t, st, g.ID, "alpha 群主悄悄话", "text", agenthub.VisibilityOwner, human, base.Add(2*time.Second))
	seedGroupSearch(t, st, g.ID, "alpha targets=ag_x", "routing_receipt", agenthub.VisibilityGroup, agenthub.Sender{Kind: agenthub.SenderSystem}, base.Add(3*time.Second))
	seedGroupSearch(t, st, g.ID, "alpha 轮次摘要", "round_summary", agenthub.VisibilityGroup, agenthub.Sender{Kind: agenthub.SenderSystem}, base.Add(4*time.Second))
	seedGroupSearch(t, st, g.ID, "alpha 审批占位", "approval_stub", agenthub.VisibilityGroup, agenthub.Sender{Kind: agenthub.SenderSystem}, base.Add(5*time.Second))
	seedGroupSearch(t, st, g.ID, "Alpha 智能体回复", "text", agenthub.VisibilityGroup,
		agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: alice.ID}, base.Add(6*time.Second))

	resp := hubSearchCmd(t, m, buf, 3, map[string]any{"query": "alpha"})
	if resp.Count != 3 || len(resp.Hits) != 3 {
		t.Fatalf("hits = %d (%+v)，owner-only 必须可见、噪声必须排除", resp.Count, resp.Hits)
	}
	// newest-first：(created_at, id) 倒序。
	if resp.Hits[0].Seq < resp.Hits[1].Seq || resp.Hits[1].Seq < resp.Hits[2].Seq {
		t.Fatalf("hits 不是 newest-first: %+v", resp.Hits)
	}
	var ownerHit, agentHit *hubSearchHit
	for i := range resp.Hits {
		h := &resp.Hits[i]
		if h.OwnerOnly {
			ownerHit = h
		}
		if h.SenderKind == "agent" {
			agentHit = h
		}
		if h.Scope != "group" || h.GroupID != string(g.ID) || h.Name != "搜索群" {
			t.Fatalf("hit 会话信息不对: %+v", h)
		}
		if h.Kind == "routing_receipt" || h.Kind == "round_summary" || h.Kind == "approval_stub" {
			t.Fatalf("系统噪声进了搜索结果: %+v", h)
		}
		if !strings.Contains(strings.ToLower(h.Snippet), "alpha") {
			t.Fatalf("snippet 不含命中词: %q", h.Snippet)
		}
	}
	if ownerHit == nil || !strings.Contains(ownerHit.Snippet, "群主悄悄话") {
		t.Fatalf("owner-only 命中不对: %+v", ownerHit)
	}
	if agentHit == nil || agentHit.Sender != "Alice" || agentHit.SenderAgentID != string(alice.ID) {
		t.Fatalf("agent 发送者应还原成显示名: %+v", agentHit)
	}
	if resp.HasMore || resp.NextBefore != nil {
		t.Fatalf("最后一页不该有游标: %+v", resp.NextBefore)
	}
}

func TestHubSearchScopeGroupAndDM(t *testing.T) {
	m, buf := testManager(t)
	alice := createHubAgent(t, m, buf, 1, "Alice")
	bob := createHubAgent(t, m, buf, 2, "Bob")
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name":    "范围群",
		"members": []map[string]any{{"agent_id": string(alice.ID), "role": "owner"}},
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	g := groupFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	seedGroupSearch(t, st, g.ID, "shared keyword 群", "text", agenthub.VisibilityGroup, agenthub.Sender{Kind: agenthub.SenderHuman}, base.Add(time.Second))
	seedDM(t, st, alice.ID, "shared keyword DM", agenthub.VisibilityGroup, base.Add(2*time.Second))
	seedDM(t, st, bob.ID, "shared keyword 另一个 DM", agenthub.VisibilityGroup, base.Add(3*time.Second))

	// scope=group 只回群命中。
	resp := hubSearchCmd(t, m, buf, 4, map[string]any{"query": "shared keyword", "scope": "group", "group_id": string(g.ID)})
	if resp.Count != 1 || resp.Hits[0].Scope != "group" || resp.Scope != "group" {
		t.Fatalf("group scope = %+v", resp)
	}
	if resp.Scanned != 1 {
		t.Fatalf("group scope scanned = %d, want 1", resp.Scanned)
	}
	// scope=dm 只回该 DM。
	resp = hubSearchCmd(t, m, buf, 5, map[string]any{"query": "shared keyword", "scope": "dm", "agent_id": string(alice.ID)})
	if resp.Count != 1 || resp.Hits[0].Scope != "dm" || resp.Hits[0].AgentID != string(alice.ID) || resp.Hits[0].Name != "Alice" {
		t.Fatalf("dm scope = %+v", resp)
	}
	// scope=all 合并两个会话（群 + 两个 DM = 3），且带 group_id/agent_id 各自的字段。
	resp = hubSearchCmd(t, m, buf, 6, map[string]any{"query": "shared keyword"})
	if resp.Count != 3 || resp.Scope != "all" {
		t.Fatalf("all scope = %+v", resp)
	}
	if resp.Scanned != 3 {
		t.Fatalf("all scanned = %d, want 3 (1 group + 2 dm)", resp.Scanned)
	}
	scopes := map[string]int{}
	for _, h := range resp.Hits {
		scopes[h.Scope]++
		if h.Scope == "group" && h.GroupID != string(g.ID) {
			t.Fatalf("group hit 缺 group_id: %+v", h)
		}
		if h.Scope == "dm" && h.AgentID == "" {
			t.Fatalf("dm hit 缺 agent_id: %+v", h)
		}
	}
	if scopes["group"] != 1 || scopes["dm"] != 2 {
		t.Fatalf("scope 分布 = %v", scopes)
	}
	// 大小写不敏感 + 空 query 拒绝。
	if resp := hubSearchCmd(t, m, buf, 7, map[string]any{"query": "SHARED KEYWORD"}); resp.Count != 3 {
		t.Fatalf("大小写不敏感搜索 = %d", resp.Count)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 8, "hub_search", map[string]any{"query": "   "}); ok || errMsg == "" {
		t.Fatalf("空 query 必须被拒绝: ok=%v err=%q", ok, errMsg)
	}
	// 删除的智能体不参与 fan-out。
	if ok, errMsg, _ := hubCmd(t, m, buf, 9, "hub_agent_delete", map[string]any{"agent_id": string(bob.ID)}); !ok {
		t.Fatalf("delete bob: %s", errMsg)
	}
	if resp := hubSearchCmd(t, m, buf, 10, map[string]any{"query": "shared keyword"}); resp.Scanned != 2 || resp.Count != 2 {
		t.Fatalf("删除后 fan-out = scanned %d hits %d, want 2/2", resp.Scanned, resp.Count)
	}
}

func TestHubSearchAllCursorPagination(t *testing.T) {
	m, buf := testManager(t)
	alice := createHubAgent(t, m, buf, 1, "Alice")
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_group_create", map[string]any{
		"name":    "翻页甲",
		"members": []map[string]any{{"agent_id": string(alice.ID), "role": "owner"}},
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	g1 := groupFromData(t, data)
	ok, errMsg, data = hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name":    "翻页乙",
		"members": []map[string]any{{"agent_id": string(alice.ID), "role": "owner"}},
	})
	if !ok {
		t.Fatalf("create group 2: %s", errMsg)
	}
	g2 := groupFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	human := agenthub.Sender{Kind: agenthub.SenderHuman}
	for i := 0; i < 3; i++ {
		seedGroupSearch(t, st, g1.ID, "hit 老群", "text", agenthub.VisibilityGroup, human, base.Add(time.Duration(i)*time.Second))
	}
	for i := 0; i < 3; i++ {
		seedGroupSearch(t, st, g2.ID, "hit 新群", "text", agenthub.VisibilityGroup, human, base.Add(time.Duration(i+3)*time.Second))
	}

	seen := map[string]bool{}
	var cursor any
	for page := 0; page < 6; page++ {
		payload := map[string]any{"query": "hit", "limit": 2}
		if cursor != nil {
			payload["before"] = cursor
		}
		resp := hubSearchCmd(t, m, buf, 10+page, payload)
		if len(resp.Hits) == 0 {
			if resp.HasMore {
				t.Fatalf("空页不该宣称 has_more")
			}
			break
		}
		for _, h := range resp.Hits {
			key := h.GroupID + "#" + h.MessageID
			if seen[key] {
				t.Fatalf("翻页重复命中 %s", key)
			}
			seen[key] = true
		}
		if resp.Limit != 2 {
			t.Fatalf("limit echo = %d", resp.Limit)
		}
		if !resp.HasMore {
			if resp.NextBefore != nil {
				t.Fatalf("has_more=false 仍给了 next_before")
			}
			break
		}
		if resp.NextBefore == nil || resp.NextBefore.ID == "" || resp.NextBefore.CreatedAt.IsZero() {
			t.Fatalf("has_more=true 缺 next_before: %+v", resp)
		}
		last := resp.Hits[len(resp.Hits)-1]
		if resp.NextBefore.ID != last.MessageID {
			t.Fatalf("next_before 应指向本页最后一条: %+v vs %+v", resp.NextBefore, last)
		}
		cursor = map[string]any{
			"created_at": resp.NextBefore.CreatedAt.Format(time.RFC3339Nano),
			"id":         resp.NextBefore.ID,
			"seq":        resp.NextBefore.Seq,
		}
	}
	if len(seen) != 6 {
		t.Fatalf("翻页共取到 %d 条，want 6", len(seen))
	}

	// 非法游标显式报错（不能静默当成第一页）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 30, "hub_search", map[string]any{
		"query": "hit", "before": map[string]any{"created_at": "not-a-time", "id": "msg_1"},
	}); ok || errMsg == "" {
		t.Fatalf("非法 before.created_at 必须报错: %v %q", ok, errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 31, "hub_search", map[string]any{
		"query": "hit", "before": map[string]any{"created_at": time.Now().UTC().Format(time.RFC3339)},
	}); ok || errMsg == "" {
		t.Fatalf("缺 before.id 必须报错: %v %q", ok, errMsg)
	}
}

// 单会话翻页走 seq 快速路径：next_before.seq 直接当 before_seq，不按时间走页。
func TestHubSearchScopedCursorUsesSeq(t *testing.T) {
	m, buf := testManager(t)
	alice := createHubAgent(t, m, buf, 1, "PageAlice")
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_group_create", map[string]any{
		"name":    "单会话翻页",
		"members": []map[string]any{{"agent_id": string(alice.ID), "role": "owner"}},
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	g := groupFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 9, 11, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		seedGroupSearch(t, st, g.ID, "分页 needle", "text", agenthub.VisibilityGroup,
			agenthub.Sender{Kind: agenthub.SenderHuman}, base.Add(time.Duration(i)*time.Second))
	}
	first := hubSearchCmd(t, m, buf, 3, map[string]any{
		"query": "needle", "scope": "group", "group_id": string(g.ID), "limit": 2,
	})
	if first.Count != 2 || !first.HasMore || first.NextBefore == nil || first.NextBefore.Seq == 0 {
		t.Fatalf("第一页 = %+v", first)
	}
	second := hubSearchCmd(t, m, buf, 4, map[string]any{
		"query": "needle", "scope": "group", "group_id": string(g.ID), "limit": 2,
		"before": map[string]any{
			"created_at": first.NextBefore.CreatedAt.Format(time.RFC3339Nano),
			"id":         first.NextBefore.ID,
			"seq":        first.NextBefore.Seq,
		},
	})
	if second.Count != 2 || second.Hits[0].Seq >= first.Hits[len(first.Hits)-1].Seq {
		t.Fatalf("第二页 = %+v（应全部比第一页更旧）", second)
	}
	if second.Hits[0].Seq != 3 || second.Hits[1].Seq != 2 {
		t.Fatalf("第二页 seq = %d,%d, want 3,2", second.Hits[0].Seq, second.Hits[1].Seq)
	}
}

func TestHubSearchRejectsBadScopeAndTargets(t *testing.T) {
	m, buf := testManager(t)
	createHubAgent(t, m, buf, 1, "ScopeA")
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"unknown scope", map[string]any{"query": "x", "scope": "everywhere"}},
		{"group without id", map[string]any{"query": "x", "scope": "group"}},
		{"dm without id", map[string]any{"query": "x", "scope": "dm"}},
		{"unknown group", map[string]any{"query": "x", "scope": "group", "group_id": "grp_missing"}},
		{"unknown agent", map[string]any{"query": "x", "scope": "dm", "agent_id": "ag_missing"}},
	}
	for i, tc := range cases {
		ok, errMsg, _ := hubCmd(t, m, buf, 10+i, "hub_search", tc.payload)
		if ok || errMsg == "" {
			t.Fatalf("%s 必须失败: ok=%v err=%q", tc.name, ok, errMsg)
		}
	}
	// limit 越界钳位（不是拒绝）：1..200，默认 50。
	resp := hubSearchCmd(t, m, buf, 20, map[string]any{"query": "x", "limit": 0})
	if resp.Limit != 50 {
		t.Fatalf("limit=0 → %d, want 50", resp.Limit)
	}
	resp = hubSearchCmd(t, m, buf, 21, map[string]any{"query": "x", "limit": 1000})
	if resp.Limit != 200 {
		t.Fatalf("limit=1000 → %d, want 200", resp.Limit)
	}
}

func TestHubSearchSnippetRuneSafe(t *testing.T) {
	long := strings.Repeat("前文", 40) + "命中关键词" + strings.Repeat("后文", 40)
	snip := hubSearchSnippet(long, "关键词", 8)
	if !utf8.ValidString(snip) {
		t.Fatalf("片段不是合法 UTF-8: %q", snip)
	}
	if !strings.Contains(snip, "命中关键词") {
		t.Fatalf("片段没包含命中的 CJK 词: %q", snip)
	}
	if !strings.HasPrefix(snip, "…") || !strings.HasSuffix(snip, "…") {
		t.Fatalf("两侧被截断时应加省略号: %q", snip)
	}
	if n := utf8.RuneCountInString(snip); n > 8+len([]rune("关键词"))+8+2 {
		t.Fatalf("片段长度 %d 超出半径: %q", n, snip)
	}
	// 换行/制表折叠成单空格：结果行永远一行。
	if got := hubSearchSnippet("第一行\n\n第二行\tkeyword  尾巴", "keyword", 32); strings.ContainsAny(got, "\n\t") || !strings.Contains(got, "第二行 keyword 尾巴") {
		t.Fatalf("空白折叠不对: %q", got)
	}
	// 关键词在前 64 个 rune 内：不加前省略号。
	if got := hubSearchSnippet("keyword 就在开头", "keyword", 32); strings.HasPrefix(got, "…") {
		t.Fatalf("未截断前缀不该有省略号: %q", got)
	}
	// 大小写不敏感命中仍保留原文大小写。
	if got := hubSearchSnippet("Prefix KEYWORD suffix", "keyword", 32); !strings.Contains(got, "KEYWORD") {
		t.Fatalf("片段应保留原文大小写: %q", got)
	}
}

// 边界：hub_search 是人类命令，智能体侧不得注册同名/等价工具（否则就是跨群越权）。
func TestHubSearchNotExposedAsAgentTool(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "NoSearch")
	eng := tools.NewToolEngine()
	registerHubGroupTools(eng, m, a.ID)
	registered := []string{}
	for _, tl := range eng.Tools() {
		name := strings.ToLower(tl.Name())
		registered = append(registered, name)
		if strings.Contains(name, "search") || name == "hub_search" {
			t.Fatalf("智能体拿到了全局搜索工具 %q（registered=%v）", name, registered)
		}
	}
	if len(registered) == 0 {
		t.Fatal("registerHubGroupTools 没注册任何工具？测试失去意义")
	}
	// 人类命令面 + 手机白名单：两者都要有，否则桌面/手机会静默 403。
	if !mobileAllowedCommands[CmdHubSearch] {
		t.Fatal("hub_search 必须在手机白名单里")
	}
	if got := CmdHubSearch.String(); got != "hub_search" {
		t.Fatalf("CmdHubSearch = %q", got)
	}
}
