package main

// checkpoint 设置的回归（2026-11-12）。
//
// 堆软上限（mem_limit_mb）的三态必须分清：
//
//	未配置（nil） → 用默认值（2048 MB）
//	配了 0/负数  → **不设上限**（回到 Go 默认行为）
//	配了正数     → 设软上限
//
// 混淆前两态会让「显式关掉限制」变成「用默认值」—— 那是反直觉的。

import "testing"

func TestMemLimitMBThreeStates(t *testing.T) {
	// 零值 = 未配置。
	var zero checkpointSettings
	mb, ok := zero.memLimitMB()
	if ok {
		t.Fatalf("零值不该是「显式配置」")
	}
	if mb != 0 {
		t.Fatalf("零值的值应为 0，实际 %d", mb)
	}

	// 显式正数。
	v := 3072
	c := checkpointSettings{MemLimitMB: &v}
	mb, ok = c.memLimitMB()
	if !ok || mb != 3072 {
		t.Fatalf("显式配置应返回 (3072, true)，实际 (%d, %v)", mb, ok)
	}

	// 显式 0 = 不设上限（**不是**「用默认值」）。
	zeroMB := 0
	c = checkpointSettings{MemLimitMB: &zeroMB}
	mb, ok = c.memLimitMB()
	if !ok {
		t.Fatalf("显式 0 应算「已配置」")
	}
	if mb != 0 {
		t.Fatalf("显式 0 的值应为 0，实际 %d", mb)
	}

	// 显式负数 = 不设上限。
	neg := -1
	c = checkpointSettings{MemLimitMB: &neg}
	mb, ok = c.memLimitMB()
	if !ok || mb != -1 {
		t.Fatalf("显式 -1 应返回 (-1, true)，实际 (%d, %v)", mb, ok)
	}
}

// TestCheckpointSettingsDefaultsUnchanged 默认值不许被这次改动悄悄改掉。
func TestCheckpointSettingsDefaultsUnchanged(t *testing.T) {
	var c checkpointSettings
	if !c.backgroundCompact() {
		t.Fatal("后台回收默认应开")
	}
	if c.keepHistory() {
		t.Fatal("history 裁剪默认应关（默认裁）")
	}
	if !c.sweepJournals() {
		t.Fatal("启动清扫默认应开")
	}
}
