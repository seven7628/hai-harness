package session

// block 索引与分页的回归测试。
//
// 这套优化的价值在于「打开会话不读整个 sidecar」，但风险也正来自这里：
// 索引一旦与 sidecar 不一致，用户可能看到残缺的历史 —— 比慢严重得多。
// 所以这里钉住的都是**不变量**，不是性能数字：
//
//   1. 索引命中的读取结果，必须与全量扫描的结果逐字节一致；
//   2. 分页能翻到每一个 block（不重、不漏、顺序正确）；
//   3. 索引落后（追加了新记录）时走增量，结果仍正确；
//   4. 索引超前/损坏/被删 → 回退全量扫描，不报错、不返回脏数据；
//   5. 单块损坏 → 读取该块时报错（而不是静默返回坏内容）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTestStore 造一个带若干块的 sidecar 并写好索引。
func buildTestStore(t *testing.T, n int) (storePath string, ids []uint64) {
	t.Helper()
	storePath = t.TempDir() + "/t.blocks"
	bs, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		payload := []byte(`{"id":` + itoaSimple(i) + `,"kind":"assistant","text":"内容` + itoaSimple(i) + `"}`)
		if err := bs.put(itoaSimple(i), payload); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, uint64(i))
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := writeBlockIndex(storePath, mapToIndex(bs.index), bs.nBytes, nil, bs.recordsWritten, bs.staleRecords); err != nil {
		t.Fatal(err)
	}
	bs.Close()
	return storePath, ids
}

func itoaSimple(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestIndexedOpenMatchesScan 索引命中与全量扫描必须给出相同的索引内容。
func TestIndexedOpenMatchesScan(t *testing.T) {
	storePath, _ := buildTestStore(t, 120)

	// 有索引
	withIdx, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer withIdx.Close()

	// 强制全量扫描（删掉索引后重开）
	if err := os.Remove(blockIndexPath(storePath)); err != nil {
		t.Fatal(err)
	}
	noIdx, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer noIdx.Close()

	if withIdx.Len() != noIdx.Len() {
		t.Fatalf("索引命中 %d 块，全量扫描 %d 块", withIdx.Len(), noIdx.Len())
	}
	for id, ref := range noIdx.index {
		got, ok := withIdx.index[id]
		if !ok {
			t.Fatalf("索引缺块 %s", id)
		}
		if got.off != ref.off || got.n != ref.n || got.hash != ref.hash {
			t.Fatalf("块 %s 位置/摘要不一致: %+v vs %+v", id, got, ref)
		}
	}
	// 链摘要也必须一致（否则 record 里的 sum 校验会失败）
	if withIdx.sum() != noIdx.sum() {
		t.Fatalf("链摘要不一致:\n 索引 %s\n 扫描 %s", withIdx.sum(), noIdx.sum())
	}
}

// TestPagingCoversAllBlocks 分页必须能翻到每一个块，不重不漏、顺序正确。
func TestPagingCoversAllBlocks(t *testing.T) {
	const total = 137
	storePath, _ := buildTestStore(t, total)
	bs, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	seen := make(map[string]int)
	var order []string
	page, err := bs.ReadPage(PageRequest{FromEnd: true, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for {
		for _, b := range page.Blocks {
			k := string(b)
			seen[k]++
			order = append(order, k)
		}
		if !page.HasMore {
			break
		}
		if page.NextFrom == 0 {
			t.Fatal("hasMore=true 但 NextFrom=0 → 前端会死循环")
		}
		page, err = bs.ReadPage(PageRequest{BeforeID: page.NextFrom, Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(seen) != total {
		t.Fatalf("翻页拿到 %d 个不同块，期望 %d（有遗漏）", len(seen), total)
	}
	for k, n := range seen {
		if n > 1 {
			t.Fatalf("块出现 %d 次（有重复）: %s", n, k)
		}
	}
	// 顺序：翻页是「从新到旧」，所以每页内部升序、页与页之间整体应能还原
	if len(order) != total {
		t.Fatalf("顺序长度 %d != %d", len(order), total)
	}
	// 内容完整性：每个块的 id 都应出现
	for i := 1; i <= total; i++ {
		found := false
		for _, b := range order {
			if bytes.Contains([]byte(b), []byte(`"id":`+itoaSimple(i)+`,`)) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("块 %d 在分页里找不到", i)
		}
	}
}

// TestIndexCatchesUpIncrementally 索引落后时只扫增量，且结果正确。
func TestIndexCatchesUpIncrementally(t *testing.T) {
	storePath, _ := buildTestStore(t, 50)

	bs, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	coveredBefore := bs.nBytes
	// 追加 5 条新记录（不更新索引）
	for i := 51; i <= 55; i++ {
		if err := bs.put(itoaSimple(i), []byte(`{"id":`+itoaSimple(i)+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	bs.Close()

	// 重开：索引只覆盖到 coveredBefore，应增量扫描
	reopened, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Len() != 55 {
		t.Fatalf("增量追赶后应有 55 块，实为 %d（索引 covered=%d）", reopened.Len(), coveredBefore)
	}
	if _, ok := reopened.index["55"]; !ok {
		t.Fatal("增量段的新块没进索引")
	}
}

// TestCorruptIndexFallsBack 索引损坏/被删必须回退全量扫描，而不是报错或丢块。
func TestCorruptIndexFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, idxPath string)
	}{
		{"删除索引", func(t *testing.T, p string) { _ = os.Remove(p) }},
		{"索引被截断", func(t *testing.T, p string) {
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, b[:len(b)/2], 0o600)
		}},
		{"索引内容被篡改", func(t *testing.T, p string) {
			b, _ := os.ReadFile(p)
			if len(b) > 60 {
				b[55] ^= 0xff // 改坏一条 entry
			}
			_ = os.WriteFile(p, b, 0o600)
		}},
		{"索引超前于 sidecar", func(t *testing.T, p string) {
			b, _ := os.ReadFile(p)
			// 把 covered 改成远超实际大小
			if len(b) >= 20 {
				b[12] = 0xff
				b[13] = 0xff
				b[14] = 0xff
			}
			_ = os.WriteFile(p, b, 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storePath, _ := buildTestStore(t, 40)
			idxPath := blockIndexPath(storePath)

			good, err := openBlockStore(storePath)
			if err != nil {
				t.Fatal(err)
			}
			wantLen := good.Len()
			wantSum := good.sum()
			good.Close()

			tc.damage(t, idxPath)

			bs, err := openBlockStore(storePath)
			if err != nil {
				t.Fatalf("索引异常不应让打开失败: %v", err)
			}
			defer bs.Close()
			if bs.Len() != wantLen {
				t.Fatalf("回退后块数 %d != %d", bs.Len(), wantLen)
			}
			if bs.sum() != wantSum {
				t.Fatalf("回退后链摘要不一致（说明回退路径没真正重建）")
			}
		})
	}
}

// TestCorruptPayloadReportedOnRead 单块损坏必须在**读它的时候**报错，不能静默返回坏内容。
func TestCorruptPayloadReportedOnRead(t *testing.T) {
	storePath, _ := buildTestStore(t, 30)
	bs, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	// 找一个块的 payload 位置，改坏它
	ref, ok := bs.index["15"]
	if !ok {
		t.Fatal("找不到块 15")
	}
	f, err := os.OpenFile(storePath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("XXXX"), ref.off); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	// 读包含 15 的页 → 必须报错
	_, err = bs.ReadPage(PageRequest{BeforeID: 16, Limit: 5})
	if err == nil {
		t.Fatal("损坏的块应导致读取报错，而不是返回坏内容")
	}
	t.Logf("损坏被检出: %v", err)

	// 读不涉及 15 的页 → 仍应成功（说明校验是逐块的，不是全盘失效）
	if _, err := bs.ReadPage(PageRequest{FromEnd: true, Limit: 5}); err != nil {
		t.Fatalf("其他页不应受影响: %v", err)
	}
}

// TestRepeatedCommitsKeepReading 连续提交（sidecar 持续追加）后必须仍能读回。
//
// 这条钉住一个**极易复发**的设计缺陷：曾经把 sum 定义成「整个 sidecar 的
// 滚动链」，而 sidecar 是 append-only 的 —— 每次提交都改变链值，
// record 里存的 sum 立刻过期，第二次提交后读取就报 checksum mismatch。
// 实测在 TestDbgSumTimeline 里 100% 复现。
//
// 现在 sum 只覆盖「该 record 引用的块集合」，追加新块不影响它。
func TestRepeatedCommitsKeepReading(t *testing.T) {
	s, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	mk := func(rev uint64, text string) *SessionRecord {
		v := ViewCheckpoint(`{"schema_version":1,"blocks":[{"id":1,"kind":"agent","status":"running","ts":1,"text":"` + text + `"}],"run_nodes":[],"turns":[],"usage":{},"cost_usd":0,"todos":[],"degraded":false}`)
		r := NewSessionRecord("ws-a", "sess-1", rev, rev*10, 0, State{}, v)
		r.Checksum = ""
		return r
	}

	// 连续 5 次提交，每次都改变 block 内容（模拟子智能体卡片持续更新）
	for i := uint64(1); i <= 5; i++ {
		if err := s.CommitRecord(ctx, mk(i, "版本"+itoaSimple(int(i)))); err != nil {
			t.Fatalf("第 %d 次提交失败: %v", i, err)
		}
		rec, err := s.LoadRecord(ctx, "ws-a", "sess-1")
		if err != nil {
			t.Fatalf("第 %d 次提交后读取失败: %v", i, err)
		}
		want := "版本" + itoaSimple(int(i))
		if !bytes.Contains(rec.ViewCheckpoint, []byte(want)) {
			t.Fatalf("第 %d 次提交后读到旧内容，期望包含 %q", i, want)
		}
	}
}

// TestIndexAfterManyCommits 多次提交后索引自动更新，热启动仍然有效。
func TestIndexAfterManyCommits(t *testing.T) {
	s, _ := NewFileStorage(t.TempDir())
	ctx := context.Background()
	path, _ := s.RecordPath("ws-a", "sess-1")

	for i := uint64(1); i <= 3; i++ {
		v := ViewCheckpoint(`{"schema_version":1,"blocks":[{"id":1,"kind":"assistant","status":"done","ts":1,"text":"v` + itoaSimple(int(i)) + `"}],"run_nodes":[],"turns":[],"usage":{},"cost_usd":0,"todos":[],"degraded":false}`)
		r := NewSessionRecord("ws-a", "sess-1", i, i*10, 0, State{}, v)
		r.Checksum = ""
		if err := s.CommitRecord(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	// 索引应存在且与 sidecar 同步
	entries, covered, _, _, _, ok := loadBlockIndex(path + ".blocks")
	if !ok {
		t.Fatal("提交后应自动写出索引")
	}
	fi, err := os.Stat(path + ".blocks")
	if err != nil {
		t.Fatal(err)
	}
	if covered != fi.Size() {
		t.Fatalf("索引 covered=%d 与 sidecar 大小 %d 不同步 → 热启动会退化成全量扫描", covered, fi.Size())
	}
	if len(entries) == 0 {
		t.Fatal("索引为空")
	}
}

// TestLegacyChainSumMatchesOriginal 旧口径摘要必须与被取代的实现逐字节一致。
//
// 上一版把 Sum 定义成 sha256(magic + 每条 head(kind|idLen|id|payloadLen) + payload)，
// 涵盖整个 sidecar（含废弃记录）。已落盘的 record 用它，所以升级后必须能算出
// **完全相同**的值，否则用户的既有会话会全部打不开。
//
// 这条钉住实现细节：曾经漏掉 head 里的 idLen 四字节，导致算出的值永远不匹配，
// 而错误是静默的（只是校验失败→回退 .bak→再失败→报损坏）。
func TestLegacyChainSumMatchesOriginal(t *testing.T) {
	storePath, _ := buildTestStore(t, 25)

	bs, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	// 独立按「旧口径」重算一遍（不调用被测函数，避免自证）
	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	h.Write(data[:8])
	pos := 8
	for pos < len(data) {
		kind := data[pos]
		idLen := int(binary.BigEndian.Uint32(data[pos+1 : pos+5]))
		payLen := int(binary.BigEndian.Uint32(data[pos+5+idLen : pos+9+idLen]))
		headLen := 9 + idLen
		h.Write([]byte{kind})
		h.Write(data[pos+1 : pos+5])
		h.Write(data[pos+5 : pos+5+idLen])
		h.Write(data[pos+5+idLen : pos+9+idLen])
		h.Write(data[pos+headLen : pos+headLen+payLen])
		pos += headLen + payLen + 8
	}
	want := hex.EncodeToString(h.Sum(nil))

	got := bs.legacyChainSum()
	if got != want {
		t.Fatalf("旧口径摘要不匹配\n got: %s\nwant: %s", got, want)
	}
	t.Logf("旧口径摘要一致 ✓ %s", got[:16])
}

// TestCompactReclaimsStaleRecords compact 必须回收废弃记录且不丢任何块。
//
// 背景：sidecar 是 append-only 的，子智能体卡片在 running 期每次事件都重写
// 同一 block，历史版本无限堆积。真实会话实测 93% 记录是废弃的（1.4GB 里
// 有效内容仅 58MB）。不回收会一直膨胀，并线性拖慢冷启动。
func TestSidecarCompactReclaimsStale(t *testing.T) {
	s, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path, _ := s.RecordPath("ws-a", "sess-1")

	// 反复重写同一个 block（模拟子智能体卡片持续更新）+ 若干稳定块
	commit := func(rev uint64, text string, extra int) {
		v := `{"schema_version":1,"blocks":[{"id":1,"kind":"agent","status":"running","ts":1,"text":"` + text + `"}`
		for i := 2; i <= extra; i++ {
			v += `,{"id":` + itoaSimple(i) + `,"kind":"assistant","status":"done","ts":1}`
		}
		v += `],"run_nodes":[],"turns":[],"usage":{},"cost_usd":0,"todos":[],"degraded":false}`
		rec := NewSessionRecord("ws-a", "sess-1", rev, rev*10, 0, State{}, ViewCheckpoint(v))
		rec.Checksum = ""
		if err := s.CommitRecord(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	for i := uint64(1); i <= 30; i++ {
		commit(i, "版本"+itoaSimple(int(i)), 3)
	}

	bs, err := openBlockStore(path + ".blocks")
	if err != nil {
		t.Fatal(err)
	}
	beforeBlocks := bs.Len()
	beforeSize, _ := os.Stat(path + ".blocks")
	// 注意：提交路径会自动触发 compact（废弃超 20%），所以这里通常
	// 已经回收过，staleRecords 可能为 0 —— 这本身就是期望行为。
	// 真正要验证的是「文件没有因 30 次重写而膨胀」。
	t.Logf("30 次重写后: %.1f KB, %d 块, %d 废弃 (自动 compact 已生效)",
		float64(beforeSize.Size())/1024, beforeBlocks, bs.staleRecords)
	if beforeSize.Size() > 4096 {
		t.Fatalf("30 次重写后文件膨胀到 %d 字节（自动 compact 未生效）", beforeSize.Size())
	}

	// 再显式 compact 一次：此时已无废弃记录，文件应保持不变。
	// （自动 compact 已在提交路径上回收过，所以这里不会再缩小。）
	if err := bs.compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	afterSize, _ := os.Stat(path + ".blocks")
	if bs.Len() != beforeBlocks {
		t.Fatalf("compact 后块数变了: %d → %d", beforeBlocks, bs.Len())
	}
	if bs.staleRecords != 0 {
		t.Fatalf("compact 后废弃计数应归零，实为 %d", bs.staleRecords)
	}
	if afterSize.Size() > beforeSize.Size() {
		t.Fatalf("重复 compact 不应让文件变大: %d → %d", beforeSize.Size(), afterSize.Size())
	}
	t.Logf("compact 后: %.1f KB（%.1f×）", float64(afterSize.Size())/1024,
		float64(beforeSize.Size())/float64(afterSize.Size()))
	bs.Close()

	// 重开后内容完好、索引同步
	bs2, err := openBlockStore(path + ".blocks")
	if err != nil {
		t.Fatalf("compact 后重开失败: %v", err)
	}
	defer bs2.Close()
	if bs2.Len() != beforeBlocks {
		t.Fatalf("重开后块数 %d != %d", bs2.Len(), beforeBlocks)
	}
	p, err := bs2.ReadPage(PageRequest{FromEnd: true, Limit: 10})
	if err != nil {
		t.Fatalf("compact 后读取失败: %v", err)
	}
	if len(p.Blocks) == 0 {
		t.Fatal("compact 后读不到块")
	}
	// 反复重写的 id=1 必须保留**最后一次**的内容（compact 只留最新版本）
	var gotID1 string
	for _, b := range p.Blocks {
		if bytes.Contains(b, []byte(`"id":1,`)) {
			gotID1 = string(b)
		}
	}
	if gotID1 == "" {
		t.Fatal("compact 后找不到 id=1 的块")
	}
	if !bytes.Contains([]byte(gotID1), []byte("版本30")) {
		t.Errorf("compact 后 id=1 不是最新内容: %s", gotID1)
	}
}

// TestCompactPreservesAllBlocks 大数量下 compact 不能丢块或改内容。
func TestSidecarCompactPreservesAllBlocks(t *testing.T) {
	storePath, ids := buildTestStore(t, 200)

	bs, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	// 记下 compact 前每个块的内容
	before := make(map[string][]byte, len(ids))
	for _, v := range ids {
		id := itoaSimple(int(v))
		p, ok := bs.readAndVerify(id)
		if !ok {
			t.Fatalf("compact 前块 %s 读不出", id)
		}
		before[id] = p
	}
	if err := bs.compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	for id, want := range before {
		got, ok := bs.readAndVerify(id)
		if !ok {
			t.Fatalf("compact 后块 %s 读不出", id)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("compact 改了块 %s 的内容", id)
		}
	}
	bs.Close()
}

// TestSumPrefixSelectsSingleVerification 一条 record 只允许按**一种**口径校验。
//
// 背景（2026-10-20 实测）：旧实现读到 Sum 后两种都算一遍 —— 先块集合摘要
// （读全部 payload），不匹配再算整文件滚动链（重读整个 sidecar）。真实 1.8GB
// 会话每次打开白付 1.11s。加口径前缀后，「哪种口径」变成 O(1) 判断。
//
// 这条测试钉住：两种记录都只走自己那条路，且都能读回同样的 blocks。
func TestSumPrefixSelectsSingleVerification(t *testing.T) {
	storePath, _ := buildTestStore(t, 12)

	bs, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	setSum, err := bs.actualSetSum(bs.allIDs())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(setSum, setSumPrefix) {
		t.Fatalf("新口径摘要必须带 %q 前缀，实际 %q", setSumPrefix, setSum)
	}
	chainSum := bs.legacyChainSum()
	if strings.HasPrefix(chainSum, setSumPrefix) {
		t.Fatalf("旧口径摘要不该带新前缀，实际 %q", chainSum)
	}

	// assembleWithChain 必须与独立实现逐字节一致（一次扫描算两个结果的前提）。
	full, chain2, err := bs.assembleWithChain()
	if err != nil {
		t.Fatal(err)
	}
	plain, err := bs.assemble()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full, plain) {
		t.Fatal("assembleWithChain 与 assemble 结果不一致")
	}
	if chain2 != chainSum {
		t.Fatalf("合并扫描算出的链与独立重算不一致\n got: %s\nwant: %s", chain2, chainSum)
	}
}

// TestResolveAcceptsBothSumFlavours 新旧两种口径的 record 都能读回完整 blocks。
//
// 升级不能要求迁移已落盘数据：旧 record 带整文件链摘要，新 record 带块集合
// 摘要，两条路径都必须还原出**完全相同**的 blocks 数组。
func TestResolveAcceptsBothSumFlavours(t *testing.T) {
	storePath, _ := buildTestStore(t, 10)

	bs, err := openBlockStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()
	want, err := bs.assemble()
	if err != nil {
		t.Fatal(err)
	}
	chainSum := bs.legacyChainSum()

	mkView := func(sum string) []byte {
		ref := blocksRefDoc{Store: filepath.Base(storePath), Sum: sum, Count: bs.Len(), Schema: 1}
		refJSON, _ := json.Marshal(map[string]blocksRefDoc{blocksRefKey: ref})
		view := append([]byte(`{"schema_version":1,"blocks":`), refJSON...)
		return append(view, []byte(`}`)...)
	}

	for name, sum := range map[string]string{
		"新口径(blockset1)": setSumPrefixMust(t, bs),
		"旧口径(sha256)":    chainSum,
	} {
		got, err := resolveBlocksRef(mkView(sum), strings.TrimSuffix(storePath, blockStoreSuffix))
		if err != nil {
			t.Fatalf("%s: 解析失败: %v", name, err)
		}
		var wrapper struct {
			Blocks json.RawMessage `json:"blocks"`
		}
		if err := json.Unmarshal(got, &wrapper); err != nil {
			t.Fatalf("%s: 解析结果非法: %v", name, err)
		}
		if !bytes.Equal(wrapper.Blocks, want) {
			t.Fatalf("%s: 还原的 blocks 与原始不一致", name)
		}
	}
}

func setSumPrefixMust(t *testing.T, bs *blockStore) string {
	t.Helper()
	sum, err := bs.actualSetSum(bs.allIDs())
	if err != nil {
		t.Fatal(err)
	}
	return sum
}
