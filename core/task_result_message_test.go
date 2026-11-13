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

// P3b-05 复审 B-1：canceled/declined（Hub 智能体任务）没有执行，文案不能伪装成
// 「完成」让派发者据此继续；原因与「改派/汇报」指引必须给到模型。
func TestTaskResultMessageCanceledDeclined(t *testing.T) {
	canceled := NewTaskResultMessageWithStatus("task_canceled", "调研", "目标不可派发", "canceled", nil).Content[0].Content
	for _, want := range []string{"已取消", "目标不可派发", "没有执行"} {
		if !strings.Contains(canceled, want) {
			t.Fatalf("canceled message missing %q: %s", want, canceled)
		}
	}
	if strings.Contains(canceled, "完成〕") || strings.Contains(canceled, "据此继续当前工作") {
		t.Fatalf("canceled 不能伪装成完成: %s", canceled)
	}
	declined := NewTaskResultMessageWithStatus("task_declined", "调研", "这块我不熟", "declined", nil).Content[0].Content
	for _, want := range []string{"已被拒绝", "这块我不熟", "没有执行"} {
		if !strings.Contains(declined, want) {
			t.Fatalf("declined message missing %q: %s", want, declined)
		}
	}
	if strings.Contains(declined, "完成〕") || strings.Contains(declined, "据此继续当前工作") {
		t.Fatalf("declined 不能伪装成完成: %s", declined)
	}
}
