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
//	→ 刚过 compactThreshold(0.20) → 触发压缩 → 回到 61.1 MB
//	→ 下一次提交再来一遍 → 无限循环
//
// 代价对比：一次压缩回收 16.1 MB，却要**重写整个 77.2 MB 文件**（实测 100ms CPU +
// 77MB 写 IO）——收益远小于代价，而每次提交都付一次。

import (
	"fmt"
	"strings"
	"testing"
)

// TestReproCompactTriggersOnEveryCommit 复现「每次提交都触发一次全量重写」。
//
// 现状（修复前）：断言不成立 —— needsCompact 在每次提交后都为 true。
// 期望（修复后）：回收要**划算**才做（废弃字节足够多才值得重写整个文件），
// 于是单次提交的小额废弃不该立刻触发。
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
	t.Logf("一次提交后: 文件 %.1f MB，废弃 %.1f MB，废弃比例 %.2f%%，needsCompact=%v",
		float64(afterFirst)/float64(mb),
		float64(afterFirst-base)/float64(mb),
		ratio*100, bs.needsCompact())

	// 第一次压缩：**应该**触发（废弃比例 20.85% 确实越线，回收 16.1MB/重写 77.2MB
	// 虽然不划算，但比例判据的语义是「该收就收」，不该被改掉）。
	if !bs.needsCompact() {
		t.Fatalf("第一次压缩未触发（废弃比例 %.2f%%）", ratio*100)
	}

	// —— 关键：压完之后，同一个提交再来一次，**不应立刻又压**（这才是循环）——
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
	compacted := fileSize(t, path)
	t.Logf("压缩后: %.1f MB", float64(compacted)/float64(mb))

	// 同样的提交再来一次（+16.1MB → 废弃比例再次 20.85%）。
	if err := bs.put("view", view); err != nil {
		t.Fatal(err)
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if bs.needsCompact() {
		t.Fatalf("循环未断：压缩后（%.1fMB）同一次提交又触发压缩（当前 %.1fMB）——"+
			"应被冷却挡住（需先长到 %.1fMB）",
			float64(compacted)/float64(mb), float64(fileSize(t, path))/float64(mb),
			float64(compacted)*compactCooldownFactor/float64(mb))
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

// TestCompactCooldownBlocksImmediateRecompact 验证冷却：刚压完必须先长够才允许再压。
func TestCompactCooldownBlocksImmediateRecompact(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/x.record.json.blocks"
	bs, err := openBlockStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer bs.Close()

	mb := 1 << 20
	// 40MB 底座 + 一个 16MB 的块 → 废弃量够大（>32MB 绝对门槛）时比例才可能越线。
	for i := 1; i <= 60; i++ {
		if err := bs.put(fmt.Sprintf("%d", i), []byte(testBlock(i, strings.Repeat("z", mb-64)))); err != nil {
			t.Fatal(err)
		}
	}
	big := []byte(testBlock(900, strings.Repeat("v", 16*mb)))
	// 写 3 份同一个 id → 废弃 32MB+（越绝对门槛），比例也高。
	for i := 0; i < 3; i++ {
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

	// 压一次 → 记下 lastCompactBytes
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

	// 压完立刻再堆 32MB 废弃（>绝对门槛、比例也越线）——此时**只有冷却**能拦住：
	// 文件从 %.1fMB 长到 %.1fMB，没到 lastCompactBytes×1.5。
	for i := 0; i < 2; i++ {
		if err := bs.put("900", big); err != nil {
			t.Fatal(err)
		}
	}
	if err := bs.flush(); err != nil {
		t.Fatal(err)
	}
	if bs.needsCompact() {
		t.Fatalf("冷却失效：刚压完（%.1fMB）就允许再压（当前 %.1fMB）",
			float64(bs.lastCompactBytes)/float64(mb), float64(bs.nBytes)/float64(mb))
	}
}
