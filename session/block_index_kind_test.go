package session

import (
	"crypto/sha256"
	"encoding/binary"
	"path/filepath"
	"testing"
)

// TestIndexCarriesBlockKind 索引必须带 kind：窗口恢复靠它只解码「卡片类」块，
// 而不必为了窗口外的子智能体卡片把 209MB 全解一遍。
func TestIndexCarriesBlockKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.blocks")
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	payloads := map[string]string{
		"1": `{"kind":"assistant","id":1,"text":"hi"}`,
		"2": `{"kind":"tool","id":2,"name":"bash"}`,
		"3": `{"kind":"agent","id":3,"run_id":"sub"}`,
		"4": `{"kind":"async_task","id":4,"task_id":"t"}`,
		"5": `{"kind":"divider","id":5,"divider_reason":"clear"}`,
	}
	for id, p := range payloads {
		if err := bs.put(id, []byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开：走索引命中路径（不扫 sidecar），kind 必须还在。
	bs2, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs2.Close()
	want := map[string]uint8{
		"1": blockKindCodeAssistant,
		"2": blockKindCodeTool,
		"3": blockKindCodeAgent,
		"4": blockKindCodeAsyncTask,
		"5": blockKindCodeDivider,
	}
	for id, w := range want {
		ref, ok := bs2.index[id]
		if !ok {
			t.Fatalf("块 %s 不在索引里", id)
		}
		if ref.kind != w {
			t.Fatalf("块 %s kind want %d got %d", id, w, ref.kind)
		}
	}
	// 卡片类判定
	if !hasCardKind(bs2.index["3"].kind) || !hasCardKind(bs2.index["4"].kind) {
		t.Fatal("agent/async_task 应被判为卡片类")
	}
	if hasCardKind(bs2.index["1"].kind) {
		t.Fatal("assistant 不应被判为卡片类")
	}
}

// TestDecodeLegacyIndexWithoutKind HBIDX001（32 字节/条、无 kind）必须继续可读，
// 否则升级后所有既有会话都要重建索引（真实 1.8GB sidecar 要扫 1s+）。
func TestDecodeLegacyIndexWithoutKind(t *testing.T) {
	entries := []indexEntry{
		{id: 1, off: 100, n: 10, digest: [8]byte{1, 2, 3}},
		{id: 2, off: 200, n: 20, digest: [8]byte{4, 5, 6}},
	}
	buf := make([]byte, 0, indexHeadSize+len(entries)*indexEntrySizeV1+indexTailSize)
	buf = append(buf, indexMagicV1...)
	var n4 [4]byte
	binary.BigEndian.PutUint32(n4[:], uint32(len(entries)))
	buf = append(buf, n4[:]...)
	var n8 [8]byte
	binary.BigEndian.PutUint64(n8[:], 300) // covered
	buf = append(buf, n8[:]...)
	buf = append(buf, make([]byte, 32)...) // chain
	binary.BigEndian.PutUint64(n8[:], 2)   // records
	buf = append(buf, n8[:]...)
	binary.BigEndian.PutUint64(n8[:], 0) // stale
	buf = append(buf, n8[:]...)
	for _, e := range entries {
		binary.BigEndian.PutUint64(n8[:], e.id)
		buf = append(buf, n8[:]...)
		binary.BigEndian.PutUint64(n8[:], uint64(e.off))
		buf = append(buf, n8[:]...)
		binary.BigEndian.PutUint64(n8[:], uint64(e.n))
		buf = append(buf, n8[:]...)
		buf = append(buf, e.digest[:]...)
	}
	sum := sha256.Sum256(buf[indexHeadSize:])
	buf = append(buf, sum[:indexTailSize]...)

	got, covered, _, records, _, ok := decodeBlockIndex(buf)
	if !ok {
		t.Fatal("旧版索引应可解码")
	}
	if covered != 300 || records != 2 || len(got) != 2 {
		t.Fatalf("头部字段不对: covered=%d records=%d n=%d", covered, records, len(got))
	}
	if got[0].id != 1 || got[1].off != 200 {
		t.Fatalf("条目解析不对: %+v", got)
	}
	if got[0].kind != blockKindUnknown {
		t.Fatalf("旧索引的 kind 应为未知(0)，得 %d", got[0].kind)
	}
}
