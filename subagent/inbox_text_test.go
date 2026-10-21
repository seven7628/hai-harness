package subagent

import (
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// inboxMessageText 决定「子会话转录里这句话长什么样」：投进 inbox 的 Message 可能带多个
// 内容块（文本 + @引用展开 + 图片），而转录需要的是可读正文，不是把 base64 铺满屏幕。
func TestInboxMessageText(t *testing.T) {
	cases := []struct {
		name string
		msg  core.Message
		want string
	}{
		{
			name: "单文本块",
			msg:  core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "把返回 401 的分支也改掉"}),
			want: "把返回 401 的分支也改掉",
		},
		{
			name: "文本 + @引用展开（多块）→ 拼接",
			msg: core.NewUserMessage(
				core.Content{Type: core.ContentTypeText, Content: "看看这个"},
				core.Content{Type: core.ContentTypeText, Content: "@./auth.go"},
			),
			want: "看看这个\n@./auth.go",
		},
		{
			name: "图片块 → 占位（不能把 data URL 打进转录）",
			msg: core.NewUserMessage(
				core.Content{Type: core.ContentTypeText, Content: "这张图"},
				core.Content{Type: "image", Content: "data:image/png;base64,AAAA"},
			),
			want: "这张图\n[图片]",
		},
		{
			name: "空文本块 → 忽略",
			msg:  core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "   "}),
			want: "",
		},
	}
	for _, c := range cases {
		if got := inboxMessageText(c.msg); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
