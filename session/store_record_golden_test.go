package session

// 提交序列化的 golden 回归。
//
// 背景：CommitRecord 曾在 90MB 的 record 上把同一份 payload 序列化 5 遍
// （Clone 的 marshal+unmarshal、canonicalJSON 的 marshal→decode→marshal、
// 与落盘 marshal），改为浅拷贝 + 复用已算出的 checksum。这类改动**只允许
// 变快，不允许变字节**：写出的 record 字节决定了磁盘上与 .bak 里已有
// record 的可读性，一旦漂移，升级后旧会话全部读不出来。所以这里把
// 「固定输入 → 固定字节」钉死。
//
// 若本测试失败，先判断是不是**有意**改了 record 的序列化格式：
//   - 有意改：这是 schema 变更，必须同时确认读取路径能读旧文件
//     （checksumMatchesStoredPayload 的 v1 回退分支），然后更新下面的期望值。
//   - 无意改：说明提交路径引入/删除了多余序列化，回到 CommitRecord 检查。
//
// 2026-10-18：checksum 口径从「规范化后 sha256」（sha256:）改为「磁盘字节
// sha256」（sha256b:），提交路径少了两遍全量 JSON 往返（90MB 上 1250ms → 35ms）。
// 期望值随之更新；旧口径的读取兼容由 TestLegacyChecksumStillReadable 覆盖。
//
// 生成新期望值：临时打印 commit 后的 r.Checksum 与文件 sha256。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"testing"
	"time"
)

func TestCommitRecordGoldenBytes(t *testing.T) {
	const (
		wantChecksum = "sha256b:1980b821adf3d082cacae7df321a8d9083f542dfcd32bef515015cfb03bf93e5"
		wantFileSHA  = "2bd60cb9289a7751cd00e60984125239fc64fe0f43a32664144706b6b865c010"
		wantSize     = 824
	)

	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := testSessionRecord(t, "workspace-a", "session-1", 7)
	// 固定时间戳：UpdatedAt 参与 checksum，用 time.Now() 会让字节不可比。
	record.UpdatedAt = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	record.Checksum = ""

	if err := storage.CommitRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if record.Checksum != wantChecksum {
		t.Errorf("checksum drifted:\n got %s\nwant %s", record.Checksum, wantChecksum)
	}

	path, err := storage.RecordPath("workspace-a", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != wantSize {
		t.Errorf("record size drifted: got %d want %d", len(data), wantSize)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != wantFileSHA {
		t.Errorf("on-disk bytes drifted:\n got %s\nwant %s", got, wantFileSHA)
	}
}

// 同一输入下「调用方自带 checksum」与「不带 checksum」两条分支必须产出
// 同一个 durable 结果：CommitRecord 里复用 expected 的那条捷径改了这条路径，
// 一旦两边算出的值不同就会写进不同字节。
func TestCommitRecordChecksumBranchesAgree(t *testing.T) {
	dir := t.TempDir()
	build := func() *SessionRecord {
		r := testSessionRecord(t, "workspace-a", "session-1", 3)
		r.UpdatedAt = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
		r.Checksum = ""
		return r
	}

	// 分支 1：调用方不带 checksum（走 else 计算分支）。
	s1, err := NewFileStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	r1 := build()
	if err := s1.CommitRecord(context.Background(), r1); err != nil {
		t.Fatal(err)
	}

	// 分支 2：调用方自带（正确的）checksum，走「校验后复用 expected」分支。
	s2, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r2 := build()
	if err := s2.CommitRecord(context.Background(), r2); err != nil {
		t.Fatal(err)
	}
	r3 := build()
	r3.Checksum = r2.Checksum // 与 r2 的 payload 完全一致
	if err := s2.CommitRecord(context.Background(), r3); err != nil {
		t.Fatalf("commit with precomputed checksum: %v", err)
	}

	if r1.Checksum != r3.Checksum {
		t.Fatalf("checksum branches disagree:\n no-checksum  = %s\n precomputed  = %s", r1.Checksum, r3.Checksum)
	}

	p1, _ := s1.RecordPath("workspace-a", "session-1")
	p2, _ := s2.RecordPath("workspace-a", "session-1")
	b1, err := os.ReadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := os.ReadFile(p2)
	if err != nil {
		t.Fatal(err)
	}
	if string(b1) != string(b2) {
		t.Fatal("durable bytes differ between checksum branches")
	}
}
