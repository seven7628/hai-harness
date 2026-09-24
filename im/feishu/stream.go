package feishu

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/im"
)

// feishuStreamer 实现 im.Streamer：用「占位卡片 + 更新」模拟流式输出。
//
// 原理：发一条初始卡片（"…"）→ 内容累积时定期 Patch 更新卡片 → 结束收尾。
// 飞书消息 API 支持 Patch 卡片消息（im/v1/message/patch），无需迁移 Channel 层。
// 节流：内容追加不立即发，500ms 内合并（减少 API 调用，飞书限流友好）。
type feishuStreamer struct {
	sdk *sdkClient
}

// newFeishuStreamer 构造流式发送器。
func newFeishuStreamer(sdk *sdkClient) *feishuStreamer {
	return &feishuStreamer{sdk: sdk}
}

// Start 实现 im.Streamer：发占位卡片 → 返回控制器。
func (fs *feishuStreamer) Start(ctx context.Context, chat im.Chat, title string) (im.StreamController, error) {
	mid, err := fs.sdk.sendInteractive(ctx, chat.ChatID, msgToStreamCard("…", title))
	if err != nil {
		return nil, err
	}
	return &feishuStream{
		sdk:       fs.sdk,
		messageID: mid,
		title:     title,
		mu:        sync.Mutex{},
		done:      false,
	}, nil
}

// feishuStream im.StreamController 实现。
type feishuStream struct {
	sdk       *sdkClient
	messageID string
	title     string

	mu        sync.Mutex
	content   string
	lastFlush time.Time
	done      bool
}

// Append 实现 im.StreamController：累积内容 + 节流刷新（500ms）。
func (fs *feishuStream) Append(ctx context.Context, text string) error {
	fs.mu.Lock()
	fs.content += text
	shouldFlush := time.Since(fs.lastFlush) > 500*time.Millisecond
	fs.mu.Unlock()
	if !shouldFlush {
		return nil
	}
	return fs.Flush(ctx)
}

// Flush 实现 im.StreamController：立即把累积内容刷到卡片。
func (fs *feishuStream) Flush(ctx context.Context) error {
	fs.mu.Lock()
	if fs.done {
		fs.mu.Unlock()
		return nil
	}
	content := fs.content
	fs.mu.Unlock()
	if content == "" {
		return nil
	}
	err := fs.sdk.updateCard(ctx, fs.messageID, msgToStreamCard(content, fs.title))
	if err == nil {
		fs.mu.Lock()
		fs.lastFlush = time.Now()
		fs.mu.Unlock()
	}
	return err
}

// Close 实现 im.StreamController：最终刷新 + 标记完成。
func (fs *feishuStream) Close(ctx context.Context) error {
	fs.mu.Lock()
	if fs.done {
		fs.mu.Unlock()
		return nil
	}
	fs.done = true
	content := fs.content
	fs.mu.Unlock()
	if strings.TrimSpace(content) == "" {
		return nil
	}
	return fs.sdk.updateCard(ctx, fs.messageID, msgToStreamCard(content, fs.title))
}

// msgToStreamCard 流式占位/更新卡片（V2 schema，markdown 元素）。
func msgToStreamCard(content, title string) map[string]any {
	if content == "" {
		content = "…"
	}
	card := map[string]any{
		"schema": "2.0",
		"config": map[string]any{"wide_screen_mode": true},
		"body": map[string]any{
			"elements": []any{
				map[string]any{"tag": "markdown", "content": content},
			},
		},
	}
	if title != "" {
		card["header"] = map[string]any{
			"title": map[string]any{"tag": "plain_text", "content": title},
		}
	}
	return card
}

// Streamer 实现 im.Gateway.Streamer。
func (g *Gateway) Streamer() im.Streamer {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sdk == nil {
		return nil
	}
	return newFeishuStreamer(g.sdk)
}

var _ im.Streamer = (*feishuStreamer)(nil)
var _ im.StreamController = (*feishuStream)(nil)
