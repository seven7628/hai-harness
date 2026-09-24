// price_refresh.go —— refresh_prices 命令：用实时价源补「无价」模型，免发版。
//
// 背景（实测 2026-09-21）：内置价表是**编译期快照**（provider/providers.json 经 go:embed），
// 网关上新模型/改价必须重新发版才生效；期间这些 (provider, model) 的成本恒 0 ——
// metrics.jsonl 96,738 次调用只有 33.4% 有价，缺失几乎全在聚合型网关
// （command-code / api-codewith / opencode2 等把上游模型换名暴露的端点）。
//
// 价源（两个都实测可用；都必须自报 UA —— models.dev 会 403 掉 Python-urllib 默认 UA）：
//   - models.dev（默认）：https://models.dev/api.json，222 个 provider 的价目表。
//     provider 按 **base_url** 匹配（与 models.dev 的 provider.api 同源判定）→ 自定义名
//     同样可解析（opencode2 与 opencode-go 同 base_url，实测命中）。
//   - openrouter（自报价）：https://openrouter.ai/api/v1/models（446 个模型全带 pricing）。
//     注意 pricing 是**每 token** 的 USD 字符串 → ×1e6 换成 ModelPrice 的 USD/1M。
//
// 语义（刻意保守，避免「刷新把好数据刷坏」）：
//   - 默认只补「当前无价」的模型；已有内置价或手填价的模型不动 —— 否则会把内置价复制进
//     userPriceTable（优先级最高）固化下来，挡住未来的内置价更新。
//   - overwrite=true 才强制覆盖已有价。
//   - 只改内存（setPrices → userPriceTable + 注册表 override，即刻影响 FillUsageCost /
//     会话成本 / MaxCost 熔断 / cost_usd 注入）；**不写 settings.json** —— 该文件是
//     Electron 独占写者（见 config_patch.go 的多写者警告），命令把合成后的价表回传给
//     调用方，由前端走既有 providerSave 落盘（桥不抢写）。
//
// 响应：data = {provider, source, filled[], filled_count, skipped_count, total,
//
//	prices{模型:价}, persisted:false, note}
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/provider"
)

// 实时价源与注入点（变量便于测试替换 httptest 地址）。
var (
	modelsDevPricesURL  = "https://models.dev/api.json"
	openRouterPricesURL = "https://openrouter.ai/api/v1/models"

	// priceRefreshClient 单独 client（30s 超时）：价目文件 ~4.7MB，不能挂死命令通道。
	priceRefreshClient = &http.Client{Timeout: 30 * time.Second}

	// priceRefreshUA 客户端自报身份（models.dev 拒绝默认库 UA → 403）。
	priceRefreshUA = "go-code-bridge/0.1.0"
)

// modelsDevProviderIDs go provider → models.dev provider id 的上游改名。
// 与 scripts/gen_providers_json.py 的 MODELSDEV_ID_OVERRIDES 同一口径（其余同名）。
var modelsDevProviderIDs = map[string]string{
	"qwen":      "alibaba-token-plan-cn", // token-plan.cn-beijing.maas.aliyuncs.com
	"together":  "togetherai",
	"fireworks": "fireworks-ai",
}

// livePrices 一次实时拉取的价目：模型名 → 价（USD / 1M tokens，与 ModelPrice 同单位）。
type livePrices struct {
	source  string // 例 "models.dev:opencode-go" / "openrouter"
	byModel map[string]provider.ModelPrice
}

// lookup 归一查找：原名 → 小写 → 去 vendor 前缀（网关常把上游模型暴露为 "vendor/model"）。
func (lp livePrices) lookup(model string) (provider.ModelPrice, bool) {
	cands := []string{model, strings.ToLower(model)}
	if i := strings.LastIndex(model, "/"); i >= 0 && i+1 < len(model) {
		rest := model[i+1:]
		cands = append(cands, rest, strings.ToLower(rest))
	}
	for _, c := range cands {
		if p, ok := lp.byModel[c]; ok {
			return p, true
		}
	}
	return provider.ModelPrice{}, false
}

// refreshPrices refresh_prices 命令处理：payload {provider?, source?, overwrite?}。
func (m *manager) refreshPrices(c command) {
	name := str(c.Payload, "provider")
	if name == "" {
		name = m.provCfg.providerName()
	}
	if name == "" {
		m.resp(c, false, "provider 为空", nil)
		return
	}
	overwrite := payloadBool(c.Payload, "overwrite")
	prices, err := fetchLivePrices(name, m.provCfg.baseURLOf(name), str(c.Payload, "source"))
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}

	models := m.provCfg.configuredModels(name)
	merged := userPrices(name)
	if merged == nil {
		merged = map[string]provider.ModelPrice{}
	}
	filled := make([]string, 0, len(models))
	skipped := 0
	for _, model := range models {
		p, ok := prices.lookup(model)
		if !ok {
			skipped++ // 价源里没有这个模型（自建/私有模型）
			continue
		}
		if _, hasUser := merged[model]; hasUser && !overwrite {
			skipped++ // 已有手填价：默认不动
			continue
		}
		if !overwrite {
			if _, ok := priceFor(name, model); ok {
				skipped++ // 已有内置价：默认不动（避免把内置价固化进用户价表）
				continue
			}
		}
		merged[model] = p
		filled = append(filled, model)
	}
	if len(filled) > 0 {
		m.provCfg.setPrices(name, merged) // 内存生效：userPriceTable + 注册表 override
	}
	m.resp(c, true, "", map[string]any{
		"provider":      name,
		"source":        prices.source,
		"filled":        filled,
		"filled_count":  len(filled),
		"skipped_count": skipped,
		"total":         len(models),
		"prices":        merged,
		"persisted":     false,
		"note":          "价已即时生效（内存）；持久化请由前端经 providerSave 写 settings.json",
	})
}

// payloadBool 读 payload 布尔（兼容 bool / 数字 / 字符串三种前端写法）。
func payloadBool(m map[string]any, k string) bool {
	switch v := m[k].(type) {
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v == "1" || v == "true"
	}
	return false
}

// fetchLivePrices 拉实时价目。source: ""/"auto"（openrouter 端点优先自报价，失败落 models.dev）
// / "modelsdev" / "openrouter"。
func fetchLivePrices(provName, baseURL, source string) (livePrices, error) {
	switch source {
	case "", "auto":
		if isOpenRouterEndpoint(baseURL) || provName == "openrouter" {
			if lp, err := fetchOpenRouterPrices(); err == nil {
				return lp, nil
			}
		}
		return fetchModelsDevPrices(provName, baseURL)
	case "modelsdev":
		return fetchModelsDevPrices(provName, baseURL)
	case "openrouter":
		return fetchOpenRouterPrices()
	default:
		return livePrices{}, fmt.Errorf("未知价源 %q（支持 auto/modelsdev/openrouter）", source)
	}
}

// fetchModelsDevPrices 拉 models.dev 全量价目，取目标 provider 的一段。
// provider 判定顺序：改名表 → 同名直取 → base_url 同源匹配（取 id 字母序最小者，保证确定性）。
func fetchModelsDevPrices(provName, baseURL string) (livePrices, error) {
	body, err := priceRefreshGet(modelsDevPricesURL)
	if err != nil {
		return livePrices{}, err
	}
	var data map[string]struct {
		API    string `json:"api"`
		Models map[string]struct {
			Cost *struct {
				Input      *float64 `json:"input"`
				Output     *float64 `json:"output"`
				CacheRead  *float64 `json:"cache_read"`
				CacheWrite *float64 `json:"cache_write"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &data); err != nil {
		return livePrices{}, fmt.Errorf("models.dev 响应解析失败: %w", err)
	}
	// 价源 provider 判定（顺序即权威性）：
	//  ① base_url 同源匹配 —— 端点才是权威判据：本地内置名 "opencode" 对应的其实是
	//     models.dev 的 **opencode-go**（另一条 opencode 是 /zen/v1，104 个模型，价不同，
	//     实测 0.14/0.028/0.28 vs 0.15/0.003/0.6）；
	//  ② 显式改名表（qwen → alibaba-token-plan-cn 等上游改名）；③ 同名直取。
	mdID := ""
	if want := normalizeEndpoint(baseURL); want != "" {
		var cands []string
		for id, p := range data {
			if normalizeEndpoint(p.API) == want {
				cands = append(cands, id)
			}
		}
		sort.Strings(cands) // map 遍历无序 → 排序保证确定性
		for _, id := range cands {
			if id == provName {
				mdID = id
				break
			}
		}
		if mdID == "" {
			if renamed := modelsDevProviderIDs[provName]; renamed != "" {
				for _, id := range cands {
					if id == renamed {
						mdID = id
						break
					}
				}
			}
		}
		if mdID == "" && len(cands) > 0 {
			mdID = cands[0]
		}
	}
	if mdID == "" {
		if renamed := modelsDevProviderIDs[provName]; renamed != "" {
			if _, ok := data[renamed]; ok {
				mdID = renamed
			}
		}
	}
	if mdID == "" {
		if _, ok := data[provName]; ok {
			mdID = provName
		}
	}
	if mdID == "" {
		return livePrices{}, fmt.Errorf("provider %s 无 models.dev 价源（base_url %q 未匹配；可用 source=openrouter 或手填价）", provName, baseURL)
	}
	entry, ok := data[mdID]
	if !ok || len(entry.Models) == 0 {
		return livePrices{}, fmt.Errorf("models.dev 的 %s 无模型价目", mdID)
	}
	out := livePrices{source: "models.dev:" + mdID, byModel: map[string]provider.ModelPrice{}}
	for id, m := range entry.Models {
		if m.Cost == nil {
			continue
		}
		p := provider.ModelPrice{}
		if m.Cost.Input != nil {
			p.Input = *m.Cost.Input
		}
		if m.Cost.Output != nil {
			p.Output = *m.Cost.Output
		}
		if m.Cost.CacheRead != nil {
			p.CacheRead = *m.Cost.CacheRead
		}
		if m.Cost.CacheWrite != nil {
			p.CacheWrite = *m.Cost.CacheWrite
		}
		if p.Input <= 0 && p.Output <= 0 {
			continue // 免费/无价条目：不写入（保持「无价」语义，不虚增成本）
		}
		out.byModel[id] = p
		out.byModel[strings.ToLower(id)] = p
	}
	return out, nil
}

// fetchOpenRouterPrices 拉 OpenRouter 自报价（公开端点，无需鉴权）。
// pricing 是**每 token** 的 USD 字符串（如 "0.00000015"）→ ×1e6 = USD/1M tokens。
func fetchOpenRouterPrices() (livePrices, error) {
	body, err := priceRefreshGet(openRouterPricesURL)
	if err != nil {
		return livePrices{}, err
	}
	var d struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt          string `json:"prompt"`
				Completion      string `json:"completion"`
				InputCacheRead  string `json:"input_cache_read"`
				InputCacheWrite string `json:"input_cache_write"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return livePrices{}, fmt.Errorf("openrouter 响应解析失败: %w", err)
	}
	if len(d.Data) == 0 {
		return livePrices{}, fmt.Errorf("openrouter 价目为空")
	}
	out := livePrices{source: "openrouter", byModel: map[string]provider.ModelPrice{}}
	for _, m := range d.Data {
		if m.ID == "" {
			continue
		}
		p := provider.ModelPrice{
			Input:      perTokenToPerMillion(m.Pricing.Prompt),
			Output:     perTokenToPerMillion(m.Pricing.Completion),
			CacheRead:  perTokenToPerMillion(m.Pricing.InputCacheRead),
			CacheWrite: perTokenToPerMillion(m.Pricing.InputCacheWrite),
		}
		if p.Input <= 0 && p.Output <= 0 {
			continue // ":free" 等免费条目
		}
		out.byModel[m.ID] = p
		out.byModel[strings.ToLower(m.ID)] = p
	}
	return out, nil
}

// perTokenToPerMillion 每 token 的 USD（字符串）→ USD/1M tokens。解析失败按 0（不计费）。
func perTokenToPerMillion(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v * 1e6
}

// isOpenRouterEndpoint 端点是否 OpenRouter（判据是 base_url 而非 provider 名 —— 同一端点
// 可配成自定义名）。
func isOpenRouterEndpoint(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "openrouter.ai")
}

// normalizeEndpoint 端点归一（协议无关、去尾斜杠、小写）：base_url 与 models.dev 的
// provider.api 未必逐字相同（一个带 /v1、一个不带），故只比 host+path 的归一形式。
func normalizeEndpoint(u string) string {
	s := strings.ToLower(strings.TrimSpace(u))
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	return strings.TrimRight(s, "/")
}

// priceRefreshGet 拉价目文件（自报 UA；限制读取体积防异常端点刷爆内存）。
func priceRefreshGet(url string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", priceRefreshUA)
	resp, err := priceRefreshClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("拉取 %s 失败: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 上限 32MB
	if err != nil {
		return nil, fmt.Errorf("读取 %s 响应失败: %w", url, err)
	}
	return body, nil
}
