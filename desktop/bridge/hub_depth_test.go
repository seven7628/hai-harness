package main

// P6-05：**每条链**独立计深度（wake.depth = parent.depth + 1）。
//
// 要证明的四件事：
//  1. 并行分支各走各的深度（A→B、A→C 都是 2；B→D、C→E 都是 3），不会互相加码；
//  2. 到 max_depth 只拒绝**那条链**继续 @，另一条分支照跑；
//  3. 人类新输入把深度归零，下一条 @ 重新从 1 开始；
//  4. 旧 generation 的 run 回来 @ 不消耗新链的深度额度。

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// depthOf 从该智能体最后一次 LLM 请求里取群唤醒 header 的深度（与生产同一条路径）。
func depthOf(t *testing.T, p *convProvider) int {
	t.Helper()
	headers := convWakeHeaders(p.lastRequest())
	if len(headers) != 1 {
		t.Fatalf("唤醒 header = %d, want 1", len(headers))
	}
	return headers[0].Depth
}

// TestHubGroupParallelBranchesKeepOwnDepth：A 分头叫 B/C，B 叫 D、C 叫 E ——
// D 与 E 的深度都是 3（各自 parent=2 加一），不是 4/5。
//
// 顺序刻意做成确定性的：**深分支先落地**（D 的 run 已经开跑、深度 3 已记账），
// 之后浅分支 C 才回来 @E。如果深度取自「群级当前值」，E 会被顶到 4；取自 C 自己
// 的 wake header 才是 3。
func TestHubGroupParallelBranchesKeepOwnDepth(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1, MaxDepth: 1000})

	a := createHubAgent(t, m, buf, 1, "深度链A")
	b := createHubAgent(t, m, buf, 2, "深度链B")
	c := createHubAgent(t, m, buf, 3, "深度链C")
	d := createHubAgent(t, m, buf, 4, "深度链D")
	e := createHubAgent(t, m, buf, 5, "深度链E")
	g := convGroupWith(t, m, buf, 10, "深度链群", nil, a, b, c, d, e)

	p3bWireFake(t, m, g, string(a.ID), "@深度链B @深度链C 你们分头查")
	p3bWireFake(t, m, g, string(b.ID), "@深度链D 你查一下")
	fd := newConvProvider("D 查完了")
	releaseD := fd.gated()
	defer close(releaseD)
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(d.ID), string(g.ID)), g, string(d.ID), fd)
	// C 卡在门闩上：等 D 的深分支落地之后，C 才回来说话。
	fc := newConvProvider("@深度链E 你也查")
	releaseC := fc.gated()
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(c.ID), string(g.ID)), g, string(c.ID), fc)
	fe := newConvProvider("E 查完了")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(e.ID), string(g.ID)), g, string(e.ID), fe)

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@深度链A 分头查一下",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	fd.waitStarted(t) // 深分支 D（深度 3）已经开跑并记账
	if got := depthOf(t, fd); got != 3 {
		t.Fatalf("D 的深度 = %d, want 3（A=1 → B=2 → D=3）", got)
	}
	close(releaseC) // 现在浅分支 C（深度 2）才 @E
	fe.waitStarted(t)
	if got := depthOf(t, fe); got != 3 {
		t.Fatalf("E 的深度 = %d, want 3（A=1 → C=2 → E=3）；两条分支必须各走各的", got)
	}
	if got := depthOf(t, fc); got != 2 {
		t.Fatalf("C 的深度 = %d, want 2", got)
	}
	// 视图 depth 是「本代见过的最大跳数」，不是某条链的当前值。
	if v := convView(m, g.ID); v.Depth != 3 {
		t.Fatalf("视图 depth = %d, want 3", v.Depth)
	}
}

// TestHubGroupDepthLimitStopsOnlyThatChain：MaxDepth=3 时，深链 D→E 被跳过
// （skipped/depth_limit），浅链 C→F（深度 2）照常跑。
func TestHubGroupDepthLimitStopsOnlyThatChain(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1, MaxDepth: 3})

	a := createHubAgent(t, m, buf, 1, "断链A")
	b := createHubAgent(t, m, buf, 2, "断链B")
	c := createHubAgent(t, m, buf, 3, "断链C")
	d := createHubAgent(t, m, buf, 4, "断链D")
	e := createHubAgent(t, m, buf, 5, "断链E")
	f := createHubAgent(t, m, buf, 6, "断链F")
	g := convGroupWith(t, m, buf, 10, "断链群", nil, a, b, c, d, e, f)

	p3bWireFake(t, m, g, string(a.ID), "@断链B @断链C 分头")
	p3bWireFake(t, m, g, string(b.ID), "@断链D 你深一层")
	p3bWireFake(t, m, g, string(c.ID), "@断链F 你走另一条")
	fd := newConvProvider("@断链E 我再深一层") // 4 > MaxDepth 3 → 被跳过
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(d.ID), string(g.ID)), g, string(d.ID), fd)
	fe := newConvProvider("E 不该被叫醒")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(e.ID), string(g.ID)), g, string(e.ID), fe)
	ff := newConvProvider("F 收到")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(f.ID), string(g.ID)), g, string(f.ID), ff)

	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@断链A 开始",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	ff.waitStarted(t)
	// 另一条链照跑（A→C→F = 3，未超限），深链的第 4 跳（A→B→D→E）被跳过。
	if got := depthOf(t, ff); got != 3 {
		t.Fatalf("F 的深度 = %d, want 3（A=1→C=2→F=3）", got)
	}
	convWait(t, "深链第 4 跳被记为 skipped(depth_limit)", func() bool {
		for _, d := range m.hubConversation().Deliveries(g.ID) {
			if d.Target == e.ID && d.Status == agenthub.DeliverySkipped && d.Reason == agenthub.DeliveryReasonDepthLimit {
				return true
			}
		}
		return false
	})
	time.Sleep(60 * time.Millisecond)
	if n := fe.callCount(); n != 0 {
		t.Fatalf("深链第 4 跳仍被唤醒 %d 次", n)
	}
	// 整群没有被判死：F 跑完了，状态不是 exhausted。
	if v := convView(m, g.ID); v.Terminal() {
		t.Fatalf("一条链到限把整群判死了: %+v", v)
	}
}

// TestHubGroupHumanResetsDepth：人类新输入把深度归零，下一条 @ 从 1 重新开始。
func TestHubGroupHumanResetsDepth(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1, MaxDepth: 1000})
	a := createHubAgent(t, m, buf, 1, "归零A")
	b := createHubAgent(t, m, buf, 2, "归零B")
	c := createHubAgent(t, m, buf, 3, "归零C")
	g := convGroupWith(t, m, buf, 10, "归零群", nil, a, b, c)
	p3bWireFake(t, m, g, string(a.ID), "@归零B 你先说")
	p3bWireFake(t, m, g, string(b.ID), "@归零A 我再说")
	fc := newConvProvider("C 收到")
	setGroupFakeLoop(t, m, mustGroupSession(t, m, string(c.ID), string(g.ID)), g, string(c.ID), fc)

	// 人类 → A(1) → B(2) → A(3)。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@归零A 走两跳",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	convWait(t, "链条走到第 3 跳", func() bool { return convView(m, g.ID).Depth >= 3 })

	// 人类插话：深度归零。
	if ok, errMsg, _ := hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "停一下，换个话题",
	}); !ok {
		t.Fatalf("human interject: %s", errMsg)
	}
	if v := convView(m, g.ID); v.Depth != 0 {
		t.Fatalf("人类插话后 depth = %d, want 0", v.Depth)
	}
	// 新链第一跳：C 的唤醒深度必须是 1。
	if ok, errMsg, _ := hubCmd(t, m, buf, 13, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@归零C 新话题",
	}); !ok {
		t.Fatalf("human send 2: %s", errMsg)
	}
	fc.waitStarted(t)
	if got := depthOf(t, fc); got != 1 {
		t.Fatalf("新链第一跳深度 = %d, want 1", got)
	}
}

// TestHubGroupDepthDefaultFromSettings：max_depth 默认 1000，settings.json 可覆盖。
func TestHubGroupDepthDefaultFromSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := loadHubConversationLimits().MaxDepth; got != 1000 {
		t.Fatalf("默认 max_depth = %d, want 1000（D3）", got)
	}
	writeHubConvSettings(t, `{"hub":{"conversation":{"max_depth":7}}}`)
	if got := loadHubConversationLimits().MaxDepth; got != 7 {
		t.Fatalf("settings max_depth = %d, want 7", got)
	}
	// 控制器视图回显生效值（UI 显示「深度 n/7」）。
	m, buf := testManager(t)
	m.hubConvLimits = &agenthub.ConversationLimits{Cooldown: -1, MaxDepth: 7}
	a := createHubAgent(t, m, buf, 1, "设置深度A")
	b := createHubAgent(t, m, buf, 2, "设置深度B")
	g := convGroupWith(t, m, buf, 10, "设置深度群", nil, a, b)
	if v := convView(m, g.ID); v.MaxDepth != 7 {
		t.Fatalf("视图 max_depth = %d, want 7", v.MaxDepth)
	}
}
