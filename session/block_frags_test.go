package session

// 分片存储的端到端回归（2026-11-11，CHECKPOINT_V3_DESIGN §6.2）。
//
// 分片是**存储层**的能力：切分、拼回、diff 全部在 session 包内完成，
// 上层（reducer / bridge / 前端）一行都不用改。因此这些测试要保证：
//
//  1. 写进去的字节 = 读出来的字节（逐字节）；
//  2. 摘要口径不变（索引项存逻辑摘要 → holdsSame / indexSetSum 照常工作）；
//  3. mmap 热读路径（bridge 冷恢复走的就是它）能正确展开分片块；
//  4. 全量重建（索引丢失）与 compact 之后内容不变。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// longAgentCard 造一张带**长 items 数组**的 agent 卡片（会被分片）。
func longAgentCard(id int, nItems, itemBytes int) []byte {
	items := make([]string, 0, nItems)
	for i := 0; i < nItems; i++ {
		items = append(items, fmt.Sprintf(`{"kind":"tool","tool_id":"c%d","name":"bash","args":"%s"}`,
			i, strings.Repeat("v", itemBytes)))
	}
	return []byte(fmt.Sprintf(
		`{"kind":"agent","id":%d,"run_id":"r1","status":"running","label":"干活","items":[%s],"items_trimmed":0}`,
		id, strings.Join(items, ",")))
}

// TestFragmentedCardRoundTripsThroughRecord 走完整的「提交 → 读回」链路。
func TestFragmentedCardRoundTripsThroughRecord(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"

	card := longAgentCard(1, 60, 4*1024) // ≈250KB → 分片
	blocksJSON, err := json.Marshal([]map[string]any{{"kind": "assistant", "id": 2, "text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	// view 里的 blocks 必须与提交时**逐字节一致**才能验证分片还原。
	view := []byte(fmt.Sprintf(
		`{"schema_version":1,"blocks":[%s,%s],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`,
		blocksJSON[1:len(blocksJSON)-1], card))

	rec := NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, view)
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	spans := labSpans(t, loaded.ViewCheckpoint)
	if len(spans) != 2 {
		t.Fatalf("块数=%d want 2", len(spans))
	}
	if string(spans[0]) != string(card) {
		t.Fatalf("分片块读回不一致：%d vs %d 字节", len(spans[0]), len(card))
	}
}

// TestFragmentedCardThroughMappedView mmap 热读路径：bridge 冷恢复用的是它。
//
// 分片块在文件里**不连续**（片段散落），映射路径必须按目录拼起来 ——
// 这是唯一会丢掉零拷贝的块类型（约占全部块的 1.7%）。
func TestFragmentedCardThroughMappedView(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"

	card := longAgentCard(1, 60, 4*1024)
	blocksJSON, err := json.Marshal([]map[string]any{{"kind": "assistant", "id": 2, "text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	view := []byte(fmt.Sprintf(
		`{"schema_version":1,"blocks":[%s,%s],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`,
		blocksJSON[1:len(blocksJSON)-1], card))
	rec := NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, view)
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.LoadRecordForView(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.ReleaseView()
	if len(loaded.LoadedBlockSpans) != 2 {
		t.Fatalf("spans=%d want 2", len(loaded.LoadedBlockSpans))
	}
	// 两个 span 里必有一个是那张分片卡片 → 必须逐字节还原。
	found := false
	for i, span := range loaded.LoadedBlockSpans {
		// 惰性解压：压缩块的 Raw 是压缩字节，先取逻辑内容。
		logical, lerr := span.Logical()
		if lerr != nil {
			t.Fatal(lerr)
		}
		if string(logical) == string(card) {
			found = true
			if d := PayloadDigest(logical); d != loaded.LoadedBlockHashes[i] {
				t.Fatalf("分片块的摘要不匹配（索引存的是逻辑摘要）")
			}
		}
	}
	if !found {
		t.Fatal("映射路径没有还原出分片卡片的原始字节")
	}
}

// TestFragmentedCardSurvivesIndexLoss 索引丢失 → 全量重建 → 内容不变。
func TestFragmentedCardSurvivesIndexLoss(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	card := longAgentCard(1, 60, 4*1024)
	view := []byte(fmt.Sprintf(
		`{"schema_version":1,"blocks":[%s],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`, card))
	if err := store.CommitRecord(context.Background(), NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, view)); err != nil {
		t.Fatal(err)
	}
	// 删掉索引 → 下次打开走全量扫描重建
	sidecar := filepath.Join(root, "sessions", wsKey, sid+".record.json.blocks")
	if err := os.Remove(sidecar + ".idx"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	loaded, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	spans := labSpans(t, loaded.ViewCheckpoint)
	if len(spans) != 1 || string(spans[0]) != string(card) {
		t.Fatalf("全量重建后内容不一致：%d 块", len(spans))
	}
}

// TestFragmentedCardSurvivesRecommit 连续提交：第二版只多一条 item 时，
// 读回必须是**第二版**（片段 diff 没写错）。
func TestFragmentedCardSurvivesRecommit(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStorage(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	mkView := func(card []byte) []byte {
		return []byte(fmt.Sprintf(
			`{"schema_version":1,"blocks":[%s],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`, card))
	}
	card1 := longAgentCard(1, 60, 4*1024)
	if err := store.CommitRecord(context.Background(), NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, mkView(card1))); err != nil {
		t.Fatal(err)
	}
	// 第二版：多一条 item（生产里 running 卡片就是这样长的）
	card2 := append(append([]byte(nil), card1[:len(card1)-len(`],"items_trimmed":0}`)]...),
		[]byte(`,{"kind":"tool","tool_id":"new","name":"bash","args":"{}"}],"items_trimmed":0}`)...)
	if err := store.CommitRecord(context.Background(), NewSessionRecord(wsKey, sid, 2, 2, 0, State{}, mkView(card2))); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	spans := labSpans(t, loaded.ViewCheckpoint)
	if len(spans) != 1 {
		t.Fatalf("块数=%d want 1", len(spans))
	}
	if string(spans[0]) != string(card2) {
		t.Fatalf("第二版读回不一致：%d vs %d 字节", len(spans[0]), len(card2))
	}
}

// TestFragDirEncodeParseRoundTrip 目录项的编解码必须严格对拍。
//
// 2026-11-11 的教训：编码按 24 字节写一条（key4+off8+len4+digest8），
// 校验却按 28 字节算 → parseFragDir 永远失败。
// 而这个 bug 只在「索引命中后重新读目录」这条路径上暴露
// （直读路径命中内存缓存）→ 必须单独对拍。
func TestFragDirEncodeParseRoundTrip(t *testing.T) {
	items := make([]fragItem, 0, 62)
	for i := 0; i < 62; i++ {
		it := fragItem{key: uint32(i), off: int64(1000 + i*37), n: int32(4000 + i)}
		it.hash = payloadDigest([]byte(fmt.Sprintf("frag-%d", i)))
		items = append(items, it)
	}
	dir := &fragDir{kind: 4, total: 123456, logical: payloadDigest([]byte("logical")), items: items}
	enc := encodeFragDir(dir)
	got, ok := parseFragDir(enc)
	if !ok {
		t.Fatalf("目录解析失败（编码 %d 字节，count=%d）", len(enc), len(items))
	}
	if got.kind != dir.kind || got.total != dir.total || got.logical != dir.logical {
		t.Fatalf("头部不对: kind=%d total=%d", got.kind, got.total)
	}
	if len(got.items) != len(items) {
		t.Fatalf("片段数 %d != %d", len(got.items), len(items))
	}
	for i := range items {
		if got.items[i] != items[i] {
			t.Fatalf("第 %d 项不对: %+v != %+v", i, got.items[i], items[i])
		}
	}
	// 截断/过短必须被识别（而不是越界 panic）
	if _, ok := parseFragDir(enc[:len(enc)-1]); ok {
		t.Fatal("截断的目录不应解析成功")
	}
	if _, ok := parseFragDir(enc[:5]); ok {
		t.Fatal("过短的目录不应解析成功")
	}
}
