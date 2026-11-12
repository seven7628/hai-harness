package session

// sidecar 逐块压缩的回归（2026-11-11，CHECKPOINT_V3_DESIGN §6 P4）。
//
// 四条不变量：
//  1. 写进去的**逻辑内容** == 读出来的字节（逐字节）；
//  2. **摘要口径不变**（算逻辑内容）⇒ 压缩不改变 blocksRefDoc.Sum；
//  3. 各种打开路径（直读 / 索引丢失全量重建 / compact 重写 / mmap）都能读回；
//  4. 旧记录（未压缩）与新记录（压缩）**混在同一个文件里**也能读。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// compressibleBlock 造一个**可压缩**的块（重复内容 → zlib 能压掉一大半）。
func compressibleBlock(id int, n int) []byte {
	return []byte(fmt.Sprintf(`{"id":%d,"kind":"tool","status":"done","text":%q}`,
		id, strings.Repeat("compressible-content-", n)))
}

// TestCompressBlockRoundTrip 压缩/解压本身。
func TestCompressBlockRoundTrip(t *testing.T) {
	payload := compressibleBlock(1, 2000)
	c, ok := compressBlock(payload)
	if !ok {
		t.Fatal("应当压缩成功")
	}
	if len(c) >= len(payload) {
		t.Fatalf("压缩后没变小：%d → %d", len(payload), len(c))
	}
	got, err := decompressBlock(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatal("解压后内容不一致")
	}
	// 太小的块不压（省 CPU）。
	if _, ok := compressBlock([]byte(`{"id":1}`)); ok {
		t.Fatal("小块不该压")
	}
	// 压不动的块不压。
	noise := []byte(`{"id":2,"text":"` + incompressible(4096) + `"}`)
	if _, ok := compressBlock(noise); ok {
		t.Log("噪声内容居然被压小了（zlib 头部开销之外的意外）")
	}
}

// TestCompressedBlockRoundTripsThroughStore 写进去 → 读回来逐字节一致。
func TestCompressedBlockRoundTripsThroughStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.record.json.blocks")
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	payload := compressibleBlock(1, 4000)
	if err := bs.put("1", payload); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	// 落盘确实变小了。
	if n := fileSize(t, path); n > int64(len(payload)) {
		t.Fatalf("落盘 %d 字节 ≥ 逻辑 %d —— 没压缩？", n, len(payload))
	}
	// 索引项应带压缩标志，摘要应是**逻辑内容**的摘要。
	ref := bs.index["1"]
	if !isCompressed(ref.kind) {
		t.Fatal("索引项没带压缩标志")
	}
	if ref.hash != payloadDigest(payload) {
		t.Fatal("索引摘要不是逻辑内容的摘要（压缩会破坏 record 的 sum）")
	}
	got, ok := bs.readAndVerify("1")
	if !ok {
		t.Fatal("读不回（校验失败）")
	}
	if string(got) != string(payload) {
		t.Fatal("读回内容不一致")
	}
}

// TestCompressedBlockSurvivesReopen 索引丢失 → 全量重建 → 仍能读回。
//
// 这条钉住「重建路径要从**解压后的前缀**取块类型码」——
// 压缩字节里直接找 `"kind":"` 是找不到的（块类型码会被索引丢掉 ⇒
// 窗口恢复的卡片摘取失效）。
func TestCompressedBlockSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.record.json.blocks")
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// 注意：必须用**无长数组**的 agent 卡 —— 有长数组的块会走**分片**而不是压缩
	// （putAuto 优先分片）。这里要测的是压缩块的重建路径。
	card := []byte(fmt.Sprintf(`{"kind":"agent","id":9000,"run_id":"r1","status":"running","label":%q,"items":[]}`,
		strings.Repeat("compressible-label-", 400)))
	if err := bs.put("9000", card); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}
	// 删掉索引 → 下次打开走全量重建。
	if err := removeIfExists(path + ".idx"); err != nil {
		t.Fatal(err)
	}
	bs2, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs2.Close()
	ref := bs2.index["9000"]
	if !isCompressed(ref.kind) {
		t.Fatal("重建后丢了压缩标志")
	}
	if !hasCardKind(ref.kind) {
		t.Fatalf("重建后块类型码丢了（kind=%#x）—— 窗口恢复的卡片摘取会失效", ref.kind)
	}
	got, ok := bs2.readAndVerify("9000")
	if !ok || string(got) != string(card) {
		t.Fatalf("重建后读不回：ok=%v len=%d want=%d", ok, len(got), len(card))
	}
}

// TestCompressedBlocksSurviveCompact compact 重写后内容不变、体积仍小。
func TestCompressedBlocksSurviveCompact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.record.json.blocks")
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	payloads := map[string][]byte{}
	for i := 1; i <= 20; i++ {
		p := compressibleBlock(i, 3000)
		payloads[fmt.Sprintf("%d", i)] = p
		if err := bs.put(fmt.Sprintf("%d", i), p); err != nil {
			t.Fatal(err)
		}
	}
	// 同一 id 多写几版 → 产生垃圾。
	for i := 0; i < 5; i++ {
		p := compressibleBlock(1, 3000+i)
		payloads["1"] = p
		if err := bs.put("1", p); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	before := fileSize(t, path)

	if err := bs.compact(); err != nil {
		t.Fatal(err)
	}
	after := fileSize(t, path)
	t.Logf("compact: %d → %d 字节", before, after)
	if after >= before {
		t.Fatalf("compact 没收回垃圾：%d → %d", before, after)
	}
	// 内容必须逐字节不变。
	for id, want := range payloads {
		got, ok := bs.readAndVerify(id)
		if !ok {
			t.Fatalf("compact 后 %s 读不回", id)
		}
		if string(got) != string(want) {
			t.Fatalf("compact 后 %s 内容变了", id)
		}
	}
}

// TestCompressedAndPlainCoexist 同一文件里新旧混存（旧记录未压缩）。
func TestCompressedAndPlainCoexist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.record.json.blocks")

	// 先关掉压缩写一块（模拟旧记录），再打开压缩写另一块。
	blockCompressEnabled = false
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	plain := compressibleBlock(1, 3000) // 内容可压缩，但**关掉压缩时**写
	if err := bs.put("1", plain); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	blockCompressEnabled = true
	defer func() { blockCompressEnabled = true }()

	packed := compressibleBlock(2, 3000)
	if err := bs.put("2", packed); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if isCompressed(bs.index["1"].kind) {
		t.Fatal("旧块不该带压缩标志")
	}
	if !isCompressed(bs.index["2"].kind) {
		t.Fatal("新块该带压缩标志")
	}
	for id, want := range map[string][]byte{"1": plain, "2": packed} {
		got, ok := bs.readAndVerify(id)
		if !ok || string(got) != string(want) {
			t.Fatalf("%s 读回不一致（ok=%v）", id, ok)
		}
	}
	// 全量重建后仍要对。
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := removeIfExists(path + ".idx"); err != nil {
		t.Fatal(err)
	}
	bs2, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs2.Close()
	for id, want := range map[string][]byte{"1": plain, "2": packed} {
		got, ok := bs2.readAndVerify(id)
		if !ok || string(got) != string(want) {
			t.Fatalf("重建后 %s 读回不一致（ok=%v）", id, ok)
		}
	}
}

// TestCompressedBlockThroughMappedView mmap 热读路径（bridge 冷恢复走它）。
func TestCompressedBlockThroughMappedView(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.record.json.blocks")
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	payload := compressibleBlock(1, 5000)
	if err := bs.put("1", payload); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}
	m, err := openMappedBlocks(path)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	got, sum, ok := m.span("1")
	if !ok {
		t.Fatal("mmap 读不回压缩块")
	}
	if string(got) != string(payload) {
		t.Fatalf("mmap 读回内容不一致（%d vs %d 字节）", len(got), len(payload))
	}
	if sum != payloadDigest(payload) {
		t.Fatal("mmap 的摘要不是逻辑内容摘要")
	}
}

// TestCompressionDoesNotChangeSetSum 压缩不改变 record 的块集合摘要。
//
// 这是「存量清扫只动 sidecar、不碰 record」的前提：
// 同一批块，压缩前/后算出的 Sum 必须**完全相同**。
func TestCompressionDoesNotChangeSetSum(t *testing.T) {
	dir := t.TempDir()
	ids := []string{"1", "2", "3"}
	var payloads [][]byte
	for i := range ids {
		payloads = append(payloads, compressibleBlock(i+1, 3000))
	}

	sumOf := func(compress bool) string {
		blockCompressEnabled = compress
		p := filepath.Join(dir, fmt.Sprintf("x%d.record.json.blocks", boolToInt(compress)))
		bs, err := openBlockStore(p)
		if err != nil {
			t.Fatal(err)
		}
		defer bs.Close()
		for i, id := range ids {
			if err := bs.put(id, payloads[i]); err != nil {
				t.Fatal(err)
			}
		}
		if err := bs.flush(); err != nil {
			t.Fatal(err)
		}
		sum, err := blocksSetSum(bs, ids)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	plain := sumOf(false)
	packed := sumOf(true)
	blockCompressEnabled = true
	if plain != packed {
		t.Fatalf("压缩改变了块集合摘要：\n不压缩=%s\n压缩  =%s", plain, packed)
	}
	t.Logf("两种形态的 Sum 一致: %s", plain)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestCompressedRecordEndToEnd 走完整的「提交 → LoadRecord」链路。
func TestCompressedRecordEndToEnd(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	blocks := make([]string, 0, 10)
	for i := 1; i <= 10; i++ {
		blocks = append(blocks, string(compressibleBlock(i, 3000)))
	}
	view := []byte(`{"schema_version":1,"blocks":[` + strings.Join(blocks, ",") + `],"run_nodes":[],"turns":[]}`)
	rec := NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, view)
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatalf("LoadRecord 失败（压缩块读不回？）: %v", err)
	}
	spans := labSpans(t, loaded.ViewCheckpoint)
	if len(spans) != 10 {
		t.Fatalf("块数=%d want 10", len(spans))
	}
	for i, span := range spans {
		if string(span) != blocks[i] {
			t.Fatalf("第 %d 块内容不一致", i)
		}
	}
}

// removeIfExists 删文件（不存在不算错）。
func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// TestSweepBlockStoresCompressesExisting 存量 sidecar 被压一遍、内容不变、幂等。
func TestSweepBlockStoresCompressesExisting(t *testing.T) {
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
	want := map[string][]byte{}
	for i := 1; i <= 10; i++ {
		p := compressibleBlock(i, 2000)
		want[fmt.Sprintf("%d", i)] = p
		if err := bs.put(fmt.Sprintf("%d", i), p); err != nil {
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
	defer func() { blockCompressEnabled = true }()

	before := fileSize(t, path)
	res, err := SweepBlockStores(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	after := fileSize(t, path)
	t.Logf("清扫: %d 个文件（%d 个重写）%d → %d 字节，压缩 %d/%d 块",
		res.Files, res.Rewritten, before, after, res.Blocks, res.BlocksTotal)
	if res.Rewritten != 1 {
		t.Fatalf("应重写 1 个文件，实际 %d", res.Rewritten)
	}
	if after >= before {
		t.Fatalf("没变小：%d → %d", before, after)
	}
	// 内容逐字节不变。
	bs2, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for id, w := range want {
		got, ok := bs2.readAndVerify(id)
		if !ok || string(got) != string(w) {
			t.Fatalf("清扫后 %s 读回不一致", id)
		}
	}
	bs2.Close()

	// 幂等：再扫一次不重写。
	res2, err := SweepBlockStores(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Rewritten != 0 {
		t.Fatalf("二次清扫不该重写，实际 %d", res2.Rewritten)
	}
}
