package main

import (
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/provider"
)

// 进程级用户价表（userPriceTable）是包级全局，生产里由 loadSettings / set_provider 写
// （读 ~/.go-code/settings.json 的 provider[].prices），**没有任何地方会把它还原**。
//
// 后果（实测，2026-11）：任何一个调了 loadProviderConfig()/loadSettings() 的用例都会把
// 用户钉的价写进全局表；它若不隔离，后面所有「按内置价断言成本」的用例就静默换了价源 ——
// 症状是 TestReducerLLMEndDedupAndUsage 偶发
//
//	cost = 0.000046, want 0.000069
//
// 而单跑必绿。0.000046 逐位等于本机钉价 (40*0.14 + 900*0.014 + 100*0.28)/1e6 ——
// 不是浮点误差、不是计时抖动，是**价表被换了**。这类「测试里另建了一个等价物/验的不是
// 生产那个状态」正是 00 §4.7 的形状，只是它藏在一个金额里，最难认。
//
// 两道闸：
//
//	assertNoUserPrices —— 断言型用例**开头**要求价表为空。泄漏时当场红在这一条用例上，
//	  报错带出被污染的具体条目（provider/model/价），而不是让人去猜一个看不懂的金额。
//	isolateUserPrices —— 任何要读真实 settings 的用例先调它（快照 + **清空** + 还原）。
//	  只快照不清空的话，进入时若已被污染，它会把污染原样传给后面的用例（旧版就是这个 bug）。

// priceTableSnapshot 复制一份用户价表（调用方持锁）。
func priceTableSnapshot(m map[string]map[string]provider.ModelPrice) map[string]map[string]provider.ModelPrice {
	out := make(map[string]map[string]provider.ModelPrice, len(m))
	for k, v := range m {
		inner := make(map[string]provider.ModelPrice, len(v))
		for kk, vv := range v {
			inner[kk] = vv
		}
		out[k] = inner
	}
	return out
}

// userPriceDump 把用户价表压成一行（排序，便于比对/贴进报错）。
func userPriceDump(m map[string]map[string]provider.ModelPrice) string {
	provs := make([]string, 0, len(m))
	for k := range m {
		provs = append(provs, k)
	}
	sort.Strings(provs)
	var parts []string
	for _, p := range provs {
		models := make([]string, 0, len(m[p]))
		for k := range m[p] {
			models = append(models, k)
		}
		sort.Strings(models)
		var ms []string
		for _, k := range models {
			v := m[p][k]
			ms = append(ms, strings.Join([]string{
				k,
				"in=" + trimNum(v.Input),
				"read=" + trimNum(v.CacheRead),
				"out=" + trimNum(v.Output),
			}, " "))
		}
		parts = append(parts, p+"{"+strings.Join(ms, ", ")+"}")
	}
	return strings.Join(parts, " ")
}

func trimNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// priceWritesAllowed 当前有多少个「允许写用户价」的用例正在跑（isolateUserPrices 进出）。
//
// 只在**顺序测试**下安全：本包没有 t.Parallel()（grep 为 0），顺序执行时这个计数就是
// 「当前用例有没有隔离过」。若将来有人开并行，这里必须改成 t-scoped 传值 —— 否则守卫
// 会静默失效（比红更糟）。
var priceWritesAllowed int

// assertNoUserPrices 断言「按内置价断言金额」的用例两头都干净。
//
// 进入时脏 → 立刻红：说明上游有用例泄漏，报错直接给出脏条目（provider/model/价）。
// 离开时脏 → 红在**本用例**：说明本用例自己调了 loadProviderConfig()/loadSettings()
//
//	（读真实 ~/.go-code/settings.json 的 provider[].prices，写进程级全局表）却没隔离。
//	这是「红在源头用例」而不是红在下一个受害用例的关键 —— 后者只会让人看见一个
//	看不懂的金额（cost = 0.000046），查半天才发现是价源被换了。
//
// 本用例**就是要**写用户价的（如 TestLoadSettingsUserPrices、price_refresh_*）：
// 开头改成 isolateUserPrices(t)，它会把写表登记成合法并负责还原。
func assertNoUserPrices(t *testing.T) {
	t.Helper()
	priceTableMu.RLock()
	dump := userPriceDump(userPriceTable)
	priceTableMu.RUnlock()
	if dump != "" {
		t.Fatalf("进程级用户价表非空：%s\n"+
			"本用例要按**内置价**断言金额，但价源已被换掉（结果会静默错，不是浮点误差）。\n"+
			"原因：上游某个用例调了 loadProviderConfig()/loadSettings()（读真实 "+
			"~/.go-code/settings.json）却没先 isolateUserPrices(t)。", dump)
	}
	t.Cleanup(func() {
		if priceWritesAllowed > 0 {
			return // 本用例声明过要写价（isolateUserPrices 负责还原），守卫让位
		}
		priceTableMu.RLock()
		leaked := userPriceDump(userPriceTable)
		priceTableMu.RUnlock()
		if leaked != "" {
			t.Errorf("本用例结束时把进程级用户价表写脏了：%s\n"+
				"这是**源头**：后续所有按内置价断言金额的用例都会静默用错价。\n"+
				"修法：本用例开头加 isolateUserPrices(t)（快照 + 清空 + 还原）。", leaked)
		}
	})
}

// isolateUserPrices 快照全局用户价表、**清空**，并在测试结束时还原。
//
// 为什么必须清空：只快照+还原时，若进入前表已被污染，它会把污染原样传下去（旧版 bug）。
// 清空后本用例读到的是内置价，还原后把进入前的状态还给进程 —— 隔离是「进出都干净」，
// 不是「借了再还」。
func isolateUserPrices(t *testing.T) {
	t.Helper()
	priceTableMu.Lock()
	saved := priceTableSnapshot(userPriceTable)
	if len(saved) > 0 {
		// 进入时就有污染：这不是本用例的责任，但一定要说出来，否则它会一路传到后面。
		t.Logf("注意：进入 isolateUserPrices 时用户价表已非空（%s）—— 是上游用例泄漏，已清空", userPriceDump(saved))
	}
	userPriceTable = map[string]map[string]provider.ModelPrice{}
	priceWritesAllowed++
	priceTableMu.Unlock()
	t.Cleanup(func() {
		priceTableMu.Lock()
		userPriceTable = saved
		priceWritesAllowed--
		priceTableMu.Unlock()
	})
}
