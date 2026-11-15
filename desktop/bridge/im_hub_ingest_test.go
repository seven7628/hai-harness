package main

// 渠道（飞书）→ Hub 入向落账回归：
// IM 消息不能只 Ask 模型，必须走 hub_send/hub_group_send 的同一条路径（落 DM/群日志
// + hub_message + @ 路由），否则 Hub 时间线看不到外部用户说的话。

import (
	"context"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
)

func TestIMExternalDMIngestWritesHubLogAndEvent(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "IMExternalDM")
	buf.Reset()

	m.hubSendExternal(string(a.ID), "帮我看下 401", nil, "飞书张三", "/mock/ws")
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := st.ReadDirectMessages(context.Background(), a.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("DM messages = %d, want 1", len(msgs))
	}
	got := msgs[0]
	if got.Sender.Kind != agenthub.SenderExternal || got.Sender.Name != "飞书张三" {
		t.Fatalf("sender = %+v", got.Sender)
	}
	if !strings.Contains(got.Text, "401") {
		t.Fatalf("text = %q", got.Text)
	}
	if !strings.Contains(buf.String(), `"event_type":"hub_message"`) {
		t.Fatalf("缺少 hub_message 事件: %s", buf.String())
	}
	if strings.Contains(buf.String(), `"event_type":"command_response"`) {
		t.Fatalf("内部入向不应写 command_response: %s", buf.String())
	}
}

func TestIMExternalGroupIngestWritesHubLogAndEvent(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "IMExternalGroupOwner")
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_group_create", map[string]any{
		"name":    "渠道群",
		"members": []map[string]any{{"agent_id": string(a.ID), "role": "owner"}},
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	g := groupFromData(t, data)
	buf.Reset()

	m.hubGroupSendExternal(string(g.ID), string(a.ID), "这个版本什么时候发", nil, "飞书李四", "/mock/ws")
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var got *agenthub.Message
	for i := range msgs {
		if msgs[i].Sender.Kind == agenthub.SenderExternal {
			got = &msgs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("group log 里没有外部消息: %+v", msgs)
	}
	if got.Sender.Name != "飞书李四" {
		t.Fatalf("sender = %+v", got.Sender)
	}
	if !strings.Contains(got.Text, "什么时候发") {
		t.Fatalf("text = %q", got.Text)
	}
	if !strings.Contains(buf.String(), `"event_type":"hub_message"`) {
		t.Fatalf("缺少 hub_message 事件: %s", buf.String())
	}
	if !strings.Contains(buf.String(), `"event_type":"hub_round"`) {
		t.Fatalf("渠道群消息应唤醒执行者（hub_round）: %s", buf.String())
	}
	if strings.Contains(buf.String(), `"event_type":"command_response"`) {
		t.Fatalf("内部入向不应写 command_response: %s", buf.String())
	}
}
