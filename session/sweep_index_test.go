package session

// 清扫索引（sweep_index.go）的回归（2026-11-12）。
//
// 索引的**唯一职责**是「跳过已经清过、且此后没动过的文件」。
// 三条不变量：
//  1. 指纹一致 → 跳过；指纹变了 → 不跳过（宁多扫一遍，不可漏）；
//  2. 版本变了 → 旧指纹全部作废（换判据时不漏）；
//  3. 索引坏了 → 视作全量（安全）。
//
// 另外：索引只用于**跳过**，不参与任何写入决策 —— 索引算错最多多扫一遍，
// 不会写坏数据（这条由「二次清扫内容不变」的既有测试兜底）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// fingerprintOf 取一个文件的当前指纹。
func fingerprintOf(t *testing.T, p string) (int64, int64) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size(), fi.ModTime().UnixNano()
}

// TestSweepIndexSkipRequiresSameFingerprint 指纹一致才跳过。
func TestSweepIndexSkipRequiresSameFingerprint(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	size, mtime := fingerprintOf(t, p)

	ix := OpenSweepIndex(filepath.Join(dir, "idx.json"), SweepIndexVersion)
	if ix.Skip(p, size, mtime) {
		t.Fatal("没标记过的文件不该被跳过")
	}
	ix.Mark(p, size, mtime)
	if !ix.Skip(p, size, mtime) {
		t.Fatal("标记过且指纹一致 → 该跳过")
	}
	// 内容变了（size 变）→ 不跳过。
	if ix.Skip(p, size+1, mtime) {
		t.Fatal("size 变了不该跳过")
	}
	// 变了但 size 相同（mtime 变）→ 也不跳过。
	if ix.Skip(p, size, mtime+1) {
		t.Fatal("mtime 变了不该跳过")
	}
}

// TestSweepIndexVersionChangeInvalidates 版本变了 → 旧指纹全部作废。
func TestSweepIndexVersionChangeInvalidates(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "idx.json")
	p := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	size, mtime := fingerprintOf(t, p)

	ix := OpenSweepIndex(idxPath, SweepIndexVersion)
	ix.Mark(p, size, mtime)
	if err := ix.Flush(); err != nil {
		t.Fatal(err)
	}
	// 同版本：能跳过。
	ix2 := OpenSweepIndex(idxPath, SweepIndexVersion)
	if !ix2.Skip(p, size, mtime) {
		t.Fatal("同版本应能跳过")
	}
	// 换版本：作废。
	ix3 := OpenSweepIndex(idxPath, SweepIndexVersion+1)
	if ix3.Skip(p, size, mtime) {
		t.Fatal("版本变了，旧指纹必须作废")
	}
}

// TestSweepIndexCorruptFallsBackToFull 索引坏了 → 视作全量（不漏）。
func TestSweepIndexCorruptFallsBackToFull(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "idx.json")
	if err := os.WriteFile(idxPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "f.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	size, mtime := fingerprintOf(t, p)
	ix := OpenSweepIndex(idxPath, SweepIndexVersion)
	if ix.Skip(p, size, mtime) {
		t.Fatal("坏索引必须退回全量（不能跳过）")
	}
}

// TestSweepIndexPrunesDeletedFiles 会话被删 → 指纹剪掉（索引不无限膨胀）。
func TestSweepIndexPrunesDeletedFiles(t *testing.T) {
	dir := t.TempDir()
	idxPath := filepath.Join(dir, "idx.json")
	keep := filepath.Join(dir, "keep.bin")
	gone := filepath.Join(dir, "gone.bin")
	for _, p := range []string{keep, gone} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ix := OpenSweepIndex(idxPath, SweepIndexVersion)
	ks, km := fingerprintOf(t, keep)
	gs, gm := fingerprintOf(t, gone)
	ix.Mark(keep, ks, km)
	ix.Mark(gone, gs, gm)
	if err := ix.Flush(); err != nil {
		t.Fatal(err)
	}
	// 删掉 gone → 下一轮 Flush 应把它剪掉。
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	ix2 := OpenSweepIndex(idxPath, SweepIndexVersion)
	ix2.Mark(keep, ks, km)
	if err := ix2.Flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Entries map[string]sweepFingerprint `json:"entries"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Entries[gone]; ok {
		t.Fatal("已删除文件的指纹应被剪掉")
	}
	if _, ok := f.Entries[keep]; !ok {
		t.Fatal("还在的文件的指纹不该被剪掉")
	}
}

// TestSweepRecordSecondRunSkipsViaIndex record 清扫：第二次全跳过。
func TestSweepRecordSecondRunSkipsViaIndex(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	path, err := store.RecordPath(wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	// 造一个「旧形态」的 record（带 messages）。
	old := NewSessionRecord(wsKey, sid, 7, 42, 0,
		State{Messages: bigMessages(32), Usage: core.Usage{Input: 3}}, []byte(`{"schema_version":1,"blocks":[]}`))
	old.Checksum = ""
	raw, err := marshalRecordEnvelope(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	idxPath := filepath.Join(root, "idx.json")
	ix := OpenSweepIndex(idxPath, SweepIndexVersion)
	res, err := SweepRecordFiles(filepath.Join(root, "sessions"), 0, ix)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Stripped != 1 {
		t.Fatalf("第一轮应处理 1 个：%+v", res)
	}
	if err := ix.Flush(); err != nil {
		t.Fatal(err)
	}

	// 第二轮：新索引（从磁盘读）→ 全部跳过。
	ix2 := OpenSweepIndex(idxPath, SweepIndexVersion)
	res2, err := SweepRecordFiles(filepath.Join(root, "sessions"), 0, ix2)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Skipped != 1 {
		t.Fatalf("第二轮应跳过 1 个，实际 %+v", res2)
	}
	if res2.Files != 0 || res2.Stripped != 0 {
		t.Fatalf("第二轮不该再处理：%+v", res2)
	}
	// 记录仍可读（跳过 ≠ 损坏）。
	loaded, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil {
		t.Fatal("record 读不回来了")
	}
}

// TestSweepRecordModifiedAfterSweepIsReprocessed 清扫后记录又被改 → 要重扫。
func TestSweepRecordModifiedAfterSweepIsReprocessed(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	path, err := store.RecordPath(wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	old := NewSessionRecord(wsKey, sid, 7, 42, 0,
		State{Messages: bigMessages(8)}, []byte(`{"schema_version":1,"blocks":[]}`))
	old.Checksum = ""
	raw, err := marshalRecordEnvelope(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	idxPath := filepath.Join(root, "idx.json")
	ix := OpenSweepIndex(idxPath, SweepIndexVersion)
	if _, err := SweepRecordFiles(filepath.Join(root, "sessions"), 0, ix); err != nil {
		t.Fatal(err)
	}
	if err := ix.Flush(); err != nil {
		t.Fatal(err)
	}
	// record 又被改写（模拟一次提交）→ 指纹变了。
	if err := os.WriteFile(path, append(raw, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	ix2 := OpenSweepIndex(idxPath, SweepIndexVersion)
	res, err := SweepRecordFiles(filepath.Join(root, "sessions"), 0, ix2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped != 0 || res.Files != 1 {
		t.Fatalf("改过的 record 必须重扫：%+v", res)
	}
}

// TestSweepBlockStoresSecondRunSkipsViaIndex sidecar 清扫：第二次全跳过。
func TestSweepBlockStoresSecondRunSkipsViaIndex(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ws")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s1.record.json.blocks")

	// 用**关掉压缩**的形态写一批块（模拟存量数据）。
	blockCompressEnabled = false
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), compressibleBlock(i, 2000)); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}
	blockCompressEnabled = true

	idxPath := filepath.Join(root, "idx.json")
	ix := OpenSweepIndex(idxPath, SweepIndexVersion)
	res, err := SweepBlockStores(root, 0, ix)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Rewritten != 1 {
		t.Fatalf("第一轮应重写 1 个：%+v", res)
	}
	if err := ix.Flush(); err != nil {
		t.Fatal(err)
	}

	// 第二轮：全跳过。
	ix2 := OpenSweepIndex(idxPath, SweepIndexVersion)
	res2, err := SweepBlockStores(root, 0, ix2)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Skipped != 1 || res2.Files != 0 {
		t.Fatalf("第二轮应跳过 1 个：%+v", res2)
	}
}

// TestSweepBlockStoresNoOpDoesNotRewriteIndex 什么都不改的 sidecar：
// 索引文件不该被重写（免 fsync 路径）。
func TestSweepBlockStoresNoOpDoesNotRewriteIndex(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ws")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "s1.record.json.blocks")
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), compressibleBlock(i, 2000)); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}

	// 全部块已压缩 → 清扫判定「不需要改」→ 不该重写 .idx。
	idxPath := blockIndexPath(path)
	before, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	res, err := SweepBlockStores(root, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rewritten != 0 {
		t.Fatalf("不该重写：%+v", res)
	}
	after, err := os.Stat(idxPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("索引文件被重写了（免 fsync 路径失效）：%v → %v", before.ModTime(), after.ModTime())
	}
}

// TestSweepSessionFilesSecondRunSkipsViaIndex jsonl 清扫：第二次全跳过。
func TestSweepSessionFilesSecondRunSkipsViaIndex(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ws")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fs, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id := "s1"
	big := []core.Message{{Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: strings.Repeat("z", 200*1024)}}}}
	for i := 0; i < 8; i++ {
		if err := fs.SaveState(context.Background(), id, &State{Messages: big}); err != nil {
			t.Fatal(err)
		}
	}

	idxPath := filepath.Join(root, "idx.json")
	ix := OpenSweepIndex(idxPath, SweepIndexVersion)
	res, err := SweepSessionFiles(root, 1<<20, 2, false, 0, ix)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 {
		t.Fatalf("第一轮应处理 1 个：%+v", res)
	}
	if err := ix.Flush(); err != nil {
		t.Fatal(err)
	}

	ix2 := OpenSweepIndex(idxPath, SweepIndexVersion)
	res2, err := SweepSessionFiles(root, 1<<20, 2, false, 0, ix2)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Skipped != 1 || res2.Files != 0 {
		t.Fatalf("第二轮应跳过 1 个：%+v", res2)
	}
}
