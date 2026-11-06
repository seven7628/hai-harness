package core

import (
	"strings"
	"testing"
)

// P2-07 复审 N1：预算阻断不能伪装成「完成」并指示模型继续——否则模型会
// 自动续跑/重试，而任务实际上已被熔断拦下。
func TestTaskResultMessageBlockedBudget(t *testing.T) {
	msg := NewTaskResultMessageWithStatus("task_budget", "", "已完成一半的表格", "blocked_budget", nil)
	text := msg.Content[0].Content
	for _, want := range []string{"预算阻断", "已完成一半的表格", "不要自动重试"} {
		if !strings.Contains(text, want) {
			t.Fatalf("blocked_budget message missing %q: %s", want, text)
		}
	}
	if strings.Contains(text, "后台任务结果，据此继续当前工作") {
		t.Fatalf("blocked_budget must not tell the model to continue: %s", text)
	}
	// 其他终态文案不变。
	done := NewTaskResultMessageWithStatus("task_done", "", "完成", "completed", nil).Content[0].Content
	if !strings.Contains(done, "完成〕") || !strings.Contains(done, "据此继续当前工作") {
		t.Fatalf("completed message changed: %s", done)
	}
}
