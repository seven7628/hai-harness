package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

func testStore(t *testing.T) *FileMetricsStore {
	t.Helper()
	return NewFileMetricsStore(filepath.Join(t.TempDir(), "metrics.jsonl"))
}

func mustRecord(t *testing.T, s *FileMetricsStore, ts time.Time, ws, sess, prov, model string, u core.Usage) {
	t.Helper()
	if err := s.Record(MetricsEntry{Timestamp: ts, Workspace: ws, SessionID: sess, Provider: prov, Model: model, Usage: u}); err != nil {
		t.Fatal(err)
	}
}

// 空存储 → 空报告（无文件/无记录不报错）
func TestMetricsAggregateEmpty(t *testing.T) {
	s := testStore(t)
	rep, err := s.Aggregate(MetricsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total.Input != 0 || rep.Total.CostUsd != 0 || len(rep.ByDay) != 0 {
		t.Fatalf("empty store should return empty report, got %+v", rep.Total)
	}
}

// Record + Aggregate：按 day/model/workspace 分组 + 价表 cost（含 CacheRead 低价）+ 过滤
func TestMetricsRecordAggregate(t *testing.T) {
	s := testStore(t)
	day1 := time.Date(2026, 8, 12, 10, 0, 0, 0, time.Local)
	day2 := time.Date(2026, 8, 13, 10, 0, 0, 0, time.Local)

	// CacheWrite：Anthropic 系端点独有（cache_creation）；聚合必须与 CacheRead 并列上抛
	//（命中率之外看「重写量」——缓存前缀失效时命中归零、写入顶到整段上下文）
	mustRecord(t, s, day1, "/wsA", "s1", "deepseek", "deepseek-v4-flash", core.Usage{Input: 1000, CacheRead: 900, CacheWrite: 60, Output: 100})
	mustRecord(t, s, day2, "/wsA", "s2", "deepseek", "deepseek-v4-flash", core.Usage{Input: 2000, Output: 500})
	mustRecord(t, s, day2, "/wsB", "s1", "deepseek", "unknown-model", core.Usage{Input: 5000, Output: 1000})

	rep, err := s.Aggregate(MetricsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	// Total tokens
	if rep.Total.Input != 8000 || rep.Total.Output != 1600 || rep.Total.CacheRead != 900 || rep.Total.CacheWrite != 60 {
		t.Fatalf("total = %+v", rep.Total)
	}
	// cost：flash = (miss40*0.15 + 900*0.003 + 60*0 + 100*0.6 + 2000*0.15 + 500*0.6)/1e6
	//        = (6+2.7+0+60+300+300)/1e6 = 668.7e-6
	//（Input=1000 为总输入，含 CacheRead=900 与 CacheWrite=60 → miss=40；写入价 0 不计费）
	if want := ((1000-900-60)*0.15 + 900*0.003 + 100*0.6 + 2000*0.15 + 500*0.6) / 1e6; abs(rep.Total.CostUsd-want) > 1e-12 {
		t.Fatalf("total cost = %v, want %v", rep.Total.CostUsd, want)
	}
	// ByDay（升序）
	if len(rep.ByDay) != 2 {
		t.Fatalf("byday = %+v", rep.ByDay)
	}
	if rep.ByDay[0].Key != "2026-08-12" || rep.ByDay[0].Input != 1000 {
		t.Fatalf("byday[0] = %+v", rep.ByDay[0])
	}
	if rep.ByDay[1].Key != "2026-08-13" || rep.ByDay[1].Input != 7000 || rep.ByDay[1].CostUsd <= 0 {
		t.Fatalf("byday[1] = %+v", rep.ByDay[1])
	}
	// ByModel：flash 计费、unknown 不计
	if len(rep.ByModel) != 2 || rep.ByModel[0].Key != "deepseek-v4-flash" || rep.ByModel[1].Key != "unknown-model" {
		t.Fatalf("bymodel = %+v", rep.ByModel)
	}
	if rep.ByModel[1].CostUsd != 0 {
		t.Fatalf("unknown model cost should be 0, got %+v", rep.ByModel[1])
	}
	// ByWorkspace
	if len(rep.ByWorkspace) != 2 {
		t.Fatalf("byws = %+v", rep.ByWorkspace)
	}
	if rep.ByWorkspace[0].Key != "/wsA" || rep.ByWorkspace[0].Input != 3000 {
		t.Fatalf("byws[0] = %+v", rep.ByWorkspace[0])
	}

	// From 过滤：只留 8-13
	f := time.Date(2026, 8, 13, 0, 0, 0, 0, time.Local)
	rep2, err := s.Aggregate(MetricsQuery{From: &f})
	if err != nil {
		t.Fatal(err)
	}
	if rep2.Total.Input != 7000 || len(rep2.ByDay) != 1 || rep2.ByDay[0].Key != "2026-08-13" {
		t.Fatalf("from-filtered = %+v", rep2)
	}

	// Workspace 过滤：只 /wsB（unknown model → cost 0）
	rep3, err := s.Aggregate(MetricsQuery{Workspace: "/wsB"})
	if err != nil {
		t.Fatal(err)
	}
	if rep3.Total.Input != 5000 || rep3.Total.CostUsd != 0 || len(rep3.ByWorkspace) != 1 {
		t.Fatalf("ws-filtered = %+v", rep3)
	}
}

// provider+model 价表：成本按记录时 provider 正确区分（价表维度是 provider → model）。
func TestMetricsProviderAwarePricing(t *testing.T) {
	s := testStore(t)
	ts := time.Date(2026, 8, 14, 10, 0, 0, 0, time.Local)
	u := core.Usage{Input: 1000, CacheRead: 1000, Output: 1000}
	mustRecord(t, s, ts, "/wsDirect", "s1", "deepseek", "deepseek-v4-flash", u)
	mustRecord(t, s, ts, "/wsGo", "s1", "opencode", "deepseek-v4-flash", u)

	// opencode 缓存读取按 opencode 价（0.003/M）
	rep, err := s.Aggregate(MetricsQuery{Workspace: "/wsGo"})
	if err != nil {
		t.Fatal(err)
	}
	// Input=1000 为总输入且全命中（CacheRead=1000）→ miss=0，只计缓存低价
	wantGo := (0*0.15 + 1000*0.003 + 1000*0.6) / 1e6 // 603e-6
	if abs(rep.Total.CostUsd-wantGo) > 1e-12 {
		t.Fatalf("opencode flash cost = %v, want %v", rep.Total.CostUsd, wantGo)
	}

	// 直连仍按 DeepSeek 价（2026-10 起 opencode 按官方价转售 → 两者同为 0.15/0.003/0.6，
	// 原「provider 不同则价不同」的判别改由下方 glm-5.1 用例承担）
	repD, err := s.Aggregate(MetricsQuery{Workspace: "/wsDirect"})
	if err != nil {
		t.Fatal(err)
	}
	wantDirect := (0*0.15 + 1000*0.003 + 1000*0.6) / 1e6 // 603e-6
	if abs(repD.Total.CostUsd-wantDirect) > 1e-12 {
		t.Fatalf("deepseek flash cost = %v, want %v", repD.Total.CostUsd, wantDirect)
	}

	// 价表维度是 provider：同名模型只有该 provider 有价才计费（glm-5.1 在 opencode 有价、
	// 在 deepseek 无条目 → 后者不计费）。这条保证「同模型名不跨 provider 套价」。
	s2 := testStore(t)
	ts2 := time.Date(2026, 8, 15, 10, 0, 0, 0, time.Local)
	mustRecord(t, s2, ts2, "/wsGo", "s1", "opencode", "glm-5.1", core.Usage{Input: 1000, Output: 1000})
	mustRecord(t, s2, ts2, "/wsDs", "s1", "deepseek", "glm-5.1", core.Usage{Input: 1000, Output: 1000})
	repGo, err := s2.Aggregate(MetricsQuery{Workspace: "/wsGo"})
	if err != nil {
		t.Fatal(err)
	}
	if want := (1000*1.40 + 1000*4.40) / 1e6; abs(repGo.Total.CostUsd-want) > 1e-12 {
		t.Fatalf("opencode glm-5.1 cost = %v, want %v", repGo.Total.CostUsd, want)
	}
	repDs, err := s2.Aggregate(MetricsQuery{Workspace: "/wsDs"})
	if err != nil {
		t.Fatal(err)
	}
	if repDs.Total.CostUsd != 0 {
		t.Fatalf("deepseek 无 glm-5.1 条目 → 不应计费, got %v", repDs.Total.CostUsd)
	}
}

// 旧数据兼容：Provider 字段引入前的 JSONL 行（Provider 空）回退 DeepSeek 直连价。
func TestMetricsCostEmptyProviderFallback(t *testing.T) {
	s := testStore(t)
	ts := time.Date(2026, 8, 14, 10, 0, 0, 0, time.Local)
	if err := s.Record(MetricsEntry{Timestamp: ts, Workspace: "/wsA", SessionID: "s1", Model: "deepseek-v4-flash", Usage: core.Usage{Input: 1000, CacheRead: 1000, Output: 1000}}); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Aggregate(MetricsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	want := (0*0.15 + 1000*0.003 + 1000*0.6) / 1e6 // Input 全命中 → 只计缓存低价 + 输出
	if abs(rep.Total.CostUsd-want) > 1e-12 {
		t.Fatalf("empty-provider fallback cost = %v, want %v", rep.Total.CostUsd, want)
	}
}

// provider 层已按价表折算的 µUSD（Usage.Cost.Total）优先 —— 前端展示与
// SDK/MaxCost/持久化同口径，不再依赖宿主并行价表（此前第三方聚合 provider 在宿主
// 硬编码表外 → costUsd 恒 0 的根因）。
func TestCostUsdPrefersFilledUsageCost(t *testing.T) {
	// command-code 这类第三方聚合 provider 不在宿主内置表：Usage.Cost 已由
	// FillUsageCost 折算（µUSD）→ costUsd 直接换算，宿主价表缺失不影响。
	u := core.Usage{Input: 1_000_000, Output: 500_000, Cost: core.Cost{Input: 100_000, Output: 50_000, Total: 150_000}}
	if got := costUsd("command-code", "deepseek/deepseek-v4-flash", u); abs(got-0.15) > 1e-12 {
		t.Fatalf("costUsd(µUSD 优先) = %v, want 0.15", got)
	}
	// 旧数据回退（Usage.Cost 未填充）：仍走宿主价表 deepseek 直连
	u2 := core.Usage{Input: 1000, CacheRead: 1000, Output: 1000}
	if got := costUsd("deepseek", "deepseek-v4-flash", u2); abs(got-((0*0.15+1000*0.003+1000*0.6)/1e6)) > 1e-12 {
		t.Fatalf("costUsd(旧数据回退) = %v", got)
	}
}

// 注册表价表（providers.json 内置价）对第三方 provider 生效：FillUsageCost 折算出的
// µUSD 与注册表内置价一致（此前 override 抹零/宿主表缺第三方 → 成本全 0）。
func TestFillUsageCostThirdPartyBuiltinPrice(t *testing.T) {
	reg := provider.NewRegistry()
	u := core.Usage{Input: 1_000_000, Output: 500_000}
	reg.FillUsageCost("openrouter", "deepseek/deepseek-v4-flash", &u)
	if u.Cost.Total <= 0 {
		t.Fatalf("第三方聚合 provider 内置价应可折算成本, got %+v", u.Cost)
	}
	// 与注册表价表自洽（µUSD = tokens × $/M）
	p, ok := reg.PriceFor("openrouter", "deepseek/deepseek-v4-flash")
	if !ok {
		t.Fatal("openrouter/deepseek/deepseek-v4-flash 应命中注册表内置价")
	}
	want := int64(1_000_000*p.Input + 500_000*p.Output)
	if u.Cost.Total != want {
		t.Fatalf("cost total = %d, want %d（p=%+v）", u.Cost.Total, want, p)
	}
}

// overrideRegistryLocked 对已配置 provider 同步时不得抹掉未覆盖模型的内置价
// （此前以零值 ModelInfo 整体 Override → Lookup 恒命中 override 零值 → 价全 0）。
func TestOverrideRegistryKeepsBuiltinCost(t *testing.T) {
	// 隔离全局价表（本包其它测试依赖内置价）
	priceTableMu.Lock()
	saved := userPriceTable
	userPriceTable = map[string]map[string]provider.ModelPrice{}
	priceTableMu.Unlock()
	t.Cleanup(func() {
		priceTableMu.Lock()
		userPriceTable = saved
		priceTableMu.Unlock()
	})

	pc := &providerConfig{
		registry: provider.NewRegistry(),
		// deepseek 的 deepseek-v4-flash 在内置注册表有价 0.14/0.28；模型表驱动 override
		models:    map[string]map[string]int64{"deepseek": {"deepseek-v4-flash": 1_000_000}},
		maxTokens: map[string]map[string]int64{"deepseek": {"deepseek-v4-flash": 384_000}},
	}
	pc.syncRegistry()
	p, ok := pc.registry.PriceFor("deepseek", "deepseek-v4-flash")
	if !ok || p.Input != 0.15 || p.Output != 0.6 {
		t.Fatalf("未覆盖模型应保留注册表内置价, got %+v ok=%v", p, ok)
	}
	// 二次 sync（幂等路径）不得因首次 override 残留而抹价
	pc.syncRegistry()
	p, ok = pc.registry.PriceFor("deepseek", "deepseek-v4-flash")
	if !ok || p.Input != 0.15 {
		t.Fatalf("二次 sync 后内置价丢失, got %+v ok=%v", p, ok)
	}
}

// 全局统计（使用统计页顶部卡片）：累计/峰值/最长聊天/连续天数 —— 全历史口径，
// 不受 From/To 裁剪（否则「累计/连续」会随 7/30 天窗口变成另一个指标）。
func TestMetricsStatsAllTime(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 8, 10, 9, 0, 0, 0, time.Local)
	// 三天连续：8-10 / 8-11 / 8-12（8-13 断档）→ 最长连续 3
	mustRecord(t, s, base, "/wsA", "s1", "deepseek", "deepseek-v4-flash", core.Usage{Input: 1000, Output: 100})
	mustRecord(t, s, base.AddDate(0, 0, 1), "/wsA", "s1", "deepseek", "deepseek-v4-flash", core.Usage{Input: 2000, Output: 200})
	mustRecord(t, s, base.AddDate(0, 0, 2), "/wsA", "s2", "deepseek", "deepseek-v4-pro", core.Usage{Input: 4000, Output: 400})
	mustRecord(t, s, base.AddDate(0, 0, 4), "/wsB", "s3", "opencode", "deepseek-v4-flash", core.Usage{Input: 8000, Output: 800})

	rep, err := s.Aggregate(MetricsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	st := rep.Stats
	if st.TotalTokens != 1000+100+2000+200+4000+400+8000+800 {
		t.Fatalf("累计 token = %d", st.TotalTokens)
	}
	if st.PeakDayTokens != 8800 || st.PeakDay != "2026-08-14" {
		t.Fatalf("峰值 = %d @ %s（应 8800 @ 2026-08-14）", st.PeakDayTokens, st.PeakDay)
	}
	if st.FirstDay != "2026-08-10" || st.LastDay != "2026-08-14" || st.ActiveDays != 4 {
		t.Fatalf("区间 = %s..%s activeDays=%d", st.FirstDay, st.LastDay, st.ActiveDays)
	}
	// s1 跨 8-10 → 8-11（24h）；s2/s3 单条（0）
	if st.LongestChatMs != 24*time.Hour.Milliseconds() || st.LongestChatAt == "" {
		t.Fatalf("最长聊天 = %dms at %s", st.LongestChatMs, st.LongestChatAt)
	}
	if st.Sessions != 3 {
		t.Fatalf("会话数 = %d，应 3", st.Sessions)
	}
	if st.LongestStreak != 3 || st.CurrentStreak != 0 {
		t.Fatalf("连续天数 当前=%d 最长=%d（最新记录距今 >1 天 → 当前 0，最长 3）", st.CurrentStreak, st.LongestStreak)
	}

	// 窗口过滤只裁剪序列，统计保持全历史
	from := base.AddDate(0, 0, 2)
	rep2, err := s.Aggregate(MetricsQuery{From: &from})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.ByDay) != 2 || rep2.Total.Input != 12000 {
		t.Fatalf("窗口序列 = %d 天 total=%d（应 2 天 / 12000）", len(rep2.ByDay), rep2.Total.Input)
	}
	if rep2.Stats.TotalTokens != st.TotalTokens || rep2.Stats.LongestStreak != 3 || rep2.Stats.ActiveDays != 4 {
		t.Fatalf("窗口内的统计被裁剪了: %+v", rep2.Stats)
	}
	if rep2.Stats.TotalCostUsd != st.TotalCostUsd {
		t.Fatalf("窗口内累计成本被裁剪: %v vs %v", rep2.Stats.TotalCostUsd, st.TotalCostUsd)
	}

	// Workspace 过滤对统计生效（= 当前项目视角）
	repB, err := s.Aggregate(MetricsQuery{Workspace: "/wsB"})
	if err != nil {
		t.Fatal(err)
	}
	if repB.Stats.TotalTokens != 8800 || repB.Stats.ActiveDays != 1 || repB.Stats.Sessions != 1 {
		t.Fatalf("按项目过滤后的统计 = %+v", repB.Stats)
	}
}

// 每日累计（热力图「累计」视图）：CumTokens 从最早记录累加，窗口只决定画哪些天。
func TestMetricsCumulativeAndDayModel(t *testing.T) {
	s := testStore(t)
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.Local)
	mustRecord(t, s, base, "/wsA", "s1", "deepseek", "deepseek-v4-flash", core.Usage{Input: 100, Output: 0})
	mustRecord(t, s, base.AddDate(0, 0, 1), "/wsA", "s1", "deepseek", "deepseek-v4-flash", core.Usage{Input: 200, Output: 0})
	mustRecord(t, s, base.AddDate(0, 0, 1), "/wsA", "s1", "opencode", "glm-5.1", core.Usage{Input: 50, Output: 0})
	mustRecord(t, s, base.AddDate(0, 0, 2), "/wsA", "s1", "deepseek", "deepseek-v4-flash", core.Usage{Input: 400, Output: 0})

	// 只取最后一天：累计值仍含更早的记录（100+250）
	from := base.AddDate(0, 0, 2)
	rep, err := s.Aggregate(MetricsQuery{From: &from})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ByDay) != 1 || rep.ByDay[0].CumTokens != 750 {
		t.Fatalf("累计序列 = %+v（应 1 天 / 750）", rep.ByDay)
	}
	// 全窗口：累计单调递增且末值 = 总量
	repAll, _ := s.Aggregate(MetricsQuery{})
	var prev int64
	for _, d := range repAll.ByDay {
		if d.CumTokens <= prev {
			t.Fatalf("累计应单调递增: %+v", repAll.ByDay)
		}
		prev = d.CumTokens
	}
	if prev != 750 {
		t.Fatalf("末值 = %d，应 750", prev)
	}

	// 每日 × 模型：provider 限定名，同日多模型分行，按 day → model 升序
	if len(repAll.ByDayModel) != 4 {
		t.Fatalf("day×model = %+v", repAll.ByDayModel)
	}
	got := repAll.ByDayModel[1]
	if got.Day != "2026-09-02" || got.Model != "deepseek/deepseek-v4-flash" || got.Input != 200 {
		t.Fatalf("day×model[1] = %+v", got)
	}
	if repAll.ByDayModel[2].Model != "opencode/glm-5.1" {
		t.Fatalf("同日第二模型 = %+v", repAll.ByDayModel[2])
	}
	// 无 provider 的历史数据退化为裸模型名
	if providerModel("", "m") != "m" || providerModel("deepseek", "") != "" || providerModel("deepseek", "m") != "deepseek/m" {
		t.Fatal("providerModel 命名口径错误")
	}
}

// streakStats 连续天数：今天/昨天为锚，跨月跨年按自然日算（含夏令时安全）。
func TestStreakStats(t *testing.T) {
	now := time.Date(2026, 9, 19, 22, 0, 0, 0, time.Local) // 今天 = 09-19
	cases := []struct {
		name         string
		days         []string
		cur, longest int
	}{
		{"空", nil, 0, 0},
		{"仅今天", []string{"2026-09-19"}, 1, 1},
		{"到今天连续三天", []string{"2026-09-17", "2026-09-18", "2026-09-19"}, 3, 3},
		{"昨天为锚（今天还没用）", []string{"2026-09-17", "2026-09-18"}, 2, 2},
		{"前天截止 → 已中断", []string{"2026-09-16", "2026-09-17"}, 0, 2},
		{"历史最长段在中间", []string{"2026-08-30", "2026-08-31", "2026-09-01", "2026-09-02", "2026-09-18", "2026-09-19"}, 2, 4},
		{"跨月连续", []string{"2026-08-31", "2026-09-01"}, 0, 2},
		{"断档分段", []string{"2026-09-01", "2026-09-05", "2026-09-06"}, 0, 2},
	}
	for _, c := range cases {
		cur, longest := streakStats(c.days, now)
		if cur != c.cur || longest != c.longest {
			t.Fatalf("%s: cur=%d longest=%d，应 cur=%d longest=%d", c.name, cur, longest, c.cur, c.longest)
		}
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// TestMetricsUnpricedRatio 有价性三分桶（2026-09-23）：成本报表要能回答「这个数字可信吗」。
//   - 有价（Cost.Priced=true）→ PricedCalls
//   - 无价（价表标 0 / 无条目）→ UnpricedCalls + 该组 Totals.UnpricedCalls（成本为 0 是
//     「不知道花了多少」而非免费）
//   - 旧行（Cost.Source 未落盘）→ UnknownPricingCalls：既不冒充有价，也不污染无价分母
func TestMetricsUnpricedRatio(t *testing.T) {
	s := testStore(t)
	day := time.Date(2026, 9, 23, 10, 0, 0, 0, time.Local)

	// 有价：金额已由 provider 填好（Cost.Total>0 → 聚合直接用）
	mustRecord(t, s, day, "/ws", "s1", "deepseek", "deepseek-v4-flash",
		core.Usage{Input: 1000, Output: 100, Cost: core.Cost{Input: 14, Output: 28, Total: 42,
			Priced: true, Source: core.CostSourceBuiltin}})
	// 无价：价表标 0（explicit_zero）
	mustRecord(t, s, day, "/ws", "s1", "zhipu", "glm-4.5",
		core.Usage{Input: 2000, Output: 200, Cost: core.Cost{Priced: false, Source: core.CostSourceExplicitZero}})
	// 旧行：Cost.Source 未落盘（有价性未知）
	mustRecord(t, s, day, "/ws", "s1", "deepseek", "deepseek-v4-flash", core.Usage{Input: 500, Output: 50})

	rep, err := s.Aggregate(MetricsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stats.PricedCalls != 1 || rep.Stats.UnpricedCalls != 1 || rep.Stats.UnknownPricingCalls != 1 {
		t.Fatalf("三分桶 = priced %d / unpriced %d / unknown %d, want 1/1/1",
			rep.Stats.PricedCalls, rep.Stats.UnpricedCalls, rep.Stats.UnknownPricingCalls)
	}
	if rep.Total.UnpricedCalls != 1 {
		t.Fatalf("窗口 Total.UnpricedCalls = %d, want 1", rep.Total.UnpricedCalls)
	}
	// 分模型：无价调用能定位到具体模型（展示层据此给该行标注「成本偏低」）
	var zhipu *GroupTotals
	for i := range rep.ByModel {
		if rep.ByModel[i].Key == "glm-4.5" {
			zhipu = &rep.ByModel[i]
		}
	}
	if zhipu == nil || zhipu.UnpricedCalls != 1 {
		t.Fatalf("ByModel glm-4.5 UnpricedCalls 缺失: %+v", rep.ByModel)
	}
}

// mustRecordKind 带行类型（Kind）的记录：失败尝试（attempt_failed）与成功调用分开统计。
func mustRecordKind(t *testing.T, s *FileMetricsStore, ts time.Time, prov, model string, u core.Usage, kind string) {
	t.Helper()
	if err := s.Record(MetricsEntry{Timestamp: ts, Workspace: "/ws", SessionID: "s1", Provider: prov, Model: model, Usage: u, Kind: kind}); err != nil {
		t.Fatal(err)
	}
}

// TestMetricsFailedAttempts 失败尝试记账（2026-09-23 决策）：llm_error 每次尝试一行，
//   - FailedAttempts 计数（含拿不到用量的：上游未回 usage 也计数）
//   - RetryCostUsd = 失败尝试的已产生成本（重试浪费，不再凭空消失）
//   - 拿不到用量的失败行**不进**有价性三分桶（那是「未知」不是「无价」）
func TestMetricsFailedAttempts(t *testing.T) {
	s := testStore(t)
	day := time.Date(2026, 9, 23, 11, 0, 0, 0, time.Local)

	// 成功调用（有价）
	mustRecordKind(t, s, day, "deepseek", "deepseek-v4-flash",
		core.Usage{Input: 1000, Output: 100, Cost: core.Cost{Input: 14, Output: 28, Total: 42,
			Priced: true, Source: core.CostSourceBuiltin}}, MetricsKindCall)
	// 失败尝试：拿到部分用量与成本（上游已计费）
	mustRecordKind(t, s, day, "deepseek", "deepseek-v4-flash",
		core.Usage{Input: 300, Output: 10, Cost: core.Cost{Input: 4, Output: 3, Total: 7,
			Priced: true, Source: core.CostSourceBuiltin}}, MetricsKindAttemptFailed)
	// 失败尝试：拿不到用量（OpenAI 系协议 usage 只在收尾帧）
	mustRecordKind(t, s, day, "deepseek", "deepseek-v4-flash", core.Usage{}, MetricsKindAttemptFailed)

	rep, err := s.Aggregate(MetricsQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Stats.FailedAttempts != 2 {
		t.Fatalf("FailedAttempts = %d, want 2", rep.Stats.FailedAttempts)
	}
	if want := 7e-6; abs(rep.Stats.RetryCostUsd-want) > 1e-12 {
		t.Fatalf("RetryCostUsd = %v, want %v（失败尝试的已产生成本）", rep.Stats.RetryCostUsd, want)
	}
	// 有价性三分桶只数「真的有用量的行」：成功 1 行有价；两行失败尝试中一行有用量（有价）
	// 一行无用量（不进桶）
	if rep.Stats.PricedCalls != 2 || rep.Stats.UnpricedCalls != 0 || rep.Stats.UnknownPricingCalls != 0 {
		t.Fatalf("三分桶 = priced %d / unpriced %d / unknown %d, want 2/0/0",
			rep.Stats.PricedCalls, rep.Stats.UnpricedCalls, rep.Stats.UnknownPricingCalls)
	}
	// 重试浪费已计入总成本（真花掉的钱）
	if want := (42 + 7) * 1e-6; abs(rep.Stats.TotalCostUsd-want) > 1e-12 {
		t.Fatalf("TotalCostUsd = %v, want %v（含重试浪费）", rep.Stats.TotalCostUsd, want)
	}
}
