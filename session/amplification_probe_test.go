package session

// 写放大探针（2026-11-11）。
//
// 钉住的事实：**块被改一次 = 整个块被追加一次** —— 除非块里有顶层数组，
// 那时它会以**片段**形式落盘（见 block_frags.go），每次只追加变化的段。
//
// 生产实测（真实会话 s-1792132279027217000-21）：
//
//	put 记录数=33,071  存活 id=30,564  live=84.1MB  垃圾=1,111.7MB
//	被重写过的 id: 242 个 → 贡献垃圾 99.95%
//	每个 id 被写 100–290 次，单次最大版本 0.6–1.2MB（agent 卡片）
//
// 放大公式：
//
//	追加字节 = Σ(每个版本的大小)   ← 整块重写，不是增量
//	放大倍数 = 块大小 ÷ 每次增量
//
// **两种形态都要测**：
//  1. 无顶层数组的块（tool / assistant 卡片：内容是字符串字段）→ 不分片，
//     仍是整块重写（它们是终态、只写一次，无所谓）；
//  2. 有顶层数组的块（**agent 卡片**：items 数组只增）→ 分片，
//     每次只追加新增的那一段 + 变化的前缀 → 放大塌到个位数。
//
// 这就是生产事故与修复的分界：**会长的东西恰好是数组型的**。

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// agentCard 造一张「真实形态」的 agent 卡片：items 是顶层数组，元素逐条追加。
func agentCard(id int, items []string) []byte {
	return []byte(fmt.Sprintf(`{"id":%d,"kind":"agent","run_id":"r1","status":"running","label":"干活","items":[%s],"items_trimmed":0}`,
		id, strings.Join(items, ",")))
}

// TestBlockRewriteAppendsWholeVersion 无顶层数组的块：仍是整块重写。
//
// 这是**有意保留**的行为：这类块（tool / assistant 卡片）是终态、只写一次，
// 分片对它们只有坏处没有好处（碎片化 + 每次多一次目录写）。
func TestBlockRewriteAppendsWholeVersion(t *testing.T) {
	// 这个测试量的是「整块重写 ⇒ 追加字节 = Σ 版本大小」这条**追加语义**，
	// 而 sidecar 现在默认逐块压缩 ⇒ 物理追加量是压缩后的尺寸 ⇒ 量不到语义。
	// 压缩本身另有专门的测试（block_compress_test.go）。
	blockCompressEnabled = false
	defer func() { blockCompressEnabled = true }()

	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	// 底座：2000 个 5KB 块（≈10MB），模拟消息/工具卡。
	for i := 1; i <= 2000; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte(testBlock(i, strings.Repeat("x", 5*1024)))); err != nil {
			t.Fatal(err)
		}
	}
	// 一个 200KB 的块，每次事件长 8KB（内容是字符串字段 → 无数组 → 不分片）。
	// 内容必须**不可压缩**（见 incompressible 的注释）：这个测试量的是
	// 「追加字节 = Σ 版本大小」这条**追加语义**，压缩会把它量歪。
	card := []byte(testBlock(9000, incompressible(200*1024)))
	if err := bs.put("9000", card); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	base := fileSize(t, path)

	const events = 40
	const step = 8 * 1024
	for i := 0; i < events; i++ {
		card = growBlock(t, card, []byte(incompressible(step)))

		if err := bs.put("9000", card); err != nil {
			t.Fatal(err)
		}
		if err := bs.flush(); err != nil {
			t.Fatal(err)
		}
	}
	appended := fileSize(t, path) - base
	effective := int64(events * step)
	t.Logf("%d 次改一个 200KB 无数组块: sidecar +%.1fMB（有效增量 %.1fMB）→ 放大 %.1fx",
		events, float64(appended)/1e6, float64(effective)/1e6, float64(appended)/float64(effective))

	// 追加字节 ≈ Σ 每个版本的大小（200KB 起，每次 +8KB，共 40 次）
	var want int64
	for i := 0; i < events; i++ {
		want += int64(200*1024 + (i+1)*step)
	}
	if appended < want*9/10 || appended > want*11/10 {
		t.Fatalf("追加字节 %.1fMB 与「Σ 版本大小」%.1fMB 不符 —— 追加语义变了？",
			float64(appended)/1e6, float64(want)/1e6)
	}
}

// TestFragmentedCardAppendsOnlyDelta 是**修复后**的关键断言：
// agent 卡片（items 是顶层数组）分片存储后，每次事件只追加「新增的那一条
// item + 变化的前缀」→ 追加量 ≈ 增量，而不是整个卡片。
//
// 这正是把生产写放大从 150–275× 压下来的那把刀。
func TestFragmentedCardAppendsOnlyDelta(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	// 底座：2000 个 5KB 块。
	for i := 1; i <= 2000; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte(testBlock(i, strings.Repeat("x", 5*1024)))); err != nil {
			t.Fatal(err)
		}
	}
	// 一张卡片：50 条 item，每条 4KB（≈200KB）。
	const nItems = 50
	const itemSize = 4 * 1024
	items := make([]string, 0, nItems)
	for i := 0; i < nItems; i++ {
		items = append(items, fmt.Sprintf(`{"kind":"tool","tool_id":"c%d","name":"bash","args":"%s"}`,
			i, strings.Repeat("v", itemSize)))
	}
	card := agentCard(9000, items)
	if err := bs.put("9000", card); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	base := fileSize(t, path)

	// 40 次「新增一条 item」的事件。
	const events = 40
	for i := 0; i < events; i++ {
		items = append(items, fmt.Sprintf(`{"kind":"tool","tool_id":"n%d","name":"bash","args":"%s"}`,
			i, strings.Repeat("w", itemSize)))
		card = agentCard(9000, items)
		if err := bs.put("9000", card); err != nil {
			t.Fatal(err)
		}
		if err := bs.flush(); err != nil {
			t.Fatal(err)
		}
	}
	appended := fileSize(t, path) - base
	effective := int64(events * itemSize)
	t.Logf("%d 次「卡片加一条 item」: sidecar +%.1fMB（有效增量 %.1fMB）→ 放大 %.2fx",
		events, float64(appended)/1e6, float64(effective)/1e6, float64(appended)/float64(effective))

	// 追加量必须远小于「整块重写」（整块重写 ≈ 每次 ~200KB × 40 = 8MB）。
	wholeRewrite := int64(events) * int64(len(card))
	if appended > wholeRewrite/2 {
		t.Fatalf("分片没生效：追加 %.1fMB，整块重写会是 %.1fMB —— 放大只降了 %.1f 倍",
			float64(appended)/1e6, float64(wholeRewrite)/1e6, float64(wholeRewrite)/float64(appended))
	}
	// 追加量应接近「有效增量 × 小倍数」（前缀 + 目录也要写）。
	if appended > effective*3 {
		t.Fatalf("追加 %.1fMB 相对有效增量 %.1fMB 仍偏大（>3×）",
			float64(appended)/1e6, float64(effective)/1e6)
	}

	// 读回必须与最后一次写入**逐字节相同**。
	got, ok := bs.readBlock("9000")
	if !ok {
		t.Fatal("读不回分片块")
	}
	if string(got) != string(card) {
		t.Fatalf("读回内容与写入不一致（%d vs %d 字节）", len(got), len(card))
	}
}

// TestFragmentedCardSurvivesReopen 分片块在「索引命中」与「全量重建」两条
// 打开路径下都必须读回同样的内容。
func TestFragmentedCardSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	// 必须用**真的会分片**的卡片（items 数组 > fragSplitMinBytes）：
	// 2026-11-11 的目录项长度 bug（编码 24 字节、校验 28 字节）就是这样潜伏下来的
	// —— 用小卡片时它根本不会分片，「重新打开」读的是整块，测不出。
	card := longAgentCard(9000, 60, 4*1024)

	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.put("9000", card); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.persistIndex(); err != nil {
		t.Fatal(err)
	}
	if err := bs.Close(); err != nil {
		t.Fatal(err)
	}

	// ① 索引命中
	bs2, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := bs2.readBlock("9000")
	if !ok || string(got) != string(card) {
		t.Fatalf("索引命中路径读回不一致：ok=%v len=%d", ok, len(got))
	}
	// 校验路径也要过（分片走逻辑摘要）
	if _, ok := bs2.readBlockVerified("9000"); !ok {
		t.Fatal("索引命中路径校验失败")
	}
	_ = bs2.Close()

	// ② 全量重建（删掉索引）
	if err := os.Remove(blockIndexPath(path)); err != nil {
		t.Fatal(err)
	}
	bs3, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got3, ok := bs3.readBlock("9000")
	if !ok || string(got3) != string(card) {
		t.Fatalf("全量重建路径读回不一致：ok=%v len=%d", ok, len(got3))
	}
	if _, ok := bs3.readBlockVerified("9000"); !ok {
		t.Fatal("全量重建路径校验失败")
	}
	_ = bs3.Close()
}

// TestFragmentedCardSurvivesCompact 分片块经过 compact（折叠成整块）后
// 内容必须不变。
func TestFragmentedCardSurvivesCompact(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	items := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		items = append(items, fmt.Sprintf(`{"kind":"tool","tool_id":"c%d","name":"bash","args":"%s"}`,
			i, strings.Repeat("v", 4*1024)))
	}
	card := agentCard(9000, items)

	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte(testBlock(i, strings.Repeat("x", 5*1024)))); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.put("9000", card); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if err := bs.compact(); err != nil {
		t.Fatal(err)
	}
	got, ok := bs.readBlock("9000")
	if !ok || string(got) != string(card) {
		t.Fatalf("compact 后读回不一致：ok=%v len=%d want=%d", ok, len(got), len(card))
	}
	if _, ok := bs.readBlockVerified("9000"); !ok {
		t.Fatal("compact 后校验失败")
	}
	_ = bs.Close()
}

// TestFragmentWriteHistogram 按 id 统计写入量（诊断用）。
func TestFragmentWriteHistogram(t *testing.T) {
	if os.Getenv("HAI_LAB_PROBE") != "1" {
		t.Skip("设置 HAI_LAB_PROBE=1 运行")
	}
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()
	for i := 1; i <= 100; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte(testBlock(i, strings.Repeat("x", 5*1024)))); err != nil {
			t.Fatal(err)
		}
	}
	card := []byte(testBlock(9000, strings.Repeat("v", 200*1024)))
	if err := bs.put("9000", card); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		card = growBlock(t, card, []byte(strings.Repeat("a", 8*1024)))
		if err := bs.put("9000", card); err != nil {
			t.Fatal(err)
		}
		if err := bs.flush(); err != nil {
			t.Fatal(err)
		}
	}
	hist := map[string][]int{}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Read(make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		kind, id, _, payload, _, _, err := readBlockRecordExt(r)
		if err != nil {
			break
		}
		if kind == blockRecPut {
			hist[id] = append(hist[id], len(payload))
		}
	}
	type row struct {
		id    string
		n     int
		bytes int
	}
	var rows []row
	for id, sizes := range hist {
		sum := 0
		for _, s := range sizes {
			sum += s
		}
		rows = append(rows, row{id, len(sizes), sum})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].bytes > rows[j].bytes })
	for i, rw := range rows {
		if i >= 3 {
			break
		}
		t.Logf("  id=%-8s 写了 %3d 次  合计 %8.2fMB  单次均 %7.1fKB", rw.id, rw.n,
			float64(rw.bytes)/1e6, float64(rw.bytes)/float64(maxInt(rw.n, 1))/1024)
	}
}
