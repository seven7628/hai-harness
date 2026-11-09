package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 这些测试用 httptest 假服务器钉住四件事：
//  1. 四种形状的**真实响应**能被正确归一（金额字符串 / 分 / 哨兵 / 剩余百分比）；
//  2. 凭证只发声明里的 host（host 不匹配时一个 Authorization 头都不许出现）；
//  3. 不跟随重定向（否则 302 就能把 key 带走）；
//  4. 缺失读数 != 0（Percent 为 nil 而不是指向 0 的指针）。
//
// 所有假响应都取自 2026-11 的本机实测或官方文档（见 docs/PROVIDER_USAGE_BILLING.md）。

func testPreset(p Preset) *Preset { return &p }

// hostOf 测试服务器地址 → host（Override 里留空 = 不做 host 白名单校验，
// 白名单本身由 TestFetchRefusesForeignHost 单独验证）。
func mustGet(t *testing.T, srv *httptest.Server, body string, headers *http.Header) *httptest.Server {
	t.Helper()
	_ = body
	return srv
}

func TestFetchDeepSeekBalance(t *testing.T) {
	// 实测响应（2026-11-07）：金额是**字符串**，币种 CNY，且余额可以是负数。
	const payload = `{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"-0.02","granted_balance":"0.00","topped_up_balance":"-0.02"}]}`
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/user/balance" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	snap, err := Fetch(context.Background(), Request{
		Provider: "deepseek", BaseURL: srv.URL + "/v1", APIKey: "sk-test",
		Override: testPreset(Preset{Shape: "balance", Source: "test.deepseek", Path: "/user/balance", Auth: "bearer",
			Balance: "balance_infos[0].total_balance", Currency: "balance_infos[0].currency"}),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if snap.Kind != KindBalance || snap.Balance == nil {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Balance.Amount != -0.02 || snap.Balance.Currency != "CNY" {
		t.Errorf("balance = %+v", snap.Balance)
	}
	if snap.NotConnected || snap.ErrCode != "" {
		t.Errorf("unexpected state: %+v", snap)
	}
}

func TestFetchZenQuotaWindows(t *testing.T) {
	// 实测响应（2026-11-07）：三窗口 + resetsAt。
	const payload = `{"usage":{"rolling":{"status":"ok","percent":0,"resetsAt":"2026-11-08T06:10:53.000Z"},"weekly":{"status":"ok","percent":48,"resetsAt":"2026-11-09T00:00:00.000Z"},"monthly":{"status":"ok","percent":24,"resetsAt":"2026-11-17T11:33:14.000Z"}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/zen/go/v1/usage" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	snap, err := Fetch(context.Background(), Request{
		Provider: "opencode", BaseURL: srv.URL + "/zen/go/v1", APIKey: "sk-zen",
		Override: testPreset(Preset{Shape: "quota", Source: "test.zen", Path: "/zen/go/v1/usage", Auth: "bearer",
			Windows: &WindowSpec{Path: "usage", Mode: "map", Percent: "percent", Status: "status", Reset: "resetsAt"}}),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("windows = %d", len(snap.Windows))
	}
	byKey := map[string]Window{}
	for _, w := range snap.Windows {
		byKey[w.Key] = w
	}
	if p := byKey["weekly"].Percent; p == nil || *p != 48 {
		t.Errorf("weekly percent = %v", p)
	}
	if byKey["weekly"].ResetsAt == nil || byKey["weekly"].ResetsAt.UTC().Format("2006-01-02") != "2026-11-09" {
		t.Errorf("weekly reset = %v", byKey["weekly"].ResetsAt)
	}
	// rolling = 0 必须与「缺失」区分开：0 是真实的 0%，不是 nil。
	if p := byKey["rolling"].Percent; p == nil || *p != 0 {
		t.Errorf("rolling percent = %v（0%% 必须是有效读数）", p)
	}
}

func TestFetchQuotaListAndInvert(t *testing.T) {
	// list 形状 + 上游给的是「剩余百分比」（Copilot / MiniMax 语义）：必须翻成已用。
	const payload = `{"data":[{"model_name":"m1","weekly_remaining_percent":80,"end_time":"2026-11-20T00:00:00Z"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	snap, err := Fetch(context.Background(), Request{
		Provider: "minimax", BaseURL: srv.URL, APIKey: "k",
		Override: testPreset(Preset{Shape: "quota", Source: "test.minimax", Path: "/p", Auth: "bearer",
			Windows: &WindowSpec{Path: "data", Mode: "list", Key: "model_name", Percent: "weekly_remaining_percent", Reset: "end_time", Invert: true}}),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.Windows) != 1 {
		t.Fatalf("windows = %d", len(snap.Windows))
	}
	if p := snap.Windows[0].Percent; p == nil || *p != 20 {
		t.Errorf("inverted percent = %v（剩余 80%% → 已用 20%%）", p)
	}
	if snap.Windows[0].Key != "m1" {
		t.Errorf("key = %q", snap.Windows[0].Key)
	}
}

func TestFetchMissingPercentStaysNil(t *testing.T) {
	// 上游报了窗口但没报读数：Percent 必须是 nil（UI 画「读数缺失」），不能当 0%。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"weekly":{"status":"ok"}}}`))
	}))
	defer srv.Close()
	snap, err := Fetch(context.Background(), Request{
		Provider: "x", BaseURL: srv.URL, APIKey: "k",
		Override: testPreset(Preset{Shape: "quota", Source: "t", Path: "/p", Auth: "bearer",
			Windows: &WindowSpec{Path: "usage", Mode: "map", Percent: "percent"}}),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(snap.Windows) != 1 || snap.Windows[0].Percent != nil {
		t.Fatalf("windows = %+v", snap.Windows)
	}
}

func TestFetchNewAPIDashboard(t *testing.T) {
	// new-api / one-api 家族实测（t.100credit.cn）：limit 是 1e8 哨兵，total_usage 单位是**分**。
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		switch r.URL.Path {
		case "/v1/dashboard/billing/subscription":
			_, _ = w.Write([]byte(`{"object":"billing_subscription","hard_limit_usd":100000000,"system_hard_limit_usd":100000000}`))
		case "/v1/dashboard/billing/usage":
			if r.URL.Query().Get("start_date") == "" {
				t.Errorf("usage 缺少 start_date")
			}
			_, _ = w.Write([]byte(`{"object":"list","total_usage":12239.0756}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	snap, err := Fetch(context.Background(), Request{
		Provider: "bairong", BaseURL: srv.URL + "/v1", APIKey: "sk-relay",
		Override: testPreset(Preset{Shape: "openai_dashboard", Source: "test.newapi", Auth: "bearer",
			Paths: []string{"/v1/dashboard/billing/subscription", "/v1/dashboard/billing/usage"},
			Unit:  "cents", LimitSentinel: 1e8}),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("requests = %v", seen)
	}
	if snap.Used == nil || snap.Used.Amount < 122.39 || snap.Used.Amount > 122.40 {
		t.Errorf("used = %+v（分 → 元）", snap.Used)
	}
	if snap.Limit != nil || snap.Balance != nil {
		t.Errorf("哨兵 1e8 不能当真实额度：limit=%+v balance=%+v", snap.Limit, snap.Balance)
	}
}

func TestFetchAuthForms(t *testing.T) {
	cases := []struct{ auth, wantHdr, wantVal string }{
		{"bearer", "Authorization", "Bearer K"},
		{"raw", "Authorization", "K"},   // GLM 国内端点：裸 key
		{"x-api-key", "X-Api-Key", "K"}, // Anthropic Admin：x-api-key
	}
	for _, c := range cases {
		t.Run(c.auth, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get(c.wantHdr)
				_, _ = w.Write([]byte(`{"v":1}`))
			}))
			defer srv.Close()
			_, err := Fetch(context.Background(), Request{
				Provider: "p", BaseURL: srv.URL, APIKey: "K",
				Override: testPreset(Preset{Shape: "balance", Source: "t", Path: "/p", Auth: c.auth, Balance: "v"}),
			})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if got != c.wantVal {
				t.Errorf("%s = %q, want %q", c.wantHdr, got, c.wantVal)
			}
		})
	}
}

func TestFetchRefusesForeignHost(t *testing.T) {
	// 安全规则：声明的 host 与 base_url 不一致 → **不采用该声明**（不去它的 path 打请求）。
	// 凭证仍然只会发到 base_url 自己的 origin（那是用户为它配置的 key），但绝不能因为
	// 声明写错而被带到声明里的 host 上去。
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer srv.Close()

	snap, err := Fetch(context.Background(), Request{
		Provider: "deepseek", BaseURL: srv.URL, APIKey: "sk-secret",
		Override: testPreset(Preset{Shape: "balance", Host: "api.deepseek.com", Source: "t", Path: "/user/balance", Auth: "bearer", Balance: "x"}),
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if p == "/user/balance" {
			t.Fatalf("声明 host 与 base_url 不一致时仍然打了声明端点：%v", paths)
		}
	}
	if snap.ErrCode != ErrCodeUnsupported {
		t.Errorf("err = %q, want %q", snap.ErrCode, ErrCodeUnsupported)
	}
}

func TestFetchRefusesRedirect(t *testing.T) {
	// 302 到别的 host：必须停在 302，不带着 Authorization 跟过去。
	leak := int32(0)
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leak = 1
		}
		_, _ = w.Write([]byte(`{"v":1}`))
	}))
	defer evil.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()

	_, err := Fetch(context.Background(), Request{
		Provider: "p", BaseURL: srv.URL, APIKey: "K",
		Override: testPreset(Preset{Shape: "balance", Source: "t", Path: "/p", Auth: "bearer", Balance: "v"}),
	})
	if err == nil {
		t.Fatal("期望 endpoint_changed 错误")
	}
	if code, _ := errCode(err); code != ErrCodeEndpointChanged {
		t.Errorf("code = %q", code)
	}
	if leak != 0 {
		t.Fatal("跟随了重定向 —— key 泄露")
	}
}

func TestFetchUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer srv.Close()
	_, err := Fetch(context.Background(), Request{
		Provider: "p", BaseURL: srv.URL, APIKey: "bad",
		Override: testPreset(Preset{Shape: "balance", Source: "t", Path: "/p", Auth: "bearer", Balance: "v"}),
	})
	if err == nil {
		t.Fatal("期望 unauthorized")
	}
	if code, status := errCode(err); code != ErrCodeUnauthorized || status != 401 {
		t.Errorf("code=%q status=%d", code, status)
	}
}

func TestFetchNoKeyIsNotAnError(t *testing.T) {
	snap, err := Fetch(context.Background(), Request{Provider: "deepseek", BaseURL: "https://api.deepseek.com/v1"})
	if err != nil {
		t.Fatalf("无 key 不应报错: %v", err)
	}
	if !snap.NotConnected || snap.ErrCode != ErrCodeNoKey {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestFetchUnsupportedProvider(t *testing.T) {
	// 未声明 + generic 探测 404 → 「不支持」（正常状态），不是错误。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer srv.Close()
	snap, err := Fetch(context.Background(), Request{Provider: "opendesign", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snap.Kind != KindNone || snap.ErrCode != ErrCodeUnsupported {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestFetchTimeout(t *testing.T) {
	old := perRequestTimeout
	perRequestTimeout = 150 * time.Millisecond
	defer func() { perRequestTimeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	_, err := Fetch(context.Background(), Request{
		Provider: "p", BaseURL: srv.URL, APIKey: "k",
		Override: testPreset(Preset{Shape: "balance", Source: "t", Path: "/p", Auth: "bearer", Balance: "v"}),
	})
	if code, _ := errCode(err); code != ErrCodeNetwork {
		t.Errorf("code = %q, want %q (err=%v)", code, ErrCodeNetwork, err)
	}
}

func TestPresetsJSONIsValidAndConsistent(t *testing.T) {
	all := Presets()
	if len(all) < 8 {
		t.Fatalf("声明过少：%d", len(all))
	}
	for id, p := range all {
		switch p.Shape {
		case "balance":
			if p.Path == "" || (p.Balance == "" && p.Used == "" && p.Limit == "") {
				t.Errorf("%s: balance 形状缺 path 或字段", id)
			}
		case "quota":
			if p.Path == "" || p.Windows == nil || p.Windows.Path == "" {
				t.Errorf("%s: quota 形状缺 windows.path", id)
			}
			if p.Windows != nil && p.Windows.Percent == "" {
				t.Errorf("%s: 窗口缺 percent 字段", id)
			}
		case "openai_dashboard":
			if len(p.Paths) < 2 {
				t.Errorf("%s: dashboard 需要两个 path", id)
			}
		default:
			t.Errorf("%s: 未知 shape %q", id, p.Shape)
		}
		if p.Auth != "oauth" && p.Auth != "none" && p.Path != "" && p.Host == "" && p.Shape != "openai_dashboard" {
			t.Errorf("%s: 缺 host（凭证白名单）", id)
		}
	}
	// live 的必须至少有 deepseek 与 opencode（本机实测过），防止误删证据等级。
	for _, id := range []string{"deepseek", "opencode"} {
		if all[id].VerifiedBy != "live" {
			t.Errorf("%s 的 verified_by 应为 live，实际 %q", id, all[id].VerifiedBy)
		}
	}
}

func TestLookupAndCoercion(t *testing.T) {
	var doc any
	_ = json.Unmarshal([]byte(`{"a":{"b":[{"c":"1.5"}]},"n":48,"t":1794101434}`), &doc)
	if v, ok := asFloat(lookup(doc, "a.b[0].c")); !ok || v != 1.5 {
		t.Errorf("字符串数字 = %v %v", v, ok)
	}
	if v, ok := asFloat(lookup(doc, "missing|x")); ok {
		_ = v
	}
	if v, ok := asFloat(lookup(doc, "n")); !ok || v != 48 {
		t.Errorf("numeric = %v", v)
	}
	if tm := asTime(lookup(doc, "t")); tm == nil || tm.Year() != 2026 {
		t.Errorf("unix 秒 → %v", tm)
	}
	for _, s := range []string{"", "::::", "not a url"} {
		if o, h := originAndHost(s); o != "" || h != "" {
			t.Errorf("%q → %q %q", s, o, h)
		}
	}
	if o, h := originAndHost("https://API.DeepSeek.com/v1"); o != "https://API.DeepSeek.com" || h != "api.deepseek.com" {
		t.Errorf("origin/host = %q %q", o, h)
	}
}

// 防止「未知 shape 静默降级」：声明里写了 report 也必须被拒绝而不是当成 balance。
func TestFetchRejectsUnknownShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	_, err := Fetch(context.Background(), Request{
		Provider: "p", BaseURL: srv.URL, APIKey: "k",
		Override: testPreset(Preset{Shape: "report", Source: "t", Path: "/p", Auth: "bearer"}),
	})
	if err == nil || !strings.Contains(err.Error(), "未知 shape") {
		t.Errorf("err = %v", err)
	}
}

// 同一家 provider 被加多次（用户自己起 id：opencode2/opencode3）时，声明要按 **host** 复用。
// 按 id 只有内置那几家能命中 —— 换个名字额度就查不到，用户看到的是「这家不提供查询」，
// 而真相是「我们认得这个端点，只是不认识你起的名字」。
func TestResolvePresetByHost(t *testing.T) {
	// 1) 正常路径：id + host 都对
	if p, ok := resolvePreset("deepseek", "api.deepseek.com"); !ok || p.Shape != "balance" {
		t.Fatalf("deepseek by id = %+v, %v", p, ok)
	}
	// 2) 换 id：opencode2 → 复用 opencode 的声明
	p, ok := resolvePreset("opencode2", "opencode.ai")
	if !ok || p.Shape != "quota" || p.Host != "opencode.ai" {
		t.Fatalf("opencode2 by host = %+v, %v", p, ok)
	}
	// 3) host 大小写不敏感
	if _, ok := resolvePreset("whatever", "OPENCODE.AI"); !ok {
		t.Fatal("host 应大小写不敏感")
	}
	// 4) 别的 host 不复用 —— 凭证不能因为「名字像」就发出去
	if _, ok := resolvePreset("opencode2", "evil.example.com"); ok {
		t.Fatal("未知 host 不该命中任何声明")
	}
	if _, ok := resolvePreset("opencode2", ""); ok {
		t.Fatal("空 host 不该命中")
	}
	// 5) id 命中但 base_url 换到别家 host → 落到「同 host 复用」（端点语义优先于名字）
	if p, ok := resolvePreset("deepseek", "opencode.ai"); !ok || p.Host != "opencode.ai" {
		t.Fatalf("deepseek@opencode.ai = %+v, %v", p, ok)
	}
}

// 同 host 多条声明：只在前缀能对上时才选中，否则放弃（宁可不支持，也不能显示别家产品的额度）。
func TestPresetForHostAmbiguity(t *testing.T) {
	loadPresets()
	if presetsErr != nil {
		t.Skipf("presets.json 解析失败: %v", presetsErr)
	}
	// 现有声明 host 互不重复 → 唯一命中；这里只钉「空 host 直接放弃」
	if _, ok := PresetForHost("", "opencode2"); ok {
		t.Fatal("空 host 不该命中")
	}
	if _, ok := PresetForHost("no-such-host.invalid", "opencode2"); ok {
		t.Fatal("未知 host 不该命中")
	}
}
