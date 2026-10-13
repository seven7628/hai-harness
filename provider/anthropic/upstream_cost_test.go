package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"

	"github.com/anthropics/anthropic-sdk-go/packages/respjson"
)

// TestUpstreamCostFromExtra usage 未建模字段里的上游自报费用：认哪个键、缺与 0 的区别。
//
// 这条通道服务的是「把 OpenAI 风格 cost 一并塞进 Anthropic usage」的兼容/聚合端点
// （官方端点不报费用）。取数方式与 looksOpenAIUsageShape 相同：未建模字段 Valid()
// 恒 false，只能用 Raw() 判存在。
func TestUpstreamCostFromExtra(t *testing.T) {
	cases := []struct {
		name   string
		extra  map[string]respjson.Field
		want   int64
		wantOK bool
	}{
		{
			name:   "cost 数字",
			extra:  map[string]respjson.Field{"cost": respjson.NewInvalidField("0.000042")},
			want:   42,
			wantOK: true,
		},
		{
			name:   "total_cost 字符串",
			extra:  map[string]respjson.Field{"total_cost": respjson.NewInvalidField(`"0.5"`)},
			want:   500000,
			wantOK: true,
		},
		{
			name:   "缺失 = 上游没报（不是 0）",
			extra:  map[string]respjson.Field{"input_tokens": respjson.NewInvalidField("1")},
			want:   0,
			wantOK: false,
		},
		{
			name:   "显式 0 = 上游说免费",
			extra:  map[string]respjson.Field{"cost": respjson.NewInvalidField("0")},
			want:   0,
			wantOK: true,
		},
		{
			name:   "null 不算存在",
			extra:  map[string]respjson.Field{"cost": respjson.NewInvalidField(respjson.Null)},
			want:   0,
			wantOK: false,
		},
		{
			name:   "负值哨兵不认",
			extra:  map[string]respjson.Field{"cost": respjson.NewInvalidField("-1000000")},
			want:   0,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := upstreamCostFromExtra(c.extra)
			if ok != c.wantOK || got != c.want {
				t.Fatalf("upstreamCostFromExtra = (%d, %v), want (%d, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}

// anthropicStreamCost 跑一轮 anthropic 流，返回 LLMEndEvent 的 Usage.Cost。
// usageExtra 追加到 message_delta 的 usage 对象里（模拟兼容端点塞费用字段）。
func anthropicStreamCost(t *testing.T, usageExtra string) core.Cost {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\",\"model\":\"deepseek-v4-flash\",\"content\":[],\"usage\":{\"input_tokens\":1000}}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":100"+usageExtra+"}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	p := NewAnthropicProvider(provider.Dependencies{Registry: provider.NewRegistry(), HTTPClient: srv.Client()}, srv.URL, "k")
	var cost core.Cost
	var got bool
	err := p.Stream(context.Background(), &provider.StreamRequest{
		Provider: "deepseek", Model: "deepseek-v4-flash",
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
	}, func(e provider.StreamEvent) error {
		if end, ok := e.(provider.LLMEndEvent); ok {
			cost, got = end.Usage.Cost, true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !got {
		t.Fatal("未收到 LLMEndEvent")
	}
	return cost
}

// TestAnthropicStreamPrefersUpstreamCost 兼容端点把费用塞进 usage 扩展字段 → 取上游值，
// 价表不参与（与 openai/responses 两条协议同一口径，见 provider.ApplyUsageCost）。
func TestAnthropicStreamPrefersUpstreamCost(t *testing.T) {
	cost := anthropicStreamCost(t, `,"cost":0.000042`)
	if cost.Source != core.CostSourceUpstream {
		t.Fatalf("应取上游自报费用，实际 %+v", cost)
	}
	if cost.Total != 42 {
		t.Fatalf("上游 cost 0.000042 → 42 µUSD, got %d", cost.Total)
	}
}

// TestAnthropicStreamFallsBackWithoutUpstreamCost 上游不给费用 → 回退本地价表。
// 同时钉住 1h 缓存写入的 2× 修正**只在估算路径**生效（对实收金额加价会把账算错）。
func TestAnthropicStreamFallsBackWithoutUpstreamCost(t *testing.T) {
	cost := anthropicStreamCost(t, "")
	if cost.Source == core.CostSourceUpstream {
		t.Fatalf("上游没报费用却标成 upstream：%+v", cost)
	}
	if cost.Total <= 0 {
		t.Fatalf("应回退价表算出正数成本，got %+v", cost)
	}
}
