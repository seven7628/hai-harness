package main

// 「窗口外卡片」**旧实现 vs 新实现**的等价性测试（2026-11-19）。
//
// ## 要证明什么
//
// 骨架级优化把「窗口外的卡片正文」从 reducer 的堆里赶出去（实测该会话
// 2017 张卡片 / 119.9MB，而真正的对话尾部只有 1.7MB —— 那 120MB 在交付时
// 必然被 detachAgents 置空 ⇒ 纯浪费）。
//
// 这次改动的全部风险 = **「交付给 UI 的 agents 变了」**。这条测试证明：
//
//	路 A（旧）：窗口全解码 → reducer → MarshalTail → .Agents
//	路 B（新）：只解码尾部 + 从 sidecar 取卡片头 → reducer → MarshalTail → .Agents
//
// ⇒ 两条路产出的 agents **字节完全相同** ⇒ 页面零影响。
//
// ## 两条必须复刻的隐式语义（第一版就踩了，测试立刻红）
//
// ① **候选边界在「归一化之后」算**：detachAgents 迭代 blocks[:start]，
//    start = len(blocks) - limit，而 blocks 已过 normalizeDanglingState
//    （丢空壳 assistant / 悬空 tool，实测 k=17）⇒ start' = 卡片类块数 +
//    尾部存活数 - limit。
// ② **候选集取 `blocks[:start]` 这个前缀** ⇒ 是 spans 索引**最小**的一段
//    ⇒ 新实现要取的是**最旧**的 N 张卡片里的**最新**的那些。
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

// legacyWindowParse 是**改造前**的窗口解析：窗口外的卡片类块 + 尾部窗口
// **全部解码**（卡片正文因此进了堆）。
//
// 保留它是为了当「参照实现」——它必须逐字复刻改造前 parseCheckpointFromRecordWindow
// 的行为（need = [卡片类块(i<start) 升序..., 尾部(start..n) 升序...]）。
func legacyWindowParse(t *testing.T, rec *session.SessionRecord, limit int) *Checkpoint {
	t.Helper()
	spans := rec.LoadedBlockSpans
	kinds := rec.LoadedBlockKinds
	n := len(spans)
	start := n - limit
	need := make([]int, 0, limit+16)
	decoded := make(map[int]struct{}, limit+16)
	for i := 0; i < start; i++ {
		if session.HasCardKind(kinds[i]) {
			need = append(need, i)
			decoded[i] = struct{}{}
		}
	}
	for i := start; i < n; i++ {
		need = append(need, i)
		decoded[i] = struct{}{}
	}
	selSpans := make([]session.BlockSpan, len(need))
	selHashes := make([][8]byte, len(need))
	hasHashes := len(rec.LoadedBlockHashes) == n
	for j, i := range need {
		selSpans[j] = spans[i]
		if hasHashes {
			selHashes[j] = rec.LoadedBlockHashes[i]
		}
	}
	blocks := make([]Block, len(need))
	var hsh [][8]byte
	if hasHashes {
		hsh = selHashes
	}
	if err := decodeBlocksParallel(selSpans, hsh, blocks); err != nil {
		t.Fatalf("旧路径解码失败: %v", err)
	}
	pagedIDs := make([]uint64, 0, start)
	pagedSpans := make([]session.BlockSpan, 0, start)
	pagedHashes := make([][8]byte, 0, start)
	for i := 0; i < start; i++ {
		if _, ok := decoded[i]; ok {
			continue
		}
		pagedIDs = append(pagedIDs, rec.LoadedBlockIDs[i])
		pagedSpans = append(pagedSpans, spans[i])
		if hasHashes {
			pagedHashes = append(pagedHashes, rec.LoadedBlockHashes[i])
		}
	}
	return &Checkpoint{
		Blocks:      blocks,
		BlocksTotal: n,
		PagedBlocks: &PagedBlocks{IDs: pagedIDs, Spans: pagedSpans, Hashes: pagedHashes},
	}
}

// loadAgents 走「窗口解析 → reducer → MarshalTail」，返回交付的 agents。
func loadAgents(t *testing.T, cp *Checkpoint) []Block {
	t.Helper()
	red := NewViewReducer()
	if err := red.Restore(cp); err != nil {
		t.Fatalf("restore 失败: %v", err)
	}
	red.NormalizeForRestore()
	pv, err := red.MarshalTail(restoreWindowBlocks)
	if err != nil {
		t.Fatalf("MarshalTail 失败: %v", err)
	}
	return pv.Agents
}

// TestDetachedAgentsEquivalence：旧实现与新实现交付的 agents 字节必须完全一致。
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

	// ——— 路 A：旧实现（窗口全解码）———
	recA, err := store.LoadRecordForView(ctx, wsKey, sid)
	if err != nil {
		t.Fatalf("加载 record 失败: %v", err)
	}
	cpA := legacyWindowParse(t, recA, restoreWindowBlocks)
	agentsA := loadAgents(t, cpA)
	recA.ReleaseView()

	// ——— 路 B：新实现（只解码尾部 + 现取卡片头）———
	recB, err := store.LoadRecordForView(ctx, wsKey, sid)
	if err != nil {
		t.Fatalf("加载 record 失败: %v", err)
	}
	cpB, err := parseCheckpointFromRecordWindow(recB, restoreWindowBlocks)
	if err != nil {
		t.Fatalf("新路径解析窗口失败: %v", err)
	}
	agentsB := loadAgents(t, cpB)
	recB.ReleaseView()

	// ——— 比较 ———
	bytesA, err := json.Marshal(agentsA)
	if err != nil {
		t.Fatal(err)
	}
	bytesB, err := json.Marshal(agentsB)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("会话 %s：旧 %d 张 / %d 字节（旧路径解码块数 %d）；新 %d 张 / %d 字节（新路径解码块数 %d）",
		sid, len(agentsA), len(bytesA), len(cpA.Blocks), len(agentsB), len(bytesB), len(cpB.Blocks))
	if len(agentsA) != len(agentsB) {
		t.Fatalf("agents 数量不同：旧=%d 新=%d", len(agentsA), len(agentsB))
	}
	if string(bytesA) != string(bytesB) {
		for i := range agentsA {
			if i >= len(agentsB) {
				break
			}
			ja, _ := json.Marshal(agentsA[i])
			jb, _ := json.Marshal(agentsB[i])
			if string(ja) != string(jb) {
				t.Fatalf("agents[%d] 不同：\n 旧=%s\n 新=%s", i, ja, jb)
			}
		}
		t.Fatalf("agents 字节不同（旧 %d 字节 / 新 %d 字节）", len(bytesA), len(bytesB))
	}
	t.Logf("✅ 旧实现与新实现交付的 agents 字节完全一致（%d 字节）⇒ 页面零影响", len(bytesA))
}
