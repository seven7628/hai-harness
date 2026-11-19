package main

// 「窗口外卡片」两条取法的**等价性测试**（2026-11-19，bridge 内存爬山第 5 步的前置）。
//
// ## 要证明什么
//
// 骨架级优化的全部风险 = **「交付给 UI 的 agents 变了」**。今天 agents 来自
// 「窗口全解码 → reducer → detachAgents(blocks[:start])」；将来的路是
// 「只解码尾部 → 从 sidecar 的 span 里取卡片头」。
//
// 这条测试**只测不改**：证明今天这两条路已经产出**字节完全相同**的 agents。
// 路 B 调的是**生产代码**（card_head.go 的 pickCardHeadsFromSpans）⇒
// 这条测试同时是那份生产代码的验收。
// ⇒ 等价性成立 ⇒ 后面的切换就是「换个来源拿同样的东西」⇒ 页面零影响。
//
// ## 第一版就红在这里（两条必须复刻的语义）
//
// ① **候选边界在「归一化之后」的块数组上算**：
//    detachAgents 迭代 `blocks[:start]`，其中 `start = len(blocks) - limit`，
//    而 blocks 已经过 normalizeDanglingState（丢弃空壳 assistant / 悬空 tool 块，
//    实测 k=17）⇒ 候选集 = **最旧**的 start' 个块，
//    start' = 卡片类块数 + 尾部存活数 - limit（实测 2017 + 483 - 500 = 2000）。
//    ⇒ 「将来那条路」必须先解码尾部窗口、跑同样的归一化、算出尾部存活数。
//
// ② **候选集取的是「最旧」的一段，不是「最新」**：
//    `blocks[:start]` 是**前缀** ⇒ 候选是 spans 索引**最小**的一段。
//    第一版从最新往回收集 ⇒ 拿到的是最新 500 张 ⇒ 与现状差 17 张 ⇒ 测试立刻红了。
//    ⇒ 这就是这条测试的价值：把「交付集合到底怎么定的」变成机器可验证的事实。
//
// （k=17 造成的「最新 17 张卡片不在候选集里」**不是缺陷**：它们在尾部窗口里，
//   作为普通块交付，左栏树照样能看到 —— 只是分工不同。）
//
// ## 跑法（需要真实会话数据）
//
//	cp -c ~/.go-code/sessions/<ws>/<sid>.record.json{,.blocks,.blocks.idx} /tmp/lab/
//	HAI_LAB_STORE_ROOT=/tmp/lab \
//	HAI_LAB_SESSION=/tmp/lab/<sid>.record.json \
//	go test ./desktop/bridge/ -run TestDetachedAgentsEquivalence -v -timeout 20m
//
// 没设环境变量就跳过（不进常规 CI 的耗时）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/session"
)

// TestDetachedAgentsEquivalence：两条路产出的 agents 字节必须完全一致。
func TestDetachedAgentsEquivalence(t *testing.T) {
	recPath := os.Getenv("HAI_LAB_SESSION")
	storeRoot := os.Getenv("HAI_LAB_STORE_ROOT")
	if recPath == "" || storeRoot == "" {
		t.Skip("需要 HAI_LAB_STORE_ROOT + HAI_LAB_SESSION=<sid>.record.json")
	}
	abs, err := filepath.Abs(recPath)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(abs)
	if filepath.Ext(base) != ".json" {
		t.Fatalf("不是 record.json: %s", base)
	}
	sid := base[:len(base)-len(".record.json")]
	wsKey := filepath.Base(filepath.Dir(abs))

	store, err := session.NewSessionRecordStorage(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// ——— 路 A：现状（窗口全解码 → reducer → MarshalTail）———
	rec, err := store.LoadRecordForView(ctx, wsKey, sid)
	if err != nil {
		t.Fatalf("加载 record 失败: %v", err)
	}
	cp, err := parseCheckpointFromRecordWindow(rec, restoreWindowBlocks)
	if err != nil {
		t.Fatalf("解析窗口失败: %v", err)
	}
	windowBlocks := len(cp.Blocks)
	red := NewViewReducer()
	if err := red.Restore(cp); err != nil {
		t.Fatalf("restore 失败: %v", err)
	}
	red.NormalizeForRestore()
	pv, err := red.MarshalTail(restoreWindowBlocks)
	if err != nil {
		t.Fatalf("MarshalTail 失败: %v", err)
	}
	agentsA := pv.Agents

	// ——— 路 B：将来的路（只解码尾部 + 从 sidecar 取卡片头）———
	rec2, err := store.LoadRecordForView(ctx, wsKey, sid)
	if err != nil {
		t.Fatalf("加载 record 失败: %v", err)
	}
	spans := rec2.LoadedBlockSpans
	kinds := rec2.LoadedBlockKinds
	n := len(spans)
	boundary := n - restoreWindowBlocks
	if boundary < 0 {
		boundary = 0
	}
	// B-1：只解码尾部窗口（restoreWindowBlocks 个），跑**同样的归一化**，
	//      得出尾部存活块数 ⇒ k = 尾部被归一化丢弃的块数。
	tail := make([]session.BlockSpan, 0, restoreWindowBlocks)
	for i := boundary; i < n; i++ {
		tail = append(tail, spans[i])
	}
	tailBlocks := make([]Block, len(tail))
	if err := decodeBlocksParallel(tail, nil, tailBlocks); err != nil {
		t.Fatalf("解码尾部失败: %v", err)
	}
	probe := &Checkpoint{Blocks: tailBlocks}
	normalizeDanglingState(probe)
	tailKept := len(probe.Blocks)
	k := len(tailBlocks) - tailKept

	// B-2：候选范围 = 尾部窗口之前的「卡片类块」，但要按归一化后的 start 截断：
	//      路 A 的 blocks 数组 = [卡片类块(全部)..., 尾部(存活 tailKept 个)]，
	//      start = len(blocks) - limit = (卡片类块数 + tailKept) - limit，
	//      而候选 = blocks[:start] ⇒ 卡片类块里只有 (卡片类块数 + tailKept - limit) 个进候选。
	cardKindBefore := 0
	for i := 0; i < boundary && i < len(kinds); i++ {
		if session.HasCardKind(kinds[i]) {
			cardKindBefore++
		}
	}
	candidateCount := cardKindBefore + tailKept - restoreWindowBlocks
	if candidateCount < 0 {
		candidateCount = 0
	}
	// 候选集 = spans 里**最旧**的 candidateCount 张卡片类块 ⇒ 找出它的索引上界。
	upperIdx := -1
	seen := 0
	for i := 0; i < boundary && i < len(kinds); i++ {
		if !session.HasCardKind(kinds[i]) {
			continue
		}
		seen++
		if seen == candidateCount {
			upperIdx = i
			break
		}
	}
	agentsB, err := pickCardHeadsFromSpans(spans, kinds, upperIdx)
	if err != nil {
		t.Fatalf("取卡片头失败: %v", err)
	}
	rec.ReleaseView()
	rec2.ReleaseView()

	// ——— 比较 ———
	bytesA, err := json.Marshal(agentsA)
	if err != nil {
		t.Fatal(err)
	}
	bytesB, err := json.Marshal(agentsB)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("会话 %s", sid)
	t.Logf("  窗口解码块数=%d（其中尾部 %d 个，归一化丢弃 k=%d）", windowBlocks, len(tailBlocks), k)
	t.Logf("  尾部之前的卡片类块=%d ⇒ 候选范围=%d", cardKindBefore, candidateCount)
	t.Logf("  路A agents %d 张 / %d 字节；路B agents %d 张 / %d 字节",
		len(agentsA), len(bytesA), len(agentsB), len(bytesB))
	if len(agentsA) != len(agentsB) {
		t.Fatalf("agents 数量不同：A=%d B=%d", len(agentsA), len(agentsB))
	}
	if string(bytesA) != string(bytesB) {
		for i := range agentsA {
			if i >= len(agentsB) {
				break
			}
			ja, _ := json.Marshal(agentsA[i])
			jb, _ := json.Marshal(agentsB[i])
			if string(ja) != string(jb) {
				t.Fatalf("agents[%d] 不同：\n A=%s\n B=%s", i, ja, jb)
			}
		}
		t.Fatalf("agents 字节不同（A %d 字节 / B %d 字节）", len(bytesA), len(bytesB))
	}
	t.Logf("✅ 两条路的 agents 字节完全一致（%d 字节）⇒ 换来源不影响交付", len(bytesA))
}
