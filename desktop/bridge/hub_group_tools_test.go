package main

// P3a-05：group_history / group_members / group_say 模型工具。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

func TestHubGroupTools(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "ToolA")
	b := createHubAgent(t, m, buf, 2, "ToolB")
	c := createHubAgent(t, m, buf, 3, "ToolC")
	ok, errMsg, data := hubCmd(t, m, buf, 4, "hub_group_create", map[string]any{
		"name": "工具群", "members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
			{"agent_id": string(c.ID), "role": "observer"},
		},
	})
	if !ok {
		t.Fatalf("create: %s", errMsg)
	}
	g := groupFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// 一条 owner-only 消息 + 一条普通消息。
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "私密指令", Visibility: agenthub.VisibilityOwner,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "公开消息",
	}); err != nil {
		t.Fatal(err)
	}

	bsA, err := m.hubDMSession(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	call := func(bs *bridgeSession, name, args string) (string, error) {
		tool, err := bs.hubEngine.GetTool(context.Background(), name)
		if err != nil {
			return "", err
		}
		return tool.Call(context.Background(), name, args)
	}
	// owner 能读；owner-only 消息被 redacted。
	out, err := call(bsA, "group_members", `{"group_id":"`+string(g.ID)+`"}`)
	if err != nil || !strings.Contains(out, "ToolA") || !strings.Contains(out, "observer") {
		t.Fatalf("members = %s, %v", out, err)
	}
	out, err = call(bsA, "group_history", `{"group_id":"`+string(g.ID)+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "私密指令") || !strings.Contains(out, "公开消息") || !strings.Contains(out, agenthub.OwnerOnlyPlaceholder) {
		t.Fatalf("history leaks = %s", out)
	}
	// search 也必须走可见性过滤，不能拿 query 布尔探测 owner-only 正文（P3a-05 B2）。
	out, err = call(bsA, "group_history", `{"group_id":"`+string(g.ID)+`","query":"私密"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "私密指令") || !strings.Contains(out, `"count":0`) {
		t.Fatalf("search leaks owner-only: %s", out)
	}
	// group_say：@ToolB 路由 + 落日志。
	out, err = call(bsA, "group_say", `{"group_id":"`+string(g.ID)+`","text":"@ToolB 帮我查日志"}`)
	if err != nil {
		t.Fatalf("say: %v", err)
	}
	var said struct {
		Seq     int64              `json:"seq"`
		Targets []agenthub.AgentID `json:"targets"`
	}
	if json.Unmarshal([]byte(out), &said) != nil || said.Seq != 3 || len(said.Targets) != 1 || said.Targets[0] != b.ID {
		t.Fatalf("say = %s", out)
	}
	msgs, err := agenthub.ReadGroupLog(ctx, st, g.ID, 0, 10)
	if err != nil || len(msgs) != 4 || msgs[2].Sender.AgentID != a.ID || msgs[3].Kind != agenthub.KindRouting {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	acts, _ := st.ReadActivity(ctx, g.ID, 0, 10)
	if len(acts) != 1 || acts[0].AfterSeq != 4 {
		t.Fatalf("receipt = %+v", acts)
	}
	// observer 不能发言；非成员不能读。
	bsC, err := m.hubDMSession(string(c.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(bsC, "group_say", `{"group_id":"`+string(g.ID)+`","text":"observer 说话"}`); err == nil {
		t.Fatal("observer must not speak")
	}
	// @all 普通成员被拒绝（路由层），且失败不能留下半条日志（P3a-05 B1）。
	if _, err := call(bsA, "group_say", `{"group_id":"`+string(g.ID)+`","text":"@all 开会"}`); err != nil {
		t.Fatalf("owner @all should pass: %v", err)
	}
	bsB, err := m.hubDMSession(string(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	before, err := agenthub.ReadGroupLog(ctx, st, g.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(bsB, "group_say", `{"group_id":"`+string(g.ID)+`","text":"@all 越权"}`); err == nil {
		t.Fatal("member @all must be rejected")
	}
	after, err := agenthub.ReadGroupLog(ctx, st, g.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("rejected @all left a log entry: before=%d after=%d", len(before), len(after))
	}
	// 非成员（用另一个没入群的智能体）读失败。
	d := createHubAgent(t, m, buf, 5, "ToolD")
	bsD, err := m.hubDMSession(string(d.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(bsD, "group_history", `{"group_id":"`+string(g.ID)+`"}`); err == nil {
		t.Fatal("non-member must not read")
	}
	// approval_stub 只进人类 UI：group_history/search 都不能把占位正文交给 agent（P3a-07 B3）。
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindApprovalStub,
		Sender: agenthub.Sender{Kind: agenthub.SenderSystem}, Text: "批准 rm -rf /tmp/secret",
	}); err != nil {
		t.Fatal(err)
	}
	out, err = call(bsA, "group_history", `{"group_id":"`+string(g.ID)+`"}`)
	if err != nil || strings.Contains(out, "rm -rf") || strings.Contains(out, "approval_stub") {
		t.Fatalf("history leaked approval stub: %s, %v", out, err)
	}
	out, err = call(bsA, "group_history", `{"group_id":"`+string(g.ID)+`","query":"rm -rf"}`)
	if err != nil || !strings.Contains(out, `"count":0`) {
		t.Fatalf("search leaked approval stub: %s, %v", out, err)
	}
	_ = time.Now
}

// 用户口径「群策略限制取消」：MaxTurnsPerHuman 字段保留但不再拦截
// （旧的 8 轮/1 轮闸门都取消）；保留名仍不能建成智能体。
func TestHubGroupSayIgnoresMaxTurnsPerHumanAndReservedName(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "LimitA")
	b := createHubAgent(t, m, buf, 2, "LimitB")
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name": "限流群",
		"members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
		"policy": map[string]any{"max_turns_per_human": 1},
	})
	if !ok {
		t.Fatalf("create: %s", errMsg)
	}
	g := groupFromData(t, data)
	bsB, err := m.hubDMSession(string(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	// 正常群一定有人类种子消息：必须从最近一条人类消息开始数 agent 轮次。
	if ok, errMsg, _ := hubCmd(t, m, buf, 10, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "开始",
	}); !ok {
		t.Fatalf("human seed: %s", errMsg)
	}
	call := func(name, args string) (string, error) {
		tool, err := bsB.hubEngine.GetTool(context.Background(), name)
		if err != nil {
			return "", err
		}
		return tool.Call(context.Background(), name, args)
	}
	// max_turns_per_human=1 已经不管用：连续 3 条都放行。
	for i := 1; i <= 3; i++ {
		if _, err := call("group_say", fmt.Sprintf(`{"group_id":%q,"text":"第 %d 轮"}`, string(g.ID), i)); err != nil {
			t.Fatalf("第 %d 条必须放行（max_turns_per_human 不再拦截）: %v", i, err)
		}
	}
	// 人类发言照旧插队（不进对话队列），但也不再有「重置后只能说一条」。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "继续说",
	}); !ok {
		t.Fatalf("human seed: %s", errMsg)
	}
	if _, err := call("group_say", `{"group_id":"`+string(g.ID)+`","text":"人类之后第一条"}`); err != nil {
		t.Fatalf("human 之后的 agent 发言必须放行: %v", err)
	}
	// 并发 group_say 仍串行过「路由→落盘」：6 个同时发，一个都不能被吞。
	var wg sync.WaitGroup
	var mu sync.Mutex
	passed := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := call("group_say", fmt.Sprintf(`{"group_id":%q,"text":"并发 %d"}`, string(g.ID), i)); err == nil {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if passed != 6 {
		t.Fatalf("concurrent group_say passed = %d, want 6（不再有限流闸门）", passed)
	}
	// 落账条数对得上：3 + 1 + 6 = 10 条 agent 消息，不丢不重。
	st := mustHubStore(t, m)
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	agentMsgs := 0
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderAgent {
			agentMsgs++
		}
	}
	if agentMsgs != 10 {
		t.Fatalf("agent messages = %d, want 10", agentMsgs)
	}
	if ok, _, _ := hubCmd(t, m, buf, 4, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "user", "provider": "deepseek", "model": "deepseek-v4-flash", "role": "x"},
	}); ok {
		t.Fatal("reserved profile name must be rejected")
	}
	// 默认策略（不显式设置 max_turns_per_human）：第 9 条同样放行。
	ok, errMsg, data = hubCmd(t, m, buf, 13, "hub_group_create", map[string]any{
		"name": "默认限流群", "members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if !ok {
		t.Fatalf("default group: %s", errMsg)
	}
	defaultGroup := groupFromData(t, data)
	if ok, errMsg, _ := hubCmd(t, m, buf, 14, "hub_group_send", map[string]any{
		"group_id": string(defaultGroup.ID), "text": "开始",
	}); !ok {
		t.Fatalf("default seed: %s", errMsg)
	}
	for i := 0; i < 8; i++ {
		if _, err := call("group_say", fmt.Sprintf(`{"group_id":%q,"text":"第 %d 轮"}`, string(defaultGroup.ID), i+1)); err != nil {
			t.Fatalf("turn %d must pass: %v", i+1, err)
		}
	}
	if _, err := call("group_say", fmt.Sprintf(`{"group_id":%q,"text":"第 9 轮"}`, string(defaultGroup.ID))); err != nil {
		t.Fatalf("第 9 条必须放行（默认 MaxTurnsPerHuman 不再拦截）: %v", err)
	}
}
