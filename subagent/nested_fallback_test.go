package subagent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// TestGrandchildResultFallsBackToMainWhenParentFinished 钉住一条**产品决定**（2026-11-10）：
// 子智能体派的孙任务，如果父先收尾了，孙任务结果**回落主会话**（并带「来自 task-x」标注），
// 不静默丢失。
//
// 为什么值得有测试：`deliverToParent` 的 false 分支同时覆盖四种情形（父已终态 / 未知 /
// 不再接收 / inbox 满），它们和「投进父 inbox」共用一条代码路径 —— 改投递策略时很容易
// 顺手把回落也删掉，而症状是「结果彻底消失」，用户只能靠 TaskOutput 手动捞。
func TestGrandchildResultFallsBackToMainWhenParentFinished(t *testing.T) {
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	defer reg.Close(context.Background())

	var mu sync.Mutex
	var pushed []string
	reg.SetOnTaskDone(func(_ string, name, result string, _ TaskStatus, _ core.Usage, _ error) {
		mu.Lock()
		pushed = append(pushed, name+"="+result)
		mu.Unlock()
	})

	// 父任务：立即收尾（终态 = 不再接收投递，与真实的 closeDelivery 同向）
	parent, err := reg.Start(context.Background(), "child agent", func(context.Context, *Task) (string, core.Usage, error) {
		return "父已收尾", core.Usage{}, nil
	})
	if err != nil {
		t.Fatalf("父任务启动: %v", err)
	}
	if _, err := reg.Wait(parent.ID); err != nil {
		t.Fatalf("父任务: %v", err)
	}

	// 孙任务：ParentTaskID 指向一个已收尾的父 → deliverToParent 必然返回 false
	child, err := reg.StartToolWithOptions(context.Background(), StartOptions{
		ToolName:     "agent_spawn",
		Name:         "grandchild",
		ParentTaskID: parent.ID,
	}, func(context.Context, *Task) (string, core.Usage, error) {
		return "孙任务结论", core.Usage{}, nil
	})
	if err != nil {
		t.Fatalf("孙任务启动: %v", err)
	}

	// 不能对孙任务用 Wait：在途 Wait 会把终止态标记为「已被 Wait 消费」，
	// 收尾时**跳过** OnTaskDone —— 那样这条回落路径就测不到了。轮询终态即可。
	deadline := time.Now().Add(3 * time.Second)
	for {
		child.mu.Lock()
		status := child.Status
		child.mu.Unlock()
		if status != TaskRunning && status != TaskInterrupting {
			if status != TaskCompleted {
				t.Fatalf("孙任务终态 = %s，期望 completed", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("孙任务未在 3s 内收尾")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 收尾回调在 runAndFinalize 里同步触发，但给调度留一拍余量
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	got := append([]string(nil), pushed...)
	mu.Unlock()
	want := "grandchild=孙任务结论"
	found := false
	for _, p := range got {
		if p == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("父已收尾时孙任务结果必须回落主会话（OnTaskDone），实际推送: %v", got)
	}
}
