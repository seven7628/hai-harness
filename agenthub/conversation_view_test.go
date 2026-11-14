package agenthub

// P7-04：ConversationView.Schedulable() 的完整判据表。
//
// 调度器「该不该为这个群取队」只有一个来源（这个函数），所以 5 个状态 × 队列 0/1
// 必须逐格钉住 —— 之前只有 running 一列被间接覆盖（B1 的回归用例），waiting_human
// 与终态两列全靠别处偶发断言。
//
// 口径（P6-15 / B1）：
//   - running / waiting_human **且**队列非空才可调度。waiting_human 只是「上一批目标
//     全被护栏跳过」的软状态；429 退避把 wake 退回队列时状态往往还停在 waiting_human，
//     这时也必须投得出去（否则队列永久泵不动）。
//   - stopped / exhausted 是终态：队列里就算还留着残 wake 也不投，人类消息会开新对话。
//   - idle 不是「在等投递」：没有人类消息就不该有调度动作。

import "testing"

func TestConversationViewSchedulable(t *testing.T) {
	cases := []struct {
		status ConversationStatus
		queue  int
		want   bool
	}{
		{ConversationIdle, 0, false},
		{ConversationIdle, 1, false},
		{ConversationRunning, 0, false},
		{ConversationRunning, 1, true},
		{ConversationWaitingHuman, 0, false},
		{ConversationWaitingHuman, 1, true},
		{ConversationStopped, 0, false},
		{ConversationStopped, 1, false},
		{ConversationExhausted, 0, false},
		{ConversationExhausted, 1, false},
	}
	for _, tc := range cases {
		v := ConversationView{Status: tc.status, QueueLen: tc.queue}
		if got := v.Schedulable(); got != tc.want {
			t.Errorf("Schedulable(status=%s, queue_len=%d) = %v, want %v",
				tc.status, tc.queue, got, tc.want)
		}
	}

	// 覆盖闸门：新增状态时这张表必须同步补两行（0/1）—— 否则新状态会悄悄默认
	// 「不可调度」，而调度器看起来一切正常。
	all := []ConversationStatus{
		ConversationIdle, ConversationRunning, ConversationWaitingHuman,
		ConversationStopped, ConversationExhausted,
	}
	for _, status := range all {
		for _, queue := range []int{0, 1} {
			found := false
			for _, tc := range cases {
				if tc.status == status && tc.queue == queue {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("状态 %s 队列 %d 没有覆盖（新增状态时请补表）", status, queue)
			}
		}
	}
}
