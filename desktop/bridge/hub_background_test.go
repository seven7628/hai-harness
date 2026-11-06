package main

// hub_background_test.go：P2.5-06 后台任务生命周期。
//
// 关键：必须能证伪「stop 只 cancel 不 Wait」。后台任务在 ctx.Done 后故意
// 多待 150ms，stop 返回时必须已经等过这段时间；仅 cancel 的实现会立刻返回。

import (
	"context"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

func TestHubBackgroundStopCancelsAndWaits(t *testing.T) {
	m, _ := testManager(t)
	started := make(chan struct{})
	exited := make(chan struct{})
	m.spawnHubBackground(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		time.Sleep(150 * time.Millisecond) // 模拟收尾 IO：不 Wait 就会在它之前返回
		close(exited)
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background task did not start")
	}
	begin := time.Now()
	if !m.stopHubBackground(2 * time.Second) {
		t.Fatal("stopHubBackground timed out")
	}
	if elapsed := time.Since(begin); elapsed < 120*time.Millisecond {
		t.Fatalf("stop returned in %s; it did not wait for the task to exit", elapsed)
	}
	select {
	case <-exited:
	default:
		t.Fatal("stop returned before background task exited")
	}
}

func TestHubBackgroundStopTimeoutAndRecovery(t *testing.T) {
	m, _ := testManager(t)
	release := make(chan struct{})
	m.spawnHubBackground(func(context.Context) { <-release })
	if m.stopHubBackground(30 * time.Millisecond) {
		t.Fatal("stop must time out while task ignores cancellation")
	}
	close(release)
	if !m.stopHubBackground(time.Second) {
		t.Fatal("stop after release must succeed")
	}
}

func TestHubBackgroundSpawnAfterStopIsNoop(t *testing.T) {
	m, _ := testManager(t)
	if !m.stopHubBackground(time.Second) {
		t.Fatal("first stop timed out")
	}
	ran := make(chan struct{}, 1)
	m.spawnHubBackground(func(context.Context) { ran <- struct{}{} })
	select {
	case <-ran:
		t.Fatal("spawn after stop must be a no-op")
	case <-time.After(100 * time.Millisecond):
	}
}

// P2.5-06 复审 N2：一次 shutdown 后新建 hub runtime，后台任务必须能重新启动。
func TestHubBackgroundReopensAfterRuntimeRecreate(t *testing.T) {
	m, _ := testManager(t)
	if !m.stopHubBackground(time.Second) {
		t.Fatal("stop timed out")
	}
	m.resetHubBackgroundIfClosed()
	ran := make(chan struct{}, 1)
	m.spawnHubBackground(func(context.Context) { ran <- struct{}{} })
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("spawn after runtime recreate must run again")
	}
	if !m.stopHubBackground(time.Second) {
		t.Fatal("second stop timed out")
	}
}

// P2.5-06 复审 B2：stop 超时后 reset 再 spawn，不能触发
// "sync: WaitGroup is reused before previous Wait has returned"。
// 旧实现共享 m.hubBgWG，旧 waiter 还在 Wait 时新 generation 就 Add。
func TestHubBackgroundResetAfterTimeoutIsGenerationLocal(t *testing.T) {
	m, _ := testManager(t)
	release := make(chan struct{})
	m.spawnHubBackground(func(context.Context) { <-release }) // 故意忽略 cancel
	if m.stopHubBackground(20 * time.Millisecond) {
		t.Fatal("stop must time out while task ignores cancellation")
	}
	// 先释放旧任务，让旧 waiter 进入“Wait 即将返回”的窗口，再 reset+spawn；
	// 这样共享 WG 的旧实现才会触发 reuse panic（P2.5-06 N1）。
	close(release)
	m.resetHubBackgroundIfClosed()
	ran := make(chan struct{}, 1)
	m.spawnHubBackground(func(context.Context) { ran <- struct{}{} })
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("new generation did not run")
	}
	if !m.stopHubBackground(time.Second) {
		t.Fatal("stop after release timed out")
	}
}

// P2.5-06 复审 B1：彻底关停后禁止重建 runtime/DM 会话/后台任务，
// 否则迟到结算会把已关闭的运行时复活并写 TempDir。
func TestHubShutdownBlocksLateRuntimeRebuild(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_shutdown")
	if ws == nil {
		t.Fatal("hub runtime not created")
	}
	m.shutdownHubRuntimes()
	if again := m.hubRuntime("ag_shutdown"); again != nil {
		t.Fatal("hubRuntime must refuse rebuild after shutdown")
	}
	if _, err := m.hubDMSession("ag_shutdown"); err == nil {
		t.Fatal("hubDMSession must fail after shutdown")
	}
	ran := make(chan struct{}, 1)
	m.spawnHubBackground(func(context.Context) { ran <- struct{}{} })
	select {
	case <-ran:
		t.Fatal("spawnHubBackground must be no-op after shutdown")
	case <-time.After(100 * time.Millisecond):
	}
}

// P2.5-06 N5：关停后恢复扫描必须 no-op（不重建 runtime/不写账本）。
func TestHubRecoveryNoopAfterShutdown(t *testing.T) {
	m, _ := testManager(t)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_off"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:  "关停后恢复", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.shutdownHubRuntimes()
	if err := m.recoverHubTasks(); err != nil {
		t.Fatalf("recover after shutdown = %v", err)
	}
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil || got == nil || got.Status != agenthub.TaskQueued {
		t.Fatalf("shutdown recovery wrote task: %+v, %v", got, err)
	}
	if ws := m.hubRuntime("ag_off"); ws != nil {
		t.Fatal("shutdown recovery rebuilt hub runtime")
	}
}
