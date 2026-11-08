package billing

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed presets.json
var presetsJSON []byte

// WindowSpec 窗口字段声明。支持两种形状：
//   - map（OpenCode Zen）：`{"rolling":{"percent":0,"status":"ok","resetsAt":"…"}, …}`
//   - list（GLM / MiniMax / Claude）：`[{type:"…", percentage:12, nextResetTime:…}, …]`
//
// 字段值都是「路径或字段名」，`a|b` 表示按顺序取第一个存在的（各厂商命名不统一）。
type WindowSpec struct {
	Path    string            `json:"path"`
	Mode    string            `json:"mode"` // map（默认）| list
	Key     string            `json:"key"`  // list 模式：每个元素里取 key 的字段
	Percent string            `json:"percent"`
	Status  string            `json:"status"`
	Reset   string            `json:"reset"`
	Labels  map[string]string `json:"labels"` // 窗口 key → 展示名（rolling→5h 等）
	// Invert 上游给的是「剩余百分比」而不是「已用」（Copilot / MiniMax）：取 100-x。
	// 不做这个转换的后果是**方向反了**——满额时显示 100%，正好与真实情况相反。
	Invert bool `json:"invert,omitempty"`
}

// Preset 一家 provider 的查询声明（presets.json 的一条）。
type Preset struct {
	ID     string `json:"id,omitempty"`
	Shape  string `json:"shape"` // balance | quota | openai_dashboard | report
	Source string `json:"source"`
	// Host 允许发凭证的 host。与 base_url 的 host 不一致时**不发送**（见包注释规则 1）。
	Host string `json:"host,omitempty"`
	Path string `json:"path,omitempty"`
	// Paths openai_dashboard 专用：[subscription, usage]。
	Paths []string `json:"paths,omitempty"`
	Auth  string   `json:"auth,omitempty"` // bearer（默认）| raw | x-api-key | oauth | none
	Unit  string   `json:"unit,omitempty"` // "" | cents（new-api 的 total_usage 是分）
	// LimitSentinel 哨兵阈值：>= 它视为「无限」（new-api 用 1e8 表示不限额）。
	LimitSentinel float64 `json:"limit_sentinel,omitempty"`
	// VerifiedBy 证据等级：live（本机实测）/ docs（官方文档）/ reference（同源参考实现，未实测）。
	// 非 live 的在 UI 上标「实验」—— 端点随时可能变。
	VerifiedBy string `json:"verified_by,omitempty"`

	// —— 字段路径（balance / report 形状）——
	Balance         string `json:"balance,omitempty"` // 剩余
	Granted         string `json:"granted,omitempty"` // 赠送额度（DeepSeek）
	Limit           string `json:"limit,omitempty"`
	Used            string `json:"used,omitempty"`
	Currency        string `json:"currency,omitempty"`         // 币种字段路径；`$CNY` 前缀表示字面量
	CurrencyDefault string `json:"currency_default,omitempty"` // 响应无币种时的默认值（默认 USD）
	PlanLabel       string `json:"plan_label,omitempty"`

	Windows *WindowSpec `json:"windows,omitempty"`
}

func (p Preset) kind() Kind {
	switch p.Shape {
	case "balance", "openai_dashboard":
		return KindBalance
	case "quota":
		return KindQuota
	case "report":
		return KindReport
	}
	return KindNone
}

type presetsFile struct {
	SchemaVersion int               `json:"schema_version"`
	Presets       map[string]Preset `json:"presets"`
	Generic       []Preset          `json:"generic"`
}

var (
	presetsOnce sync.Once
	presetsByID map[string]Preset
	generic     []Preset
	presetsErr  error
)

func loadPresets() {
	presetsOnce.Do(func() {
		var f presetsFile
		if err := json.Unmarshal(presetsJSON, &f); err != nil {
			presetsErr = fmt.Errorf("billing/presets.json 解析失败: %w", err)
			return
		}
		presetsByID = f.Presets
		generic = f.Generic
	})
}

// PresetFor 按 provider id 取声明（不校验 host —— 调用方用 Fetch，host 校验在那边）。
func PresetFor(providerID string) (Preset, bool) {
	loadPresets()
	if presetsErr != nil {
		return Preset{}, false
	}
	p, ok := presetsByID[strings.ToLower(strings.TrimSpace(providerID))]
	return p, ok
}

// Presets 全部声明（排障 / 文档用）。
func Presets() map[string]Preset {
	loadPresets()
	out := make(map[string]Preset, len(presetsByID))
	for k, v := range presetsByID {
		out[k] = v
	}
	return out
}

func presetFor(providerID string) (Preset, bool) { return PresetFor(providerID) }

// genericPreset 未声明 provider 的同 origin 探测（new-api / one-api 家族）。
func genericPreset() (Preset, bool) {
	loadPresets()
	for _, p := range generic {
		if p.Shape == "openai_dashboard" {
			return p, true
		}
	}
	return Preset{}, false
}

// —— 路径取值 ——

// lookup 按 `a.b[0].c` 取值；path 为空返回 nil。
// 也支持 `a|b`：依次尝试，返回第一个存在且非 nil 的。
func lookup(doc any, path string) any {
	if path == "" {
		return nil
	}
	for _, alt := range strings.Split(path, "|") {
		if v := lookupOne(doc, strings.TrimSpace(alt)); v != nil {
			return v
		}
	}
	return nil
}

func lookupOne(doc any, path string) any {
	cur := doc
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			continue
		}
		// 段内可能有多个下标：limits[0][1]
		for {
			name, idx, rest := splitIndex(seg)
			if name != "" {
				m, ok := cur.(map[string]any)
				if !ok {
					return nil
				}
				cur = m[name]
			}
			if idx >= 0 {
				arr, ok := cur.([]any)
				if !ok || idx >= len(arr) {
					return nil
				}
				cur = arr[idx]
			}
			if rest == "" {
				break
			}
			seg = rest
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

// splitIndex 把 "limits[0]" 拆成 ("limits", 0, "")；"a[0][1]" → ("a",0,"[1]")。
func splitIndex(seg string) (name string, idx int, rest string) {
	idx = -1
	i := strings.IndexByte(seg, '[')
	if i < 0 {
		return seg, -1, ""
	}
	name = seg[:i]
	j := strings.IndexByte(seg[i:], ']')
	if j < 0 {
		return seg, -1, ""
	}
	if n, err := strconv.Atoi(seg[i+1 : i+j]); err == nil {
		idx = n
	}
	return name, idx, seg[i+j+1:]
}

// asFloat 数字归一：JSON 里的金额可能是字符串（DeepSeek 实测 "−0.02"）或整数。
func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	}
	return 0, false
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	return ""
}

// asTime 重置时间：RFC3339 字符串 / unix 秒 / unix 毫秒。
func asTime(v any) *time.Time {
	if v == nil {
		return nil
	}
	if f, ok := asFloat(v); ok {
		if f > 1e12 { // 毫秒
			f /= 1000
		}
		t := time.Unix(int64(f), 0)
		return &t
	}
	s := strings.TrimSpace(asString(v))
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t
		}
	}
	return nil
}

// parseBalance A 形状：余额（+ 可选 limit/used）。
func parseBalance(snap *Snapshot, p Preset, doc any) error {
	cur := currencyOf(p, doc)
	if v, ok := asFloat(lookup(doc, p.Balance)); ok {
		snap.Balance = &Money{Amount: v, Currency: cur}
		snap.Currency = cur
	}
	if v, ok := asFloat(lookup(doc, p.Used)); ok {
		snap.Used = &Money{Amount: convertUnit(v, p.Unit), Currency: cur}
	}
	if v, ok := asFloat(lookup(doc, p.Limit)); ok {
		if p.LimitSentinel <= 0 || v < p.LimitSentinel {
			snap.Limit = &Money{Amount: convertUnit(v, p.Unit), Currency: cur}
		}
	}
	// 三家都没取到 = 字段路径已变（端点存在但形状变了）。
	if snap.Balance == nil && snap.Used == nil && snap.Limit == nil {
		return &FetchError{Code: ErrCodeEndpointChanged, Err: fmt.Errorf("响应中没有可识别的金额字段（%s）", p.Source)}
	}
	return nil
}

// parseQuota B 形状：窗口百分比。
func parseQuota(snap *Snapshot, p Preset, doc any) error {
	if p.Windows == nil {
		return &FetchError{Code: ErrCodeEndpointChanged, Err: fmt.Errorf("%s 声明缺少 windows", p.Source)}
	}
	w := p.Windows
	node := lookup(doc, w.Path)
	if node == nil {
		return &FetchError{Code: ErrCodeEndpointChanged, Err: fmt.Errorf("响应中没有 %s", w.Path)}
	}
	switch w.Mode {
	case "list":
		arr, ok := node.([]any)
		if !ok {
			return &FetchError{Code: ErrCodeEndpointChanged, Err: fmt.Errorf("%s 不是数组", w.Path)}
		}
		for i, el := range arr {
			key := strings.TrimSpace(asString(lookup(el, w.Key)))
			if key == "" {
				key = "w" + strconv.Itoa(i)
			}
			snap.Windows = append(snap.Windows, windowFrom(el, w, key))
		}
	default: // map
		m, ok := node.(map[string]any)
		if !ok {
			return &FetchError{Code: ErrCodeEndpointChanged, Err: fmt.Errorf("%s 不是对象", w.Path)}
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys) // 固定顺序（rolling < weekly < monthly 按字母序恰好是 5h/week/month 的常见顺序）
		for _, k := range keys {
			snap.Windows = append(snap.Windows, windowFrom(m[k], w, k))
		}
	}
	if len(snap.Windows) == 0 {
		return &FetchError{Code: ErrCodeEndpointChanged, Err: fmt.Errorf("%s 窗口为空", p.Source)}
	}
	snap.Kind = KindQuota
	return nil
}

func windowFrom(el any, w *WindowSpec, key string) Window {
	out := Window{Key: key, Label: w.Labels[key]}
	if out.Label == "" {
		out.Label = key
	}
	if v, ok := asFloat(lookup(el, w.Percent)); ok {
		// 归一到 0..100：上游有 0..1 也有 0..100（Zen 实测 48 表示 48%）。
		if v > 0 && v <= 1 {
			v *= 100
		}
		if w.Invert {
			v = 100 - v
		}
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		out.Percent = &v
	}
	out.Status = strings.TrimSpace(asString(lookup(el, w.Status)))
	out.ResetsAt = asTime(lookup(el, w.Reset))
	return out
}

func currencyOf(p Preset, doc any) string {
	if p.Currency != "" {
		if strings.HasPrefix(p.Currency, "$") { // 字面量（如 $CNY）
			return strings.TrimPrefix(p.Currency, "$")
		}
		if s := strings.TrimSpace(asString(lookup(doc, p.Currency))); s != "" {
			return strings.ToUpper(s)
		}
	}
	if p.CurrencyDefault != "" {
		return p.CurrencyDefault
	}
	return "USD"
}

func convertUnit(v float64, unit string) float64 {
	if unit == "cents" {
		return v / 100
	}
	return v
}
