package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func testSessionRecord(t *testing.T, workspace, id string, revision uint64) *SessionRecord {
	t.Helper()
	return NewSessionRecord(workspace, id, revision, revision*10, revision, State{}, ViewCheckpoint([]byte(`{"schema_version":1,"blocks":[{"id":"b"}],"run_nodes":[],"turns":[],"usage":{},"cost_usd":0,"todos":[],"degraded":false}`)))
}

func TestFileStorageRecordRoundTripAndAtomicPath(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testSessionRecord(t, "workspace-a", "session-1", 1)
	if err := storage.CommitRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	// 新写入用 v2 字节口径（sha256b:）；旧口径 sha256: 仅用于读兼容。
	if record.Checksum == "" || !strings.HasPrefix(record.Checksum, "sha256b:") {
		t.Fatalf("commit should publish checksum to caller, got %q", record.Checksum)
	}
	path, err := storage.RecordPath("workspace-a", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "session-1.record.json" {
		t.Fatalf("unexpected record path: %s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(storage.Root(), "workspace-a", "session-1.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("record commit must not create legacy jsonl, err=%v", err)
	}

	loaded, err := storage.LoadRecord(context.Background(), "workspace-a", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 1 || loaded.ReducerSeq != 10 || loaded.ClearGeneration != 1 {
		t.Fatalf("loaded envelope mismatch: %+v", loaded)
	}
	if string(loaded.ViewCheckpoint) != string(record.ViewCheckpoint) {
		t.Fatalf("checkpoint changed on round trip: %s != %s", loaded.ViewCheckpoint, record.ViewCheckpoint)
	}
	if err := loaded.Validate(); err != nil {
		t.Fatalf("loaded record must validate: %v", err)
	}
}

func TestFileStorageChecksumRejectsTampering(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testSessionRecord(t, "workspace-a", "session-1", 1)
	if err := storage.CommitRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	path, _ := storage.RecordPath("workspace-a", "session-1")
	var wire map[string]any
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	wire["reducer_seq"] = float64(999)
	tampered, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LoadRecord(context.Background(), "workspace-a", "session-1"); !errors.Is(err, ErrRecordCorrupt) {
		t.Fatalf("tampered record should be corrupt, got %v", err)
	}
}

// TestFileStorageAcceptsRecordWrittenByOlderSchema 回归（2026-09-23 现场）：
// checksum 若只认「按当前结构体重编码」的口径，那么任何一次**可达结构体新增字段**
// （usage 维度补齐给 core.Usage/core.Cost 加字段即如此）都会让全部旧 record 读不出来 ——
// 实测 93 个 record 里 92 个失效 → bridge 退化为 legacy snapshot 恢复 → 对话里
// Reasoning/Content 全空。本测试钉住「存盘字节口径」这条兼容路径：
//  1. 旧编码 record（payload 少字段 + checksum 按当时字节算）必须**能加载**；
//  2. 但按类型口径单独看它确实是失配的（证明走的是兼容路径，而不是校验被整体关掉）。
func TestFileStorageAcceptsRecordWrittenByOlderSchema(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testSessionRecord(t, "workspace-a", "session-legacy", 1)
	if err := storage.CommitRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	path, _ := storage.RecordPath("workspace-a", "session-legacy")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatal(err)
	}
	// 模拟「旧编码」：sdk_state 缺掉当前结构体必然重编码出来的键（旧版本没这些字段）+
	// checksum 按这份旧字节重算（= 旧进程写盘时算出的值）。
	wire["sdk_state"] = json.RawMessage(`{"messages":[],"usage":{"Input":7},"updated_at":"2026-09-23T00:00:00Z"}`)
	delete(wire, "checksum")
	payload, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	legacyChecksum, err := checksumForStoredPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	wire["checksum"] = json.RawMessage(strconv.Quote(legacyChecksum))
	legacyBytes, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(legacyBytes, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	// 路径断言：类型口径确实失配（否则本测试没覆盖到兼容分支）
	var parsed SessionRecord
	if err := json.Unmarshal(legacyBytes, &parsed); err != nil {
		t.Fatal(err)
	}
	if err := parsed.Validate(); !errors.Is(err, ErrRecordChecksum) {
		t.Fatalf("前置条件：类型口径应失配（ErrRecordChecksum），got %v", err)
	}
	// 兼容路径：加载必须成功
	loaded, err := storage.LoadRecord(context.Background(), "workspace-a", "session-legacy")
	if err != nil {
		t.Fatalf("旧编码 record 必须可加载（存盘字节口径），got %v", err)
	}
	if loaded.Revision != 1 || loaded.SessionID != "session-legacy" {
		t.Fatalf("加载内容不对: %+v", loaded)
	}
	// 但字节被改动（torn write/手改）依旧要拒绝
	if err := os.WriteFile(path, append(bytes.Replace(legacyBytes, []byte(`"Input":7`), []byte(`"Input":8`), 1), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LoadRecord(context.Background(), "workspace-a", "session-legacy"); !errors.Is(err, ErrRecordCorrupt) {
		t.Fatalf("字节被改的 record 必须判 corrupt，got %v", err)
	}
}

func TestFileStorageRevisionMonotonicAndIdempotent(t *testing.T) {

	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := testSessionRecord(t, "workspace-a", "session-1", 1)
	if err := storage.CommitRecord(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	checksum := first.Checksum
	if err := storage.CommitRecord(context.Background(), first); err != nil {
		t.Fatalf("same revision and payload should be idempotent: %v", err)
	}
	if first.Checksum != checksum {
		t.Fatalf("idempotent retry changed checksum: %q != %q", first.Checksum, checksum)
	}
	stale := testSessionRecord(t, "workspace-a", "session-1", 1)
	stale.ViewCheckpoint = ViewCheckpoint([]byte(`{"schema_version":1,"blocks":[{"id":"different"}],"run_nodes":[],"turns":[],"usage":{},"cost_usd":0,"todos":[],"degraded":false}`))
	if err := storage.CommitRecord(context.Background(), stale); !errors.Is(err, ErrRecordRevision) {
		t.Fatalf("same revision with different payload should conflict, got %v", err)
	}
	newer := testSessionRecord(t, "workspace-a", "session-1", 2)
	if err := storage.CommitRecord(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	older := testSessionRecord(t, "workspace-a", "session-1", 1)
	if err := storage.CommitRecord(context.Background(), older); !errors.Is(err, ErrRecordRevision) {
		t.Fatalf("older revision should conflict, got %v", err)
	}
	loaded, err := storage.LoadRecord(context.Background(), "workspace-a", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 2 {
		t.Fatalf("stale commit must not replace newer record: revision=%d", loaded.Revision)
	}
}

// TestFileStorageWorkspaceKeyWithHashSuffix 真实 workspaceKey（扁平化路径 + 短哈希，
// 含数字 0-9 / 字母 a-f / 下划线）必须被接受——此前反引号字面量把 'x'/'0' 误入拒绝集。
func TestFileStorageWorkspaceKeyWithHashSuffix(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"private_tmp_Users_admin_workspace-abc123-def456-00ff", // 真实形态：路径分段 + 哈希
		"ws_0x_test", // 含 0/x
		"a_b_c-0123", // 下划线 + 数字
	} {
		path, err := storage.RecordPath(key, "session-1")
		if err != nil {
			t.Fatalf("workspace key %q should be accepted: %v", key, err)
		}
		if !strings.HasSuffix(path, "/"+key+"/session-1.record.json") {
			t.Fatalf("unexpected path for %q: %s", key, path)
		}
	}
}

func TestFileStorageRejectsPathTraversalComponents(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"a/b", `a\b`, "a..b", "..", ".", "", "a\x00b"} {
		if _, err := storage.RecordPath(bad, "s1"); !errors.Is(err, ErrInvalidSessionRecord) {
			t.Fatalf("workspace key %q should be rejected, got %v", bad, err)
		}
		if _, err := storage.RecordPath("ok", bad); !errors.Is(err, ErrInvalidSessionRecord) {
			t.Fatalf("session id %q should be rejected, got %v", bad, err)
		}
	}
}

func TestFileStorageBackupFallbackAndRecovery(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first := testSessionRecord(t, "workspace-a", "session-1", 1)
	if err := storage.CommitRecord(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := testSessionRecord(t, "workspace-a", "session-1", 2)
	if err := storage.CommitRecord(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	path, _ := storage.RecordPath("workspace-a", "session-1")
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("second commit should retain bounded backup: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := storage.LoadRecord(context.Background(), "workspace-a", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 1 {
		t.Fatalf("corrupt formal should fall back to previous backup, got revision=%d", loaded.Revision)
	}
	third := testSessionRecord(t, "workspace-a", "session-1", 3)
	if err := storage.CommitRecord(context.Background(), third); err != nil {
		t.Fatalf("newer commit should recover from corrupt formal using backup: %v", err)
	}
	loaded, err = storage.LoadRecord(context.Background(), "workspace-a", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 3 {
		t.Fatalf("recovered formal should be revision 3, got %d", loaded.Revision)
	}
}

func TestFileStorageRejectsInvalidIdentityAndCheckpoint(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range []*SessionRecord{
		testSessionRecord(t, "../escape", "session-1", 1),
		testSessionRecord(t, "workspace-a", "../escape", 1),
		func() *SessionRecord {
			r := testSessionRecord(t, "workspace-a", "session-1", 1)
			r.ViewCheckpoint = nil
			return r
		}(),
		func() *SessionRecord {
			r := testSessionRecord(t, "workspace-a", "session-1", 1)
			r.ViewCheckpoint = ViewCheckpoint([]byte(`{"schema_version":2,"blocks":[],"run_nodes":[],"turns":[]}`))
			return r
		}(),
	} {
		if err := storage.CommitRecord(context.Background(), record); !errors.Is(err, ErrInvalidSessionRecord) {
			t.Fatalf("invalid record should be rejected with ErrInvalidSessionRecord, got %v", err)
		}
	}
	for _, workspace := range []string{"", "..", "a/b", "a\\b", "a..b"} {
		if _, err := storage.RecordPath(workspace, "session-1"); !errors.Is(err, ErrInvalidSessionRecord) {
			t.Fatalf("workspace %q should be rejected, got %v", workspace, err)
		}
	}
}

func TestFileStorageDeleteDoesNotTouchLegacyOrSubagents(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testSessionRecord(t, "workspace-a", "session-1", 1)
	if err := storage.CommitRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	workspaceDir := filepath.Join(storage.Root(), "workspace-a")
	legacy := filepath.Join(workspaceDir, "session-1.jsonl")
	journal := filepath.Join(workspaceDir, "session-1", "agents", "task-abc123fg.jsonl")
	if err := os.MkdirAll(filepath.Dir(journal), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("journal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := storage.DeleteSessionRecord(context.Background(), "workspace-a", "session-1"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{legacy, journal} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("delete record must not remove %s: %v", path, err)
		}
	}
	if _, err := storage.LoadRecord(context.Background(), "workspace-a", "session-1"); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("deleted record should be not found, got %v", err)
	}
}

func TestFileStorageLoadNotFoundAndCanceled(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LoadRecord(context.Background(), "workspace-a", "session-1"); !errors.Is(err, ErrRecordNotFound) {
		t.Fatalf("missing record should wrap ErrNotFound, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storage.LoadRecord(ctx, "workspace-a", "session-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled load should stop before I/O, got %v", err)
	}
}

func TestFileStorageRecordPermissions(t *testing.T) {
	if os.Getuid() == -1 {
		t.Skip("unavailable uid")
	}
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testSessionRecord(t, "workspace-a", "session-1", 1)
	if err := storage.CommitRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	path, _ := storage.RecordPath("workspace-a", "session-1")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record should be owner-readable/writable only, mode=%o", info.Mode().Perm())
	}
}
