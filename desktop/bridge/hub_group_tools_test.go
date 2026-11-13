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

	// 群工具只在群作用域会话里注册（场景1 隔离）：拿工具的会话必须是群会话。
	bsA, err := m.hubGroupSession(string(a.ID), string(g.ID))
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
	bsC, err := m.hubGroupSession(string(c.ID), string(g.ID))
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
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
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
	// 群作用域钉死：DM 会话里没有群工具；「同 agent 在另一个群」的会话也不能借
	// 参数里的 group_id 读别的群（跨群越权）。
	d := createHubAgent(t, m, buf, 5, "ToolD")
	ok, errMsg, data = hubCmd(t, m, buf, 6, "hub_group_create", map[string]any{
		"name": "旁群", "members": []map[string]any{
			{"agent_id": string(d.ID), "role": "owner"},
		},
	})
	if !ok {
		t.Fatalf("create side group: %s", errMsg)
	}
	sideGroup := groupFromData(t, data)
	bsD, err := m.hubGroupSession(string(d.ID), string(sideGroup.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := call(bsD, "group_history", `{"group_id":"`+string(g.ID)+`"}`); err == nil {
		t.Fatal("旁群会话不能借 group_id 读本群历史（群必须钉死在会话作用域）")
	}
	if dmD, err := m.hubDMSession(string(d.ID)); err != nil {
		t.Fatal(err)
	} else if _, err := dmD.hubEngine.GetTool(context.Background(), "group_history"); err == nil {
		t.Fatal("DM 会话不该注册 group_history（单聊能读群历史 = 串话）")
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
	// 图片附件进 group_history 只能留元数据：data URL（几 MB base64）会撑爆下一轮请求。
	imgURL := testImageDataURL("history-image")
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
		Text:   "看这张图",
		Attachments: []agenthub.Attachment{{
			Kind: agenthub.AttachmentKindImage, Name: "history.png", MIME: "image/png", DataURL: imgURL,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	out, err = call(bsA, "group_history", `{"group_id":"`+string(g.ID)+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "base64,") || strings.Contains(out, imgURL) {
		t.Fatalf("group_history 把图片 data URL 交给模型: %s", out)
	}
	if !strings.Contains(out, "history.png") || !strings.Contains(out, `"kind":"image"`) {
		t.Fatalf("group_history 丢了附件元数据: %s", out)
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
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
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
	// 群作用域会话不能跨群：第二条群说话必须用它在那个群的会话。
	bsB2, err := m.hubGroupSession(string(b.ID), string(defaultGroup.ID))
	if err != nil {
		t.Fatal(err)
	}
	callB2 := func(name, args string) (string, error) {
		tool, err := bsB2.hubEngine.GetTool(context.Background(), name)
		if err != nil {
			return "", err
		}
		return tool.Call(context.Background(), name, args)
	}
	for i := 0; i < 8; i++ {
		if _, err := callB2("group_say", fmt.Sprintf(`{"group_id":%q,"text":"第 %d 轮"}`, string(defaultGroup.ID), i+1)); err != nil {
			t.Fatalf("turn %d must pass: %v", i+1, err)
		}
	}
	if _, err := callB2("group_say", fmt.Sprintf(`{"group_id":%q,"text":"第 9 轮"}`, string(defaultGroup.ID))); err != nil {
		t.Fatalf("第 9 条必须放行（默认 MaxTurnsPerHuman 不再拦截）: %v", err)
	}
}

// 场景1（会话隔离回归）：群作用域会话把群**钉死**在 spec.Scope.GroupID。
//
// 同一智能体同时在群一/群二时，群一会话不能借参数里的 group_id 读写群二：
// group_history / group_members / agent_status（读）、group_say / handoff（写）、
// handoff_ack（归属按交接记录判）全部拒绝，群二日志不受影响；不传 group_id 时
// 缺省就是本群。与 assign_task/task_ack 同口径（群来自会话作用域，模型换不了群）。
func TestHubGroupSessionCannotTouchOtherGroup(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "IsoScopeA")
	b := createHubAgent(t, m, buf, 2, "IsoScopeB")
	g1 := groupWithMembers(t, m, buf, 3, "钉死群一", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	g2 := groupWithMembers(t, m, buf, 4, "钉死群二", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})

	const secret1 = "GROUP1-ONLY-内容-9b1"
	const secret2 = "GROUP2-ONLY-内容-777"
	st := mustHubStore(t, m)
	ctx := context.Background()
	for gid, text := range map[agenthub.GroupID]string{g1.ID: secret1, g2.ID: secret2} {
		if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
			GroupID: gid, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: text,
		}); err != nil {
			t.Fatal(err)
		}
	}
	before2 := mustReadLog(t, m, g2.ID)

	bsA1, err := m.hubGroupSession(string(a.ID), string(g1.ID))
	if err != nil {
		t.Fatal(err)
	}
	call := func(bs *bridgeSession, name, args string) (string, error) {
		t.Helper()
		tool, err := bs.hubEngine.GetTool(ctx, name)
		if err != nil {
			return "", err
		}
		return tool.Call(ctx, name, args)
	}

	// ① 读：带群二的 group_id 一律拒绝（读不到群二的历史/名册/成员状态）。
	for _, tc := range []struct{ name, args string }{
		{"group_history", `{"group_id":"` + string(g2.ID) + `"}`},
		{"group_members", `{"group_id":"` + string(g2.ID) + `"}`},
		{"agent_status", `{"group_id":"` + string(g2.ID) + `","agent_id":"` + string(b.ID) + `"}`},
	} {
		out, err := call(bsA1, tc.name, tc.args)
		if err == nil {
			t.Fatalf("群一会话用 %s 读到了群二: %s", tc.name, out)
		}
		if strings.Contains(err.Error(), secret2) {
			t.Fatalf("%s 的错误里带出了群二内容: %v", tc.name, err)
		}
	}
	// ② 写：group_say / handoff 带群二一律拒绝。
	if out, err := call(bsA1, "group_say", `{"group_id":"`+string(g2.ID)+`","text":"越权发言"}`); err == nil {
		t.Fatalf("群一会话在群二发言成功: %s", out)
	}
	if out, err := call(bsA1, "handoff", `{"group_id":"`+string(g2.ID)+`","to_agent_id":"`+string(b.ID)+`","instruction":"越权交接"}`); err == nil {
		t.Fatalf("群一会话在群二发起交接成功: %s", out)
	}
	// ③ handoff_ack 没有 group_id 参数：按交接记录归属判，群一会话不能 ack 群二的交接。
	bsB2, err := m.hubGroupSession(string(b.ID), string(g2.ID))
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(bsB2, "handoff", `{"group_id":"`+string(g2.ID)+`","to_agent_id":"`+string(a.ID)+`","instruction":"群二里的交接"}`)
	if err != nil {
		t.Fatalf("群二内发起交接（正对照）: %v", err)
	}
	var created struct {
		Handoff agenthub.Handoff `json:"handoff"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Handoff.ID == "" {
		t.Fatalf("交接结果 = %s (%v)", out, err)
	}
	if out, err := call(bsA1, "handoff_ack", `{"handoff_id":"`+created.Handoff.ID+`","accept":true}`); err == nil {
		t.Fatalf("群一会话 ack 了群二的交接: %s", out)
	}
	if h, err := st.LoadHandoff(ctx, created.Handoff.ID); err != nil {
		t.Fatal(err)
	} else if h.Status != agenthub.HandoffPendingAck {
		t.Fatalf("被拒绝的越权 ack 改了交接状态: %+v", h)
	}
	// 正对照：群二会话能 ack。
	bsA2, err := m.hubGroupSession(string(a.ID), string(g2.ID))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := call(bsA2, "handoff_ack", `{"handoff_id":"`+created.Handoff.ID+`","accept":true}`); err != nil {
		t.Fatalf("群二会话 ack（正对照）: %v", err)
	} else if !strings.Contains(out, string(agenthub.HandoffAccepted)) {
		t.Fatalf("ack 结果 = %s", out)
	}

	// ④ 群二日志没有被越权写入：新增的只能是群二自己产生的交接卡/回执卡。
	for _, msg := range mustReadLog(t, m, g2.ID)[len(before2):] {
		if msg.Kind != agenthub.KindHandoff {
			t.Fatalf("群二出现越权写入: %+v", msg)
		}
	}
	for _, msg := range mustReadLog(t, m, g2.ID) {
		if strings.Contains(msg.Text, "越权发言") || strings.Contains(msg.Text, "越权交接") {
			t.Fatalf("越权内容进了群二日志: %+v", msg)
		}
	}

	// ⑤ 缺省 = 本群：不传 group_id 时读写的都是群一。
	if out, err := call(bsA1, "group_history", `{}`); err != nil {
		t.Fatalf("不传 group_id 应缺省为本群: %v", err)
	} else if !strings.Contains(out, secret1) || strings.Contains(out, secret2) {
		t.Fatalf("缺省群不是本群: %s", out)
	}
	if out, err := call(bsA1, "group_members", `{}`); err != nil {
		t.Fatalf("group_members 缺省本群: %v", err)
	} else if !strings.Contains(out, "IsoScopeB") || strings.Contains(out, string(g2.ID)) {
		t.Fatalf("缺省群名册不对: %s", out)
	}
	const localSay = "本群缺省发言"
	if _, err := call(bsA1, "group_say", `{"text":"`+localSay+`"}`); err != nil {
		t.Fatalf("缺省本群发言: %v", err)
	}
	foundLocal := false
	for _, msg := range mustReadLog(t, m, g1.ID) {
		if strings.Contains(msg.Text, localSay) {
			foundLocal = true
		}
	}
	if !foundLocal {
		t.Fatalf("缺省 group_say 没落进群一")
	}
	for _, msg := range mustReadLog(t, m, g2.ID) {
		if strings.Contains(msg.Text, localSay) {
			t.Fatalf("缺省 group_say 落进了群二: %+v", msg)
		}
	}
}
