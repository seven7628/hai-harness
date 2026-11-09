package main

// hub_search_test.go：hub_search（人类全历史搜索）命令层验收（P4-04b-1）。
//
// 覆盖：owner-only 对人类可见、系统噪声排除、scope=group/dm、scope=all 的游标翻页、
// 片段按 rune 截断、非法 payload、以及「智能体没有全局搜索工具」的结构性边界。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// writeGroupLogFixture / writeDMLogFixture 直接按存储格式写日志：
// 深翻页 fixture 有上千条消息，逐条 AppendMessage 会 fsync 上千次（太慢），
// 搜索只读不写，落盘成同样的 JSONL 即可。
func writeGroupLogFixture(t *testing.T, st *agenthub.FileStore, gid agenthub.GroupID, n int, text string, base time.Time) {
	t.Helper()
	dir := filepath.Join(st.Root(), "groups", string(gid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir group log: %v", err)
	}
	writeMessageLogFixture(t, filepath.Join(dir, "log.jsonl"), gid, n, text, base)
}

func writeDMLogFixture(t *testing.T, st *agenthub.FileStore, agentID agenthub.AgentID, n int, text string, base time.Time) {
	t.Helper()
	dir := filepath.Join(st.Root(), "dms", string(agentID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir dm log: %v", err)
	}
	writeMessageLogFixture(t, filepath.Join(dir, "log.jsonl"), "", n, text, base)
}

func writeMessageLogFixture(t *testing.T, path string, gid agenthub.GroupID, n int, text string, base time.Time) {
	t.Helper()
	var buf bytes.Buffer
	for i := 1; i <= n; i++ {
		seq := int64(i)
		m := agenthub.Message{
			ID: agenthub.MessageID(seq), Seq: seq, GroupID: gid,
			Kind: agenthub.KindText, Text: fmt.Sprintf("%s #%d", text, i),
			Visibility: agenthub.VisibilityGroup,
			Sender:     agenthub.Sender{Kind: agenthub.SenderHuman},
			CreatedAt:  base.Add(time.Duration(i) * time.Second),
		}
		line, err := json.Marshal(&m)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
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

// hubSearchNextPayload 把响应里的 next_before 原样带回请求（含逐会话续页游标）。
// 客户端必须 opaque 地回传 conversations，否则 scope=all 会退回第一页（B1）。
func hubSearchNextPayload(cur *hubSearchCursor) map[string]any {
	payload := map[string]any{
		"created_at": cur.CreatedAt.Format(time.RFC3339Nano),
		"id":         cur.ID,
		"seq":        cur.Seq,
	}
	if len(cur.Conversations) > 0 {
		payload["conversations"] = cur.Conversations
	}
	return payload
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
	var cursor map[string]any
	pages := 0
	for page := 0; page < 10; page++ {
		payload := map[string]any{"query": "hit", "limit": 2}
		if cursor != nil {
			payload["before"] = cursor
		}
		resp := hubSearchCmd(t, m, buf, 10+page, payload)
		pages++
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
		if resp.NextBefore == nil || len(resp.NextBefore.Conversations) == 0 {
			t.Fatalf("has_more=true 必须带逐会话续页游标: %+v", resp.NextBefore)
		}
		last := resp.Hits[len(resp.Hits)-1]
		if resp.NextBefore.ID != last.MessageID {
			t.Fatalf("next_before 应指向本页最后一条: %+v vs %+v", resp.NextBefore, last)
		}
		cursor = hubSearchNextPayload(resp.NextBefore)
	}
	if len(seen) != 6 || pages != 3 {
		t.Fatalf("翻页共取到 %d 条 / %d 页，want 6 条 3 页", len(seen), pages)
	}

	// 旧格式游标（只有 created_at/id）：按设计当成第一页，不会错位丢命中。
	legacy := hubSearchCmd(t, m, buf, 40, map[string]any{
		"query": "hit", "limit": 2,
		"before": map[string]any{
			"created_at": cursor["created_at"], "id": cursor["id"], "seq": cursor["seq"],
		},
	})
	if legacy.Count != 2 || legacy.NextBefore == nil || len(legacy.NextBefore.Conversations) == 0 {
		t.Fatalf("旧游标应退回第一页（有 has_more + 新格式游标）: %+v", legacy)
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

// 深翻页回归（复审 B1）：单个群 1000 条命中、limit 50。旧实现每页都从 before=0
// 重新扫、按 (created_at,id) 过滤，翻到第 9 页就 has_more=false，599 条永远拿不到。
// 现在逐会话 before_seq 只向前走，必须正好取满 1000 条、无重复、最后 has_more=false。
func TestHubSearchAllDeepPaginationSingleGroup(t *testing.T) {
	m, buf := testManager(t)
	alice := createHubAgent(t, m, buf, 1, "DeepGroup")
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_group_create", map[string]any{
		"name":    "深翻页群",
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
	base := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	const total = 1000
	writeGroupLogFixture(t, st, g.ID, total, "needle 深历史", base)

	const limit = 50
	seen := map[string]bool{}
	var cursor map[string]any
	pages := 0
	for page := 0; page < total/limit+5; page++ {
		payload := map[string]any{"query": "needle", "limit": limit}
		if cursor != nil {
			payload["before"] = cursor
		}
		resp := hubSearchCmd(t, m, buf, 100+page, payload)
		pages++
		if resp.Count == 0 {
			t.Fatalf("第 %d 页是空的，但 has_more=%v（不该出现空页）", pages, resp.HasMore)
		}
		for _, h := range resp.Hits {
			if seen[h.MessageID] {
				t.Fatalf("第 %d 页重复命中 %s", pages, h.MessageID)
			}
			seen[h.MessageID] = true
		}
		if !resp.HasMore {
			if resp.NextBefore != nil {
				t.Fatalf("has_more=false 仍给了 next_before")
			}
			break
		}
		if resp.NextBefore == nil {
			t.Fatalf("has_more=true 但缺 next_before")
		}
		cursor = hubSearchNextPayload(resp.NextBefore)
	}
	t.Logf("单群深翻页：%d 条命中 / limit=%d → %d 页取到 %d 条（无重复）", total, limit, pages, len(seen))
	if len(seen) != total {
		t.Fatalf("深翻页共取到 %d 条，want %d（漏 %d 条）", len(seen), total, total-len(seen))
	}
	if pages != total/limit {
		t.Fatalf("深翻页页数 = %d，want %d（不多不少）", pages, total/limit)
	}
	for i := 1; i <= total; i++ {
		if !seen[agenthub.MessageID(int64(i))] {
			t.Fatalf("漏掉 %s", agenthub.MessageID(int64(i)))
		}
	}
}

// 深翻页（混排 all scope）：群 300 + DM 300 交替时间戳，逐页取完必须正好 600 条，
// 两个会话都被完整取到（群/DM 的续页游标互不干扰）。
func TestHubSearchAllDeepPaginationMixedGroupDM(t *testing.T) {
	m, buf := testManager(t)
	alice := createHubAgent(t, m, buf, 1, "MixedDM")
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_group_create", map[string]any{
		"name":    "混排群",
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
	base := time.Date(2026, 11, 9, 13, 0, 0, 0, time.UTC)
	const per = 300
	writeGroupLogFixture(t, st, g.ID, per, "needle 群", base)
	writeDMLogFixture(t, st, alice.ID, per, "needle 私聊", base.Add(time.Second))

	const limit = 50
	seen := map[string]bool{}
	byScope := map[string]int{}
	var cursor map[string]any
	pages := 0
	for page := 0; page < (2*per)/limit+5; page++ {
		payload := map[string]any{"query": "needle", "limit": limit}
		if cursor != nil {
			payload["before"] = cursor
		}
		resp := hubSearchCmd(t, m, buf, 100+page, payload)
		pages++
		if resp.Count == 0 {
			t.Fatalf("第 %d 页是空的（has_more=%v）", pages, resp.HasMore)
		}
		for _, h := range resp.Hits {
			key := h.Scope + "#" + h.GroupID + h.AgentID + "#" + h.MessageID
			if seen[key] {
				t.Fatalf("第 %d 页重复命中 %s", pages, key)
			}
			seen[key] = true
			byScope[h.Scope]++
		}
		if !resp.HasMore {
			break
		}
		cursor = hubSearchNextPayload(resp.NextBefore)
	}
	t.Logf("混排深翻页（群 %d + DM %d，limit=%d）→ %d 页取到 %d 条，分布 %v", per, per, limit, pages, len(seen), byScope)
	if len(seen) != 2*per {
		t.Fatalf("混排深翻页共取到 %d 条，want %d", len(seen), 2*per)
	}
	if byScope["group"] != per || byScope["dm"] != per {
		t.Fatalf("分布 = %v，want group=%d dm=%d（两个会话都要取完）", byScope, per, per)
	}
}

// N1：msg_<seq> 只在会话内唯一。两个群各一条命中，created_at 相同、seq/id 都是
// msg_1，limit=1。旧实现用 (created_at,id) 做游标会把另一条永久跳过。
func TestHubSearchAllSameTimestampSameIDNoSkip(t *testing.T) {
	m, buf := testManager(t)
	alice := createHubAgent(t, m, buf, 1, "TieA")
	bob := createHubAgent(t, m, buf, 2, "TieB")
	mk := func(id int, name string) *agenthub.Group {
		ok, errMsg, data := hubCmd(t, m, buf, id, "hub_group_create", map[string]any{
			"name":    name,
			"members": []map[string]any{{"agent_id": string(alice.ID), "role": "owner"}, {"agent_id": string(bob.ID), "role": "member"}},
		})
		if !ok {
			t.Fatalf("create %s: %s", name, errMsg)
		}
		return groupFromData(t, data)
	}
	g1 := mk(3, "同刻甲")
	g2 := mk(4, "同刻乙")
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	same := time.Date(2026, 11, 9, 14, 0, 0, 0, time.UTC)
	human := agenthub.Sender{Kind: agenthub.SenderHuman}
	seedGroupSearch(t, st, g1.ID, "tie tie", "text", agenthub.VisibilityGroup, human, same)
	seedGroupSearch(t, st, g2.ID, "tie tie", "text", agenthub.VisibilityGroup, human, same)

	seen := map[string]bool{}
	var cursor map[string]any
	for page := 0; page < 4; page++ {
		payload := map[string]any{"query": "tie", "limit": 1}
		if cursor != nil {
			payload["before"] = cursor
		}
		resp := hubSearchCmd(t, m, buf, 10+page, payload)
		for _, h := range resp.Hits {
			if h.Seq != 1 || h.MessageID != "msg_1" {
				t.Fatalf("fixture 应为同刻同 id：%+v", h)
			}
			seen[h.GroupID] = true
		}
		if !resp.HasMore {
			break
		}
		cursor = hubSearchNextPayload(resp.NextBefore)
	}
	if !seen[string(g1.ID)] || !seen[string(g2.ID)] {
		t.Fatalf("同刻同 id 两条命中必须都能取到，got=%v", seen)
	}
}

// N2：软删的群/智能体不进 scope=all 的 fan-out（也不占 scanned）。
func TestHubSearchAllExcludesDeletedTargets(t *testing.T) {
	m, buf := testManager(t)
	alice := createHubAgent(t, m, buf, 1, "AliveGroupOwner")
	bob := createHubAgent(t, m, buf, 2, "DeadGroupOwner")
	carol := createHubAgent(t, m, buf, 3, "AliveDM")
	dave := createHubAgent(t, m, buf, 4, "DeadDM")
	ok, errMsg, data := hubCmd(t, m, buf, 5, "hub_group_create", map[string]any{
		"name":    "活着的群",
		"members": []map[string]any{{"agent_id": string(alice.ID), "role": "owner"}},
	})
	if !ok {
		t.Fatalf("create live group: %s", errMsg)
	}
	liveGroup := groupFromData(t, data)
	ok, errMsg, data = hubCmd(t, m, buf, 6, "hub_group_create", map[string]any{
		"name":    "要删的群",
		"members": []map[string]any{{"agent_id": string(bob.ID), "role": "owner"}},
	})
	if !ok {
		t.Fatalf("create dead group: %s", errMsg)
	}
	deadGroup := groupFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	human := agenthub.Sender{Kind: agenthub.SenderHuman}
	for i, g := range []*agenthub.Group{liveGroup, deadGroup} {
		seedGroupSearch(t, st, g.ID, "zombie 群命中", "text", agenthub.VisibilityGroup, human, base.Add(time.Duration(i)*time.Second))
	}
	for i, p := range []agenthub.AgentProfile{carol, dave} {
		seedDM(t, st, p.ID, "zombie 私聊命中", agenthub.VisibilityGroup, base.Add(time.Duration(i)*time.Second))
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 7, "hub_group_delete", map[string]any{"group_id": string(deadGroup.ID)}); !ok {
		t.Fatalf("delete group: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 8, "hub_agent_delete", map[string]any{"agent_id": string(dave.ID)}); !ok {
		t.Fatalf("delete agent: %s", errMsg)
	}

	resp := hubSearchCmd(t, m, buf, 9, map[string]any{"query": "zombie", "limit": 50})
	if resp.Count != 2 {
		t.Fatalf("all scope = %d hits，want 2（软删的群/智能体必须排除）: %+v", resp.Count, resp.Hits)
	}
	// scanned = 活群(1) + 活着的智能体 DM(3)：alice/bob/carol 都没删，
	// 已删群与已删智能体不进 fan-out。
	if resp.Scanned != 4 {
		t.Fatalf("scanned = %d，want 4（1 活群 + 3 活智能体 DM）", resp.Scanned)
	}
	for _, h := range resp.Hits {
		if h.GroupID == string(deadGroup.ID) || h.AgentID == string(dave.ID) {
			t.Fatalf("软删目标出现在结果里: %+v", h)
		}
	}
	// 单会话 scope 也拒绝已删除目标。
	if ok, _, _ := hubCmd(t, m, buf, 10, "hub_search", map[string]any{
		"query": "zombie", "scope": "group", "group_id": string(deadGroup.ID),
	}); ok {
		t.Fatal("scope=group 指向已删除群必须失败")
	}
	if ok, _, _ := hubCmd(t, m, buf, 11, "hub_search", map[string]any{
		"query": "zombie", "scope": "dm", "agent_id": string(dave.ID),
	}); ok {
		t.Fatal("scope=dm 指向已删除智能体必须失败")
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
