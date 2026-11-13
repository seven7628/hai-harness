package main

// 附件协议在 bridge 的落点：DM/群发消息带图片 → 图片块进模型上下文 + 附件随消息
// 落账（刷新后可回显）；纯图消息存 `[图片]` 占位；超限/坏附件拒绝且不落账。

import (
	"context"
	"encoding/base64"
	"fmt"
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

func requestHasTextContaining(req *provider.StreamRequest, sub string) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeText && strings.Contains(c.Content, sub) {
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

// 目标忙时图片消息不排队（wake 返回 busy）；下一次被 @ 时必须把积压的图片补进
// 上下文，否则模型只看到快照里的 `[图片]` 占位，永远看不见图（B2）。
func TestHubGroupBusyImageDeliveredOnNextWake(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "BusyImgOwner")
	b := createHubAgent(t, m, buf, 2, "BusyImgMember")
	g := groupWithMembers(t, m, buf, 3, "忙图群",
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
	fake := &evalProvider{reply: "第二次看到图了"}
	setGroupFakeLoop(t, m, bs, g, string(b.ID), fake)

	bs.hubGroupBusy.Store(true)
	dataURL := testImageDataURL("busy-pending-image")
	ok, errMsg, data := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id":    string(g.ID),
		"text":        "@BusyImgMember 这张图稍后看",
		"attachments": []any{imageAttachmentPayload(dataURL)},
	})
	if !ok {
		t.Fatalf("忙目标也要先落消息: %s", errMsg)
	}
	if errs, _ := data["wake_errors"].([]any); len(errs) == 0 {
		t.Fatal("忙目标应报告 wake_errors")
	}
	if fake.callCount() != 0 {
		t.Fatalf("忙目标不该跑模型，实际调用 %d 次", fake.callCount())
	}

	// 下一轮（无附件）被 @：积压的图片必须补进这次请求。
	bs.hubGroupBusy.Store(false)
	ok, errMsg, _ = hubCmd(t, m, buf, 5, "hub_group_send", map[string]any{
		"group_id": string(g.ID),
		"text":     "@BusyImgMember 现在看上面那张图",
	})
	if !ok {
		t.Fatalf("第二次 send: %s", errMsg)
	}
	req := waitEvalRequest(t, fake)
	if !requestHasImageBlock(req, dataURL) {
		t.Fatal("下一次 wake 没有补发积压的图片：模型只能看到 [图片] 占位")
	}
	if !requestHasTextContaining(req, "此前未送达") {
		t.Fatal("补发积压图片时缺少说明块，模型不知道图对应哪条消息")
	}
}

// 场景 5-3 的负对照：老图被挤出「最近 500 条群消息」之后，下一次 @ 仍然必须补发。
// 旧实现用 ReadMessages(0, 500) 当补发窗口，seq 1 的图永远看不见（模型只看到
// 快照里的 `[图片]` 占位，然后永远当作自己看过）。
func TestHubGroupWakeBacklogImageBeyondRecentPage(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "BacklogOwner")
	b := createHubAgent(t, m, buf, 2, "BacklogB")
	g := groupWithMembers(t, m, buf, 3, "积压图群",
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
	fake := &evalProvider{reply: "看到那张老图了"}
	setGroupFakeLoop(t, m, bs, g, string(b.ID), fake)

	st := mustHubStore(t, m)
	ctx := context.Background()
	oldImage := testImageDataURL("old-backlog-image")
	oldSeq, err := st.AppendMessage(ctx, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
		Text:   hubImageOnlyText,
		Attachments: []agenthub.Attachment{{
			Kind: agenthub.AttachmentKindImage, Name: "old.png", MIME: "image/png", DataURL: oldImage,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 纯文本灌到一页之外：老图被挤出「最近 500 条」。
	for i := 0; i < hubWakeBacklogPage; i++ {
		if _, err := st.AppendMessage(ctx, &agenthub.Message{
			GroupID: g.ID, Kind: agenthub.KindText,
			Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
			Text:   fmt.Sprintf("灌水 %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 负对照前提：最近一页（旧实现的补发窗口）里没有这张图。
	head, err := st.ReadMessages(ctx, g.ID, 0, hubWakeBacklogPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) != hubWakeBacklogPage || head[0].Seq <= oldSeq {
		t.Fatalf("最近一页 = %d 条、最旧 seq %d（老图 seq %d），负对照不成立",
			len(head), head[0].Seq, oldSeq)
	}
	for _, msg := range head {
		if len(msg.Attachments) > 0 {
			t.Fatalf("最近一页不该有图：seq %d", msg.Seq)
		}
	}

	// 人类 @ 唤醒：补发必须翻过最近 500 条，把游标之后的老图带上。
	ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID),
		"text":     "@BacklogB 上面那张老图讲的是什么",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	req := waitEvalRequest(t, fake)
	if !requestHasImageBlock(req, oldImage) {
		t.Fatal("游标之后、最近 500 条之外的老图没有补发：模型只看到 [图片] 占位")
	}
	if !requestHasTextContaining(req, "此前未送达") {
		t.Fatal("补发说明块缺失：模型不知道图对应哪条消息")
	}
}

// seedGroupLog 把消息按顺序写进内存 store（seq 由 store 分配 1..N，旧→新），返回该
// store。构造积压窗口用它比走 hub_group_send 快得多：后者每条都要路由/回执/唤醒。
func seedGroupLog(t *testing.T, gid agenthub.GroupID, msgs ...agenthub.Message) *agenthub.MemoryStore {
	t.Helper()
	ctx := context.Background()
	st := agenthub.NewMemoryStore()
	for i := range msgs {
		m := msgs[i]
		m.GroupID = gid
		if m.Kind == "" {
			m.Kind = agenthub.KindText
		}
		if m.Sender.Kind == "" {
			m.Sender = agenthub.Sender{Kind: agenthub.SenderHuman}
		}
		if _, err := st.AppendMessage(ctx, &m); err != nil {
			t.Fatalf("append message %d: %v", i+1, err)
		}
	}
	return st
}

// 补发上限：只保留最近 10 张（与前端附件张数上限对齐，按时间顺序给模型），
// 更早的计入 omitted，不静默丢。这里的 10 是**硬编码的验收值**（不是引用常量）：
// 前端 MAX_IMAGE_ATTACHMENTS=10，补发窗口偷偷退回 6 张必须让这条测试红。
func TestHubPendingImagesCapsNewest(t *testing.T) {
	const wantLimit = 10
	if hubWakePendingImageLimit != wantLimit {
		t.Fatalf("hubWakePendingImageLimit = %d, want %d（对齐前端 MAX_IMAGE_ATTACHMENTS）",
			hubWakePendingImageLimit, wantLimit)
	}
	gid := agenthub.GroupID("g_caps")
	msgs := make([]agenthub.Message, 0, wantLimit+1)
	for i := 1; i <= wantLimit+1; i++ {
		msgs = append(msgs, agenthub.Message{
			Text: "图", Attachments: []agenthub.Attachment{{
				Kind: agenthub.AttachmentKindImage, Name: fmt.Sprintf("img%d", i),
				MIME: "image/png", DataURL: "data:image/png;base64,QUJD",
			}},
		})
	}
	st := seedGroupLog(t, gid, msgs...)
	got, err := hubPendingImagesFromStore(context.Background(), st, gid, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.atts) != wantLimit || got.omitted != 1 {
		t.Fatalf("atts=%d omitted=%d, want %d/1", len(got.atts), got.omitted, wantLimit)
	}
	if len(got.seqs) != len(got.atts) || got.seqs[0] != 2 || got.seqs[len(got.seqs)-1] != int64(wantLimit+1) {
		t.Fatalf("seqs = %v, want 2..%d（从新到旧取，按时间顺序给）", got.seqs, wantLimit+1)
	}
	if got.atts[0].Name != "img2" || got.atts[len(got.atts)-1].Name != fmt.Sprintf("img%d", wantLimit+1) {
		t.Fatalf("顺序 = %s..%s, want img2..img%d", got.atts[0].Name, got.atts[len(got.atts)-1].Name, wantLimit+1)
	}
	// 省略必须落到文字上：模型看不到的图不能假装看过。
	blocks := hubPendingImageBlocks(got)
	var omittedText string
	images := 0
	for _, b := range blocks {
		if b.Type == core.ContentTypeImage {
			images++
		}
		if b.Type == core.ContentTypeText && strings.Contains(b.Content, "另有 1 张") {
			omittedText = b.Content
		}
	}
	if images != wantLimit || omittedText == "" {
		t.Fatalf("blocks: images=%d omittedText=%q，want %d 张 + omitted 说明", images, omittedText, wantLimit)
	}
}

// 字节预算：单张就超预算的图不能硬塞进请求，超出的计入 omitted（数字必须落到文字上）。
func TestHubPendingImagesBytesBudget(t *testing.T) {
	gid := agenthub.GroupID("g_bytes")
	big := func(name string, n int) agenthub.Attachment {
		return agenthub.Attachment{
			Kind: agenthub.AttachmentKindImage, Name: name, MIME: "image/png",
			DataURL: "data:image/png;base64," + strings.Repeat("A", n),
		}
	}
	st := seedGroupLog(t, gid,
		// 单张就吃掉整个预算：无论从哪头取都不能进上下文。
		agenthub.Message{Text: "巨图", Attachments: []agenthub.Attachment{big("huge", hubWakePendingImageBytes+1)}},
		agenthub.Message{Text: "小图", Attachments: []agenthub.Attachment{big("small", 1024)}},
	)
	got, err := hubPendingImagesFromStore(context.Background(), st, gid, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.atts) != 1 || got.atts[0].Name != "small" || got.omitted != 1 {
		t.Fatalf("atts=%v omitted=%d, want [small]/1", got.atts, got.omitted)
	}
	blocks := hubPendingImageBlocks(got)
	last := blocks[len(blocks)-1]
	if last.Type != core.ContentTypeText || !strings.Contains(last.Content, "另有 1 张") {
		t.Fatalf("末块 = %+v, want omitted 文字说明", last)
	}
}

// 采集口径：顺序（同一条消息多图稳定、整体时间正序）、seq 去重、过滤（纯工具轮 /
// owner-only / 非图片附件都不进补发窗口）。
func TestHubPendingImagesOrderFilterAndDedup(t *testing.T) {
	gid := agenthub.GroupID("g_order")
	img := func(name string) agenthub.Attachment {
		return agenthub.Attachment{
			Kind: agenthub.AttachmentKindImage, Name: name, MIME: "image/png",
			DataURL: testImageDataURL(name),
		}
	}
	file := agenthub.Attachment{Kind: agenthub.AttachmentKindFile, Name: "a.txt", Path: "/tmp/a.txt"}
	st := seedGroupLog(t, gid,
		agenthub.Message{Text: "游标之前", Attachments: []agenthub.Attachment{img("beforeCursor")}},
		agenthub.Message{Text: "更早的一张", Attachments: []agenthub.Attachment{img("img0")}},
		agenthub.Message{Text: "owner-only", Visibility: agenthub.VisibilityOwner,
			Attachments: []agenthub.Attachment{img("ownerOnly")}},
		agenthub.Message{Kind: agenthub.KindRouting, Text: "回执",
			Attachments: []agenthub.Attachment{img("routing")}},
		agenthub.Message{Text: "纯文件", Attachments: []agenthub.Attachment{file}},
		// 触发消息之前的最后一条：一条消息两张图（顺序必须保持 imgA → imgB）。
		agenthub.Message{Text: "两张", Attachments: []agenthub.Attachment{img("imgA"), img("imgB")}},
		agenthub.Message{Text: "@B 看图", Attachments: []agenthub.Attachment{img("trigger")}},
	)
	got, err := hubPendingImagesFromStore(context.Background(), st, gid, nil, 1, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got.omitted != 0 {
		t.Fatalf("omitted = %d, want 0（只有 3 张在窗口内）", got.omitted)
	}
	want := []string{"img0", "imgA", "imgB"}
	if len(got.atts) != len(want) {
		t.Fatalf("atts = %+v, want %v", got.atts, want)
	}
	for i, name := range want {
		if got.atts[i].Name != name {
			t.Fatalf("顺序 = %v, want %v", got.atts, want)
		}
	}
	if len(got.seqs) != 2 || got.seqs[0] != 2 || got.seqs[1] != 6 {
		t.Fatalf("seqs = %v, want [2 6]（同一消息多图只记一次 seq）", got.seqs)
	}
}

// 分页窗口：游标之后超过一页（500 条）的老图片必须能补上 —— 旧实现只看最近
// 500 条，seq 1 的图永远看不见（场景 5-3 的负对照）。
func TestHubPendingImagesPagePastRecentWindow(t *testing.T) {
	ctx := context.Background()
	gid := agenthub.GroupID("g_page")
	oldImage := testImageDataURL("old-backlog-image")
	msgs := []agenthub.Message{{
		Text: hubImageOnlyText, Attachments: []agenthub.Attachment{{
			Kind: agenthub.AttachmentKindImage, Name: "old.png", MIME: "image/png", DataURL: oldImage,
		}},
	}}
	// 再堆 600 条纯文本：老图被挤出「最近 500 条」窗口。
	for i := 0; i < 600; i++ {
		msgs = append(msgs, agenthub.Message{Text: fmt.Sprintf("闲聊 %d", i)})
	}
	msgs = append(msgs, agenthub.Message{Text: "@PageB 看上面那张图"})
	st := seedGroupLog(t, gid, msgs...)
	triggerSeq := int64(len(msgs)) // store 按 append 顺序分配 seq

	// 负对照：最近 500 条里没有这张图（旧实现的输入就是这个窗口）。
	head, err := st.ReadMessages(ctx, gid, 0, hubWakeBacklogPage)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) != hubWakeBacklogPage || head[0].Seq <= 1 {
		t.Fatalf("最近一页 = %d 条、最旧 seq %d，want 500 条且不含 seq 1", len(head), head[0].Seq)
	}
	for _, m := range head {
		if len(m.Attachments) > 0 {
			t.Fatalf("最近一页不该有图（负对照失效）：seq %d", m.Seq)
		}
	}

	got, err := hubPendingImagesFromStore(ctx, st, gid, nil, 0, triggerSeq)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.atts) != 1 || got.atts[0].DataURL != oldImage {
		t.Fatalf("分页补发 = %+v, want seq 1 的老图（游标之后的所有消息都要看）", got.atts)
	}
	if len(got.seqs) != 1 || got.seqs[0] != 1 {
		t.Fatalf("seqs = %v, want [1]", got.seqs)
	}
	if got.truncated || got.omitted != 0 {
		t.Fatalf("truncated=%v omitted=%d, want false/0", got.truncated, got.omitted)
	}
	if got.scanned != 602 { // 601 条历史 + 触发消息本身（它也要扫一眼，只是不收图）
		t.Fatalf("scanned = %d, want 602（游标 0 → 触发消息前的 601 条 + 触发消息）", got.scanned)
	}
}

// 游标是最强的停止条件：更早的消息都已读过，不该再往前翻（否则每轮 wake 都要
// 把整个群日志读一遍）。
func TestHubPendingImagesCursorStopsPaging(t *testing.T) {
	gid := agenthub.GroupID("g_cursor")
	msgs := make([]agenthub.Message, 0, 600)
	for i := 0; i < 600; i++ {
		msgs = append(msgs, agenthub.Message{Text: fmt.Sprintf("消息 %d", i)})
	}
	st := seedGroupLog(t, gid, msgs...)
	got, err := hubPendingImagesFromStore(context.Background(), st, gid, nil, 300, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.scanned != 300 {
		t.Fatalf("scanned = %d, want 300（seq 301..600）", got.scanned)
	}
	if len(got.atts) != 0 {
		t.Fatalf("atts = %+v, want none", got.atts)
	}
}

// 扫描预算：超过上限的积压不静默丢 —— 停在上限并标记 truncated，图片数量未知时
// 用文字说明「还有更早的消息没读」。
func TestHubPendingImagesScanBudgetTruncates(t *testing.T) {
	gid := agenthub.GroupID("g_budget")
	oldImage := testImageDataURL("unreachable-image")
	msgs := []agenthub.Message{{
		Text: hubImageOnlyText, Attachments: []agenthub.Attachment{{
			Kind: agenthub.AttachmentKindImage, Name: "old.png", MIME: "image/png", DataURL: oldImage,
		}},
	}}
	for i := 0; i < hubWakeBacklogMessages; i++ { // +1 条老图 → 老图刚好在预算之外
		msgs = append(msgs, agenthub.Message{Text: fmt.Sprintf("灌水 %d", i)})
	}
	st := seedGroupLog(t, gid, msgs...)
	got, err := hubPendingImagesFromStore(context.Background(), st, gid, nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.truncated {
		t.Fatal("超过扫描上限必须标记 truncated（不得静默丢弃）")
	}
	if got.scanned != hubWakeBacklogMessages {
		t.Fatalf("scanned = %d, want %d", got.scanned, hubWakeBacklogMessages)
	}
	if len(got.atts) != 0 {
		t.Fatalf("atts = %+v, want none（seq 1 在预算之外）", got.atts)
	}
	blocks := hubPendingImageBlocks(got)
	if len(blocks) != 1 || !strings.Contains(blocks[0].Content, "扫描上限") {
		t.Fatalf("blocks = %+v, want 一条扫描上限说明", blocks)
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
