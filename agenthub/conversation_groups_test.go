package agenthub

// P7-04：Groups() —— bridge 的「设置保存后立即刷新状态条」靠它逐群重播视图。

import (
	"testing"
	"time"
)

// TestConversationControllerGroups：只报**已有对话状态**的群，升序、去重、返回副本；
// 读一次不建状态（否则每次保存设置都会给没有对话的群长出一段空对话）。
func TestConversationControllerGroups(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{})
	if got := c.Groups(); len(got) != 0 {
		t.Fatalf("空控制器 Groups() = %v, want 空", got)
	}
	var nilCtrl *ConversationController
	if got := nilCtrl.Groups(); got != nil {
		t.Fatalf("nil 控制器 Groups() = %v, want nil", got)
	}

	// 故意乱序建，验证输出稳定（UI/测试逐条比对不能靠 map 迭代顺序）。
	for _, gid := range []GroupID{"group_c", "group_a", "group_b"} {
		c.BeginHuman(gid, 1, time.Time{})
	}
	want := []GroupID{"group_a", "group_b", "group_c"}
	got := c.Groups()
	if len(got) != len(want) {
		t.Fatalf("Groups() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Groups() = %v, want %v（升序）", got, want)
		}
	}

	// 返回副本：调用方改它不该污染控制器。
	got[0] = "group_mutated"
	if again := c.Groups(); len(again) != len(want) || again[0] != want[0] {
		t.Fatalf("Groups() 返回了内部状态: %v", again)
	}
	// 没有对话的群不因为「查了一次」而出现。
	if _, ok := c.convs["group_never"]; ok {
		t.Fatal("Groups() 不该给未知群建状态")
	}
}
