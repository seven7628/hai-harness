package session

// 复现：后台回收（compact）在「每次提交只重写一个视图块」的场景下**每次提交都触发**，
// 形成无限循环（2026-11-09 实测：desktop bridge 每 100ms 压一次，压完又长回去）。
//
// 真实数据（取自出问题的那两个会话，比例照搬）：
//
//	压缩后 sidecar 有效体积        61.1 MB（其中视图块 16.1 MB，其余历史 45 MB）
//	每次提交追加（重写的视图块）      16.1 MB
//	→ 文件回到 77.2 MB，废弃字节 = 16.1 MB
//	→ 废弃比例 = 16.1 / 77.2 = 20.85%
//	→ 刚过旧阈值 compactThreshold(0.20) → 触发全量重写（回收 16.1MB 却要重写 77.2MB）
//	→ 回到 61.1 MB → 下次提交再来一遍 → 无限循环
//
// P0 修复（2026-11-11，CHECKPOINT_V3_DESIGN §6.1）：判据从「废弃**占比** > 20%」
// 改成「**回收量 ≥ live×compactGainRatio**」（收益 > 成本）。
//
//	本例：stale=16.1MB / live=61.1MB → 16.1 < 30.6 → **不触发**（回收 16.1MB
//	不值重写 77.2MB，写放大 5×）。于是循环在**第一次**就被掐断，而不是靠
//	「压完再等 1.5×」的冷却兜底。
//
//	并且「该收还是收」：当废弃字节真的攒到 live 的一半以上（≥32MB 绝对下限），
//	压缩照常触发 —— 见 TestCompactTriggersWhenGainExceedsCost。

import (
	"fmt"
	"strings"
	"testing"
)

// TestReproCompactTriggersOnEveryCommit 复现「每次提交都触发一次全量重写」。
//
// P0 之前：一次提交后 needsCompact 立刻为 true → 循环。
// P0 之后：**不该**触发 —— 单次提交的废弃量不值一次全量重写。
func TestReproCompactTriggersOnEveryCommit(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	mb := 1 << 20

	// 历史有效内容：45 MB（真实值 45 MB / 61.1 MB 底座的一部分）。
	for i := 1; i <= 45; i++ {
		payload := []byte(testBlock(i, strings.Repeat("z", mb-64)))
		if err := bs.put(fmt.Sprintf("%d", i), payload); err != nil {
			t.Fatal(err)
		}
	}
	// 每次提交被重写的那个视图块：16.1 MB（真实值）。
	viewSize := int(float64(mb) * 16.1)
	view := []byte(testBlock(100, strings.Repeat("v", viewSize)))
	if err := bs.put("view", view); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	base := fileSize(t, path)
	t.Logf("压缩前有效体积: %.1f MB", float64(base)/float64(mb))

	// —— 第一次提交：重写视图块（追加 16.1 MB，新版取代旧版 → 旧版成废弃）——
	if err := bs.put("view", view); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	afterFirst := fileSize(t, path)
	ratio := bs.staleByteRatio()
	stale := afterFirst - base
	t.Logf("一次提交后: 文件 %.1f MB，废弃 %.1f MB，废弃比例 %.2f%%，needsCompact=%v",
		float64(afterFirst)/float64(mb),
		float64(stale)/float64(mb),
		ratio*100, bs.needsCompact())

	// P0 的关键断言：**不该触发**。
	//
	// 旧判据（废弃占比 > 20%）在这里是 true（20.85% 刚过线）—— 那正是循环的成因。
	// 新判据（收益 > 成本）要求 stale ≥ live×0.5：16.1MB < 61.1MB×0.5 = 30.6MB
	// → false。写放大从 5×（省 16MB 却重写 77MB）降到「不做」。
	if bs.needsCompact() {
		t.Fatalf("循环未断：一次提交只产生 %.1fMB 废弃（live %.1fMB），"+
			"回收量不足 live×%.2f，不该触发全量重写",
			float64(stale)/float64(mb), float64(bs.liveBytes)/float64(mb), compactGainRatio)
	}

	// —— 模拟生产循环：连续 20 次提交，每次重写视图块（各 +16.1MB 废弃）——
	//
	// 旧判据（废弃占比 > 20%）：几乎**每次**提交都触发 → 每 100ms 压一次。
	// 新判据：只有攒够收益才触发一次；压完废弃归零，必须重新攒，
	// 且最小间隔保证不会连着压。
	triggers := 0
	for i := 0; i < 20; i++ {
		if err := bs.put("view", view); err != nil {
			t.Fatal(err)
		}
		if err := bs.flush(); err != nil {
			t.Fatal(err)
		}
		if !bs.needsCompact() {
			continue
		}
		triggers++
		snap, err := bs.beginCompact()
		if err != nil {
			t.Fatal(err)
		}
		cw, err := bs.rewriteCompact(snap)
		if err != nil {
			t.Fatal(err)
		}
		if err := bs.finishCompact(snap, cw); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("20 次提交（每次 +16.1MB 废弃）→ 触发压缩 %d 次", triggers)
	if triggers > 1 {
		t.Fatalf("循环未断：20 次提交触发了 %d 次压缩（旧判据是 ~10 次/轮）", triggers)
	}
}

// TestCompactTriggersWhenGainExceedsCost 钉住「该收还是收」：
// 废弃字节攒到 live 的一半以上（且 ≥ 32MB 绝对下限）时，压缩照常触发。
//
// 这一条是 P0 判据的另一半 —— 不能把「不划算」变成「永不回收」。
func TestCompactTriggersWhenGainExceedsCost(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	mb := 1 << 20
	// 60MB 底座。
	// 内容**不可压缩**（见 incompressible 的注释）：这个测试量的是 compact 的
	// 收益判据（stale/live 的字节比 + 绝对下限），压缩会把字节量改掉。
	for i := 1; i <= 60; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte(testBlock(i, incompressible(mb-64)))); err != nil {
			t.Fatal(err)
		}
	}
	// 一个 16MB 的块写 5 次 → live=76MB / stale=64MB（stale/live = 0.84 ≥ 0.5 ✓，stale ≥ 32MB ✓）。
	big := []byte(testBlock(900, incompressible(16*mb)))
	for i := 0; i < 5; i++ {
		if err := bs.put("900", big); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}

	stale := bs.nBytes - bs.liveBytes
	t.Logf("live=%.1fMB stale=%.1fMB (stale/live=%.2f)", float64(bs.liveBytes)/float64(mb),
		float64(stale)/float64(mb), float64(stale)/float64(bs.liveBytes))
	if !bs.needsCompact() {
		t.Fatalf("该收没收：stale %.1fMB / live %.1fMB（收益 > 成本）却不触发",
			float64(stale)/float64(mb), float64(bs.liveBytes)/float64(mb))
	}
}

// TestLiveBlockUnchangedIsNotRewritten 验证「进行中的块内容没变就不重写」。
//
// 这是「每次提交追加 16.1MB」的源头：agent 卡片在整个子任务 running 期都是
// status=running（isLiveBlock 为真），而内容只在子任务产出新东西时才变。
// 原实现对 live 块无条件整块重写 → 每次提交都追加一遍全部 running 卡片。
func TestLiveBlockUnchangedIsNotRewritten(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	// 一个 running 状态的块（isLiveBlock 判 true）——走真实的 refBlocks 路径。
	view := []byte(`{"schema_version":1,"turns":[],"blocks":[{"id":9001,"kind":"agent","status":"running","text":"running..."}],"run_nodes":[]}`)
	if _, _, err := refBlocks(bs, view); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	before := fileSize(t, path)

	// 下一次提交：内容**完全没变**（子任务这段时间没产出新东西）。
	if _, _, err := refBlocks(bs, view); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	after := fileSize(t, path)

	if after != before {
		t.Fatalf("内容未变的 running 块被重写了：文件 %d → %d 字节（应不变）", before, after)
	}
}

// TestCompactMinIntervalBlocksImmediateRecompact 验证最小间隔：
// 刚压完，即使收益已经超过成本，也不允许立刻再压。
//
// 为什么需要（2026-11-11）：「收益 > 成本」把单次写放大封顶了，但一个**高频写入**
// 的会话仍可能反复达到阈值。间隔把「即使划算也不连续做」钉死，避免压缩本身
// 成为 IO 抖动源（今天的生产事故就是「每 100ms 压一次」）。
func TestCompactMinIntervalBlocksImmediateRecompact(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	mb := 1 << 20
	for i := 1; i <= 60; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte(testBlock(i, strings.Repeat("z", mb-64)))); err != nil {
			t.Fatal(err)
		}
	}
	big := []byte(testBlock(900, strings.Repeat("v", 16*mb)))
	for i := 0; i < 5; i++ {
		if err := bs.put("900", big); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if !bs.needsCompact() {
		t.Skip("废弃量/比例未同时越线，本用例不适用")
	}

	// 压一次 → 记下 lastCompactAt
	snap, err := bs.beginCompact()
	if err != nil {
		t.Fatal(err)
	}
	cw, err := bs.rewriteCompact(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := bs.finishCompact(snap, cw); err != nil {
		t.Fatal(err)
	}

	// 压完立刻再堆 5 份 16MB 废弃（收益条件已满足：stale=64MB ≥ live×0.5）
	// —— 此时**只有最小间隔**能拦住。
	for i := 0; i < 5; i++ {
		if err := bs.put("900", big); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	stale := bs.nBytes - bs.liveBytes
	if float64(stale) < float64(bs.liveBytes)*compactGainRatio {
		t.Skipf("废弃量不足以满足收益判据（stale %.1fMB / live %.1fMB）", float64(stale)/float64(mb), float64(bs.liveBytes)/float64(mb))
	}
	if bs.needsCompact() {
		t.Fatalf("最小间隔失效：刚压完就允许再压（stale %.1fMB / live %.1fMB）",
			float64(stale)/float64(mb), float64(bs.liveBytes)/float64(mb))
	}
}
