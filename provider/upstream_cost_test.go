package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// TestUsdToMicro 上游金额解析的口径（数字/字符串/哨兵/垃圾）。
//
// 关键：负值必须判「没报」而不是「倒贴」—— 上游价目里的负数是哨兵
// （如 openrouter/auto 的 -1000000），把哨兵写进账会让整月成本变成负数。
func TestUsdToMicro(t *testing.T) {
	cases := []struct {
		raw    string
		want   int64
		wantOK bool
	}{
		{"0.000042", 42, true},
		{"0.5", 500000, true},
		{"1", 1000000, true},
		{"\"0.25\"", 250000, true}, // 带引号金额（ExtraFields.Raw 给的是原样 token）
		{"2.5e-5", 25, true},
		{"0", 0, true},   // 上游**明确**报了 0（免费）→ ok，不是「没报」
		{"-1", 0, false}, // 哨兵
		{"-1000000", 0, false},
		{"", 0, false},
		{"null", 0, false},
		{"abc", 0, false},
		{"NaN", 0, false},
	}
	for _, c := range cases {
		got, ok := UsdToMicro(c.raw)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("UsdToMicro(%q) = (%d, %v), want (%d, %v)", c.raw, got, ok, c.want, c.wantOK)
		}
	}
}

// TestUpstreamCostTailExtraction 传输层尾部回看：认哪个键、取哪一帧、缺与 0 的区别。
func TestUpstreamCostTailExtraction(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   int64
		wantOK bool
		why    string
	}{
		{
			name: "openrouter 收尾帧带 cost",
			frames: []string{
				`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}`,
				`data: {"choices":[],"usage":{"prompt_tokens":31,"completion_tokens":1,"cost":0.000042}}`,
			},
			want: 42, wantOK: true,
			why: "usage.cost → 42 µUSD",
		},
		{
			name: "上游没报费用",
			frames: []string{
				`data: {"choices":[],"usage":{"prompt_tokens":31,"completion_tokens":1}}`,
			},
			want: 0, wantOK: false,
			why: "只有 token → 必须回退本地价表（不是 0）",
		},
		{
			name: "上游明确报 0（免费额度）",
			frames: []string{
				`data: {"choices":[],"usage":{"prompt_tokens":31,"cost":0}}`,
			},
			want: 0, wantOK: true,
			why: "显式 0 是结论，不能当「没数据」再折算一遍",
		},
		{
			name: "负值哨兵不认",
			frames: []string{
				`data: {"choices":[],"usage":{"cost":-1000000}}`,
			},
			want: 0, wantOK: false,
			why: "哨兵 → 回退本地价表",
		},
		{
			name: "cost_details 不能被 cost 前缀误命中",
			frames: []string{
				`data: {"choices":[],"usage":{"cost_details":{"upstream_inference_cost":0.9}}}`,
			},
			want: 0, wantOK: false,
			why: "嵌套明细对象不是总额；宁可不认",
		},
		{
			name: "多帧累积上报取最后一个",
			frames: []string{
				`data: {"choices":[],"usage":{"cost":0.1}}`,
				`data: {"choices":[],"usage":{"cost":0.3}}`,
			},
			want: 300000, wantOK: true,
			why: "逐帧累积的网关，最后一个才是终值",
		},
		{
			name: "total_cost 变体",
			frames: []string{
				`data: {"choices":[],"usage":{"total_cost":"0.00075"}}`,
			},
			want: 750, wantOK: true,
			why: "字符串金额也要认（带引号由正则剥掉）",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := NewUpstreamCost()
			for _, f := range c.frames {
				up.record([]byte(f + "\n\n"))
			}
			got, ok := up.CostMicro()
			if ok != c.wantOK || got != c.want {
				t.Errorf("CostMicro() = (%d, %v), want (%d, %v) —— %s", got, ok, c.want, c.wantOK, c.why)
			}
		})
	}
}

// TestUpstreamCostTailBounded 尾部缓冲不随响应体增长（长回答/长思考不得把内存顶起来）。
func TestUpstreamCostTailBounded(t *testing.T) {
	up := NewUpstreamCost()
	chunk := []byte(strings.Repeat("x", 4096) + "\n")
	for i := 0; i < 64; i++ { // 256KB 响应
		up.record(chunk)
	}
	up.record([]byte(`data: {"usage":{"cost":0.5}}`))
	if len(up.tail) > upstreamCostTailMax {
		t.Fatalf("尾部缓冲 %d 字节，超过上限 %d", len(up.tail), upstreamCostTailMax)
	}
	if got, ok := up.CostMicro(); !ok || got != 500000 {
		t.Fatalf("CostMicro() = (%d, %v), want (500000, true)", got, ok)
	}
}

// TestUpstreamCostTransport 端到端：包过 transport 的 client + ctx 槽位 → 读到费用。
// 同时钉住「没有槽位时一个字节都不多读」（旁路请求零开销）。
func TestUpstreamCostTransport(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	body = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":31,\"cost\":0.000042}}\n\ndata: [DONE]\n\n"

	client := WrapUpstreamCostClient(srv.Client())

	ctx, up := WithUpstreamCost(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, ok := up.CostMicro(); !ok || got != 42 {
		t.Fatalf("经传输层读到 (%d, %v), want (42, true)", got, ok)
	}

	// 无槽位：同一 client、同一响应 → 不采集（也不出错）。
	req2, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if UpstreamCostFromContext(context.Background()) != nil {
		t.Fatal("裸 ctx 不应有槽位")
	}
}

// TestWrapUpstreamCostClientNilAndIdempotent nil 进 nil 出（调用方靠它保持
// 「没传 client 就不动 SDK 默认客户端」的既有行为）；重复包装不叠层。
func TestWrapUpstreamCostClientNilAndIdempotent(t *testing.T) {
	if WrapUpstreamCostClient(nil) != nil {
		t.Fatal("nil 应原样返回 nil")
	}
	c := &http.Client{}
	once := WrapUpstreamCostClient(c)
	if _, ok := once.Transport.(*upstreamCostTransport); !ok {
		t.Fatal("应装上尾部回看 transport")
	}
	twice := WrapUpstreamCostClient(once)
	if _, ok := twice.Transport.(*upstreamCostTransport); !ok {
		t.Fatal("二次包装应幂等")
	}
	if c.Transport != nil {
		t.Fatal("原 client 不应被就地改动（复制语义）")
	}
}

// TestApplyUsageCost 计费口径的唯一入口：上游有值用它、没有回退价表、上游报 0 不回退。
func TestApplyUsageCost(t *testing.T) {
	reg := NewRegistry()
	const prov, model = "deepseek", "deepseek-v4-flash"

	// ① 价表本身有价（否则「回退」这一支测不出来）
	price, ok := reg.PriceFor(prov, model)
	if !ok {
		t.Fatalf("前置条件：%s/%s 应有内置价", prov, model)
	}

	// ② 上游报了费用 → 用上游，价表完全不参与；四段留 0（不伪造分摊）
	u := &core.Usage{Input: 1000, Output: 100}
	up := NewUpstreamCost()
	up.SetMicro(1234)
	reg.ApplyUsageCost(prov, model, u, up)
	if u.Cost.Source != core.CostSourceUpstream || !u.Cost.Priced || u.Cost.Total != 1234 {
		t.Fatalf("上游有值时应全取上游：%+v", u.Cost)
	}
	if u.Cost.Input != 0 || u.Cost.Output != 0 || u.Cost.CacheRead != 0 || u.Cost.CacheWrite != 0 {
		t.Fatalf("上游只报总额时不拆段（四段应为 0）：%+v", u.Cost)
	}

	// ③ 上游没报 → 回退价表（金额 = 1000×0.15 + 100×0.6 = 210 µUSD）
	u2 := &core.Usage{Input: 1000, Output: 100}
	reg.ApplyUsageCost(prov, model, u2, NewUpstreamCost())
	if u2.Cost.Source == core.CostSourceUpstream {
		t.Fatal("上游没报时不得标成 upstream")
	}
	if want := int64(1000*price.Input + 100*price.Output); u2.Cost.Total != want {
		t.Fatalf("回退价表 = %d µUSD, want %d", u2.Cost.Total, want)
	}

	// ④ 上游**明确**报 0（免费/订阅额度）→ 结论就是 0，不得再按价表折算一遍。
	u3 := &core.Usage{Input: 1000, Output: 100}
	up3 := NewUpstreamCost()
	up3.SetMicro(0)
	reg.ApplyUsageCost(prov, model, u3, up3)
	if u3.Cost.Source != core.CostSourceUpstream || u3.Cost.Total != 0 {
		t.Fatalf("上游报 0 应保持 0 且标 upstream：%+v", u3.Cost)
	}

	// ⑤ 槽位为 nil（旁路/测试 mock）→ 与 ③ 同为回退
	u4 := &core.Usage{Input: 1000, Output: 100}
	reg.ApplyUsageCost(prov, model, u4, nil)
	if u4.Cost.Source == core.CostSourceUpstream || u4.Cost.Total == 0 {
		t.Fatalf("nil 槽位应回退价表：%+v", u4.Cost)
	}
}

// TestOpenAIProviderStreamPrefersUpstreamCost 端到端（SDK 通道）：上游回带 cost 的
// SSE → LLMEndEvent.Usage.Cost 取上游值。这是本改动的**主要路径**——SDK 结构体里
// 没有 cost 字段，全靠传输层尾部回看把它捞回来。
func TestOpenAIProviderStreamPrefersUpstreamCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"req-1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"req-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":31,"completion_tokens":2,"total_tokens":33,"cost":0.000042}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := NewOpenAIProviderWithCapabilities(srv.URL, "test-key", FullOpenAICompatCapabilities())
	var end *LLMEndEvent
	err := p.Stream(context.Background(), &StreamRequest{
		Provider: "deepseek", Model: "deepseek-v4-flash",
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
	}, func(e StreamEvent) error {
		if v, ok := e.(LLMEndEvent); ok {
			ev := v
			end = &ev
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if end == nil {
		t.Fatal("未收到 LLMEndEvent")
	}
	if end.Usage.Cost.Source != core.CostSourceUpstream {
		t.Fatalf("应取上游自报费用，实际 Cost = %+v", end.Usage.Cost)
	}
	if end.Usage.Cost.Total != 42 {
		t.Fatalf("上游 cost 0.000042 → 42 µUSD, got %d", end.Usage.Cost.Total)
	}
	// token 维度照常归一（与计费来源无关）
	if end.Usage.Input != 31 || end.Usage.Output != 2 {
		t.Fatalf("token 归一 = %d/%d, want 31/2", end.Usage.Input, end.Usage.Output)
	}
}

// TestOpenAIProviderStreamFallsBackWithoutUpstreamCost 同一路径的反面：上游不给费用
// → 必须回退本地价表（回归护栏：加了上游优先后，回退支不能被漏掉）。
func TestOpenAIProviderStreamFallsBackWithoutUpstreamCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"id":"req-1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"id":"req-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":31,"completion_tokens":2,"total_tokens":33}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := NewOpenAIProviderWithCapabilities(srv.URL, "test-key", FullOpenAICompatCapabilities())
	var end *LLMEndEvent
	if err := p.Stream(context.Background(), &StreamRequest{
		Provider: "deepseek", Model: "deepseek-v4-flash",
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
	}, func(e StreamEvent) error {
		if v, ok := e.(LLMEndEvent); ok {
			ev := v
			end = &ev
		}
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if end == nil {
		t.Fatal("未收到 LLMEndEvent")
	}
	if end.Usage.Cost.Source == core.CostSourceUpstream {
		t.Fatalf("上游没报费用却标成 upstream：%+v", end.Usage.Cost)
	}
	if end.Usage.Cost.Total <= 0 {
		t.Fatalf("应回退价表算出正数成本，got %+v", end.Usage.Cost)
	}
}
