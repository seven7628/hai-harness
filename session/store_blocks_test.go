package session

// blocks sidecar（增量落盘）回归测试。
//
// 这套优化动的是**持久化格式**，一旦出错就是「长会话打开后历史丢失」——
// 比慢严重得多。所以这里钉住的不是性能数字，而是正确性不变量：
//   1. 往返等价：写入→读回，view 与内嵌完整 blocks 的旧格式完全等价；
//   2. 终态 block 不被重复写（增量生效的直接证据）；
//   3. 进行中的 block 每次覆盖写（否则会读到陈旧内容）；
//   4. sidecar 损坏/截断 → 报错并回退，绝不给半份数据；
//   5. 删除 record 时 sidecar 一起清理（否则留几十 MB 孤儿文件）。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

// sidecarTestView 造一个含 blocks 的 view（blocks 用真实结构的子集）。
func sidecarTestView(t *testing.T, blocks string) ViewCheckpoint {
	t.Helper()
	v := `{"schema_version":1,"blocks":` + blocks + `,"run_nodes":[],"turns":[],"usage":{},"cost_usd":0,"todos":[],"degraded":false}`
	if !json.Valid([]byte(v)) {
		t.Fatalf("测试 fixture 不是合法 JSON: %s", v)
	}
	return ViewCheckpoint(v)
}

func sidecarRecord(t *testing.T, rev uint64, view ViewCheckpoint) *SessionRecord {
	t.Helper()
	r := NewSessionRecord("ws-a", "sess-1", rev, rev*10, 0, State{}, view)
	r.Checksum = ""
	return r
}

// TestSidecarRoundTripEquivalence 是这套改动最重要的不变量：
// 走 sidecar 落盘后读回的 view，必须与「内嵌完整 blocks」的语义完全一致。
func TestSidecarRoundTripEquivalence(t *testing.T) {
	s, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	blocks := `[` +
		`{"id":1,"kind":"assistant","status":"done","ts":100,"text":"hello"},` +
		`{"id":2,"kind":"tool","status":"done","ts":101,"name":"bash"},` +
		`{"id":3,"kind":"user","status":"done","ts":102,"text":"世界"}` +
		`]`
	view := sidecarTestView(t, blocks)
	rec := sidecarRecord(t, 1, view)
	if err := s.CommitRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.LoadRecord(ctx, "ws-a", "sess-1")
	if err != nil {
		t.Fatalf("LoadRecord: %v", err)
	}

	var got, want any
	if err := json.Unmarshal(loaded.ViewCheckpoint, &got); err != nil {
		t.Fatalf("读回的 view 不是合法 JSON: %v", err)
	}
	if err := json.Unmarshal(view, &want); err != nil {
		t.Fatal(err)
	}
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if string(gj) != string(wj) {
		t.Fatalf("往返不等价\n got: %s\nwant: %s", gj, wj)
	}

	// 读回的 blocks 必须是真实数组（引用已被解析掉）
	path, _ := s.RecordPath("ws-a", "sess-1")
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(onDisk), blocksRefKey) == false {
		t.Fatal("落盘 record 里应保留 blocks 引用（否则退回了内嵌全量）")
	}
	if _, err := os.Stat(blockStorePath(path)); err != nil {
		t.Fatalf("sidecar 未生成: %v", err)
	}
}

// TestSidecarSkipsTerminalBlocks 终态 block 第二次提交不应被重写（增量的直接证据）。
func TestSidecarSkipsTerminalBlocks(t *testing.T) {
	s, _ := NewFileStorage(t.TempDir())
	ctx := context.Background()

	view1 := sidecarTestView(t, `[{"id":1,"kind":"assistant","status":"done","ts":1,"text":"a"}]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 1, view1)); err != nil {
		t.Fatal(err)
	}
	path, _ := s.RecordPath("ws-a", "sess-1")
	size1, err := os.Stat(blockStorePath(path))
	if err != nil {
		t.Fatal(err)
	}

	// 同样的 blocks 再提交一次 → sidecar 不应增长
	if err := s.CommitRecord(ctx, sidecarRecord(t, 2, view1)); err != nil {
		t.Fatal(err)
	}
	size2, _ := os.Stat(blockStorePath(path))
	if size2.Size() != size1.Size() {
		t.Fatalf("终态 block 被重复写入：sidecar %d → %d 字节", size1.Size(), size2.Size())
	}
}

// TestSidecarRewritesLiveBlocks 进行中的 block 必须每次覆盖写 —— 否则读回的是陈旧内容。
func TestSidecarRewritesLiveBlocks(t *testing.T) {
	s, _ := NewFileStorage(t.TempDir())
	ctx := context.Background()

	v1 := sidecarTestView(t, `[{"id":1,"kind":"agent","status":"running","ts":1,"text":"阶段一"}]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 1, v1)); err != nil {
		t.Fatal(err)
	}
	v2 := sidecarTestView(t, `[{"id":1,"kind":"agent","status":"running","ts":1,"text":"阶段二"}]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 2, v2)); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.LoadRecord(ctx, "ws-a", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	var cp struct {
		Blocks []struct {
			Text string `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(loaded.ViewCheckpoint, &cp); err != nil {
		t.Fatal(err)
	}
	if len(cp.Blocks) != 1 || cp.Blocks[0].Text != "阶段二" {
		t.Fatalf("进行中的 block 读了陈旧内容: %+v", cp.Blocks)
	}

	// 终态化之后再提交，内容变化也必须落盘（从 running→done 是内容变化）
	v3 := sidecarTestView(t, `[{"id":1,"kind":"agent","status":"done","ts":1,"text":"完成"}]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 3, v3)); err != nil {
		t.Fatal(err)
	}
	loaded, _ = s.LoadRecord(ctx, "ws-a", "sess-1")
	if err := json.Unmarshal(loaded.ViewCheckpoint, &cp); err != nil {
		t.Fatal(err)
	}
	if cp.Blocks[0].Text != "完成" {
		t.Fatalf("running→done 的内容变化没落盘: %+v", cp.Blocks[0])
	}
}

// TestSidecarCorruptionIsRejected sidecar 被篡改/截断必须报错，绝不返回半份数据。
func TestSidecarCorruptionIsRejected(t *testing.T) {
	s, _ := NewFileStorage(t.TempDir())
	ctx := context.Background()

	// 同时写 .bak，这样损坏 formal 后能回退（验证回退链路而不是只验证报错）
	view1 := sidecarTestView(t, `[{"id":1,"kind":"assistant","status":"done","ts":1,"text":"原始"}]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 1, view1)); err != nil {
		t.Fatal(err)
	}
	view2 := sidecarTestView(t, `[{"id":1,"kind":"assistant","status":"done","ts":1,"text":"第二版"}]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 2, view2)); err != nil {
		t.Fatal(err)
	}
	path, _ := s.RecordPath("ws-a", "sess-1")

	// 篡改 sidecar 中间部分（模拟磁盘损坏）
	bs, err := os.OpenFile(blockStorePath(path), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bs.WriteAt([]byte("XXXX"), 40); err != nil {
		t.Fatal(err)
	}
	_ = bs.Close()
	evictBlockStore(path) // 丢弃缓存，强制重新读盘

	loaded, err := s.LoadRecord(ctx, "ws-a", "sess-1")
	if err != nil {
		// 允许直接报错（没有可用 .bak 时）
		if !errors.Is(err, ErrRecordCorrupt) {
			t.Fatalf("应报 ErrRecordCorrupt，实为 %v", err)
		}
		return
	}
	// 若成功，必须是从 .bak 恢复出来的完整数据，不能是半份
	var cp struct {
		Blocks []struct {
			Text string `json:"text"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(loaded.ViewCheckpoint, &cp); err != nil {
		t.Fatalf("回退后的 view 不合法: %v", err)
	}
	if len(cp.Blocks) != 1 || (cp.Blocks[0].Text != "原始" && cp.Blocks[0].Text != "第二版") {
		t.Fatalf("回退数据不完整: %+v", cp.Blocks)
	}
}

// TestSidecarRemovedWithRecord 删除 record 必须连带删除 sidecar。
func TestSidecarRemovedWithRecord(t *testing.T) {
	s, _ := NewFileStorage(t.TempDir())
	ctx := context.Background()
	view := sidecarTestView(t, `[{"id":1,"kind":"assistant","status":"done","ts":1}]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 1, view)); err != nil {
		t.Fatal(err)
	}
	path, _ := s.RecordPath("ws-a", "sess-1")
	if _, err := os.Stat(blockStorePath(path)); err != nil {
		t.Fatalf("sidecar 应存在: %v", err)
	}
	if err := s.DeleteSessionRecord(ctx, "ws-a", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(blockStorePath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar 应被删除，stat err = %v", err)
	}
}

// TestSidecarBlockOrderByID 读回时 block 必须按 id 递增排序（与 reducer 的投影一致）。
func TestSidecarBlockOrderByID(t *testing.T) {
	s, _ := NewFileStorage(t.TempDir())
	ctx := context.Background()

	// 故意乱序写入（模拟不同提交时序）
	v := sidecarTestView(t, `[` +
		`{"id":5,"kind":"assistant","status":"done","ts":5},` +
		`{"id":2,"kind":"assistant","status":"done","ts":2}` +
		`]`)
	if err := s.CommitRecord(ctx, sidecarRecord(t, 1, v)); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadRecord(ctx, "ws-a", "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	var cp struct {
		Blocks []struct {
			ID uint64 `json:"id"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(loaded.ViewCheckpoint, &cp); err != nil {
		t.Fatal(err)
	}
	if len(cp.Blocks) != 2 {
		t.Fatalf("块数不对: %d", len(cp.Blocks))
	}
	if cp.Blocks[0].ID != 2 || cp.Blocks[1].ID != 5 {
		t.Fatalf("读回顺序应按 id 递增，实为 %d,%d", cp.Blocks[0].ID, cp.Blocks[1].ID)
	}
}
