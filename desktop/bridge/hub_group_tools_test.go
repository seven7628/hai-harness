package main

// P3a-05：group_history / group_members / group_say 模型工具。

import (
	"context"
	"encoding/json"
	"strings"
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
	if err != nil || len(msgs) != 3 || msgs[2].Sender.AgentID != a.ID {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	acts, _ := st.ReadActivity(ctx, g.ID, 0, 10)
	if len(acts) != 1 || acts[0].AfterSeq != 3 {
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
	// @all 普通成员被拒绝（路由层）。
	if _, err := call(bsA, "group_say", `{"group_id":"`+string(g.ID)+`","text":"@all 开会"}`); err != nil {
		t.Fatalf("owner @all should pass: %v", err)
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
	_ = time.Now
}
