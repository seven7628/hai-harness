package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// 指标存储：跨会话 usage/成本聚合看板的数据源。
//
// 存储抽象（用户拍板）：消费方（metrics 命令 / 前端）不关心底层是文件还是 DB——
// 接口定死，v1 用 FileMetricsStore（JSONL 追加），SQL 版以后换实现零改动。
// 数据在宿主层（bridge），前端只发查询、渲染报告。

// ModelPrice 每百万 token 单价（USD）。CacheRead 用低价（DeepSeek 缓存命中价远低于未命中）；
// CacheWrite 缓存写入价（Anthropic/OpenAI 长缓存 1h 有独立写入价；多数厂商无 = 0，不计费）。
// 类型已下沉到 provider 包（provider.ModelPrice，含 json tags）；此处别名保持桥内旧引用名。
type ModelPrice = provider.ModelPrice

// priceTableMu 保护 priceTable / userPriceTable：启动期内置价只读；Provider 设置热更新
// （loadSettings/set_provider 写 userPriceTable）与事件流并发读（costUsd /
// overrideRegistryLocked）都经此锁。
var priceTableMu sync.RWMutex

// userPriceTable 每模型【用户价】覆盖（USD/1M tokens），维度 provider → model。
// 与内置 priceTable 分离：删除覆盖 = 从本表删条目（内置表永不污染）；读价时用户价优先。
var userPriceTable = map[string]map[string]provider.ModelPrice{}

// priceTable 模型价表（费率已由用户核实 2026-08-13），维度 = provider → model：
// 同模型名在不同 provider 下费率不同（如 deepseek-v4-flash 缓存读取：DeepSeek 直连
// $0.014/M，OpenCode Go $0.0028/M —— 1M 上下文下缓存读取占大头，混记会失真）。
// OpenCode Go 价表来自官方端点表（2026-08）；CacheRead 用低价（缓存命中价远低于未命中）。
// 用户覆盖：Provider 设置面板每模型单价会经 settings.json provider[].prices 落盘，
// 启动/热更新时覆盖此表（见 syncUserPrices；注册表内置价经 overrideRegistryLocked
// 写 info.Cost 时也是查此表 —— 因此覆盖此表 = 全链路（记账/成本字段）一致生效）。
var priceTable = map[string]map[string]ModelPrice{
	"deepseek": {
		"deepseek-v4-flash": {Input: 0.14, CacheRead: 0.014, Output: 0.28},
		"deepseek-chat":     {Input: 0.28, CacheRead: 0.028, Output: 0.56}, // flash 的 2 倍，占位待核
	},
	"opencode": {
		"deepseek-v4-flash": {Input: 0.14, CacheRead: 0.0028, Output: 0.28},
		"deepseek-v4-pro":   {Input: 0.435, CacheRead: 0.003625, Output: 0.87},
		"glm-5.1":           {Input: 1.40, CacheRead: 0.26, Output: 4.40},
		"glm-5.2":           {Input: 1.40, CacheRead: 0.26, Output: 4.40},
		"glm-5.3":           {Input: 1.40, CacheRead: 0.26, Output: 4.40},
		"kimi-k3":           {Input: 3.00, CacheRead: 0.30, Output: 15.00},
		"kimi-k2.7-code":    {Input: 0.95, CacheRead: 0.19, Output: 4.00},
		"kimi-k2.6":         {Input: 0.95, CacheRead: 0.16, Output: 4.00},
		"mimo-v2.5":         {Input: 0.14, CacheRead: 0.0028, Output: 0.28},
		"mimo-v2.5-pro":     {Input: 0.435, CacheRead: 0.003625, Output: 0.87},
		"hy3":               {Input: 0.14, CacheRead: 0.035, Output: 0.58},
	},
}

// priceFor 读生效价：用户价优先（userPriceTable），否则内置价（priceTable）。
func priceFor(provider, model string) (ModelPrice, bool) {
	priceTableMu.RLock()
	defer priceTableMu.RUnlock()
	if ps, ok := userPriceTable[provider]; ok {
		if p, ok := ps[model]; ok {
			return p, true
		}
	}
	ps, ok := priceTable[provider]
	if !ok {
		return ModelPrice{}, false
	}
	p, ok := ps[model]
	return p, ok
}

// userPrices 现有手填价副本（价表刷新合并用；setUserPrice 是整表替换，必须先读旧表再合并）。
func userPrices(prov string) map[string]provider.ModelPrice {
	priceTableMu.RLock()
	defer priceTableMu.RUnlock()
	src := userPriceTable[prov]
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]provider.ModelPrice, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// setUserPrice 写某 provider 的整表用户价覆盖（loadSettings/set_provider 调用；
// 整表替换——prices 里没有的模型 = 删除覆盖，回退内置价）。
func setUserPrice(provider string, prices map[string]provider.ModelPrice) {
	priceTableMu.Lock()
	defer priceTableMu.Unlock()
	if len(prices) == 0 {
		delete(userPriceTable, provider)
		return
	}
	userPriceTable[provider] = prices
}

// costUsd 算一次模型调用的金额（USD，前端展示）。
// 优先 provider 层已按注册表价表（providers.json 单一事实源 + 用户价覆盖）折算的
// µUSD（Usage.Cost，见 provider.FillUsageCost）——与 SDK MaxCost/持久化同口径，
// 事件/checkpoint/指标聚合不再依赖宿主并行价表。
// 回退（历史 JSONL 等 Usage.Cost 未填充的旧数据）：priceFor（用户价 > 内置价表）。
func costUsd(provider, model string, u core.Usage) float64 {
	if u.Cost.Total > 0 {
		return float64(u.Cost.Total) / 1e6
	}
	p, ok := priceFor(provider, model)
	if !ok {
		if provider == "" {
			p, ok = priceFor("deepseek", model)
		}
		if !ok {
			return 0 // 未知 provider/模型无价表：不计费（不阻塞指标）
		}
	}
	// 未命中输入（core 口径唯一实现；下限 0 防异常负数）
	miss := u.CacheMiss()
	// CacheWrite 有价才计（多数厂商无缓存写入价 = 0）
	return (float64(miss)*p.Input + float64(u.CacheRead)*p.CacheRead + float64(u.CacheWrite)*p.CacheWrite + float64(u.Output)*p.Output) / 1e6
}

// MetricsEntry 单次模型调用的原始用量（不含价——价表在聚合层，改价可回溯重算）。
type MetricsEntry struct {
	Timestamp time.Time
	Workspace string
	SessionID string
	Provider  string // 服务该调用的 provider（deepseek/openai/opencode）；价表按 provider+model 查
	Model     string
	Usage     core.Usage
	// Kind 行类型（2026-09-23）：call = 成功调用（llm_end / compress_end）；
	// attempt_failed = 失败尝试（llm_error，Usage 可能为零 = 上游没回用量）。
	// 空 = call（旧行兼容）。用途：调用数与「重试浪费」分开统计，不互相污染。
	Kind string `json:"Kind,omitempty"`
}

// MetricsEntry.Kind 取值。
const (
	MetricsKindCall          = "call"
	MetricsKindAttemptFailed = "attempt_failed"
)

// MetricsQuery 聚合查询。From/To nil = 全部；Workspace 空 = 全局（跨项目）。
type MetricsQuery struct {
	From, To  *time.Time
	Workspace string
	GroupBy   string // 预留："day"|"model"|"workspace"（v1 恒返回全部分组）
}

// Totals 一组指标的 token 合计 + 成本（USD）。
type Totals struct {
	Input int64
	// Output 总输出（含 Reasoning）；Reasoning 是它的**拆分视图**（2026-09-23 补齐：
	// 此前 metrics 完全没有思考维度）——与 Output 并列上抛，做占比/成本分析用，**勿相加**。
	Output     int64
	Reasoning  int64
	CacheRead  int64
	CacheWrite int64 // 写入缓存量（Anthropic cache_creation；其余端点 0）——命中率之外看「重写量」
	// CacheWrite1h ⊆ CacheWrite：1h extended TTL 写入量（Anthropic 专用；其余端点 0）。
	// 用途：区分「5m 短缓存」与「1h 长缓存」的写入规模（成本差异在价表侧，统一设计后置）。
	CacheWrite1h int64
	CostUsd      float64
	// UnpricedCalls 本组里**无可用价**的调用数（Cost.Priced=false 且非旧行）：成本为 0 是
	// 「不知道花了多少」而非免费 —— 展示层据此标注该组成本偏低（如某模型未收录价）。
	UnpricedCalls int64
}

// GroupTotals 一个分组的合计。Key = 日期（YYYY-MM-DD）/ 模型名 / workspace 路径。
type GroupTotals struct {
	Key string
	Totals
}

// DayTotals 单日合计 + 该日的全历史累计 token（0 = 无记录）。
// 热力图「累计」视图用它：窗口只画近一年，累计值仍从最早一条记录算起，不被窗口截断。
// （匿名内嵌 → JSON 与 GroupTotals 同形，前端旧字段读写不变。）
type DayTotals struct {
	GroupTotals
	CumTokens int64
}

// DayModelTotals 单日 × 单模型用量（趋势图分模型折线）。Model = "provider/model"（无 provider
// 时退化为裸模型名）——同模型名在不同 provider 下费率/额度不同，混成一条线会失真。
type DayModelTotals struct {
	Day   string
	Model string
	Totals
}

// MetricsStats 全局统计（使用统计页顶部卡片）。
// 口径（2026-09-19 用户拍板）：累计 = 全历史；峰值 = 单日最高；最长聊天 = 单会话首尾跨度；
// 连续天数 = 连续有使用的自然日。Workspace 过滤生效（= 「当前项目」视角），From/To 不生效
// ——统计是要看「总量/连续性」的，跟随 7/30 天窗口会变成另一个指标。
type MetricsStats struct {
	TotalTokens   int64   // 累计 token（Input+Output）
	TotalCostUsd  float64 // 累计成本（USD，价表现算）
	PeakDayTokens int64   // 单日峰值 token
	PeakDay       string  // 峰值日（YYYY-MM-DD）
	LongestChatMs int64   // 最长会话跨度（毫秒，首尾记录时间差）
	LongestChatAt string  // 该会话末条记录时间（RFC3339）
	CurrentStreak int     // 当前连续天数（今天没用但昨天用了仍连续，见 streakStats）
	LongestStreak int     // 历史最长连续天数
	ActiveDays    int     // 有使用的天数
	Sessions      int     // 会话数
	FirstDay      string  // 最早记录日
	LastDay       string  // 最近记录日

	// 有价性三分桶（2026-09-23，配合 core.Cost.Priced/Source）：成本报表要能回答
	// 「这个数字可信吗」。旧行（Cost.Source 未落盘）单列 unknown —— 既不冒充有价，
	// 也不污染无价分母。
	PricedCalls         int64 // 命中可用价表的调用数
	UnpricedCalls       int64 // 无可用价（含价表标 0）的调用数：成本偏低，非免费
	UnknownPricingCalls int64 // 旧行（字段缺失），有价性未知

	// 失败尝试（2026-09-23 决策「失败尝试也要记账」）：llm_error 每次尝试一行，
	// 与成功调用（call）分开统计 —— 调用数看 call，重试浪费看 RetryCostUsd。
	FailedAttempts int64   // 失败尝试次数（含拿不到用量的：上游未回 usage 也计数）
	RetryCostUsd   float64 // 失败尝试的已产生成本（USD；上游已计费的那部分）
}

// MetricsReport 聚合报告：总合计 + 全局统计 + 四组维度（每日 / 每日×模型 / 分模型 / 分项目）。
type MetricsReport struct {
	Total       Totals
	Stats       MetricsStats
	ByDay       []DayTotals
	ByDayModel  []DayModelTotals
	ByModel     []GroupTotals
	ByWorkspace []GroupTotals
}

// MetricsStore 指标存储抽象。
type MetricsStore interface {
	Record(e MetricsEntry) error
	Aggregate(q MetricsQuery) (MetricsReport, error)
}

// FileMetricsStore JSONL 追加实现（v1）：~/.go-code/metrics.jsonl（全局跨 workspace）。
// 不 fsync（可重建的展示层数据）；查询全扫 + 内存聚合（数据量小，单条 ~200B）。
// 价格在聚合层经 costUsd 现算（用户价覆盖即时生效、改价可回溯重算），无独立价表引用。
type FileMetricsStore struct {
	path string
	mu   sync.Mutex
}

// metricsHomeDir 全局指标目录（~/.go-code，跨 workspace）；home 解析失败回退当前目录。
func metricsHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, ".go-code")
}

// NewFileMetricsStore 创建文件版指标存储；path 指向 metrics.jsonl。
func NewFileMetricsStore(path string) *FileMetricsStore {
	return &FileMetricsStore{path: path}
}

func (s *FileMetricsStore) Record(e MetricsEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil { // 指标含用量/成本，0700
		return err
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// Aggregate 全扫 + 按查询过滤 + 分组聚合。恒返回 Total + Stats + ByDay/ByDayModel/ByModel/
// ByWorkspace（一次给全，前端看板一屏渲染；GroupBy 预留，v1 不据此裁剪）。
//
// 过滤口径（两段）：Workspace 过滤对**全部**字段生效（= 「当前项目」视角）；From/To 只裁剪
// 序列（Total + 四组维度），Stats 恒为全历史 —— 累计 / 峰值 / 连续天数 / 最长聊天是「看总量」
// 的指标，跟随 7/30 天窗口会变成另一个东西（热力图窗口取一年，顶部累计仍是全史）。
func (s *FileMetricsStore) Aggregate(q MetricsQuery) (MetricsReport, error) {
	var rep MetricsReport
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return rep, nil // 无记录：空报告
		}
		return rep, err
	}
	defer f.Close()

	dayIdx := map[string]int{}
	modelIdx := map[string]int{}
	wsIdx := map[string]int{}
	dmIdx := map[string]int{}
	var byDayGroups []GroupTotals    // 窗口内每日合计（累计值在收尾时按全历史补齐 → DayTotals）
	allDays := map[string]int64{}    // 全历史每日 token（累计曲线 / 峰值 / 连续天数）
	spanLo := map[string]time.Time{} // 会话首条记录时间（最长聊天时长）
	spanHi := map[string]time.Time{} // 会话末条记录时间
	var stats MetricsStats

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var e MetricsEntry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue // 坏行跳过（向前兼容）
		}
		if q.Workspace != "" && e.Workspace != q.Workspace {
			continue
		}
		cost := costUsd(e.Provider, e.Model, e.Usage) // 价表在聚合层：改价重算历史
		// 行类型（2026-09-23）：失败尝试与成功调用分开计数 —— 调用数看 call，
		// 重试浪费看 RetryCostUsd（拿不到用量的失败尝试也计数，只是成本为 0）。
		if e.Kind == MetricsKindAttemptFailed {
			stats.FailedAttempts++
			stats.RetryCostUsd += cost
		}
		// 有价性三分桶（2026-09-23）：Source 为空 = 旧行（字段尚未落盘）→ unknown，
		// 既不冒充「有价」也不污染「无价」分母。**只有真的产生用量的行**才进桶 ——
		// 失败尝试常拿不到用量（上游未回 usage），那是「未知」而不是「无价」。
		hasUsage := e.Usage.Input+e.Usage.Output+e.Usage.CacheRead+e.Usage.CacheWrite > 0
		unpriced := false
		switch {
		case !hasUsage:
			// 不计入任何桶（无用量信息）
		case e.Usage.Cost.Source == "":
			stats.UnknownPricingCalls++
		case e.Usage.Cost.Priced:
			stats.PricedCalls++
		default:
			stats.UnpricedCalls++
			unpriced = true
		}
		day := e.Timestamp.Local().Format("2006-01-02")
		// 全历史段（Stats 数据源；不受 From/To 影响）
		allDays[day] += e.Usage.Input + e.Usage.Output
		stats.TotalTokens += e.Usage.Input + e.Usage.Output
		stats.TotalCostUsd += cost
		if e.SessionID != "" {
			if lo, ok := spanLo[e.SessionID]; !ok || e.Timestamp.Before(lo) {
				spanLo[e.SessionID] = e.Timestamp
			}
			if hi, ok := spanHi[e.SessionID]; !ok || e.Timestamp.After(hi) {
				spanHi[e.SessionID] = e.Timestamp
			}
		}
		// 窗口段（序列数据源）
		if q.From != nil && e.Timestamp.Before(*q.From) {
			continue
		}
		if q.To != nil && e.Timestamp.After(*q.To) {
			continue
		}
		rep.Total.Input += e.Usage.Input
		rep.Total.Output += e.Usage.Output
		rep.Total.Reasoning += e.Usage.Reasoning
		rep.Total.CacheRead += e.Usage.CacheRead
		rep.Total.CacheWrite += e.Usage.CacheWrite
		rep.Total.CacheWrite1h += e.Usage.CacheWrite1h
		rep.Total.CostUsd += cost
		if unpriced {
			rep.Total.UnpricedCalls++
		}

		addGroup := func(idx map[string]int, groups *[]GroupTotals, key string) {
			i, ok := idx[key]
			if !ok {
				i = len(*groups)
				idx[key] = i
				*groups = append(*groups, GroupTotals{Key: key})
			}
			g := &(*groups)[i]
			g.Input += e.Usage.Input
			g.Output += e.Usage.Output
			g.Reasoning += e.Usage.Reasoning
			g.CacheRead += e.Usage.CacheRead
			g.CacheWrite += e.Usage.CacheWrite
			g.CacheWrite1h += e.Usage.CacheWrite1h
			g.CostUsd += cost
			if unpriced {
				g.UnpricedCalls++
			}
		}
		addGroup(dayIdx, &byDayGroups, day)
		addGroup(modelIdx, &rep.ByModel, e.Model)
		addGroup(wsIdx, &rep.ByWorkspace, e.Workspace)

		// 每日 × 模型（趋势图分模型折线）：provider 限定名，便于同模型跨 provider 区分
		dmKey := dmDayKey(day, providerModel(e.Provider, e.Model))
		i, ok := dmIdx[dmKey]
		if !ok {
			i = len(rep.ByDayModel)
			dmIdx[dmKey] = i
			rep.ByDayModel = append(rep.ByDayModel, DayModelTotals{Day: day, Model: providerModel(e.Provider, e.Model)})
		}
		dg := &rep.ByDayModel[i]
		dg.Input += e.Usage.Input
		dg.Output += e.Usage.Output
		dg.Reasoning += e.Usage.Reasoning
		dg.CacheRead += e.Usage.CacheRead
		dg.CacheWrite += e.Usage.CacheWrite
		dg.CacheWrite1h += e.Usage.CacheWrite1h
		dg.CostUsd += cost
		if unpriced {
			dg.UnpricedCalls++
		}
	}
	if err := sc.Err(); err != nil {
		return rep, err
	}

	// 全历史统计：累计曲线（供「累计」视图） + 峰值 + 连续天数 + 最长会话
	days := make([]string, 0, len(allDays))
	for k := range allDays {
		days = append(days, k)
	}
	sort.Strings(days)
	cumOf := make(map[string]int64, len(days))
	var cum int64
	for _, k := range days {
		cum += allDays[k]
		cumOf[k] = cum
		if allDays[k] > stats.PeakDayTokens {
			stats.PeakDayTokens, stats.PeakDay = allDays[k], k
		}
	}
	if len(days) > 0 {
		stats.FirstDay, stats.LastDay = days[0], days[len(days)-1]
	}
	stats.ActiveDays = len(days)
	stats.CurrentStreak, stats.LongestStreak = streakStats(days, time.Now())
	stats.Sessions = len(spanLo)
	for id, lo := range spanLo {
		d := spanHi[id].Sub(lo)
		if ms := d.Milliseconds(); ms > stats.LongestChatMs {
			stats.LongestChatMs = ms
			stats.LongestChatAt = spanHi[id].Format(time.RFC3339)
		}
	}
	rep.Stats = stats

	// 稳定序：day/model/workspace 按 key 升序，(day, model) 按 day → model 升序
	for _, g := range byDayGroups {
		rep.ByDay = append(rep.ByDay, DayTotals{GroupTotals: g, CumTokens: cumOf[g.Key]})
	}
	sort.Slice(rep.ByDay, func(i, j int) bool { return rep.ByDay[i].Key < rep.ByDay[j].Key })
	sort.Slice(rep.ByModel, func(i, j int) bool { return rep.ByModel[i].Key < rep.ByModel[j].Key })
	sort.Slice(rep.ByWorkspace, func(i, j int) bool { return rep.ByWorkspace[i].Key < rep.ByWorkspace[j].Key })
	sort.Slice(rep.ByDayModel, func(i, j int) bool {
		if rep.ByDayModel[i].Day != rep.ByDayModel[j].Day {
			return rep.ByDayModel[i].Day < rep.ByDayModel[j].Day
		}
		return rep.ByDayModel[i].Model < rep.ByDayModel[j].Model
	})
	return rep, nil
}

// providerModel 趋势图分组名：provider 非空 → "provider/model"（与图例「deepseek/deepseek-v4.1-flash」
// 一致）；空 provider（旧数据/自建网关）→ 裸模型名。
func providerModel(prov, model string) string {
	if prov == "" || model == "" {
		return model
	}
	return prov + "/" + model
}

// dmDayKey ByDayModel 的去重键（day 与 model 之间用不可见分隔，避免模型名含 '|' 时串键）。
func dmDayKey(day, model string) string { return day + "\x00" + model }

// streakStats 连续天数：历史最长连续段 + 当前段。当前段以「今天或昨天」为锚 —— 今天还没用
// 但昨天用过仍算连续（清早打开不该看到归零）；更早则当前段 = 0（已中断）。
// 入参 days 为升序去重的本地日期（YYYY-MM-DD）；按自然日比较（AddDate(0,0,1)，跨夏令时不丢天）。
func streakStats(days []string, now time.Time) (cur, longest int) {
	if len(days) == 0 {
		return 0, 0
	}
	nextOf := func(k string) string {
		d, err := time.ParseInLocation("2006-01-02", k, time.Local)
		if err != nil {
			return ""
		}
		return d.AddDate(0, 0, 1).Format("2006-01-02")
	}
	run := 0
	for i, k := range days {
		if i > 0 && nextOf(days[i-1]) == k {
			run++
		} else {
			run = 1
		}
		if run > longest {
			longest = run
		}
	}
	last := days[len(days)-1]
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	if last != today && last != yesterday {
		return 0, longest
	}
	cur = 1
	for i := len(days) - 1; i > 0; i-- {
		if nextOf(days[i-1]) != days[i] {
			break
		}
		cur++
	}
	return cur, longest
}
