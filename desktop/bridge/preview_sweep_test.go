package main

// 预览缓存**运行期**周期清扫的回归网（2026-11-18「资源有界化」第 3 步）。
//
// 钉住：清扫**在运行期真的会跑**（改造前只在启动时扫一次 ⇒ 桌面端连开一天，
// 512MiB 的预算从不生效），且 stop 之后**不再跑**（不留后台协程）。

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestPreviewCacheSweeperRunsPeriodically(t *testing.T) {
	var calls atomic.Int64
	s := startPreviewCacheSweeperEvery(func() { calls.Add(1) }, 20*time.Millisecond)
	if s == nil {
		t.Fatal("清扫器没起来")
	}
	defer s.stop()

	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("周期清扫没跑（calls=%d）", calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPreviewCacheSweeperStops(t *testing.T) {
	var calls atomic.Int64
	s := startPreviewCacheSweeperEvery(func() { calls.Add(1) }, 20*time.Millisecond)
	time.Sleep(80 * time.Millisecond)
	s.stop()
	after := calls.Load()
	// stop 之后再等几轮：调用数不该再涨。
	time.Sleep(120 * time.Millisecond)
	if now := calls.Load(); now != after {
		t.Fatalf("stop 后仍在清扫（%d → %d）", after, now)
	}
	// 幂等：stop 调两次不炸。
	s.stop()
}

func TestPreviewCacheSweeperNilSafe(t *testing.T) {
	if s := startPreviewCacheSweeper(nil); s != nil {
		t.Fatalf("nil sweep 不该起清扫器")
	}
	var nilSweeper *previewCacheSweeper
	nilSweeper.stop() // nil 接收者：空操作
}
