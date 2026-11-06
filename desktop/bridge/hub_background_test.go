package main

// hub_background_test.go：P2.5-06 后台任务生命周期。
//
// 关键：必须能证伪「stop 只 cancel 不 Wait」。后台任务在 ctx.Done 后故意
// 多待 150ms，stop 返回时必须已经等过这段时间；仅 cancel 的实现会立刻返回。

import (
	"context"
	"testing"
	"time"
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
