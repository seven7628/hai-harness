package session

// checksum 双口径兼容测试（2026-10-18 性能优化）。
//
// 背景：提交路径原先把 90MB payload 做 canonicalJSON（marshal → decode → marshal）
// 再 sha256，实测 1250ms，占 commit 的 90%+。改成「按磁盘字节 sha256」（v2，
// 前缀 sha256b:）后降到 35ms。
//
// **这个改动的全部风险在兼容性**：本机已有 371 个 record 用的是旧口径
// （sha256:，规范化 JSON）。读取路径必须两者都认，否则升级后历史会话全部读不出。
// 本文件钉住三条：
//  1. 旧口径（sha256:）的 record 仍能读；
//  2. 新口径（sha256b:）的 record 能读；
//  3. 两种口径都能检出字节级损坏（改一个字节 → 拒绝）。

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// writeLegacyRecord 用**旧口径**写一条 record（模拟本次改动之前的历史文件）。
func writeLegacyRecord(t *testing.T, storage *FileStorage, rec *SessionRecord) string {
	t.Helper()
	sum, err := checksumForRecord(rec) // 旧口径：canonicalJSON
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sum, "sha256:") {
		t.Fatalf("旧口径前缀应为 sha256:，得到 %s", sum)
	}
	rec.Checksum = sum
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	path, err := storage.RecordPath(rec.WorkspaceKey, rec.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(strings.TrimSuffix(path, "/"+rec.SessionID+".record.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 旧口径（sha256:）的 record 必须仍可读 —— 兼容 371 个历史文件的生命线。
func TestLegacyChecksumStillReadable(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := testSessionRecord(t, "workspace-a", "session-1", 5)
	rec.UpdatedAt = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	rec.Checksum = ""
	path := writeLegacyRecord(t, storage, rec)

	loaded, err := storage.LoadRecord(context.Background(), "workspace-a", "session-1")
	if err != nil {
		t.Fatalf("旧口径 record 必须仍能读: %v", err)
	}
	if loaded.Revision != 5 {
		t.Fatalf("revision 错: %d", loaded.Revision)
	}
	if !strings.HasPrefix(loaded.Checksum, "sha256:") {
		t.Fatalf("不应改写旧 checksum: %s", loaded.Checksum)
	}
	_ = path

	// 元数据路径（只读头部）也必须接受旧口径。
	meta, err := storage.LoadRecordMetadata(context.Background(), "workspace-a", "session-1")
	if err != nil {
		t.Fatalf("旧口径 record 的元数据必须可读: %v", err)
	}
	if meta.Revision != 5 {
		t.Fatalf("元数据 revision 错: %d", meta.Revision)
	}
}

// 新口径（sha256b:）的 record 必须可读。
func TestV2ChecksumReadable(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := testSessionRecord(t, "workspace-a", "session-1", 6)
	rec.UpdatedAt = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if err := storage.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.Checksum, checksumV2Prefix) {
		t.Fatalf("新写入应为 v2 口径，得到 %s", rec.Checksum)
	}
	loaded, err := storage.LoadRecord(context.Background(), "workspace-a", "session-1")
	if err != nil {
		t.Fatalf("v2 record 必须可读: %v", err)
	}
	if loaded.Revision != 6 {
		t.Fatalf("revision 错: %d", loaded.Revision)
	}
}

// 两种口径都必须检出字节级损坏 —— 这是 checksum 存在的唯一理由。
func TestBothChecksumsDetectCorruption(t *testing.T) {
	cases := []struct {
		name   string
		legacy bool
	}{
		{"v1 旧口径", true},
		{"v2 新口径", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			storage, err := NewFileStorage(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			rec := testSessionRecord(t, "workspace-a", "session-1", 7)
			rec.UpdatedAt = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
			rec.Checksum = ""

			var path string
			if tc.legacy {
				path = writeLegacyRecord(t, storage, rec)
			} else {
				if err := storage.CommitRecord(context.Background(), rec); err != nil {
					t.Fatal(err)
				}
				path, _ = storage.RecordPath("workspace-a", "session-1")
			}

			// 篡改 payload 内容（保持 JSON 合法）：把某个 block 的 id 改掉。
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			tampered := strings.Replace(string(data), `"blocks":[{"id":"b"}]`, `"blocks":[{"id":"c"}]`, 1)
			if tampered == string(data) {
				t.Skip("夹具里没找到可篡改的片段")
			}
			// 同时删掉 .bak，确保判定落在 formal 上。
			_ = os.Remove(path + ".bak")
			if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, err := storage.LoadRecord(context.Background(), "workspace-a", "session-1"); err == nil {
				t.Fatal("篡改过的 record 必须被拒绝")
			}
		})
	}
}

// 元数据路径不校验 payload（有意），但结构损坏仍必须拒绝。
// 这条确保「快路径」没有把安全网一起丢掉。
func TestMetadataPathStillRejectsStructuralDamage(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := testSessionRecord(t, "workspace-a", "session-1", 8)
	rec.UpdatedAt = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	if err := storage.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	path, _ := storage.RecordPath("workspace-a", "session-1")
	_ = os.Remove(path + ".bak")

	// 截断文件：尾部被砍掉，JSON 不完整。
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.LoadRecordMetadata(context.Background(), "workspace-a", "session-1"); err == nil {
		t.Fatal("截断的 record 必须被元数据路径拒绝")
	}
}
