package main

// hub_background_test.go：P2.5-06 后台任务生命周期。

import (
	"context"
	"testing"
	"time"
)

func TestHubBackgroundStopCancelsAndWaits(t *testing.T) {
	m, _ := testManager(t)
	started := make(chan struct{})
	m.spawnHubBackground(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
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
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("stop took %s, want prompt cancellation", elapsed)
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

func TestScheduleHubPumpIsTracked(t *testing.T) {
	m, _ := testManager(t)
	// 没有队列：泵应立即返回；关键是它被 WaitGroup 记账，stop 不等满超时。
	m.scheduleHubPump("/tmp/not-a-workspace", "s-none")
	if !m.stopHubBackground(2 * time.Second) {
		t.Fatal("scheduled pump did not finish")
	}
}
