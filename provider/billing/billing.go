// Package billing 查询 Provider 的余额 / 配额（订阅窗口）/ 用量。
//
// ## 为什么要一个包，而不是在 bridge 里拼几段 http.Get
//
// 余额与用量**没有统一 API**（2026-11 调研，见 docs/PROVIDER_USAGE_BILLING.md）：
//   - A 余额：DeepSeek `GET /user/balance`、Moonshot `/v1/users/me/balance`、OpenRouter `/api/v1/credits`
//   - B 订阅窗口：OpenCode Zen `/zen/go/v1/usage`（三窗口百分比）、GLM `/api/monitor/usage/quota/limit`…
//   - C 官方报表：OpenAI/Anthropic 需要另一把 Admin Key（本版不做）
//   - D 无接口：绝大多数第三方平台 —— 「没有」必须是一等公民，而不是错误
//
// 所以这里做三件事：**声明**（presets.json：端点/鉴权/字段路径）、**归一化**（四种形状 → 一个
// Snapshot）、**失败软**（每家独立超时、错误分码、绝不把 key 发到非自家 host）。
//
// ## 三条硬规则（都有测试）
//
//  1. 凭证只发声明里的 host（descriptor.Host 必须与 base_url 的 host 相等），否则退化为
//     同 origin 的 generic 探测 —— 防「用户把 base_url 填成别人的域名 → key 被带走」。
//  2. 拒绝重定向（CheckRedirect 返回 ErrUseLastResponse）—— 否则 302 就能把 Authorization 带走。
//  3. 缺失读数 ≠ 0：窗口读到就填 Percent 指针，读不到留 nil（UI 画「读数缺失」而不是 0%）。
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Kind 快照类型。决定 UI 画什么（金额 / 窗口条 / 什么都没有）。
type Kind string

const (
	KindBalance Kind = "balance" // A：预付余额（货币金额）
	KindQuota   Kind = "quota"   // B：订阅窗口（百分比 + 重置时间）
	KindReport  Kind = "report"  // C：官方报表（本版不实现，留位）
	KindNone    Kind = "none"    // D：没有查询能力
)

// 错误码（前端据此说人话；HTTP 状态码只进 ErrStatus，不上屏）。
const (
	ErrCodeNoKey           = "billing_no_key"
	ErrCodeUnsupported     = "billing_unsupported"
	ErrCodeUnauthorized    = "billing_unauthorized"
	ErrCodeRateLimited     = "billing_rate_limited"
	ErrCodeEndpointChanged = "billing_endpoint_changed"
	ErrCodeNetwork         = "billing_network"
	ErrCodeBadJSON         = "billing_bad_json"
)

// Money 金额（已归一：字符串金额 → 数字，分 → 元）。
type Money struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"` // 来自响应（DeepSeek 实测 CNY），不硬编码 USD
}

// Window 订阅配额窗口。
type Window struct {
	Key   string `json:"key"`             // rolling / weekly / monthly / 5h / session…
	Label string `json:"label,omitempty"` // 展示名（presets 可映射；空 = 用 Key）
	// Percent 已用百分比。**指针**：nil = 上游给了窗口但没给读数 —— 不能画成 0%。
	Percent  *float64   `json:"percent,omitempty"`
	Status   string     `json:"status,omitempty"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// Snapshot 归一化结果（前端只认它）。
type Snapshot struct {
	Provider  string    `json:"provider"`
	FetchedAt time.Time `json:"fetched_at"`
	Kind      Kind      `json:"kind"`
	Source    string    `json:"source,omitempty"` // "deepseek.balance"，排障用，不上屏
	Currency  string    `json:"currency,omitempty"`

	Balance   *Money   `json:"balance,omitempty"` // 剩余
	Limit     *Money   `json:"limit,omitempty"`   // 额度上限（哨兵/缺失 → nil，不画占比）
	Used      *Money   `json:"used,omitempty"`    // 本期已用
	Windows   []Window `json:"windows,omitempty"` // B 形状
	PlanLabel string   `json:"plan_label,omitempty"`

	// NotConnected 没配 key / 没登录 —— 不是错误（UI 一句灰字，不给重试）。
	NotConnected bool `json:"not_connected,omitempty"`
	// Experimental 端点未经实测（presets.json 的 verified_by != "live"）：UI 标「实验」。
	Experimental bool   `json:"experimental,omitempty"`
	ErrCode      string `json:"error_code,omitempty"`
	ErrStatus    int    `json:"error_status,omitempty"` // HTTP 状态（隐藏字段，排障用）
	// Stale 本次刷新失败，但字段里是**上一次成功**的读数（UI 显示旧值变灰 + 错误一行）。
	// 没有它的话刷新失败会把好数据抹成空白 —— 用户反而以为额度没了。
	Stale bool `json:"stale,omitempty"`
}

// FetchError 带稳定错误码的抓取失败。
type FetchError struct {
	Code   string
	Status int
	Err    error
}

func (e *FetchError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s (status=%d): %v", e.Code, e.Status, e.Err)
	}
	return fmt.Sprintf("%s (status=%d)", e.Code, e.Status)
}
func (e *FetchError) Unwrap() error { return e.Err }

func errCode(err error) (string, int) {
	var fe *FetchError
	if errors.As(err, &fe) {
		return fe.Code, fe.Status
	}
	return ErrCodeNetwork, 0
}

// Request 一次抓取的输入（由 bridge 组装：key / OAuth 头都由调用方解析后传入）。
type Request struct {
	Provider string
	BaseURL  string
	APIKey   string            // API Key（与 OAuthHeaders 二选一）
	OAuth    map[string]string // OAuth 派生请求头（Flow.ToAuth 的 Headers 原样传入）
	// Override 用户在设置里手填的端点（可选；留空 = 用内置声明）。
	Override *Preset
}

const (
	// maxBodyBytes 响应体上限：余额/配额响应都是几百字节，64KB 足够且防「返回一整页 HTML」。
	maxBodyBytes = 64 << 10
)

// perRequestTimeout 单请求上限。8s 是「上游正常但慢」与「已经挂了」的分界，再长用户会以为
// 按钮没反应（对齐 pi-usage-meters 的取值）。**变量**是为了测试能压到 200ms。
var perRequestTimeout = 8 * time.Second

// httpClient 不跟随重定向的客户端（见包注释规则 2）。
var httpClient = &http.Client{
	Timeout: perRequestTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Fetch 抓取一个 provider 的余额/配额快照。
//
// 返回的 error 只在「完全无法判断」时非 nil；**NotConnected / 不支持** 都走 Snapshot，
// 因为它们是正常状态而不是故障（UI 要区别对待）。
func Fetch(ctx context.Context, req Request) (*Snapshot, error) {
	snap := &Snapshot{Provider: req.Provider, Kind: KindNone, FetchedAt: time.Now()}
	base := strings.TrimSpace(req.BaseURL)
	if base == "" {
		snap.NotConnected = true
		snap.ErrCode = ErrCodeNoKey
		return snap, nil
	}
	origin, host := originAndHost(base)
	if origin == "" {
		snap.ErrCode = ErrCodeEndpointChanged
		return snap, &FetchError{Code: ErrCodeEndpointChanged, Err: fmt.Errorf("base_url 无法解析: %s", base)}
	}
	hasAuth := strings.TrimSpace(req.APIKey) != "" || len(req.OAuth) > 0

	// 1) 声明优先：先按 id 精确命中，命不中再按 host 复用（同一家加了 opencode2/opencode3）；
	//    两条路径都要求声明里的 host 与 base_url 一致 —— 凭证只发自家 host。
	p := req.Override
	if p == nil {
		if cand, ok := resolvePreset(req.Provider, host); ok {
			p = &cand
		}
	} else if p.Host != "" && !strings.EqualFold(p.Host, host) {
		p = nil // 用户手填的声明也不能把 key 发到别的 host
	}

	if p != nil {
		snap.Kind = p.kind()
		snap.Source = p.Source
		snap.Experimental = p.VerifiedBy != "" && p.VerifiedBy != "live"
		if p.Auth != "none" && !hasAuth {
			snap.NotConnected = true
			snap.ErrCode = ErrCodeNoKey
			return snap, nil
		}
		if err := fetchWith(ctx, snap, *p, origin, req); err != nil {
			return nil, err
		}
		return snap, nil
	}

	// 2) 未声明 → 同 origin 的 generic 探测（new-api / one-api 家族的中转网关）。
	if !hasAuth {
		snap.NotConnected = true
		snap.ErrCode = ErrCodeNoKey
		return snap, nil
	}
	if gp, ok := genericPreset(); ok {
		snap.Kind = KindBalance
		snap.Source = gp.Source
		snap.Experimental = true
		if err := fetchWith(ctx, snap, gp, origin, req); err == nil {
			return snap, nil
		} else if code, _ := errCode(err); code != ErrCodeEndpointChanged {
			// 探测失败 = 这家没有余额面（常见），按「不支持」返回而不是报错刷屏。
			return &Snapshot{
				Provider: req.Provider, Kind: KindNone, FetchedAt: snap.FetchedAt,
				ErrCode: ErrCodeUnsupported,
			}, nil
		}
	}
	return &Snapshot{Provider: req.Provider, Kind: KindNone, FetchedAt: snap.FetchedAt, ErrCode: ErrCodeUnsupported}, nil
}

// fetchWith 按声明抓取并写入 snapshot。
func fetchWith(ctx context.Context, snap *Snapshot, p Preset, origin string, req Request) error {
	if p.Shape == "openai_dashboard" {
		return fetchOpenAIDashboard(ctx, snap, p, origin, req)
	}
	if p.Path == "" {
		return &FetchError{Code: ErrCodeEndpointChanged, Err: errors.New("声明缺少 path")}
	}
	body, status, err := getJSON(ctx, origin+p.Path, req, p.Auth)
	if err != nil {
		return err
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return &FetchError{Code: ErrCodeBadJSON, Status: status, Err: err}
	}
	switch p.Shape {
	case "balance":
		return parseBalance(snap, p, doc)
	case "quota":
		return parseQuota(snap, p, doc)
	default:
		return &FetchError{Code: ErrCodeUnsupported, Err: fmt.Errorf("未知 shape: %s", p.Shape)}
	}
}

// fetchOpenAIDashboard new-api / one-api 家族：
//
//	GET /v1/dashboard/billing/subscription → hard_limit_usd（**1e8 是「无限」哨兵**）
//	GET /v1/dashboard/billing/usage?start_date&end_date → total_usage（**单位是分**）
//
// 两次请求都成功才算数；limit 是哨兵时置 nil（UI 只显示用量，不画占比）。
func fetchOpenAIDashboard(ctx context.Context, snap *Snapshot, p Preset, origin string, req Request) error {
	if len(p.Paths) < 2 {
		return &FetchError{Code: ErrCodeEndpointChanged, Err: errors.New("dashboard 声明需要两个 path")}
	}
	body, status, err := getJSON(ctx, origin+p.Paths[0], req, p.Auth)
	if err != nil {
		return err
	}
	var sub struct {
		HardLimitUSD       float64 `json:"hard_limit_usd"`
		SystemHardLimitUSD float64 `json:"system_hard_limit_usd"`
	}
	if err := json.Unmarshal(body, &sub); err != nil {
		return &FetchError{Code: ErrCodeBadJSON, Status: status, Err: err}
	}
	today := time.Now()
	usageURL := fmt.Sprintf("%s%s?start_date=%s&end_date=%s", origin, p.Paths[1],
		today.AddDate(0, 0, -30).Format("2006-01-02"), today.Format("2006-01-02"))
	body2, status2, err := getJSON(ctx, usageURL, req, p.Auth)
	if err != nil {
		return err
	}
	var usg struct {
		TotalUsage float64 `json:"total_usage"`
	}
	if err := json.Unmarshal(body2, &usg); err != nil {
		return &FetchError{Code: ErrCodeBadJSON, Status: status2, Err: err}
	}
	used := usg.TotalUsage
	if p.Unit == "cents" {
		used /= 100
	}
	snap.Used = &Money{Amount: used, Currency: "USD"}
	lim := sub.HardLimitUSD
	if lim <= 0 || (p.LimitSentinel > 0 && lim >= p.LimitSentinel) {
		lim = sub.SystemHardLimitUSD
		if p.LimitSentinel > 0 && lim >= p.LimitSentinel {
			lim = 0
		}
	}
	if lim > 0 {
		snap.Limit = &Money{Amount: lim, Currency: "USD"}
		snap.Balance = &Money{Amount: lim - used, Currency: "USD"}
	}
	return nil
}

// getJSON 发一次 GET：不跟重定向、限体积、按声明加鉴权头。
func getJSON(ctx context.Context, rawURL string, req Request, authKind string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, perRequestTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, &FetchError{Code: ErrCodeEndpointChanged, Err: err}
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "hai-harness/usage-meter")
	// 兜底：即使声明写错也不会把凭证发到非 https 的明文端点（防降级）。
	if !strings.HasPrefix(rawURL, "https://") && !strings.HasPrefix(rawURL, "http://127.0.0.1") && !strings.HasPrefix(rawURL, "http://localhost") {
		return nil, 0, &FetchError{Code: ErrCodeEndpointChanged, Err: errors.New("仅允许 https 端点")}
	}
	switch authKind {
	case "oauth":
		for k, v := range req.OAuth {
			httpReq.Header.Set(k, v)
		}
	case "raw": // GLM 国内端点要裸 key，不是 Bearer
		if req.APIKey != "" {
			httpReq.Header.Set("Authorization", req.APIKey)
		}
	case "x-api-key":
		if req.APIKey != "" {
			httpReq.Header.Set("x-api-key", req.APIKey)
		}
	default: // bearer
		if req.APIKey != "" {
			httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
		}
		for k, v := range req.OAuth { // OAuth 头优先覆盖（部分流程带自定义头）
			httpReq.Header.Set(k, v)
		}
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, 0, &FetchError{Code: ErrCodeNetwork, Err: err}
		}
		return nil, 0, &FetchError{Code: ErrCodeNetwork, Err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, resp.StatusCode, &FetchError{Code: ErrCodeUnauthorized, Status: resp.StatusCode, Err: fmt.Errorf("%s", firstLine(body))}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, resp.StatusCode, &FetchError{Code: ErrCodeRateLimited, Status: resp.StatusCode}
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest:
		return nil, resp.StatusCode, &FetchError{Code: ErrCodeEndpointChanged, Status: resp.StatusCode}
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// 不跟重定向（CheckRedirect）→ 3xx 落在这里：端点搬家了，而不是「解析失败」。
		return nil, resp.StatusCode, &FetchError{Code: ErrCodeEndpointChanged, Status: resp.StatusCode}
	case resp.StatusCode >= 400:
		return nil, resp.StatusCode, &FetchError{Code: ErrCodeNetwork, Status: resp.StatusCode}
	}
	return body, resp.StatusCode, nil
}

// firstLine 错误体只留一行（不落盘、不上屏，仅用于日志）。
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// originAndHost 拆 base_url：origin（scheme://host[:port]）+ host（小写，无端口）。
// 所有声明里的 path 都是 **origin 相对**的绝对路径（DeepSeek 的 /user/balance 就在根上，
// 不在 /v1 下）—— 所以这里只取 origin，不拼接 base_url 的 path。
func originAndHost(raw string) (origin, host string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return "", ""
	}
	host = strings.ToLower(u.Hostname())
	// 只接受单调的 host 形状：拒绝 ":::" / 空 / 带路径分隔符的伪 host —— 否则
	// 「base_url 填错」会变成一个永远匹配不上的白名单，静默走到 generic 探测。
	if host == "" || strings.ContainsAny(host, " :/\\?#") {
		return "", ""
	}
	return u.Scheme + "://" + u.Host, host
}
