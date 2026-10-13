package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/subagent"
)

// —— 主「停止」不级联后台任务：无条件规则回归 ——
//
// 规则：主「停止」（Composer 停止按钮 → interrupt 命令）只 abort 当前主 run。
// 后台子 agent / promoted 后台工具任务设计上脱离主 run 存活，**不受主停止影响**；
// 停单个任务只走 interrupt_task / agent_interrupt（按 taskId）。
//
// 回归锚点：这条规则曾以 `runtime.stop_background_on_interrupt` 开关存在，且 Electron
// 把「缺省」当持久值写进了真机 settings.json（文件里的 true 覆盖了默认语义，用户一按
// 停止后台任务全灭）。开关已删除：本测试把那个键写进 settings.json（复现真机形态），
// 断言它**不产生任何效果** —— 键存在但失效，绝不级联。
func TestInterruptDoesNotCascadeToBackgroundTask(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t) // 内部已把 HOME 隔离到临时目录
	// 显式再取一个隔离 HOME 并落配置：settings.json 只写在这里，绝不碰真实 ~/.go-code。
	// 注意顺序 —— 必须在 testManager 之后写（它会重设 HOME，先写会被丢弃）。
	t.Setenv("HOME", t.TempDir())
	writeCfg(t, `{"runtime":{"stop_background_on_interrupt":true}}`)

	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	// 后台子 agent（task-*）阻塞在 release 上 → 稳定处于 running
	var releases []chan struct{}
	startTask := func() string {
		release := make(chan struct{})
		releases = append(releases, release)
		task, err := bs.s.Registry().Start(context.Background(), "review", func(_ context.Context, _ *subagent.Task) (string, core.Usage, error) {
			<-release
			return "done", core.Usage{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, ti := range bs.s.Registry().List() {
			if ti.ID == task.ID && ti.Status == subagent.TaskRunning {
				return task.ID
			}
		}
		t.Fatalf("task %s should be running", task.ID)
		return ""
	}
	// 收尾：先放行全部后台任务、等它们跑完（Wait 期间任务会写本测试临时目录），
	// 再停会话并等落盘稳定 —— 迟到写入会与 t.TempDir RemoveAll 竞态（directory not empty）。
	defer func() {
		for _, r := range releases {
			close(r)
		}
		for _, ti := range bs.s.Registry().List() {
			if ti.Status == subagent.TaskRunning || ti.Status == subagent.TaskInterrupting {
				_, _ = bs.s.Registry().Wait(ti.ID)
			}
		}
		_ = bs.s.Shutdown(context.Background())
		bs.cancel()
		waitStoreSettled(t, ws, bs.s.ID())
	}()

	// 主「停止」：settings.json 里写着 true（已删除的旧开关）也不得波及后台任务
	taskID := startTask()
	buf.Reset()
	m.dispatch(cmd(0, "interrupt", map[string]any{"workspace": ws, "session_id": sid}))
	if resp := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(0)
	}); resp == nil || resp["ok"] != true {
		t.Fatalf("interrupt: %+v", resp)
	}
	if got := taskStatus(bs, taskID); got != subagent.TaskRunning {
		t.Fatalf("stale stop_background_on_interrupt must be inert: background task got %s", got)
	}
	if ev := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "task_result_delivered" && str(mm, "task_id") == taskID
	}); ev != nil {
		t.Fatalf("main stop must not deliver a task result: %+v", ev)
	}

	// 唯一入口仍是 interrupt_task：按 taskId 精确中断（中断请求已下发 → interrupting）
	buf.Reset()
	m.dispatch(cmd(1, "interrupt_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": taskID}))
	if resp := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(1)
	}); resp == nil || resp["ok"] != true {
		t.Fatalf("interrupt_task: %+v", resp)
	}
	if got := taskStatus(bs, taskID); got != subagent.TaskInterrupting {
		t.Fatalf("interrupt_task must stop the exact task, got %s", got)
	}
}

// TestInterruptFallbackDoesNotCascade 兜底路径（session_id 失配 → interruptAllIn）同样
// 只停主会话：断掉「停止」的第二条潜在级联入口，后台任务不受影响。
func TestInterruptFallbackDoesNotCascade(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	release := make(chan struct{})
	defer func() {
		close(release)
		for _, ti := range bs.s.Registry().List() {
			if ti.Status == subagent.TaskRunning || ti.Status == subagent.TaskInterrupting {
				_, _ = bs.s.Registry().Wait(ti.ID)
			}
		}
		_ = bs.s.Shutdown(context.Background())
		bs.cancel()
		waitStoreSettled(t, ws, sid)
	}()
	task, err := bs.s.Registry().Start(context.Background(), "review", func(_ context.Context, _ *subagent.Task) (string, core.Usage, error) {
		<-release
		return "done", core.Usage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	buf.Reset()
	// 过期/错误的 session_id → 走 interruptAllIn 兜底（code=interrupt_fallback）
	m.dispatch(cmd(0, "interrupt", map[string]any{"workspace": ws, "session_id": "nope"}))
	got := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(0)
	})
	if got == nil || got["ok"] != true || got["code"] != "interrupt_fallback" {
		t.Fatalf("fallback interrupt: %+v", got)
	}
	if st := taskStatus(bs, task.ID); st != subagent.TaskRunning {
		t.Fatalf("fallback interrupt must not stop background tasks, got %s", st)
	}
}

// writeCfg 写测试用 settings.json：固定落 <HOME>/.go-code/settings.json。settings.json
// 读取链是 homeCfgPath（os.UserHomeDir），故 HOME 必须由调用方隔离到 t.TempDir()，且
// 本函数必须在 testManager（重设 HOME）之后调用 —— 真机用户配置永远不被触碰。
func writeCfg(t *testing.T, content string) {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), ".go-code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
