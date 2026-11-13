package subagent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// waitDone 阻塞到任务收尾（done 通道由 Registry 在 goroutine 退出前关闭），
// 超时视为失败——测试收尾要等后台 goroutine 真正结束，否则 t.Cleanup 删除临时目录
// 会与仍在写的 journal / 落盘竞态。
// 注：本文件、task_output_test.go、task_output_wait_test.go 共用此 helper；
// 原先定义处缺失导致 `subagent` 包测试整体编译失败，此处补齐。
func waitDone(t *testing.T, task *Task) {
	t.Helper()
	select {
	case <-task.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("task %s did not finish in time", task.ID)
	}
}

// ---------- Phase 2：outputFile 线程化 + Snapshot 契约（docs §6 P2） ----------
//
// 设计修正（v3.1）：taskId 由 Registry 在 start() 内生成，宿主无法先于 Start 知道 id，
// 故 StartOptions 传「路径解析器」而非路径字符串 —— start() 在 Task 构造期调用解析器
// （拿到生成 id），产物仍满足「发布前就绪、读侧免锁」。

// TestStartToolWithOptionsCarriesOutputFile 解析器在构造期被调用且收到真实生成 id；
// Start 返回时（goroutine 可能尚未跑）OutputFile 已可见。
func TestStartToolWithOptionsCarriesOutputFile(t *testing.T) {
	r := NewRegistry()
	release := make(chan struct{})
	gotID := make(chan string, 1)
	task, err := r.StartToolWithOptions(context.Background(), StartOptions{
		ToolName: "agent_spawn",
		Name:     "code review",
		JournalPath: func(taskID string) (string, error) {
			gotID <- taskID
			return "/data/journal/" + taskID + ".jsonl", nil
		},
	}, func(ctx context.Context, tk *Task) (string, core.Usage, error) {
		<-release
		return "ok", core.Usage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-gotID:
		if id != task.ID {
			t.Fatalf("resolver got id %q, want generated %q", id, task.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resolver not called during construction")
	}
	if want := "/data/journal/" + task.ID + ".jsonl"; task.OutputFile() != want {
		t.Fatalf("OutputFile = %q, want %q", task.OutputFile(), want)
	}
	if task.ToolName != "agent_spawn" || task.Name != "code review" {
		t.Fatalf("meta lost: %+v", task)
	}
	close(release)
	waitDone(t, task)
}

// TestStartToolWithOptionsResolverError 解析器报错 → 降级 outputFile=""，任务照常启动。
func TestStartToolWithOptionsResolverError(t *testing.T) {
	r := NewRegistry()
	task, err := r.StartToolWithOptions(context.Background(), StartOptions{
		Name: "degraded",
		JournalPath: func(string) (string, error) {
			return "", errors.New("mkdir failed")
		},
	}, func(context.Context, *Task) (string, core.Usage, error) {
		return "r", core.Usage{}, nil
	})
	if err != nil {
		t.Fatalf("resolver error must not fail start: %v", err)
	}
	waitDone(t, task)
	if task.OutputFile() != "" {
		t.Fatalf("OutputFile = %q, want empty on resolver error", task.OutputFile())
	}
}

// TestStartToolWithOptionsDefaults 未传 JournalPath → 空串（未启用落盘），其余默认行为不变。
func TestStartToolWithOptionsDefaults(t *testing.T) {
	r := NewRegistry()
	task, err := r.StartToolWithOptions(context.Background(), StartOptions{
		Name: "plain",
	}, func(context.Context, *Task) (string, core.Usage, error) {
		return "r", core.Usage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, task)
	if task.OutputFile() != "" {
		t.Fatalf("default OutputFile = %q, want empty", task.OutputFile())
	}
}

// TestRegistryListExposesOutputFile List 的 TaskInfo 透出 output_file。
func TestRegistryListExposesOutputFile(t *testing.T) {
	r := NewRegistry()
	task, err := r.StartToolWithOptions(context.Background(), StartOptions{
		Name:        "n",
		JournalPath: func(taskID string) (string, error) { return "/j/" + taskID + ".jsonl", nil },
	}, func(context.Context, *Task) (string, core.Usage, error) {
		return "r", core.Usage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, task)

	list := r.List()
	if len(list) != 1 {
		t.Fatalf("List len = %d", len(list))
	}
	if list[0].ID != task.ID || list[0].OutputFile != "/j/"+task.ID+".jsonl" || list[0].Status != TaskCompleted {
		t.Fatalf("TaskInfo = %+v", list[0])
	}
}

// TestRegistrySnapshotAllStatuses Snapshot 对全部状态可得（含 abandoned）；
// 未知 id → ErrTaskNotFound。
func TestRegistrySnapshotAllStatuses(t *testing.T) {
	r := NewRegistry()

	// running（阻塞在途）
	release := make(chan struct{})
	running, err := r.Start(context.Background(), "blk", func(context.Context, *Task) (string, core.Usage, error) {
		<-release
		return "", core.Usage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := r.Snapshot(running.ID)
	if err != nil {
		t.Fatalf("Snapshot(running): %v", err)
	}
	if info.Status != TaskRunning {
		t.Fatalf("running status = %s", info.Status)
	}

	// completed
	done, err := r.Start(context.Background(), "ok", func(context.Context, *Task) (string, core.Usage, error) {
		return "res", core.Usage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, done)
	if info, err = r.Snapshot(done.ID); err != nil || info.Status != TaskCompleted {
		t.Fatalf("completed: info=%+v err=%v", info, err)
	}

	// failed
	fail, err := r.Start(context.Background(), "bad", func(context.Context, *Task) (string, core.Usage, error) {
		return "", core.Usage{}, errors.New("boom")
	})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, fail)
	if info, err = r.Snapshot(fail.ID); err != nil || info.Status != TaskFailed || info.Error != "boom" {
		t.Fatalf("failed: info=%+v err=%v", info, err)
	}

	// abandoned：AbandonAll 标记在途任务；get/Snapshot 仍可取到（E 决策）
	r.AbandonAll("test")
	if info, err = r.Snapshot(running.ID); err != nil || info.Status != TaskAbandoned {
		t.Fatalf("abandoned: info=%+v err=%v", info, err)
	}

	// unknown
	if _, err := r.Snapshot("task-999"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown id err = %v, want ErrTaskNotFound", err)
	}

	close(release) // 清理：放行被弃置任务收尾
	select {
	case <-running.done:
	case <-time.After(2 * time.Second):
		t.Fatal("abandoned task should finish after release")
	}
}
