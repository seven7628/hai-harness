package main

// 冷恢复「打开会话」路径的实验台（2026-11-11）。
//
// 目的：P4 的验收目标是「打开 < 100 ms；内存 4.5 GB → < 300 MB」——
// 这里量**今天真实路径**的耗时与峰值内存，判断 P4 还剩多少价值。
//
// 真实路径 = 桌面端打开一个已有会话时走的那条：
//
//	LoadRecordForView → parseCheckpointFromRecordWindow → reducer.Restore
//	                     → NormalizeForRestore
//
// 跑法：
//
//	HAI_LAB_SESSION=$HOME/.go-code/sessions/<ws>/<sid>.record.json \
//	go test ./desktop/bridge/ -run TestOpenLab -v -timeout 20m

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestOpenLab 量冷恢复的耗时分解与峰值内存。
func TestOpenLab(t *testing.T) {
	recordPath := os.Getenv("HAI_LAB_SESSION")
	if recordPath == "" {
		t.Skip("需要 HAI_LAB_SESSION=<sid>.record.json")
	}
	wsKey := filepath.Base(filepath.Dir(recordPath))
	sid := filepath.Base(recordPath)
	sid = sid[:len(sid)-len(".record.json")]

	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 预热（打开 sidecar 索引 / 页缓存），量的是「热」的第二次。
	for i := 0; i < 2; i++ {
		rec, lerr := store.LoadRecordForView(ctx, wsKey, sid)
		if lerr != nil {
			t.Fatal(lerr)
		}
		cp, perr := parseCheckpointFromRecordWindow(rec, restoreWindowBlocks)
		if perr != nil {
			t.Fatal(perr)
		}
		red := NewViewReducer()
		if err := red.Restore(cp); err != nil {
			t.Fatal(err)
		}
		red.NormalizeForRestore()
		rec.ReleaseView()
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	start := time.Now()
	rec, lerr := store.LoadRecordForView(ctx, wsKey, sid)
	if lerr != nil {
		t.Fatal(lerr)
	}
	loadDur := time.Since(start)

	start = time.Now()
	cp, perr := parseCheckpointFromRecordWindow(rec, restoreWindowBlocks)
	if perr != nil {
		t.Fatal(perr)
	}
	parseDur := time.Since(start)

	start = time.Now()
	red := NewViewReducer()
	if err := red.Restore(cp); err != nil {
		t.Fatal(err)
	}
	red.NormalizeForRestore()
	restoreDur := time.Since(start)

	runtime.ReadMemStats(&after)
	t.Logf("=== 冷恢复 %s ===", sid)
	t.Logf("LoadRecordForView=%s  parse窗口=%s  Restore=%s  合计=%s",
		loadDur.Round(time.Millisecond), parseDur.Round(time.Millisecond),
		restoreDur.Round(time.Millisecond), (loadDur + parseDur + restoreDur).Round(time.Millisecond))
	t.Logf("恢复后块数=%d  HeapAlloc 增量=%.1f MB",
		red.BlockCount(), float64(after.HeapAlloc-before.HeapAlloc)/(1<<20))
}
