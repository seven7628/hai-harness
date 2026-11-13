package core

// P0-07：hub_task 内容块的 allowlist 回归。

import (
	"strings"
	"testing"
)

// hub_task header 块必须能通过 Validate（方案 §6.6.2：header 随 user 消息落盘并送
// provider），而未知块类型仍要被拒——allowlist 加一项不能顺手把它整段放开。
func TestMessageValidateHubTask(t *testing.T) {
	header := Content{Type: ContentTypeHubTask, Content: `{"task_id":"task_31"}`}

	msg := NewUserMessage(header, Content{Type: ContentTypeText, Content: "写一份竞品价格对比"})
	if err := msg.Validate(); err != nil {
		t.Fatalf("hub_task block should validate: %v", err)
	}
	headerOnly := NewUserMessage(header)
	if err := headerOnly.Validate(); err != nil {
		t.Fatalf("header-only hub_task message should validate: %v", err)
	}

	bad := NewUserMessage(Content{Type: "hub_tasks", Content: header.Content})
	err := bad.Validate()
	if err == nil {
		t.Fatal("unknown content type must be rejected")
	}
	if !strings.Contains(err.Error(), "hub_tasks") {
		t.Fatalf("error should name the offending type, got %v", err)
	}
}
