package main

// 附件协议在 bridge 的落点：DM/群发消息带图片 → 图片块进模型上下文 + 附件随消息
// 落账（刷新后可回显）；纯图消息存 `[图片]` 占位；超限/坏附件拒绝且不落账。

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

func testImageDataURL(seed string) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(seed))
}

func imageAttachmentPayload(dataURL string) map[string]any {
	return map[string]any{
		"kind": "image", "name": "shot.png", "mime_type": "image/png",
		"bytes": float64(9), "data_url": dataURL,
	}
}

// waitEvalRequest 等 fake provider 至少跑一轮并返回最后一次请求。
func waitEvalRequest(t *testing.T, p *evalProvider) *provider.StreamRequest {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.callCount() > 0 {
			return p.lastRequest()
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("provider 没有被调用：附件消息没有进入模型上下文")
	return nil
}

func requestHasImageBlock(req *provider.StreamRequest, dataURL string) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeImage && c.Content == dataURL && c.MimeType == "image/png" {
				return true
			}
		}
	}
	return false
}

func requestHasTextBlock(req *provider.StreamRequest, text string) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeText && c.Content == text {
				return true
			}
		}
	}
	return false
}

// 带图 DM：Ask 的 content 必须有 image 块（进模型上下文），索引里存完整附件。
func TestHubSendWithImageAttachment(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "ImageDM")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	fake := &evalProvider{reply: "收到图片"}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(fake)))

	dataURL := testImageDataURL("dm-image")
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_send", map[string]any{
		"target":      map[string]any{"kind": "dm", "agent_id": agentID},
		"text":        "看这张图",
		"attachments": []any{imageAttachmentPayload(dataURL)},
	})
	if !ok {
		t.Fatalf("send failed: %s", errMsg)
	}
	if data["seq"] != float64(1) {
		t.Fatalf("seq = %v, want 1", data["seq"])
	}
	req := waitEvalRequest(t, fake)
	if !requestHasTextBlock(req, "看这张图") {
		t.Fatal("正文块没有进 Ask")
	}
	if !requestHasImageBlock(req, dataURL) {
		t.Fatal("image 块没有进 Ask（模型看不到图片）")
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := st.ReadDirectMessages(context.Background(), p.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("DM 索引没有落账")
	}
	got := msgs[0]
	if got.Text != "看这张图" {
		t.Fatalf("text = %q", got.Text)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].DataURL != dataURL ||
		got.Attachments[0].Kind != agenthub.AttachmentKindImage ||
		got.Attachments[0].MIME != "image/png" {
		t.Fatalf("持久化附件 = %+v, want 完整 data URL（前端刷新要能显示）", got.Attachments)
	}
}

// 纯图 DM：text 为空也合法；落账 Text 是 `[图片]`，Ask 不带 text 块。
func TestHubSendImageOnlyStoresPlaceholder(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "ImageOnly")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	fake := &evalProvider{reply: "看到图了"}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(fake)))

	dataURL := testImageDataURL("only-image")
	ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_send", map[string]any{
		"target":      map[string]any{"kind": "dm", "agent_id": agentID},
		"attachments": []any{imageAttachmentPayload(dataURL)},
	})
	if !ok {
		t.Fatalf("纯图消息应被接受: %s", errMsg)
	}
	req := waitEvalRequest(t, fake)
	if !requestHasImageBlock(req, dataURL) {
		t.Fatal("纯图消息的 image 块没有进 Ask")
	}
	if requestHasTextBlock(req, "") {
		t.Fatal("纯图消息不该带空 text 块")
	}
	st, _ := m.hubStore()
	msgs, err := st.ReadDirectMessages(context.Background(), p.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 || msgs[0].Text != hubImageOnlyText {
		t.Fatalf("纯图消息落账 = %+v, want Text %q", msgs, hubImageOnlyText)
	}
	if len(msgs[0].Attachments) != 1 {
		t.Fatalf("纯图消息附件 = %+v", msgs[0].Attachments)
	}
}

// 超限/坏附件：命令失败、不落账、不进模型上下文。
func TestHubSendAttachmentValidationRejected(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "BadAttach")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	fake := &evalProvider{reply: "不该被调用"}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(fake)))

	tooBig := "data:image/png;base64," + strings.Repeat("A", agenthub.MaxAttachmentImageBytes)
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name: "图片总量超限",
			payload: map[string]any{
				"text": "大图", "attachments": []any{imageAttachmentPayload(tooBig)},
			},
			want: "超过上限",
		},
		{
			name: "kind 非法",
			payload: map[string]any{
				"text": "x", "attachments": []any{map[string]any{"kind": "video", "name": "a.mp4"}},
			},
			want: "kind",
		},
		{
			name: "图片外链",
			payload: map[string]any{
				"text": "x", "attachments": []any{map[string]any{"kind": "image", "data_url": "https://example.com/a.png"}},
			},
			want: "data_url",
		},
		{
			name: "纯文件且无正文",
			payload: map[string]any{
				"attachments": []any{map[string]any{"kind": "file", "name": "r.pdf", "path": "/tmp/r.pdf"}},
			},
			want: "text 不能为空",
		},
	}
	cmdID := 2
	for _, tc := range cases {
		tc.payload["target"] = map[string]any{"kind": "dm", "agent_id": agentID}
		ok, errMsg, _ := hubCmd(t, m, buf, cmdID, "hub_send", tc.payload)
		cmdID++
		if ok {
			t.Fatalf("%s: 非法附件被接受", tc.name)
		}
		if !strings.Contains(errMsg, tc.want) {
			t.Fatalf("%s: err = %q, want contains %q", tc.name, errMsg, tc.want)
		}
	}
	// 一条都不该落账，也不该跑一轮。
	st, _ := m.hubStore()
	msgs, err := st.ReadDirectMessages(context.Background(), p.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("非法附件落账了: %+v", msgs)
	}
	if fake.callCount() != 0 {
		t.Fatalf("非法附件进了模型上下文（provider 调用 %d 次）", fake.callCount())
	}
}

// 群消息带图：附件存进 msg，唤醒目标的 Ask 里有 image 块。
func TestHubGroupSendWithImageAttachment(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "ImgOwner")
	b := createHubAgent(t, m, buf, 2, "ImgMember")
	g := groupWithMembers(t, m, buf, 3, "图片群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	fake := &evalProvider{reply: "群里收到图片"}
	setGroupFakeLoop(t, m, bs, g, string(b.ID), fake)

	dataURL := testImageDataURL("group-image")
	ok, errMsg, data := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id":    string(g.ID),
		"text":        "@ImgMember 看这张图",
		"attachments": []any{imageAttachmentPayload(dataURL)},
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	round, _ := data["round"].(map[string]any)
	if round == nil {
		t.Fatalf("send payload = %+v", data)
	}
	st := mustHubStore(t, m)
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("群消息没有落账")
	}
	trigger := msgs[0]
	if len(trigger.Attachments) != 1 || trigger.Attachments[0].DataURL != dataURL {
		t.Fatalf("群消息附件 = %+v, want 完整 data URL", trigger.Attachments)
	}
	req := waitEvalRequest(t, fake)
	if !requestHasImageBlock(req, dataURL) {
		t.Fatal("群唤醒的 Ask 里没有 image 块")
	}
	if !requestHasTextBlock(req, "@ImgMember 看这张图") {
		t.Fatal("群唤醒的 Ask 里没有触发消息正文")
	}
}

// 纯图群消息 + 结构化 @：落 `[图片]`，仍能唤醒目标并带上图片块。
func TestHubGroupSendImageOnlyWithStructuredMention(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "ImgOnlyOwner")
	b := createHubAgent(t, m, buf, 2, "ImgOnlyMember")
	g := groupWithMembers(t, m, buf, 3, "纯图群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	fake := &evalProvider{reply: "看到图"}
	setGroupFakeLoop(t, m, bs, g, string(b.ID), fake)

	dataURL := testImageDataURL("group-only-image")
	ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id":    string(g.ID),
		"mentions":    []any{string(b.ID)},
		"attachments": []any{imageAttachmentPayload(dataURL)},
	})
	if !ok {
		t.Fatalf("纯图群消息应被接受: %s", errMsg)
	}
	st := mustHubStore(t, m)
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 || msgs[0].Text != hubImageOnlyText {
		t.Fatalf("纯图群消息落账 = %+v, want Text %q", msgs, hubImageOnlyText)
	}
	if len(msgs[0].Attachments) != 1 {
		t.Fatalf("纯图群消息附件 = %+v", msgs[0].Attachments)
	}
	req := waitEvalRequest(t, fake)
	if !requestHasImageBlock(req, dataURL) {
		t.Fatal("纯图群消息的唤醒里没有 image 块")
	}
}

// 旧 text-only 行为：无附件字段时照旧，且不会写出附件。
func TestHubSendTextOnlyUnchanged(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "PlainDM")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	fake := &evalProvider{reply: "ok"}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(fake)))

	ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID},
		"text":   "纯文本",
	})
	if !ok {
		t.Fatalf("send failed: %s", errMsg)
	}
	req := waitEvalRequest(t, fake)
	if !requestHasTextBlock(req, "纯文本") {
		t.Fatal("纯文本消息的正文块丢了")
	}
	st, _ := m.hubStore()
	msgs, err := st.ReadDirectMessages(context.Background(), p.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 || msgs[0].Text != "纯文本" || msgs[0].Attachments != nil {
		t.Fatalf("纯文本消息落账 = %+v", msgs)
	}
}
