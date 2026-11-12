package session

// 提交一致性回归：这三条钉住的是「checkpoint 的持久化语义」，不是性能。
//
// 背景（真实故障，2026-11-08）：bridge 的 checkpoint 提交有 30s 预算，而预算
// 到期时**sidecar 已经 flush、record 还没换**。旧实现把这个中间态留在盘上，
// 并且读路径要求「record 的块集合 == sidecar 索引集合」严格相等，于是：
//
//	一次超时 → 磁盘上 record 与 sidecar 不再匹配 → 会话再也读不出来。
//
// 同一个洞还让 .bak 变成一个永远读不出来的摆设（它的块集合必然比当前 sidecar
// 旧）。下面三条测试分别钉住：超时不再产生撕裂、compact 按字节回收、.bak 可用。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testBlock 生成一个终态 block（不含 running 标记 → 内容不变时不重写）。
func testBlock(id int, text string) string {
	return fmt.Sprintf(`{"id":%d,"kind":"tool","status":"done","text":%q}`, id, text)
}

// testView 用现成的 block 字面量拼一个合法的 view_checkpoint。
func testView(blocks ...string) []byte {
	return []byte(`{"schema_version":1,"turns":[],"blocks":[` + strings.Join(blocks, ",") + `],"run_nodes":[]}`)
}

// TestCommitHonorsDeadlineBeforeWritingOnly 钉住「点不可返回」语义：
//
//   - 预算在写入前就到 → 直接失败，磁盘不留任何痕迹；
//   - 预算在写入过程中到期 → **必须把本次提交写完**（record 与 sidecar 一致），
//     不能停在「sidecar 已落、record 未换」的撕裂态。
//
// 旧实现两处都在返回 ctx.Err()，第二处会留下撕裂态。
func TestCommitHonorsDeadlineBeforeWritingOnly(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, sid := "ws-deadline", "s-deadline"

	// 规模做够大，让 sidecar 阶段确实跨过 deadline。
	blocks := make([]string, 0, 4000)
	for i := 1; i <= 4000; i++ {
		blocks = append(blocks, testBlock(i, strings.Repeat("x", 2048)))
	}
	view := testView(blocks...)

	// 1) 写入前就已到期：必须失败且不留文件。
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := store.CommitRecord(expired, NewSessionRecord(ws, sid, 1, 1, 0, State{}, view)); err == nil {
		t.Fatal("已到期的 ctx 仍提交成功")
	}
	path, err := store.RecordPath(ws, sid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("已到期的 ctx 在磁盘上留下了 record")
	}

	// 先提交一版基线（小载荷，正常完成）。
	small := testView(testBlock(1, "baseline"))
	if err := store.CommitRecord(context.Background(), NewSessionRecord(ws, sid, 1, 1, 0, State{}, small)); err != nil {
		t.Fatal(err)
	}
	// 2) 写入过程中到期：无论如何都必须自洽（旧实现在这里留下撕裂态）。
	short, cancel2 := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel2()
	rec := NewSessionRecord(ws, sid, 2, 2, 0, State{}, view)
	_ = store.CommitRecord(short, rec) // 返回值允许是 ctx 错误，但盘上必须自洽

	got, lerr := store.LoadRecord(context.Background(), ws, sid)
	if lerr != nil {
		t.Fatalf("提交在写入中途超时后会话不可读（撕裂态）: %v", lerr)
	}
	if len(got.ViewCheckpoint) == 0 {
		t.Fatal("读回空 view")
	}
	if got.Revision != 1 && got.Revision != 2 {
		t.Fatalf("revision 异常: %d", got.Revision)
	}
	// 自洽的另一半：新记录的引用必须能被解开（sidecar 与 record 匹配）。
	if _, err := resolveBlocksRef(got.ViewCheckpoint, path); err != nil {
		t.Fatalf("record 与其 sidecar 不匹配: %v", err)
	}
}

// TestCompactTriggeredByBytesNotRecords 钉住 compact 判据按**字节**：
// 真实会话里 94% 的体积是废弃的旧版本，但废弃**条数**只有 18%（32000 条里
// 7000 条）。按条数判 → 3.45GB 的文件永不回收。
func TestCompactTriggeredByBytesNotRecords(t *testing.T) {
	dir := t.TempDir()
	storePath := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	// 200 个小块各写一次（占条数的 99%，占体积的很小一部分）。
	for i := 1; i <= 200; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte(`{"id":`+fmt.Sprintf("%d", i)+`,"kind":"tool","status":"done","text":"small"}`)); err != nil {
			t.Fatal(err)
		}
	}
	// 一个 1MB 的大块写 3 次：条数占比 <1%，字节占比 >60%。
	// 内容必须**不可压缩**（见 incompressible 的注释）：这个测试量的是
	// 「按字节占比触发 compact」这条判据，压缩会把字节量改掉 ⇒ 量不到判据。
	big := `{"id":1000000,"kind":"agent","status":"done","text":"` + incompressible(1<<20) + `"}`
	for i := 0; i < 3; i++ {
		if err := bs.put("1000000", []byte(big+" ")); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}

	recRatio := float64(bs.staleRecords) / float64(bs.recordsWritten)
	if recRatio > compactThreshold {
		t.Fatalf("测试构型不对：废弃条数占比 %.1f%% 已超阈值", recRatio*100)
	}
	if !bs.needsCompactAt(1 << 20) {
		t.Fatalf("废弃字节占比 %.1f%% 却没触发 compact（判据只看条数）",
			bs.staleByteRatio()*100)
	}
}

// TestBlocksRefResolvesNamedStore 钉住引用里的 store 名必须被尊重。
//
// 旧实现用 `recordPath + ".blocks"` 推导 sidecar，忽略 ref.Store —— 于是读
// `<sid>.record.json.bak` 时会去找 `.bak.blocks`（一个空壳），块数 0 vs N，
// .bak 永远读不出来。
func TestBlocksRefResolvesNamedStore(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, sid := "ws-store", "s-store"
	view := testView(testBlock(1, "a"), testBlock(2, "b"))
	if err := store.CommitRecord(context.Background(), NewSessionRecord(ws, sid, 1, 1, 0, State{}, view)); err != nil {
		t.Fatal(err)
	}
	path, err := store.RecordPath(ws, sid)
	if err != nil {
		t.Fatal(err)
	}

	// 把 sidecar 改名成另一个 store（仍在同目录），引用里记下新名字。
	renamed := path + ".other.blocks"
	if err := os.Rename(path+".blocks", renamed); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".blocks.idx", renamed+".idx"); err != nil {
		t.Fatal(err)
	}
	bs, err := openBlockStore(renamed)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := blocksSetSum(bs, []string{"1", "2"})
	if err != nil {
		t.Fatal(err)
	}
	count := bs.Len()
	ids, err := sortedIDNums([]string{"1", "2"})
	if err != nil {
		t.Fatal(err)
	}
	_ = bs.Close()

	ref := blocksRefDoc{Store: filepath.Base(renamed), Sum: sum, Count: count, Schema: 1, IDs: ids}
	patched, err := replaceJSONField(view, "blocks", mustJSON(t, map[string]blocksRefDoc{blocksRefKey: ref}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveBlocksRef(patched, path)
	if err != nil {
		t.Fatalf("引用里指定的 store 没有被尊重: %v", err)
	}
	if !strings.Contains(string(got), `"text":"a"`) || !strings.Contains(string(got), `"text":"b"`) {
		t.Fatalf("还原出的 blocks 不完整: %s", truncateForLog(got))
	}
}

// TestBakRecordReadableAfterAppend 钉住 .bak 在「后续提交只追加了新块」时可用。
//
// 旧实现要求引用集合与 sidecar 索引集合**完全相等**，追加一个新块就让 .bak
// 失效；而 .bak 的存在意义恰恰就是 formal 损坏时能读回来。
func TestBakRecordReadableAfterAppend(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, sid := "ws-bak", "s-bak"
	v1 := testView(testBlock(1, "one"), testBlock(2, "two"))
	if err := store.CommitRecord(context.Background(), NewSessionRecord(ws, sid, 1, 1, 0, State{}, v1)); err != nil {
		t.Fatal(err)
	}
	// 第二次提交：只追加一个新块（不重写已有块）。
	v2 := testView(testBlock(1, "one"), testBlock(2, "two"), testBlock(3, "three"))
	if err := store.CommitRecord(context.Background(), NewSessionRecord(ws, sid, 2, 2, 0, State{}, v2)); err != nil {
		t.Fatal(err)
	}

	path, err := store.RecordPath(ws, sid)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟 formal 损坏：截断成垃圾，逼迫走 .bak。
	if err := os.WriteFile(path, []byte("{truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadRecord(context.Background(), ws, sid)
	if err != nil {
		t.Fatalf(".bak 兜底读失败: %v", err)
	}
	if got.Revision != 1 {
		t.Fatalf("应恢复 revision 1，实际 %d", got.Revision)
	}
	if !strings.Contains(string(got.ViewCheckpoint), `"text":"one"`) {
		t.Fatalf("恢复内容不完整: %s", truncateForLog(got.ViewCheckpoint))
	}
}

// ---- 测试小工具 ----

// blocksRefFromView 取出 view 里的 blocks 引用对象。
func blocksRefFromView(t *testing.T, view []byte) blocksRefDoc {
	t.Helper()
	raw, ok := splitViewBlocks(view)
	if !ok {
		t.Fatalf("view 没有 blocks 字段: %s", truncateForLog(view))
	}
	var wrapper map[string]blocksRefDoc
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("解析 blocks 引用: %v", err)
	}
	ref, ok := wrapper[blocksRefKey]
	if !ok {
		t.Fatalf("blocks 不是引用形态: %s", truncateForLog(raw))
	}
	return ref
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func truncateForLog(b []byte) string {
	const n = 400
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// incompressible 造一段**不可压缩**的文本（伪随机字节）。
//
// 为什么需要（2026-11-11）：sidecar 起逐块压缩后（block_compress.go），
// 用 `strings.Repeat("x", n)` 造的内容会被压掉 —— 于是「按字节口径」的测试
// （追加语义、compact 的收益判据）量到的体积对不上。
//
// 这些测试的意图是**判据**而不是压缩器，所以它们必须用不可压缩的内容；
// 压缩本身另有专门的测试（block_compress_test.go）。
func incompressible(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/"
	b := make([]byte, n)
	seed := uint64(0x9E3779B97F4A7C15)
	for i := range b {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		b[i] = alphabet[seed&63]
	}
	return string(b)
}
