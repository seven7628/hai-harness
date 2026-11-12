package agenthub

// Message.Attachments：JSON 往返、旧数据（无字段）兼容、Redacted 清附件。

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestMessageAttachmentsJSONRoundTrip(t *testing.T) {
	msg := Message{
		Seq: 7, GroupID: "grp_x", Kind: KindText,
		Sender: Sender{Kind: SenderHuman}, Text: "看这两张",
		Visibility: VisibilityGroup,
		Attachments: []Attachment{
			{Kind: AttachmentKindImage, Name: "a.png", MIME: "image/png", Bytes: 3, DataURL: "data:image/png;base64,QUJD"},
			{Kind: AttachmentKindFile, Name: "r.pdf", Path: "/tmp/r.pdf", Bytes: 9},
		},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	var back Message
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Attachments, msg.Attachments) {
		t.Fatalf("attachments roundtrip = %+v, want %+v", back.Attachments, msg.Attachments)
	}
}

// 旧 JSONL 没有 attachments 字段：解出来必须是 nil（omitempty 也不会多写字段）。
func TestMessageAttachmentsLegacyJSON(t *testing.T) {
	const legacy = `{"seq":1,"id":"msg_1","sender":{"kind":"human"},"kind":"text","text":"旧消息","visibility":"group","created_at":"2026-01-02T03:04:05Z"}`
	var msg Message
	if err := json.Unmarshal([]byte(legacy), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Attachments != nil {
		t.Fatalf("旧消息 attachments = %+v, want nil", msg.Attachments)
	}
	raw, err := json.Marshal(Message{Seq: 2, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "无附件"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); containsField(raw, "attachments") {
		t.Fatalf("无附件的消息不该写出 attachments 字段: %s", got)
	}
}

func containsField(raw []byte, field string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	_, ok := m[field]
	return ok
}

// Redacted 是 owner-only 的防泄漏边界：附件（含图片 data URL 与文件名）必须一起清空。
func TestRedactedClearsAttachments(t *testing.T) {
	msg := Message{
		Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "只给群主看",
		Visibility: VisibilityOwner,
		Attachments: []Attachment{
			{Kind: AttachmentKindImage, Name: "secret.png", MIME: "image/png", DataURL: "data:image/png;base64,QUJD"},
		},
		Refs:     map[string]any{"client_msg_id": "c-1"},
		Mentions: []AgentID{"agent_a"},
	}
	forAgent := msg.Redacted(false)
	if forAgent.Attachments != nil {
		t.Fatalf("owner-only 附件泄漏给智能体: %+v", forAgent.Attachments)
	}
	if forAgent.Text != OwnerOnlyPlaceholder || forAgent.Refs != nil || forAgent.Mentions != nil {
		t.Fatalf("redacted = %+v, want 只留占位正文", forAgent)
	}
	// 人读自己的 owner-only 消息：原样保留。
	forHuman := msg.Redacted(true)
	if !reflect.DeepEqual(forHuman.Attachments, msg.Attachments) || forHuman.Text != msg.Text {
		t.Fatalf("human redacted = %+v, want 原样", forHuman)
	}
}

// AgentToolView 是 group_history 等文本工具的边界：保留附件元数据（模型知道有图），
// 但绝不能带 data URL —— 工具结果是文本，几 MB base64 会直接撑爆下一轮请求。
func TestAgentToolViewStripsImageDataURL(t *testing.T) {
	msg := Message{
		Seq: 3, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "看这张图",
		Visibility: VisibilityGroup,
		Attachments: []Attachment{
			{Kind: AttachmentKindImage, Name: "a.png", MIME: "image/png", Bytes: 3, DataURL: "data:image/png;base64,QUJD"},
			{Kind: AttachmentKindFile, Name: "r.pdf", Path: "/tmp/r.pdf", Bytes: 9},
		},
	}
	view := msg.AgentToolView()
	if len(view.Attachments) != 2 {
		t.Fatalf("附件元数据丢了: %+v", view.Attachments)
	}
	if view.Attachments[0].DataURL != "" || view.Attachments[0].Name != "a.png" || view.Attachments[0].Kind != AttachmentKindImage {
		t.Fatalf("image view = %+v, want 只留元数据", view.Attachments[0])
	}
	if view.Attachments[1].Path != "/tmp/r.pdf" {
		t.Fatalf("file view = %+v, want 保留 path", view.Attachments[1])
	}
	// 不能改动原消息（工具视图是副本）。
	if msg.Attachments[0].DataURL == "" {
		t.Fatal("AgentToolView 修改了原消息")
	}
	// owner-only 仍然整体清空。
	msg.Visibility = VisibilityOwner
	if got := msg.AgentToolView(); got.Attachments != nil {
		t.Fatalf("owner-only 附件泄漏: %+v", got.Attachments)
	}
}
