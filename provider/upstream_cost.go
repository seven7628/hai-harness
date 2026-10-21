// upstream_cost.go —— 「上游自报费用优先，拿不到才本地折算」的唯一实现。
//
// 口径（全链路唯一，见 Registry.ApplyUsageCost）：
//
//	上游响应带回实际支出 → 用它（core.CostSourceUpstream，价表完全不参与）；
//	上游没带           → 回退注册表价表折算（FillUsageCost）。
//
// 为什么需要这一层：三协议里有两条走官方 SDK（sashabaranov/go-openai 的 chat-completions
// 与 responses），**未建模的字段在 json.Unmarshal 阶段就被丢弃**，provider 层拿不到；
// anthropic-sdk-go 则把未建模字段收进 respjson.ExtraFields。因此两条取数路径：
//
//	① 传输层尾部回看（openai chat-completions / responses）：usage 只在收尾帧，
//	   响应尾部若干 KB 内必有；在 RoundTripper 处留一个固定大小的尾部缓冲，流结束回看。
//	   不动任何 SDK 结构体、不合并既有两通道（见 openai.go 关于 SDK/minimal 通道分工的注释）。
//	② SDK 未建模字段（anthropic）：直接读 ExtraFields（见 anthropic 的 upstreamCostFromExtra）。
//
// 两条路径都写进同一个 UpstreamCost 槽位，口径因此完全一致。
package provider

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/seven7628/hai-harness/core"
)

// upstreamCostTailMax 传输层尾部回看缓冲上限（字节）。
// usage 收尾帧只有几百字节，16KB 足以覆盖「最后一个 usage 对象完整落在缓冲里」，
// 且不随响应体大小增长（长回答/长思考同样只留尾部）。
const upstreamCostTailMax = 16 << 10

// UpstreamCostKeys 上游可能用来报「这一笔实际花了多少钱」的键（USD）。
// 只认这几个明确的名字，不做模糊匹配 —— 认错一个键就等于把金额算错若干倍，
// 宁可判「上游没报」而回退本地价表（那是可解释的估算）。
// 依据：OpenRouter usage.cost（实测为 credits=USD）；其余为聚合网关常见变体。
var UpstreamCostKeys = []string{
	"cost",
	"total_cost",
	"cost_usd",
	"total_cost_usd",
}

// upstreamCostRe 从响应尾部抓金额：键 + JSON 数字（容忍字符串形态）。
// 键名带引号做边界，故 "cost_details" 不会被 "cost" 命中。
var upstreamCostRe = regexp.MustCompile(
	`"(?:` + strings.Join(UpstreamCostKeys, "|") + `)"\s*:\s*"?(-?[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?)"?`)

// UpstreamCost 本轮调用的「上游自报费用」槽位。并发安全（传输层读、主流程写）。
//
// 零值可用；nil 安全（所有方法对 nil 接收者返回「上游没报」）。
type UpstreamCost struct {
	mu sync.Mutex

	// tail 响应尾部回看缓冲（传输层路径）。
	tail []byte

	// micro 显式写入的金额（SDK 未建模字段路径）；has 标记是否写过。
	// 显式写入优先于尾部回看 —— 前者是协议层直接读到的字段，后者是字节扫描的兜底。
	micro int64
	has   bool
}

// NewUpstreamCost 新建槽位（协议层不需要 ctx 时用，如 anthropic 的 ExtraFields 路径）。
func NewUpstreamCost() *UpstreamCost { return &UpstreamCost{} }

type upstreamCostCtxKey struct{}

// WithUpstreamCost 把槽位挂到 ctx 上（传输层从 ctx 取）。
func WithUpstreamCost(ctx context.Context) (context.Context, *UpstreamCost) {
	c := NewUpstreamCost()
	return context.WithValue(ctx, upstreamCostCtxKey{}, c), c
}

// UpstreamCostFromContext 取 ctx 上的槽位（无则 nil = 不采集）。
func UpstreamCostFromContext(ctx context.Context) *UpstreamCost {
	c, _ := ctx.Value(upstreamCostCtxKey{}).(*UpstreamCost)
	return c
}

// SetMicro 显式记下上游报的费用（µUSD）。
func (c *UpstreamCost) SetMicro(micro int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.has || micro > c.micro {
		// 同一轮可能多帧都带（message_start 与 message_delta）：取更大的那个 ——
		// 累积型上报只增不减，取最大即最终值，也避免早帧的 0 覆盖后帧的实际值。
		c.micro, c.has = micro, true
	}
}

// CostMicro 本轮上游自报费用（µUSD）。ok=false 表示**上游没报**（≠「报了 0」）。
func (c *UpstreamCost) CostMicro() (int64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.has {
		return c.micro, true
	}
	return upstreamCostFromTail(c.tail)
}

// record 累积响应尾部（只保留最后 upstreamCostTailMax 字节）。
func (c *UpstreamCost) record(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(p) >= upstreamCostTailMax {
		c.tail = append(c.tail[:0], p[len(p)-upstreamCostTailMax:]...)
		return
	}
	if drop := len(c.tail) + len(p) - upstreamCostTailMax; drop > 0 {
		c.tail = append(c.tail[:0], c.tail[drop:]...)
	}
	c.tail = append(c.tail, p...)
}

// upstreamCostFromTail 在尾部缓冲里定位**最后一个 usage 对象**并取其费用字段。
// 取最后一个：中间帧的 usage 多为 null，且部分网关逐帧累积上报（最后一个才完整）。
func upstreamCostFromTail(tail []byte) (int64, bool) {
	if len(tail) == 0 {
		return 0, false
	}
	i := bytes.LastIndex(tail, []byte(`"usage"`))
	if i < 0 {
		return 0, false
	}
	m := upstreamCostRe.FindSubmatch(tail[i:])
	if m == nil {
		return 0, false
	}
	return UsdToMicro(string(m[1]))
}

// UsdToMicro 把上游报的美元金额（JSON 数字，或带引号的字符串）换成 µUSD 面额。
// 拒绝负值/NaN/Inf：负值是**哨兵**（如 openrouter/auto 的 -1000000 价目），
// 不是「倒贴钱」；宁可判「上游没报」走本地价表，也不把哨兵写进账。
func UsdToMicro(raw string) (int64, bool) {
	s := strings.TrimSpace(raw)
	// 带引号的字符串形态：尾部回看的正则已剥掉，SDK 未建模字段（ExtraFields.Raw）
	// 给的是原样 JSON token（字符串带头尾引号）→ 这里统一剥一层。
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	if s == "" || s == "null" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0, false
	}
	return int64(math.Round(f * 1e6)), true
}

// upstreamCostTransport 在传输层给响应体挂尾部回看（不改变任何既有语义）。
type upstreamCostTransport struct{ base http.RoundTripper }

// Unwrap 暴露被包装的 transport —— 调用方/测试要按类型断言既有 transport 时必须穿过本层。
func (t *upstreamCostTransport) Unwrap() http.RoundTripper { return t.base }

// BaseTransport 剥掉本包在各层加的响应体包装（尾部回看 / 思考字段归一化），
// 返回原始 transport（不是本包包装则原样返回）。
// 用途：需要在包装后仍按类型断言既有 transport 的调用方（如 *openCodeGatewayTransport 的
// 网关准入检查）。
func BaseTransport(rt http.RoundTripper) http.RoundTripper {
	switch t := rt.(type) {
	case *upstreamCostTransport:
		return BaseTransport(t.base)
	case *reasoningStreamTransport:
		return BaseTransport(t.base)
	}
	return rt
}

func (t *upstreamCostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := UpstreamCostFromContext(req.Context())
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	// 未挂槽位（绝大多数调用：标题生成、压缩、探活等旁路）→ 一个字节都不多读。
	if err != nil || rec == nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &upstreamCostBody{ReadCloser: resp.Body, rec: rec}
	return resp, nil
}

type upstreamCostBody struct {
	io.ReadCloser
	rec *UpstreamCost
}

func (b *upstreamCostBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.rec.record(p[:n])
	}
	return n, err
}

// WrapUpstreamCostClient 就地把客户端包上尾部回看（保留既有 Transport / Timeout / Jar）。
// nil 进 nil 出（调用方据此保持「没传 client 就不动默认客户端」的既有行为）。
func WrapUpstreamCostClient(c *http.Client) *http.Client {
	if c == nil {
		return nil
	}
	if _, done := c.Transport.(*upstreamCostTransport); done {
		return c
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out := *c
	out.Transport = &upstreamCostTransport{base: base}
	return &out
}

// ApplyUsageCost 计费口径的**唯一入口**：上游自报费用优先，拿不到才按注册表价表折算。
//
// 只在四处调用（三协议、成功与失败路径各自收口）：openai chat-completions、responses、
// anthropic。改口径只改这里。
func (r *Registry) ApplyUsageCost(provider, model string, u *core.Usage, up *UpstreamCost) {
	if u == nil {
		return
	}
	if micro, ok := up.CostMicro(); ok {
		// 只填 Total：上游报的是「这一笔一共扣了多少」，多数不给维度拆分，
		// 按价表反推四段等于用估算冒充实收（见 core.CostSourceUpstream 注释）。
		u.Cost = core.Cost{Total: micro, Priced: true, Source: core.CostSourceUpstream}
		return
	}
	r.FillUsageCost(provider, model, u)
}
