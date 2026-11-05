package session

// P0-07：ContentTypeHubTask 的 collect 语义 + PendingCount（Hub 空闲闸门的数据源）。

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// hub_task header 不算「用户输入」：纯 header 消息不触发 UserInputsConsumed；
// 但 header + text 的任务消息要上报，且上报的 contents 里保留 header 块
// （bridge 靠它把 run 绑定到 task_id，方案 §6.7.4）。
func TestCollectUserContentsHubTask(t *testing.T) {
	header := core.Content{Type: core.ContentTypeHubTask, Content: `{"task_id":"task_31"}`}
	batch := []core.Message{
		core.NewUserMessage(header),
		core.NewUserMessage(header, core.Content{Type: core.ContentTypeText, Content: "写一份竞品价格对比"}),
	}

	texts, contents := collectUserContents(batch)
	if len(texts) != 1 || texts[0] != "写一份竞品价格对比" {
		t.Fatalf("only the body text counts as user text, got %q", texts)
	}
	if len(contents) != 1 {
		t.Fatalf("header-only message must not be reported as user input, got %d entries", len(contents))
	}
	if len(contents[0]) != 2 || contents[0][0].Type != core.ContentTypeHubTask {
		t.Fatalf("reported contents must keep the hub_task header block, got %+v", contents[0])
	}
}

// PendingCount = inbox 积压 + 未搬进 inbox 的恢复积压；恢复完成后不得双算，
// 否则 Hub 的空闲闸门（§6.6.4）永远「忙」，任务永远派不出去。
func TestPendingCountInboxAndRecovery(t *testing.T) {
	header := core.Content{Type: core.ContentTypeHubTask, Content: `{"task_id":"task_31"}`}

	// ① 无 store：恢复积压搬不进 inbox，PendingCount 必须把它算上。
	state := &State{Pending: []core.Message{core.NewUserMessage(header), core.NewUserMessage(header)}}
	s, err := NewSession("s_pending_nostore", nil, nil, WithRecoveryState(state))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if got := s.PendingCount(); got != 2 {
		t.Fatalf("recovery pending not counted: got %d want 2", got)
	}

	// ② 有 store：积压已进 inbox，仍只能是 2（不是 4），且不篡改宿主持有的 State。
	s2, err := NewSession("s_pending_store", nil, stubStore{}, WithRecoveryState(state))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if got := s2.PendingCount(); got != 2 {
		t.Fatalf("recovery pending double counted: got %d want 2", got)
	}
	if len(state.Pending) != 2 {
		t.Fatalf("host-owned recovery State must not be mutated, got %d pending", len(state.Pending))
	}

	// ③ 新输入入队后计数随之增加（闸门看到非 0 就该排队而不是派发）。
	s2.inbox <- core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "在吗"})
	if got := s2.PendingCount(); got != 3 {
		t.Fatalf("inbox depth not counted: got %d want 3", got)
	}
}

// stubStore 只满足 Store 接口：Load 返回新会话，其余 no-op。
type stubStore struct{}

func (stubStore) Load(context.Context, string) (*SessionData, error)          { return &SessionData{}, nil }
func (stubStore) AppendHistory(context.Context, string, []core.Message) error { return nil }
func (stubStore) SaveState(context.Context, string, *State) error             { return nil }
