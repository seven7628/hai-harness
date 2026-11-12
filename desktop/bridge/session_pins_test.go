package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSessionPinsBasic pin 设置/取消/持久化（文件落盘 + 内存缓存）。
func TestSessionPinsBasic(t *testing.T) {
	home := t.TempDir()
	appDataDirOverride = home
	defer func() { appDataDirOverride = "" }()

	wsKey := "ws-test-1"
	if got := sessionPins.load(wsKey); len(got) != 0 {
		t.Fatalf("初始应为空: %v", got)
	}
	// pin 两个
	pins, ok := sessionPins.set(wsKey, "s1", true)
	if !ok || len(pins) != 1 || pins[0] != "s1" {
		t.Fatalf("pin s1: %v %v", pins, ok)
	}
	pins, ok = sessionPins.set(wsKey, "s2", true)
	if !ok || len(pins) != 2 || pins[0] != "s2" || pins[1] != "s1" {
		t.Fatalf("pin s2（最新在前）: %v", pins)
	}
	// 文件已落盘
	f := sessionPinsFile(wsKey)
	if _, err := os.Stat(f); err != nil {
		t.Fatalf("pin 文件应存在: %v", err)
	}
	// 内存缓存命中
	if !sessionPins.isPinned(wsKey, "s1") || !sessionPins.isPinned(wsKey, "s2") {
		t.Fatalf("isPinned 应命中")
	}
	// 取消 pin
	pins, ok = sessionPins.set(wsKey, "s1", false)
	if !ok || len(pins) != 1 || pins[0] != "s2" {
		t.Fatalf("unpin s1: %v", pins)
	}
	if sessionPins.isPinned(wsKey, "s1") {
		t.Fatalf("s1 应已取消置顶")
	}
}

// TestSessionPinsLimit 上限 5：第 6 个拒绝且不覆盖已有。
func TestSessionPinsLimit(t *testing.T) {
	home := t.TempDir()
	appDataDirOverride = home
	defer func() { appDataDirOverride = "" }()

	wsKey := "ws-test-limit"
	for i := 1; i <= 5; i++ {
		pins, ok := sessionPins.set(wsKey, "s"+string(rune('0'+i)), true)
		if !ok || len(pins) != i {
			t.Fatalf("pin %d: %v %v", i, pins, ok)
		}
	}
	// 第 6 个拒绝
	pins, ok := sessionPins.set(wsKey, "s6", true)
	if ok || len(pins) != 5 {
		t.Fatalf("第 6 个应拒绝: %v %v", pins, ok)
	}
	// 已有 pin 未受影响
	if !sessionPins.isPinned(wsKey, "s1") || sessionPins.isPinned(wsKey, "s6") {
		t.Fatalf("上限拒绝不得覆盖已有 pin")
	}
	// 重复 pin 已有项：幂等（移到最前）
	pins, ok = sessionPins.set(wsKey, "s3", true)
	if !ok || len(pins) != 5 || pins[0] != "s3" {
		t.Fatalf("重复 pin 应移到最前: %v", pins)
	}
}

// TestSessionPinsPersistence 跨实例持久化：新 store（清缓存）从磁盘读。
func TestSessionPinsPersistence(t *testing.T) {
	home := t.TempDir()
	appDataDirOverride = home
	defer func() { appDataDirOverride = "" }()

	wsKey := "ws-test-persist"
	sessionPins.set(wsKey, "s1", true)
	sessionPins.set(wsKey, "s2", true)

	// 模拟新进程：清内存缓存（读盘）
	sessionPins.mu.Lock()
	delete(sessionPins.cache, wsKey)
	sessionPins.mu.Unlock()

	pins := sessionPins.load(wsKey)
	if len(pins) != 2 || pins[0] != "s2" || pins[1] != "s1" {
		t.Fatalf("跨实例持久化失败: %v", pins)
	}
}

// TestSessionPinsCorruptFile 损坏文件 → 空列表（不 panic）。
func TestSessionPinsCorruptFile(t *testing.T) {
	home := t.TempDir()
	appDataDirOverride = home
	defer func() { appDataDirOverride = "" }()

	wsKey := "ws-test-corrupt"
	f := sessionPinsFile(wsKey)
	_ = os.MkdirAll(filepath.Dir(f), 0o755)
	_ = os.WriteFile(f, []byte("{not-json"), 0o644)

	if pins := sessionPins.load(wsKey); len(pins) != 0 {
		t.Fatalf("损坏文件应回退空: %v", pins)
	}
}
