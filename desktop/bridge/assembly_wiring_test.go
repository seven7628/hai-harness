// C7 生产装配断言 —— 让「机制被悄悄关掉」**必然导致测试失败**。
//
// 背景（本批的实证样本）：agents.WithPostToolNudgeEnabled 机制完整、agents 侧有测试，
// 但**生产从未接线**，长达数月 = 「看起来有、实际失效」的第三类。「装配断言本身就是
// 第三类失效」的风险是真实的：一条永远为真的断言比没有断言更坏。故本文件三层都要有：
//
//	① 值层    TestCodeAgentAssemblyTuningValues / ...WiringFollowsSettings
//	          —— 读的是**生产装配用的同一个值类型**（codeAgentTuningFor），不是副本；
//	② 应用层  TestCodeAgentAssemblyOptionsApplyTuning
//	          —— 值经 options() 真的写进了 agents.Config（字段名写错 / 忘记 emit / emit 成
//	             别的值，三种「看起来对」的写法都在这里红）；
//	③ 行为层  TestCodeAgentPostToolNudgeBehavior / ...DisabledStopsSilently
//	          TestCodeAgentGoalAlignmentReminderBehavior
//	          —— 三宿主各自的生产装配路径 + 脚本化 provider（真 HTTP 流式），断言
//	             「模型这一轮的请求里到底有没有收到那段提醒」。值断言无法覆盖「值 → 选项的
//	             应用」与「选项 → agents 侧消费」两段，行为断言覆盖它们。
//
// 可证伪性由变异验证保证（破坏生产值 ⇒ 对应测试必须红，见 FIX_PLAN_BATCH3 C7）。
// 范本：TestExecSandboxForWorkPersonaRunPython（装配链上任一处短路的教训）、
// TestPlanSubmitNotRegistered（反向断言）、agents/agent_loop_test.go 的脚本化 provider。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// c7ProbeModel 探针模型名：注册表未收录 ⇒ 窗口/输出上限按 pc.models/maxTokens 显式给出，
// 不依赖真实模型表（协议层与生产同构：真 HTTP provider + chat-completions 流式）。
const c7ProbeModel = "c7-assembly-probe"

// c7DoneText 脚本化 provider 的收尾文本（断言「任务真的被跑完」的可见产出）。
const c7DoneText = "c7 done"

// codeAgentHostCases 三个宿主的生产装配入口：loop 一律经生产函数产出，测试不自己拼 loop
// （断言对象是「装配」，不是「测试怎么写 loop」）。
// prep 在 HOME 隔离之后、装配之前执行（主宿主在装配期读 settings，写配置必须卡在这个窗口）；
// 子宿主不读 settings，prep 传 nil 即可。
func codeAgentHostCases() []struct {
	host  codeAgentHost
	build func(t *testing.T, baseURL string, prep func(t *testing.T)) *agents.AgentLoop
} {
	return []struct {
		host  codeAgentHost
		build func(t *testing.T, baseURL string, prep func(t *testing.T)) *agents.AgentLoop
	}{
		{hostMainLoop, func(t *testing.T, baseURL string, prep func(t *testing.T)) *agents.AgentLoop {
			m, _ := testManager(t) // 隔离 HOME；清理时统一停会话
			if prep != nil {
				prep(t)
			}
			m.provCfg.setBaseURL("deepseek", baseURL)
			m.provCfg.setKey("deepseek", "sk-c7-probe")
			bs, err := m.create(t.TempDir(), "c7-main", "") // 生产新会话路径（内部 buildLoop）
			if err != nil {
				t.Fatalf("create 主会话: %v", err)
			}
			if bs.loop == nil {
				t.Fatal("主会话应有 loop（buildLoop 装配）")
			}
			return bs.loop
		}},
		{hostSpawnLoop, func(t *testing.T, baseURL string, _ func(t *testing.T)) *agents.AgentLoop {
			ws := t.TempDir()
			pc := c7ProbeProviderConfig(baseURL)
			return buildSpawnLoop(c7ProbeEngine(t, ws), ws, "c7-spawn", pc, "deepseek", c7ProbeModel, "low", nil)
		}},
		{hostExploreLoop, func(t *testing.T, baseURL string, _ func(t *testing.T)) *agents.AgentLoop {
			ws := t.TempDir()
			pc := c7ProbeProviderConfig(baseURL)
			return buildExploreLoop(ws, "c7-explore", pc, "deepseek", c7ProbeModel, "low", nil)
		}},
	}
}

// c7ProbeProviderConfig 探针 provider 配置：与生产同构，只把 base_url 指向脚本化假服务。
// 生产装配内部自建 provider（pc.newProvider），包内没有别的注入接缝 —— 改 base_url 是
// 唯一能走通「真 HTTP 流式 + 生产装配」的做法（同 desktop_coordination_test /
// opencode_gateway_test 的做法）。
func c7ProbeProviderConfig(baseURL string) *providerConfig {
	return &providerConfig{
		active:    "deepseek",
		baseURL:   map[string]string{"deepseek": baseURL},
		apiKey:    map[string]string{"deepseek": "sk-c7-probe"},
		models:    map[string]map[string]int64{"deepseek": {c7ProbeModel: 200000}},
		maxTokens: map[string]map[string]int64{"deepseek": {c7ProbeModel: 8192}},
		compat:    map[string]provider.OpenAICompatCapabilities{"deepseek": provider.FullOpenAICompatCapabilities()},
		registry:  provider.NewRegistry(),
		protocol:  map[string]string{"deepseek": protocolChatCompletions},
	}
}

// c7ProbeEngine spawn 宿主的工具引擎（生产下 spawn 继承主会话引擎；此处只需一个含 glob
// 的真引擎，脚本化工具轮才能真实执行 —— 工具轮是否成功不影响断言，触发条件只看「上一轮
// 是工具轮」）。
func c7ProbeEngine(t *testing.T, ws string) *tools.Engine {
	t.Helper()
	eng := tools.NewToolEngine()
	for _, tl := range builtin.NewFileTools(ws).Tools() {
		eng.RegisterTool(context.Background(), tl)
	}
	if _, err := eng.GetTool(context.Background(), "glob"); err != nil {
		t.Fatalf("探针引擎缺 glob（脚本化工具轮需要它）: %v", err)
	}
	return eng
}

// c7UserInput 每个宿主都从同一条用户输入起跑。
func c7UserInput() []core.Message {
	return []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "c7 task"})}
}

// scriptedCodeAgentLLM 脚本化 chat-completions 流式假服务：按「第 N 次请求」回放 reply
// 给出的轮次，并逐次记录**请求体原文**。请求体是「模型这一轮到底看到了什么」的唯一观测
// 点 —— 提醒/续跑都是 ephemeral 注入到请求消息里的，不进会话历史，事件流里也没有。
type scriptedCodeAgentLLM struct {
	mu     sync.Mutex
	bodies []string
	srv    *httptest.Server
}

func newScriptedCodeAgentLLM(t *testing.T, reply func(round int, body string) []map[string]any) *scriptedCodeAgentLLM {
	t.Helper()
	s := &scriptedCodeAgentLLM{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		round := len(s.bodies)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range reply(round, string(body)) {
			b, _ := json.Marshal(chunk)
			if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
				return
			}
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *scriptedCodeAgentLLM) url() string { return s.srv.URL }

// requests 已收到的请求次数。
func (s *scriptedCodeAgentLLM) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// requestBody 第 n 次（1-based）请求体原文；越界直接失败（调用方断言的是「请求序列」，
// 次数不对本身就是装配失效的证据）。
func (s *scriptedCodeAgentLLM) requestBody(t *testing.T, n int) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 1 || n > len(s.bodies) {
		t.Fatalf("第 %d 次请求不存在（共 %d 次）", n, len(s.bodies))
	}
	return s.bodies[n-1]
}

// c7Chunk 构造一个 chat-completions 流式 chunk（格式与 provider/openai_test.go 的假流一致）。
func c7Chunk(delta map[string]any, finish string) map[string]any {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != "" {
		choice["finish_reason"] = finish
	}
	return map[string]any{
		"id": "c7-chunk", "object": "chat.completion.chunk", "model": c7ProbeModel,
		"choices": []any{choice},
	}
}

// c7ToolRound 工具轮：调 glob（白名单外的只读工具 —— 在 goalAlignment 判据里不算「进展」，
// 故连续工具轮会持续累积无进展轮数）。
func c7ToolRound() []map[string]any {
	return []map[string]any{
		c7Chunk(map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_c7", "type": "function",
				"function": map[string]any{"name": "glob", "arguments": `{"pattern":"*"}`},
			}},
		}, ""),
		c7Chunk(map[string]any{}, "tool_calls"),
	}
}

// c7EmptyRound 空回复轮：只有 reasoning、无 content、无工具调用即 stop —— 2026-09-18 静默
// 死亡的形态（85/2438 run = 3.5% 以该形态收尾）。
func c7EmptyRound() []map[string]any {
	return []map[string]any{
		c7Chunk(map[string]any{"reasoning_content": "……"}, ""),
		c7Chunk(map[string]any{}, "stop"),
	}
}

// c7TextRound 可见文本收尾轮。
func c7TextRound(text string) []map[string]any {
	return []map[string]any{
		c7Chunk(map[string]any{"content": text}, ""),
		c7Chunk(map[string]any{}, "stop"),
	}
}

// bodyContainsText 请求体 JSON 里是否出现过 want（递归收集所有字符串叶子）。
// 为什么不能直接对原文做子串匹配：提醒文案含换行，JSON 转义后原文里是 "\n"。
func bodyContainsText(t *testing.T, body, want string) bool {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("请求体不是合法 JSON: %v", err)
	}
	var walk func(any) bool
	walk = func(x any) bool {
		switch x := x.(type) {
		case string:
			return strings.Contains(x, want)
		case []any:
			for _, it := range x {
				if walk(it) {
					return true
				}
			}
		case map[string]any:
			for _, it := range x {
				if walk(it) {
					return true
				}
			}
		}
		return false
	}
	return walk(v)
}

// applyCodeAgentTuning 把可调项经生产 options() 应用到 agents.Config —— 应用层的观测点。
// 用 agents 导出的 Config/Option（Option = func(*Config)）即可，无需 agents 提供 getter；
// 零值 Config 的 CompressThreshold = 0、EnablePostToolNudge = false ⇒ 「忘记 emit」会暴露。
func applyCodeAgentTuning(tuning codeAgentTuning) agents.Config {
	var cfg agents.Config
	for _, o := range tuning.options() {
		o(&cfg)
	}
	return cfg
}

// writeCodeAgentSettings 把 agent.reminder_rounds 写进**当前（已隔离的）HOME** 的
// settings.json（loadAgentSettings 的读取源）。
func writeCodeAgentSettings(t *testing.T, rounds int) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	dir := filepath.Join(home, ".go-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(fmt.Sprintf(`{"agent":{"reminder_rounds":%d}}`, rounds))
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ============================== ① 值层 ==============================

// TestCodeAgentAssemblyTuningValues 断言清单 ①③（值层）+ ② 的「接线与否」：
//   - postToolNudge：三宿主全员 true（该机制曾在生产零接线数月 —— 第三类样本本身）；
//   - compressThreshold：三宿主全为 0.8（**钉住现值**，不是改动：谁想再动它 —— 例如回到
//     2026-08-30 试过的 0.72 —— 必须先让本测试红起来，而不是悄悄改一处）；
//   - goalAlignmentRounds：主会话必须接线（非 nil）；子宿主当前刻意不接线；
//   - firstChunkTimeout（2026-10-11）：三宿主必须全接线且 >0 —— 本项与 postToolNudge
//     同病相怜（曾零生产接线），「不接线」在这里必须是红脸，不能是默认放行。
//
// 读的是生产装配用的同一个值类型（codeAgentTuningFor），不是硬编码副本 ⇒ 改动生产取值
// 本测试即红（已用变异验证证明）。
func TestCodeAgentAssemblyTuningValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // 封闭：不读用户真实 ~/.go-code/settings.json
	for _, host := range []codeAgentHost{hostMainLoop, hostSpawnLoop, hostExploreLoop} {
		tuning := codeAgentTuningFor(host)
		if !tuning.postToolNudge {
			t.Errorf("%s: postToolNudge = false —— 三宿主必须全开（该机制生产零接线数月，见 codeAgentTuning 注释）", host)
		}
		if tuning.compressThreshold != 0.8 {
			t.Errorf("%s: compressThreshold = %v, want 0.8（钉住现值；用户 2026-09-18 决策：压缩阈值不改）", host, tuning.compressThreshold)
		}
		// 首字预算：未配置 settings 时兜底 90s（agents 默认同值）。钉住的是「必须接线」
		// 与「必须是正数」两条，不钉具体秒数（用户可按 provider 特性改，见
		// TestFirstChunkTimeoutWiringFollowsSettings）。
		if tuning.firstChunkTimeout <= 0 {
			t.Errorf("%s: firstChunkTimeout = %v —— 三宿主必须全接线且为正（0 = 立即超时，绝不是合法策略；曾零生产接线，见 codeAgentTuning 注释）",
				host, tuning.firstChunkTimeout)
		}
		if host == hostMainLoop {
			if tuning.goalAlignmentRounds == nil {
				t.Errorf("%s: goalAlignmentRounds 未接线（nil）—— 主会话必须接线到 loadAgentSettings().ReminderRounds", host)
			}
		} else if tuning.goalAlignmentRounds != nil {
			t.Errorf("%s: goalAlignmentRounds 意外接线（= %d）—— 子宿主当前刻意不接线（agents 缺省 0 = 禁用）；若是有意改动请同步更新本断言",
				host, *tuning.goalAlignmentRounds)
		}
	}
}

// TestCodeAgentGoalAlignmentWiringFollowsSettings 断言 ②「被接线」而非固定数字：
// settings.agent.reminder_rounds 写多少，主会话装配值就是多少（同一次测试内改两次文件，
// 值跟着走 ⇒ 证明确实读的 settings，而不是硬编码），并经 options() 落到 Config。
// 默认值 30 / 负值回退默认由既有 TestLoadAgentSettingsDefaultAndConfigured（agent_settings_test.go）
// 覆盖，本测试不重复。含 0：0 是合法取值（显式禁用），必须与「本宿主不接线（nil）」区分
// —— 这正是 goalAlignmentRounds 用指针而非 0 值哨兵的原因。
func TestCodeAgentGoalAlignmentWiringFollowsSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, rounds := range []int{17, 5, 0} {
		writeCodeAgentSettings(t, rounds)
		tuning := codeAgentTuningFor(hostMainLoop)
		if tuning.goalAlignmentRounds == nil {
			t.Fatalf("reminder_rounds=%d：主会话 goalAlignmentRounds 未接线（nil）", rounds)
		}
		if *tuning.goalAlignmentRounds != rounds {
			t.Fatalf("reminder_rounds=%d：装配值 = %d（没跟着设置走 ⇒ 未接线或读错源）", rounds, *tuning.goalAlignmentRounds)
		}
		if got := applyCodeAgentTuning(tuning).GoalAlignmentReminderRounds; got != rounds {
			t.Fatalf("reminder_rounds=%d：Config.GoalAlignmentReminderRounds = %d（options() 未应用该值）", rounds, got)
		}
	}
}

// ============================== ② 应用层 ==============================

// TestCodeAgentAssemblyOptionsApplyTuning 值 → 选项的**应用**环节（值断言的固有缺口）：
// 三个机制都必须经 options() 真正写进 agents.Config —— 字段名写错、忘记 emit、emit 成别的
// 值，三种「看起来有」的写法都会在这里红。
func TestCodeAgentAssemblyOptionsApplyTuning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, host := range []codeAgentHost{hostMainLoop, hostSpawnLoop, hostExploreLoop} {
		tuning := codeAgentTuningFor(host)
		cfg := applyCodeAgentTuning(tuning)
		if !cfg.EnablePostToolNudge {
			t.Errorf("%s: options() 未把 postToolNudge 应用到 Config.EnablePostToolNudge", host)
		}
		if cfg.CompressThreshold != 0.8 {
			t.Errorf("%s: Config.CompressThreshold = %v, want 0.8（emit 了阈值但值不对，或没 emit）", host, cfg.CompressThreshold)
		}
		// 首字预算：值必须一路落到 Config.FirstChunkTimeout（这是「零生产接线」的
		// 直接防线 —— 少这一条，测试只断言值层，options() 忘记 emit 仍会全绿）。
		if cfg.FirstChunkTimeout != tuning.firstChunkTimeout || cfg.FirstChunkTimeout <= 0 {
			t.Errorf("%s: Config.FirstChunkTimeout = %v, want %v（options() 未应用 firstChunkTimeout）",
				host, cfg.FirstChunkTimeout, tuning.firstChunkTimeout)
		}
		if host == hostMainLoop {
			if tuning.goalAlignmentRounds == nil || cfg.GoalAlignmentReminderRounds != *tuning.goalAlignmentRounds || cfg.GoalAlignmentReminderRounds == 0 {
				t.Errorf("%s: Config.GoalAlignmentReminderRounds = %d, want %v（options() 未应用 goalAlignmentRounds）",
					host, cfg.GoalAlignmentReminderRounds, tuning.goalAlignmentRounds)
			}
		} else if cfg.GoalAlignmentReminderRounds != 0 {
			t.Errorf("%s: Config.GoalAlignmentReminderRounds = %d, want 0（子宿主不接线）", host, cfg.GoalAlignmentReminderRounds)
		}
	}
}

// TestFirstChunkTimeoutWiringFollowsSettings 首字预算**跟着 settings 走**（2026-10-11）：
// 本测试存在的理由 = 该参数曾零生产接线（桌面端三宿主全吃 agents 默认 30s，用户/OPS
// 无法覆盖），弱网/长首字模型每次尝试都被切成「上游故障」、重试 10 次全废。
//
// 断三层（缺一层就仍有缺口）：
//  1. 值层：settings 写多少，三宿主装配值就是多少（含未配置 → 90s 兜底）；
//  2. 应用层：值经 options() 落到 agents.Config.FirstChunkTimeout；
//  3. 压缩器层：run 内第二个 LLM 调用点拿同一份值（否则主对话改了、压缩仍按旧值 = 漂移）。
func TestFirstChunkTimeoutWiringFollowsSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, sec := range []int{30, 90, 180, 900} {
		writeAgentThresholdSettings(t, fmt.Sprintf(`{"agent":{"first_chunk_timeout":%d}}`, sec))
		want := time.Duration(sec) * time.Second
		for _, host := range []codeAgentHost{hostMainLoop, hostSpawnLoop, hostExploreLoop} {
			tuning := codeAgentTuningFor(host)
			if tuning.firstChunkTimeout != want {
				t.Errorf("%s: first_chunk_timeout=%ds → 装配值 %v（没跟着设置走 ⇒ 未接线或读错源）", host, sec, tuning.firstChunkTimeout)
			}
			if got := applyCodeAgentTuning(tuning).FirstChunkTimeout; got != want {
				t.Errorf("%s: first_chunk_timeout=%ds → Config.FirstChunkTimeout %v", host, sec, got)
			}
		}
	}

	// 未配置 → 90s 兜底（与 agents 默认同值，未配置用户的实跑预算）
	writeAgentThresholdSettings(t, `{"agent":{"reminder_rounds":7}}`)
	for _, host := range []codeAgentHost{hostMainLoop, hostSpawnLoop, hostExploreLoop} {
		if got := codeAgentTuningFor(host).firstChunkTimeout; got != 90*time.Second {
			t.Errorf("%s: 未配置 first_chunk_timeout → %v, want 90s（兜底失败 ⇒ 变成 0 = 立即超时）", host, got)
		}
	}

	// 误配一律回退 90s，绝不把「0 = 立即超时」「>900 ≈ 关掉」当真值吃进去
	for _, raw := range []string{
		`{"agent":{"first_chunk_timeout":0}}`,    // 0 = 每次尝试瞬间失败
		`{"agent":{"first_chunk_timeout":-5}}`,   // 负数同理
		`{"agent":{"first_chunk_timeout":3600}}`, // 超上界 ≈ 关掉预算
		`{"agent":{"first_chunk_timeout":"90"}}`, // 类型错（字符串）
		`{"agent":{"first_chunk_timeout":true}}`, // 类型错（布尔）
		`{"agent":{"first_chunk_timeout":null}}`, // 显式 null = 未配置
		`{"agent":`,                              // 坏 JSON
	} {
		writeAgentThresholdSettings(t, raw)
		if got := loadAgentSettings().firstChunkTimeout(); got != 90*time.Second {
			t.Errorf("%s：firstChunkTimeout = %v, want 90s 兜底", raw, got)
		}
	}

	// 边界值本身是合法的：1s（激进排查用）与 900s（上界，15min）
	for _, sec := range []int{1, 900} {
		writeAgentThresholdSettings(t, fmt.Sprintf(`{"agent":{"first_chunk_timeout":%d}}`, sec))
		if got := loadAgentSettings().firstChunkTimeout(); got != time.Duration(sec)*time.Second {
			t.Errorf("first_chunk_timeout=%d → %v（边界值被误伤）", sec, got)
		}
	}
}

// TestCompressFirstChunkTimeoutApplied compressTorOptions 真的把首字预算注进压缩器：
// 装配层断言的最后一环（值层 → 选项生成 → 落进 LLMCompressor 字段）。压缩是 run 内的
// 第二个 LLM 调用点，无首字挂死 = 整个会话静默，必须与主对话同源同值。
func TestCompressFirstChunkTimeoutApplied(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeAgentThresholdSettings(t, `{"agent":{"first_chunk_timeout":150}}`)
	c := agents.NewLLMCompressor(nil, "m", compressFirstChunkTimeout())
	if c.FirstChunkTimeout != 150*time.Second {
		t.Fatalf("压缩器首字预算 = %v, want 150s（settings → compressTorOptions → WithCompressFirstChunkTimeout 断链）", c.FirstChunkTimeout)
	}
	// 未配置 → 与主对话同值（两处独立兜底同源，防「主对话 90s / 压缩 0 = 无预算」漂移）
	writeAgentThresholdSettings(t, `{"agent":{}}`)
	if got := agents.NewLLMCompressor(nil, "m", compressFirstChunkTimeout()).FirstChunkTimeout; got != 90*time.Second {
		t.Fatalf("未配置时压缩器首字预算 = %v, want 90s", got)
	}
}

// ============================== ③ 行为层 ==============================

// TestCodeAgentPostToolNudgeBehavior 行为断言（C7 硬要求 3：堵「值 → 选项应用 → agents 侧
// 消费」的固有缺口）。三个宿主各自的生产装配路径产出的 loop，喂脚本化 provider
// （1: 工具轮 → 2: 空回复轮 → 3: 可见文本收尾），断言：
//   - 第 2 次请求**不含**续跑提醒（提醒只在空回复轮之后注入）；
//   - 第 3 次请求**含** prompt.PostToolContinueNudge 原文 ⇒ 机制在真机上确实存在；
//   - run 最终拿到可见收尾 ⇒ 空轮没有把任务静默掐死。
//
// 为什么必须补这条：值断言只证明「字段是 true」；装配点漏调 options()、或 agents 侧把
// 该选项吃掉，值断言照样全绿 —— 那正是 postToolNudge 生产零接线数月时的样子。
func TestCodeAgentPostToolNudgeBehavior(t *testing.T) {
	for _, tc := range codeAgentHostCases() {
		t.Run(string(tc.host), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir()) // 封闭：本测试不读用户真实 ~/.go-code
			srv := newScriptedCodeAgentLLM(t, func(round int, _ string) []map[string]any {
				switch round {
				case 1:
					return c7ToolRound()
				case 2:
					return c7EmptyRound()
				default:
					return c7TextRound(c7DoneText)
				}
			})
			loop := tc.build(t, srv.url(), nil)
			ac := loop.Run(context.Background(), c7UserInput(), nil)
			if ac.Err != nil {
				t.Fatalf("run err: %v", ac.Err)
			}
			if got := srv.requests(); got != 3 {
				t.Fatalf("请求次数 = %d, want 3（工具轮 → 空轮 → nudge 后收尾轮）：少于 3 = 空轮直接结束（nudge 未生效）；多于 3 = 有多余重试", got)
			}
			if bodyContainsText(t, srv.requestBody(t, 2), prompt.PostToolContinueNudge) {
				t.Error("第 2 次请求不该含续跑提醒（提醒只在空回复轮之后注入）")
			}
			if !bodyContainsText(t, srv.requestBody(t, 3), prompt.PostToolContinueNudge) {
				t.Errorf("第 3 次请求应含续跑提醒（prompt.PostToolContinueNudge）—— 缺它说明装配链上 postToolNudge 在某处被丢掉")
			}
			if ac.FinalResponse != c7DoneText {
				t.Errorf("FinalResponse = %q, want %q（空轮未续跑 ⇒ 任务被静默掐死）", ac.FinalResponse, c7DoneText)
			}
		})
	}
}

// TestCodeAgentPostToolNudgeDisabledStopsSilently 反面腿（**证伪对照**）：用同一个生产
// options() 生成器，只把 postToolNudge 置 false ⇒ 同样的脚本只发 2 次请求、无提醒、
// 最终无可见收尾 —— 这正是 2026-09-18 前生产的样子（机制在，但没人打开）。
// 它证明上面那条行为断言对「开关的值」敏感，而不是恒真。
//
// 说明：生产 builders（buildLoop/buildSpawnLoop/buildExploreLoop）不接受 tuning 参数
// （最小提取的代价），故本腿用「同一个 options() + 同一个假服务 + 同一个工具引擎」直接
// 装配；「装配点是否漏调 options()」由变异验证（破坏生产取值 ⇒ 行为断言变红）覆盖。
func TestCodeAgentPostToolNudgeDisabledStopsSilently(t *testing.T) {
	srv := newScriptedCodeAgentLLM(t, func(round int, _ string) []map[string]any {
		switch round {
		case 1:
			return c7ToolRound()
		case 2:
			return c7EmptyRound()
		default:
			return c7TextRound(c7DoneText) // 不该被消费；被消费即说明 nudge 生效了
		}
	})
	ws := t.TempDir()
	tuning := codeAgentTuning{postToolNudge: false, compressThreshold: codeAgentCompressThreshold}
	opts := []agents.Option{
		agents.WithProvider(c7ProbeProviderConfig(srv.url()).newProvider("deepseek")),
		agents.WithProviderName("deepseek"),
		agents.WithModel(c7ProbeModel),
		agents.WithToolEngine(c7ProbeEngine(t, ws)),
		agents.WithLLMTimeout(0),
		agents.WithMaxRetries(1), // 假服务不会失败；不重试以免测试变慢
	}
	opts = append(opts, tuning.options()...)
	loop := agents.NewAgentLoop(opts...)
	ac := loop.Run(context.Background(), c7UserInput(), nil)
	if ac.Err != nil {
		t.Fatalf("run err: %v", ac.Err)
	}
	if got := srv.requests(); got != 2 {
		t.Fatalf("请求次数 = %d, want 2（nudge 关闭 ⇒ 空回复轮直接结束）", got)
	}
	if bodyContainsText(t, srv.requestBody(t, 2), prompt.PostToolContinueNudge) {
		t.Error("nudge 关闭时请求里不该出现续跑提醒")
	}
	if ac.FinalResponse != "" {
		t.Errorf("FinalResponse = %q, want 空（关闭 nudge ⇒ 静默结束就是结果）", ac.FinalResponse)
	}
}

// TestCodeAgentGoalAlignmentReminderBehavior 断言 ② 的行为层：主会话的 goal alignment
// 阈值**真的来自 settings 并流到真机上**（不只是字段值对）。
// 装配前把 reminder_rounds 写成 2，脚本化 provider 连续回工具轮（glob 属白名单外 ⇒ 不算
// 「进展」，无进展轮数持续累积），第 3 次请求应带 prompt.GoalAlignmentReminder；前两次不带。
// 阈值 0（未接线）时提醒永不出现 ⇒ 本测试会红 —— 这是把「主会话已接线」从值断言升级为
// 行为断言的证据。
//
// 子宿主腿（spawn / explore）断言**不出现**提醒：它们当前刻意不接线（阈值 0 = 禁用），
// 属「钉住现状」；若将来有意给子宿主接线，请连同本断言一起改。
func TestCodeAgentGoalAlignmentReminderBehavior(t *testing.T) {
	cases := []struct {
		host           codeAgentHost
		build          func(t *testing.T, baseURL string, prep func(t *testing.T)) *agents.AgentLoop
		wantReminderAt int // 第几次请求应带提醒（0 = 本宿主不该出现提醒）
	}{
		{hostMainLoop, codeAgentHostCases()[0].build, 3},
		{hostSpawnLoop, codeAgentHostCases()[1].build, 0},
		{hostExploreLoop, codeAgentHostCases()[2].build, 0},
	}
	for _, tc := range cases {
		t.Run(string(tc.host), func(t *testing.T) {
			// 脚本：请求里带提醒 ⇒ 收尾；否则继续工具轮。第 4 轮起无条件收尾（安全阀：
			// 阈值未生效时不得跑到 MaxIterations=1000，否则失败用例要跑上千轮）。
			srv := newScriptedCodeAgentLLM(t, func(round int, body string) []map[string]any {
				if bodyContainsText(t, body, prompt.GoalAlignmentReminder) {
					return c7TextRound(c7DoneText)
				}
				if round >= 4 {
					return c7TextRound(c7DoneText)
				}
				return c7ToolRound()
			})
			// 主宿主：装配前把阈值写进 settings（阈值 2 ⇒ 无进展第 2 轮后开始提醒）。
			// 子宿主：同样隔离 HOME 并写同一份 settings —— 它们**不该**读它；一旦有人给子宿主
			// 接线，阈值立刻变成 2、第 3 次请求就带提醒，下面的反向断言当场变红（可证伪）。
			if tc.wantReminderAt == 0 {
				t.Setenv("HOME", t.TempDir())
				writeCodeAgentSettings(t, 2)
			}
			loop := tc.build(t, srv.url(), func(t *testing.T) { writeCodeAgentSettings(t, 2) })
			var gotReminderRounds []int
			ac := loop.Run(context.Background(), c7UserInput(), func(_ context.Context, e events.Event) {
				if r, ok := e.(*events.GoalAlignmentReminder); ok {
					gotReminderRounds = append(gotReminderRounds, r.Round)
				}
			})
			if ac.Err != nil {
				t.Fatalf("run err: %v", ac.Err)
			}
			for n := 1; n <= srv.requests(); n++ {
				has := bodyContainsText(t, srv.requestBody(t, n), prompt.GoalAlignmentReminder)
				switch {
				case tc.wantReminderAt == 0 && has:
					t.Errorf("第 %d 次请求带了 goal alignment 提醒，但 %s 不该接线（阈值应为 agents 缺省 0 = 禁用）", n, tc.host)
				case tc.wantReminderAt == n && !has:
					t.Errorf("第 %d 次请求应带 goal alignment 提醒（reminder_rounds 已写进 settings，阈值=2 ⇒ 无进展第 2 轮后注入）", n)
				case tc.wantReminderAt != 0 && tc.wantReminderAt != n && has:
					t.Errorf("第 %d 次请求不该带 goal alignment 提醒（只在达到阈值的那一轮注入一次）", n)
				}
			}
			if tc.wantReminderAt != 0 {
				if len(gotReminderRounds) != 1 || gotReminderRounds[0] != 2 {
					t.Errorf("GoalAlignmentReminder 事件 = %v, want 恰好一次且 Round=2（= settings 阈值）", gotReminderRounds)
				}
			} else if len(gotReminderRounds) != 0 {
				t.Errorf("GoalAlignmentReminder 事件 = %v, want 无（本宿主不接线）", gotReminderRounds)
			}
			if ac.FinalResponse != c7DoneText {
				t.Errorf("FinalResponse = %q, want %q", ac.FinalResponse, c7DoneText)
			}
		})
	}
}
