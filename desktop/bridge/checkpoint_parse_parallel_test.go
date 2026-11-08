package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/session"
)

// TestParseCheckpointFromRecordMatchesFullParse 锁住「骨架 + 并行块解码」与
// 「整份 json.Unmarshal」逐字段等价。
//
// 两条路径必须一致的原因：快路径把 blocks 字段替换成 [] 后先解骨架，再把
// LoadedBlockSpans 分给多个 worker 并行解码。任何一处错位（顺序、字段、
// 切片共享）都会让用户看到「历史块串位/丢字段」，而这种错误不会在
// 只跑慢路径的测试里暴露。
func TestParseCheckpointFromRecordMatchesFullParse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	wsKey := workspaceKey(ws)

	newManagerWithAppData(t, t.TempDir()) // 装配 appDataDirOverride（先于 newRecordStore）
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}

	cp := &Checkpoint{
		SchemaVersion: checkpointSchemaVersion,
		Blocks:        parallelTestBlocks(200),
		RunNodes:      []RunNode{{ID: "root-1", Label: "主会话", Status: statusRunning, Depth: 0, Kind: runNodeKindRun}},
		Turns:         []Turn{{Input: 1200, Output: 340, CostUSD: 0.0123}},
		Usage:         Usage{Input: 1200, Output: 340},
		CostUSD:       0.0123,
		Todos:         []TodoItem{{ID: "t1", Title: "检查并行解码"}},
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	rec := session.NewSessionRecord(wsKey, "s-par", 1, 5, 0, session.State{}, raw)
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadRecord(context.Background(), wsKey, "s-par")
	if err != nil {
		t.Fatal(err)
	}
	// 前置：落盘形态必须是 sidecar 引用（否则本测试只覆盖了慢路径）。
	if len(loaded.LoadedBlockSpans) != len(cp.Blocks) {
		t.Fatalf("LoadRecord 应带回 %d 个块切片（sidecar 引用形态），得 %d",
			len(cp.Blocks), len(loaded.LoadedBlockSpans))
	}
	if !strings.Contains(string(loaded.BodyView), "$blocks_ref") {
		t.Fatalf("BodyView 应是未展开的引用形态")
	}

	full, err := parseCheckpoint(loaded.ViewCheckpoint)
	if err != nil {
		t.Fatalf("慢路径解析失败: %v", err)
	}
	fast, err := parseCheckpointFromRecord(loaded)
	if err != nil {
		t.Fatalf("快路径解析失败: %v", err)
	}
	if !reflect.DeepEqual(full, fast) {
		t.Fatalf("快慢路径结果不一致：blocks %d vs %d", len(full.Blocks), len(fast.Blocks))
	}
	if len(fast.Blocks) != len(cp.Blocks) {
		t.Fatalf("块数丢失：want %d got %d", len(cp.Blocks), len(fast.Blocks))
	}
	// 顺序敏感：逐块 ID 必须与写入顺序一致（并行解码不能重排）。
	for i := range cp.Blocks {
		if fast.Blocks[i].ID != cp.Blocks[i].ID || fast.Blocks[i].Kind != cp.Blocks[i].Kind {
			t.Fatalf("块 %d 错位：want id=%d kind=%s got id=%d kind=%s",
				i, cp.Blocks[i].ID, cp.Blocks[i].Kind, fast.Blocks[i].ID, fast.Blocks[i].Kind)
		}
	}

	// SkeletonView：blocks 必须是空数组，其余字段与整份解析一致。
	skel, err := loaded.SkeletonView()
	if err != nil {
		t.Fatalf("SkeletonView: %v", err)
	}
	var skelCP Checkpoint
	if err := json.Unmarshal(skel, &skelCP); err != nil {
		t.Fatalf("骨架解码失败: %v", err)
	}
	if len(skelCP.Blocks) != 0 {
		t.Fatalf("骨架里不应有块，得 %d", len(skelCP.Blocks))
	}
	if !reflect.DeepEqual(skelCP.RunNodes, full.RunNodes) ||
		!reflect.DeepEqual(skelCP.Turns, full.Turns) ||
		!reflect.DeepEqual(skelCP.Todos, full.Todos) ||
		skelCP.Usage != full.Usage {
		t.Fatalf("骨架的正文部分与整份解析不一致")
	}
}

// TestParseCheckpointFromRecordInlineFallback 旧格式（blocks 内嵌）没有 spans，
// 必须自动回退到整份解析 —— 而不是返回空 blocks。
func TestParseCheckpointFromRecordInlineFallback(t *testing.T) {
	cp := &Checkpoint{
		SchemaVersion: checkpointSchemaVersion,
		Blocks:        parallelTestBlocks(8),
		RunNodes:      []RunNode{},
		Turns:         []Turn{{Input: 1, Output: 2}},
		Todos:         []TodoItem{},
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	rec := &session.SessionRecord{ViewCheckpoint: session.ViewCheckpoint(raw)}

	got, err := parseCheckpointFromRecord(rec)
	if err != nil {
		t.Fatalf("内嵌形态解析失败: %v", err)
	}
	if len(got.Blocks) != len(cp.Blocks) {
		t.Fatalf("内嵌形态块数：want %d got %d", len(cp.Blocks), len(got.Blocks))
	}
	if !reflect.DeepEqual(got, cp) {
		t.Fatalf("内嵌形态解析结果与原始快照不一致")
	}
}

// TestDecodeBlocksParallelRejectsCorrupt 并行解码必须把坏块报成错误（带下标），
// 不能静默产出零值块 —— 那会让损坏记录看起来「只是某个块空白」。
func TestDecodeBlocksParallelRejectsCorrupt(t *testing.T) {
	spans := make([][]byte, 128)
	out := make([]Block, 128)
	for i := range spans {
		spans[i] = []byte(fmt.Sprintf(`{"kind":"assistant","id":%d,"text":"x"}`, i+1))
	}
	spans[77] = []byte(`{"kind":`)
	if err := decodeBlocksParallel(spans, out); err == nil {
		t.Fatal("坏块应报错")
	} else if !strings.Contains(err.Error(), "block 77") {
		t.Fatalf("错误信息应带块下标，得 %v", err)
	}
	if out[0].ID != 1 || out[127].ID != 128 {
		t.Fatalf("其余块应已解出：first=%d last=%d", out[0].ID, out[127].ID)
	}
}

// parallelTestBlocks 造 n 个大小差异明显的块（并行解码的分片平衡要靠它压出来）。
func parallelTestBlocks(n int) []Block {
	blocks := make([]Block, n)
	for i := range blocks {
		kind := blockKindAssistant
		if i%3 == 0 {
			kind = blockKindTool
		}
		blocks[i] = Block{
			Kind: kind,
			ID:   uint64(i + 1),
			Text: strings.Repeat("x", 10+i*7),
		}
	}
	return blocks
}
