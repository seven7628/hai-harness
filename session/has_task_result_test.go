package session

import (
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// P2-05 复审 B-1：checkpoint-first 重启后结果可能只在 pending inbox /
// runningSnapshot / messages 里；HasTaskResult 必须四路都认，且不排空 inbox。
func TestHasTaskResultCoversAllCheckpointChannels(t *testing.T) {
	msg := core.NewTaskResultMessageWithStatus("task_has", "t", "done", "completed", nil)
	other := core.NewTaskResultMessageWithStatus("task_has_2", "t", "done", "completed", nil)

	s := &Session{inbox: make(chan core.Message, 4)}
	s.inbox <- msg
	if !s.HasTaskResult("task_has") {
		t.Fatal("pending inbox 里的 task_result 未被识别")
	}
	if s.HasTaskResult("task_has_2") {
		t.Fatal("相似 task id 误命中")
	}
	select {
	case got := <-s.inbox:
		if len(got.Content) == 0 || got.Content[0].Content != msg.Content[0].Content {
			t.Fatal("HasTaskResult 排空了 inbox 或改变了内容")
		}
	default:
		t.Fatal("HasTaskResult 排空了 inbox")
	}

	s2 := &Session{}
	s2.messages = []core.Message{other}
	if !s2.HasTaskResult("task_has_2") {
		t.Fatal("messages 里的 task_result 未被识别")
	}
	s2.runningSnapshot = []core.Message{msg}
	if !s2.HasTaskResult("task_has") {
		t.Fatal("runningSnapshot 里的 task_result 未被识别")
	}
	s2.history = []core.Message{other}
	if !s2.HasTaskResult("task_has_2") {
		t.Fatal("history 里的 task_result 未被识别")
	}
}
