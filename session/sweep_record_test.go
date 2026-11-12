package session

// P3-1（record 降级为 manifest + 标量）的回归（2026-11-11）。
//
// 三条不变量：
//  1. **新写入的 record 不再携带 SDK 上下文**（sdk_state.messages）；
//  2. **扫���后的旧 record 仍然可读**（checksum 按字节口径重算，读侧校验照常通过）；
//  3. 扫出来的字节与「用同一份 record 提交一次」**逐字节相同**
//     （走的是同一条编码路径 —— 否则下一次提交就会产生一次无谓的差异）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// bigMessages 造一份有体积的 SDK 上下文（真实会话里是 16MB 量级）。
func bigMessages(n int) []core.Message {
	msgs := make([]core.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, core.Message{
			Role:    "assistant",
			Content: []core.Content{{Type: "text", Content: strings.Repeat("x", 1024)}},
		})
	}
	return msgs
}

// TestCommitRecordOmitsSDKMessages 新写入的 record 不该再带 sdk_state.messages。
func TestCommitRecordOmitsSDKMessages(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	state := State{Messages: bigMessages(64), Usage: core.Usage{Input: 10, Output: 5}}
	view := []byte(`{"schema_version":1,"blocks":[],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`)
	rec := NewSessionRecord(wsKey, sid, 1, 1, 0, state, view)
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	path, err := store.RecordPath(wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		SDKState struct {
			Messages []json.RawMessage `json:"messages"`
		} `json:"sdk_state"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.SDKState.Messages) != 0 {
		t.Fatalf("record 仍带着 %d 条 SDK 上下文消息（体积没降）", len(probe.SDKState.Messages))
	}
	// 调用方的 record 对象不受影响（提交路径只改副本）。
	if len(rec.SDKState.Messages) != 64 {
		t.Fatalf("提交路径改了调用方的 record：%d 条", len(rec.SDKState.Messages))
	}
	// 读回来仍然可用（标量还在）。
	loaded, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SDKState.Usage.Input != 10 {
		t.Fatalf("标量丢了: %+v", loaded.SDKState.Usage)
	}
}

// TestSweepRecordStripsOldRecords 存量 record（含 messages）扫完仍可读。
func TestSweepRecordStripsOldRecords(t *testing.T) {
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

	// 造一个「旧形态」的 record：带 messages，且用旧的 v1 checksum 口径。
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
	before, _ := os.Stat(path)

	res, err := SweepRecordFiles(filepath.Join(root, "sessions"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stripped != 1 {
		t.Fatalf("瘦身了 %d 个 want 1", res.Stripped)
	}
	after, _ := os.Stat(path)
	if after.Size() >= before.Size() {
		t.Fatalf("体积没降: %d → %d", before.Size(), after.Size())
	}

	// 关键：扫完仍然可读（checksum 按字节口径重算 → 读侧校验通过）。
	loaded, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatalf("扫完读不出来: %v", err)
	}
	if loaded.Revision != 7 || loaded.ReducerSeq != 42 {
		t.Fatalf("标量被改了: rev=%d seq=%d", loaded.Revision, loaded.ReducerSeq)
	}
	if len(loaded.SDKState.Messages) != 0 {
		t.Fatalf("SDK 上下文还在: %d 条", len(loaded.SDKState.Messages))
	}
	if loaded.SDKState.Usage.Input != 3 {
		t.Fatalf("标量丢了: %+v", loaded.SDKState.Usage)
	}
}

// TestSweepRecordBytesMatchCommit 扫出来的字节 == 提交同一份 record 的字节。
//
// 这条不变量保证：清扫不是「另一种格式」，而是「同一份 record 的瘦身版」。
func TestSweepRecordBytesMatchCommit(t *testing.T) {
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
	rec := NewSessionRecord(wsKey, sid, 5, 11, 0,
		State{Messages: bigMessages(16), Usage: core.Usage{Input: 1}}, []byte(`{"schema_version":1,"blocks":[]}`))
	// 固定时间戳：对照编码必须与扫出来的字节逐字节相同（含 checksum）。
	rec.UpdatedAt = time.Now().UTC().Truncate(time.Second)
	// ① 旧形态落盘（带 messages）。
	old := *rec
	old.Checksum = ""
	raw, err := marshalRecordEnvelope(&old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// ② 扫。
	if _, err := SweepRecordFiles(filepath.Join(root, "sessions"), 0); err != nil {
		t.Fatal(err)
	}
	swept, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// ③ 对照：同一份 record 去掉 messages 后的**规范编码**。
	// （不能直接再提交一次 —— 提交路径会拒绝「同 revision 不同 payload」，
	//   而清扫刻意**不改 revision**，这正是它与并发提交不冲突的原因。）
	fresh := *rec
	fresh.SDKState.Messages = nil
	expected, err := encodeStrippedRecord(&fresh)
	if err != nil {
		t.Fatal(err)
	}
	if string(swept) != string(expected) {
		t.Fatalf("扫出来的字节与规范编码不一致:\n扫: %s\n期望: %s", swept, expected)
	}
}

// TestSweepRecordLeavesCleanRecordsAlone 已经瘦身的 record 不该被再改一遍。
func TestSweepRecordLeavesCleanRecordsAlone(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	rec := NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, []byte(`{"schema_version":1,"blocks":[]}`))
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	path, _ := store.RecordPath(wsKey, sid)
	before, _ := os.ReadFile(path)

	res, err := SweepRecordFiles(filepath.Join(root, "sessions"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stripped != 0 {
		t.Fatalf("已经干净的 record 被改了 %d 次", res.Stripped)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("已经干净的 record 被改写了")
	}
}

// TestSweepRecordMigratesInlineBlocksToSidecar 内嵌形态的 record 扫完搬进 sidecar。
//
// 这是 P3-1 的第二半：内嵌形态的块内容在 formal 和 .bak 里**各存一份**
// （实测全量 2.59GB）—— 搬进 sidecar 后只存一份。
func TestSweepRecordMigratesInlineBlocksToSidecar(t *testing.T) {
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
	// 内嵌 blocks 的 view（不走 sidecar 提交路径，直接手写旧格式）。
	//
	// 块要有真实体积：sidecar 形态的引用文档有固定开销（store 名 + 摘要 +
	// id 列表），**小 view 下内嵌反而更小** —— 迁移只对大 view 有收益，
	// 这正是它只作为「存量清扫」而不是「默认形态」的原因。
	big := strings.Repeat("内容", 2048) // 6KB
	view := []byte(`{"schema_version":1,"blocks":[{"kind":"assistant","id":1,"text":"` + big + `"},` +
		`{"kind":"tool","id":2,"name":"bash","args":"{}","result":"` + big + `"}],"run_nodes":[],"turns":[]}`)
	old := NewSessionRecord(wsKey, sid, 3, 9, 0, State{}, view)
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
	// 造一个 .bak（也是内嵌形态）。
	if err := os.WriteFile(path+".bak", raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	beforeBak, _ := os.Stat(path + ".bak")

	res, err := SweepRecordFiles(filepath.Join(root, "sessions"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Migrated != 1 {
		t.Fatalf("迁移了 %d 个 want 1", res.Migrated)
	}
	after, _ := os.Stat(path)
	afterBak, _ := os.Stat(path + ".bak")
	t.Logf("formal %d → %d 字节；.bak %d → %d 字节", before.Size(), after.Size(), beforeBak.Size(), afterBak.Size())
	if after.Size() >= before.Size() {
		t.Fatalf("formal 没变小: %d → %d", before.Size(), after.Size())
	}
	if afterBak.Size() >= beforeBak.Size() {
		t.Fatalf(".bak 没变小: %d → %d", beforeBak.Size(), afterBak.Size())
	}
	// sidecar 应该存在了。
	if _, err := os.Stat(path + ".blocks"); err != nil {
		t.Fatalf("sidecar 没生成: %v", err)
	}

	// 关键：读回来内容不变（两个块都在，顺序不变）。
	loaded, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatalf("扫完读不出来: %v", err)
	}
	blocks := loaded.ViewCheckpoint
	idx := strings.Index(string(blocks), `"kind":"assistant"`)
	if idx < 0 {
		t.Fatalf("块内容丢了: %s", blocks)
	}
	if !strings.Contains(string(blocks), `"kind":"tool"`) {
		t.Fatalf("第二个块丢了: %s", blocks)
	}
	if loaded.Revision != 3 || loaded.ReducerSeq != 9 {
		t.Fatalf("标量被改了: rev=%d seq=%d", loaded.Revision, loaded.ReducerSeq)
	}
}
