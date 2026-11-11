package session

// 写放大探针（2026-11-11）。
//
// 钉住的事实：**块被改一次 = 整个块被追加一次**。
//
// 生产实测（真实会话 s-1792132279027217000-21）：
//
//	put 记录数=33,071  存活 id=30,564  live=84.1MB  垃圾=1,111.7MB
//	被重写过的 id: 242 个 → 贡献垃圾 99.95%
//	每个 id 被写 100–290 次，单次最大版本 0.6–1.2MB（agent 卡片）
//
// 机制（lab 实测，40 次改一个 200KB 卡片）：sidecar +14.9MB，
// 而有效增量只有 0.3MB → 放大 45.5×。
//
//	追加字节 = Σ(每个版本的大小)   ← 整块重写，不是增量
//	放大倍数 = 块大小 ÷ 每次增量
//
// **这个测试是 P1（卡片分片存储）的金丝雀**：分片落地后，
// 追加字节会从「Σ 版本大小」掉到「Σ 增量大小」，断言会失败 ——
// 那时把它改成新的期望值。
//
// 另一个重要结论（别被误导）：**裁剪 items 上限并不能降低写放大** ——
// 卡片裁到 50 条后仍是 1MB，每事件照样整块重写 1MB。
// 只有「同 id 多片段」的存储形态能让追加量≈增量。

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

// TestBlockRewriteAppendsWholeVersion 验证「块被改一次 = 整个块被追加一次」。
func TestBlockRewriteAppendsWholeVersion(t *testing.T) {
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
	// 一个 200KB 的「running 卡片」，每次事件长 8KB。
	card := []byte(testBlock(9000, strings.Repeat("v", 200*1024)))
	if err := bs.put("card", card); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	base := fileSize(t, path)

	const events = 40
	const step = 8 * 1024
	for i := 0; i < events; i++ {
		card = growBlock(t, card, []byte(strings.Repeat("a", step)))
		if err := bs.put("card", card); err != nil {
			t.Fatal(err)
		}
		if err := bs.flush(); err != nil {
			t.Fatal(err)
		}
	}
	appended := fileSize(t, path) - base
	effective := int64(events * step)
	t.Logf("%d 次改一个 200KB 卡片: sidecar +%.1fMB（有效增量 %.1fMB）→ 放大 %.1fx",
		events, float64(appended)/1e6, float64(effective)/1e6, float64(appended)/float64(effective))

	// 追加字节 ≈ Σ 每个版本的大小（200KB 起，每次 +8KB，共 40 次）
	var want int64
	for i := 0; i < events; i++ {
		want += int64(200*1024 + (i+1)*step)
	}
	if appended < want*9/10 || appended > want*11/10 {
		t.Fatalf("追加字节 %.1fMB 与「Σ 版本大小」%.1fMB 不符 —— 追加语义变了？"+
			"（若这是 P1 分片落地后的行为，请更新本断言）",
			float64(appended)/1e6, float64(want)/1e6)
	}

	// 按 id 分组统计：确认放大集中在「被反复写的那一个 id」上。
	hist := map[string][]int{}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Read(make([]byte, 8)); err != nil { // 跳过 magic
		t.Fatal(err)
	}
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		kind, id, payload, err := readBlockRecord(r)
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
	if len(rows) > 0 {
		t.Logf("按 id 的写入字节 Top 3：")
		for i, rw := range rows {
			if i >= 3 {
				break
			}
			t.Logf("  id=%-8s 写了 %3d 次  合计 %8.2fMB  单次均 %7.1fKB", rw.id, rw.n,
				float64(rw.bytes)/1e6, float64(rw.bytes)/float64(maxInt(rw.n, 1))/1024)
		}
	}
}
