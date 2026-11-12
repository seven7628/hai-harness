package main

// P5-10：真实 Provider 的 Hub 端到端评测（7 个场景）。
//
// 现有 hub_eval*_test.go 全部用确定性 fake provider（不碰网络），所以「单智能体 DM /
// 工具调用 / 审批 / 群 @all / 群 @指定 / @A→@B handoff / 自主开项目会话」都**没有真实
// Provider 的端到端证据**。本文件补这个缺口：默认 skip，只有显式设 HAI_EVAL_HUB_REAL=1
// 才跑（会产生真实付费调用）。
//
// 门控与隔离：
//   - HAI_EVAL_HUB_REAL=1 才跑，否则 t.Skip；
//   - provider/model 默认 opencode / deepseek-v4.1-flash，可由 HAI_EVAL_HUB_PROVIDER /
//     HAI_EVAL_HUB_MODEL 覆盖；
//   - 从**真实** ~/.go-code/settings.json 读出该 provider 的配置块（base_url/models/
//     prices…，**不含 api_key**），复制到 testManager 建的临时 HOME 的 settings.json；
//     loadSettings 之后再 setKey 只注入内存。真实 HOME 只读不写；
//   - 缺少 api_key 时可用 HAI_EVAL_HUB_API_KEY 覆盖，否则写 skip 报告并 skip；
//   - 临时 settings.json 不含 api_key（写后自检）；不单独注册删除 cleanup，
//     随 t.TempDir 在 testManager 的 shutdownHubRuntimes 之后删除；
//   - 报告/事件/诊断里的 key 一律替换为 [REDACTED]；报告不含 base_url；
//   - HAI_EVAL_HUB_ONLY 未知名直接 fail；5/6 必须显式带上 4；0 场景不能报 pass；
//   - 门控 skip 写 <report>.skip.json，不覆盖真实报告，且不创建 manager/不发请求。
//
// 运行：
//
//	HAI_EVAL_HUB_REAL=1 HAI_EVAL_HUB_PROVIDER=opencode HAI_EVAL_HUB_MODEL=deepseek-v4.1-flash \
//	  go test ./desktop/bridge -run TestHubRealE2E -count=1 -v -timeout 20m
//
// 报告默认 /tmp/hub-real-e2e/report-<pid>-<时间戳>.json（绝对路径），原始事件写同目录
// events.jsonl。失败也写报告（defer），且每个失败场景带最近 5 条消息 /
// 最近 tool/approval/LLM 事件摘要。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// 真实评测的协议标识（报告里自证用；schema 变更时一起改）。
const hubRealProtocol = "hub-real-e2e/v1"

// 等待真实模型/run 的上限：群 @all 要等两个成员各跑一轮，留足余量。
// 7 个场景 × 120s = 840s < 文档里的 `-timeout 20m`；单场景内部可能有多段等待，
// 所以整轮仍应留出余量（真实全量通常 4~8 分钟，不是每个等待都打满）。
const hubRealTimeout = 120 * time.Second

// hubRealDefaultProvider / hubRealDefaultModel 是任务指定的真实 provider/model。
const (
	hubRealDefaultProvider = "opencode"
	hubRealDefaultModel    = "deepseek-v4.1-flash"
)

// hubRealDefaultReport 默认报告路径带 PID + 启动时间：默认路径不会互相覆盖，
// 也避免上一次的 pass 报告被本次 skip 误当成“本次结果”。仍然保持绝对路径。
func hubRealDefaultReport() string {
	return filepath.Join(os.TempDir(), "hub-real-e2e",
		fmt.Sprintf("report-%d-%s.json", os.Getpid(), time.Now().Format("20060102-150405")))
}

// —— 场景表（HAI_EVAL_HUB_ONLY 的合法名与依赖的唯一事实源） ——

type hubRealScenarioDef struct {
	name    string
	aliases []string // 序号别名（1…7）与短名（dm/tool/…）
	deps    []string // 必须先选中的场景（5/6 复用 4 建的群与成员）
	prompt  string
	run     func(*hubRealScenario)
}

var hubRealScenarioDefs = []hubRealScenarioDef{
	{
		name: "1_dm", aliases: []string{"1", "dm"},
		prompt: "请只回复：HAI_DM_OK",
		run:    (*hubRealScenario).scenarioDM,
	},
	{
		name: "2_tool_auto", aliases: []string{"2", "tool", "tool_auto"},
		prompt: "调用 bash 工具一次，command 必须是 `printf HAI_TOOL_OK`，然后回复工具输出。",
		run:    (*hubRealScenario).scenarioToolAuto,
	},
	{
		name: "3_approval_hitl", aliases: []string{"3", "approval", "approval_hitl"},
		prompt: "调用 bash 工具一次，command 必须是 `printf HAI_APPROVAL_OK`，然后回复工具输出。",
		run:    (*hubRealScenario).scenarioApprovalHITL,
	},
	{
		name: "4_group_mention_all", aliases: []string{"4", "all", "mention_all", "group_mention_all"},
		prompt: "@all 每人只回复自己的名字 + HAI_ALL_OK",
		run:    (*hubRealScenario).scenarioGroupMentionAll,
	},
	{
		name: "5_group_mention_one", aliases: []string{"5", "one", "mention_one", "group_mention_one"},
		deps:   []string{"4_group_mention_all"},
		prompt: "@RealOneB 只回复 HAI_ONE_OK",
		run:    (*hubRealScenario).scenarioGroupMentionOne,
	},
	{
		name: "6_handoff", aliases: []string{"6", "handoff"},
		deps:   []string{"4_group_mention_all"},
		prompt: "@RealHandA 调用 handoff 工具，把任务交给 RealHandB。",
		run:    (*hubRealScenario).scenarioHandoff,
	},
	{
		name: "7_project_session", aliases: []string{"7", "project", "project_session"},
		prompt: "请依次调用 workspace_create 创建项目，再调用 session_create 创建一个会话，然后调用 session_send 给该会话派任务：只回复 HAI_PROJECT_SESSION_OK。最后把 workspace 路径、session_id、task_id 回给我。",
		run:    (*hubRealScenario).scenarioProjectSession,
	},
}

// hubRealParseOnly 解析 HAI_EVAL_HUB_ONLY（逗号分隔；空项忽略；序号/短名映射到合法名）。
// 任何未知 token / 空选择 / 只选 5、6 而未选 4 都返回错误——调用方必须在创建
// manager、发真实请求之前 t.Fatalf，不能静默跑 0 个场景还报 pass。
func hubRealParseOnly(raw string) (map[string]bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	alias := map[string]string{}
	var legal []string
	for _, def := range hubRealScenarioDefs {
		legal = append(legal, def.name)
		alias[def.name] = def.name
		for _, a := range def.aliases {
			alias[a] = def.name
		}
	}
	only := map[string]bool{}
	var bad []string
	for _, part := range strings.Split(raw, ",") {
		tok := strings.TrimSpace(part)
		if tok == "" {
			continue // 空项忽略（例如 "1,,4"）
		}
		name, ok := alias[tok]
		if !ok {
			bad = append(bad, tok)
			continue
		}
		only[name] = true
	}
	if len(bad) > 0 {
		return nil, fmt.Errorf("HAI_EVAL_HUB_ONLY 含未知场景 %s；合法名：%s（也接受序号别名 1…7 及 dm/tool/approval/all/one/handoff/project）",
			strings.Join(bad, ", "), strings.Join(legal, ", "))
	}
	if len(only) == 0 {
		return nil, fmt.Errorf("HAI_EVAL_HUB_ONLY=%q 没有有效场景名；合法名：%s", raw, strings.Join(legal, ", "))
	}
	for _, def := range hubRealScenarioDefs {
		if !only[def.name] {
			continue
		}
		for _, dep := range def.deps {
			if !only[dep] {
				return nil, fmt.Errorf("HAI_EVAL_HUB_ONLY 选择了 %s，但它依赖 %s（场景 4 建的同一个群与成员，场景 5/6 复用）；请显式同时选择 %s",
					def.name, dep, dep)
			}
		}
	}
	return only, nil
}

// hubRealValidateReportPath HAI_EVAL_HUB_REPORT 必须是绝对路径：相对路径会写进
// 仓库工作区（go test 的 cwd 是包目录），必须在任何写入/请求之前 fail。
func hubRealValidateReportPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("HAI_EVAL_HUB_REPORT 不能为空")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("HAI_EVAL_HUB_REPORT 必须是绝对路径（避免写进仓库）: %q", path)
	}
	return nil
}

// —— 报告 schema ——

type hubRealReport struct {
	Protocol   string                   `json:"protocol"`
	StartedAt  time.Time                `json:"started_at"`
	FinishedAt time.Time                `json:"finished_at"`
	DurationMS int64                    `json:"duration_ms"`
	Provider   string                   `json:"provider"`
	Model      string                   `json:"model"`
	Gate       string                   `json:"gate"`
	Status     string                   `json:"status"` // pass | fail | skipped
	SkipReason string                   `json:"skip_reason,omitempty"`
	ReportPath string                   `json:"report_path,omitempty"`
	EventsPath string                   `json:"events_path,omitempty"`
	Scenarios  []*hubRealScenarioReport `json:"scenarios"`
	Totals     hubRealTotals            `json:"totals"`
	Notes      []string                 `json:"notes,omitempty"`
}

type hubRealTotals struct {
	Scenarios    int     `json:"scenarios"`
	Passed       int     `json:"passed"`
	Failed       int     `json:"failed"`
	Skipped      int     `json:"skipped"`
	LLMCalls     int     `json:"llm_calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}

type hubRealScenarioReport struct {
	Name         string            `json:"name"`
	Status       string            `json:"status"` // pass | fail | skip
	StartedAt    time.Time         `json:"started_at"`
	DurationMS   int64             `json:"duration_ms"`
	Prompt       string            `json:"prompt,omitempty"`
	FinalText    string            `json:"final_text,omitempty"`
	AgentReplies []string          `json:"agent_replies,omitempty"`
	Messages     []hubRealMessage  `json:"messages,omitempty"`
	ToolCalls    []hubRealToolCall `json:"tool_calls,omitempty"`
	Approvals    []hubRealApproval `json:"approvals,omitempty"`
	Handoffs     []hubRealHandoff  `json:"handoffs,omitempty"`
	LLM          hubRealLLM        `json:"llm"`
	LLMErrors    []string          `json:"llm_errors,omitempty"`
	Project      *hubRealProject   `json:"project,omitempty"`
	Errors       []string          `json:"errors,omitempty"`
	Notes        []string          `json:"notes,omitempty"`
	Diagnostics  []string          `json:"diagnostics,omitempty"`
}

type hubRealLLM struct {
	Calls         int      `json:"calls"`
	InputTokens   int64    `json:"input_tokens"`
	OutputTokens  int64    `json:"output_tokens"`
	CacheRead     int64    `json:"cache_read,omitempty"`
	CacheWrite    int64    `json:"cache_write,omitempty"`
	Reasoning     int64    `json:"reasoning,omitempty"`
	CostUSD       float64  `json:"cost_usd"`
	PricedCalls   int      `json:"priced_calls"`
	UnpricedCalls int      `json:"unpriced_calls"`
	Models        []string `json:"models,omitempty"`
}

type hubRealMessage struct {
	Seq     int64  `json:"seq"`
	Sender  string `json:"sender"`
	AgentID string `json:"agent_id,omitempty"`
	Kind    string `json:"kind,omitempty"`
	RoundID string `json:"round_id,omitempty"`
	Text    string `json:"text,omitempty"`
}

type hubRealToolCall struct {
	Name       string `json:"name"`
	Arguments  string `json:"arguments,omitempty"`
	Result     string `json:"result,omitempty"`
	IsError    bool   `json:"is_error"`
	SessionID  string `json:"session_id,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type hubRealApproval struct {
	ToolCallID string `json:"tool_call_id"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments,omitempty"`
	Decision   string `json:"decision,omitempty"` // approved | rejected | (空 = 没决策)
	SessionID  string `json:"session_id,omitempty"`
}

type hubRealHandoff struct {
	ID          string `json:"id"`
	GroupID     string `json:"group_id"`
	FromAgentID string `json:"from_agent_id"`
	ToAgentID   string `json:"to_agent_id"`
	Instruction string `json:"instruction,omitempty"`
	Status      string `json:"status"`
	MessageSeq  int64  `json:"message_seq,omitempty"`
}

// hubRealProject 场景 7 的独立证据：workspace_create / session_create / session_send
// 的参数与结果，以及项目会话自己的 LLM 用量。
type hubRealProject struct {
	Root            string           `json:"root"`
	RequestedName   string           `json:"requested_name"`
	WorkspacePath   string           `json:"workspace_path,omitempty"`
	SessionID       string           `json:"session_id,omitempty"`
	TaskID          string           `json:"task_id,omitempty"`
	TaskStatus      string           `json:"task_status,omitempty"`
	Accepted        bool             `json:"accepted"`
	Queued          bool             `json:"queued"`
	WorkspaceCreate *hubRealToolCall `json:"workspace_create,omitempty"`
	WorkspaceList   *hubRealToolCall `json:"workspace_list,omitempty"`
	SessionCreate   *hubRealToolCall `json:"session_create,omitempty"`
	SessionList     *hubRealToolCall `json:"session_list,omitempty"`
	SessionSend     *hubRealToolCall `json:"session_send,omitempty"`
	SessionLLM      hubRealLLM       `json:"session_llm"`
	// 项目会话自己的终态证据（不是工具返回值）：
	SessionAgentEnd        string `json:"session_agent_end,omitempty"`
	SessionFinalText       string `json:"session_final_text,omitempty"`
	TaskResultStatus       string `json:"task_result_status,omitempty"` // 账本终态（completed/delivered…）
	TaskResultFinishReason string `json:"task_result_finish_reason,omitempty"`
	TaskResultSummary      string `json:"task_result_summary,omitempty"`
}

// —— 入口 ——

// TestHubRealE2E 真实 Provider 的 Hub 端到端评测（7 场景）。
//
// 默认 skip：这是会产生真实付费调用的 gated 测试，只允许显式开启。
func TestHubRealE2E(t *testing.T) {
	// 相对路径会写进仓库（go test 的 cwd 是包目录）：必须在任何写入之前拦住。
	// 默认路径是绝对路径（/tmp 下，含 PID + 时间戳）。
	reportPath := hubRealEnvOr("HAI_EVAL_HUB_REPORT", hubRealDefaultReport())
	if err := hubRealValidateReportPath(reportPath); err != nil {
		t.Fatalf("%v", err)
	}

	// 门控 skip 也要留下自己的 skip 报告：不覆盖上一次的真实报告，也绝不创建
	// manager/发请求。文件是 <report>.skip.json（事件文件 events.skip.jsonl）。
	if os.Getenv("HAI_EVAL_HUB_REAL") != "1" {
		reason := "真实 Provider Hub E2E：默认 skip。要跑请设 HAI_EVAL_HUB_REAL=1（会产生真实付费调用），" +
			"provider/model 用 HAI_EVAL_HUB_PROVIDER / HAI_EVAL_HUB_MODEL 覆盖"
		rep := &hubRealReport{
			Protocol:   hubRealProtocol,
			StartedAt:  time.Now().UTC(),
			Provider:   hubRealEnvOr("HAI_EVAL_HUB_PROVIDER", hubRealDefaultProvider),
			Model:      hubRealEnvOr("HAI_EVAL_HUB_MODEL", hubRealDefaultModel),
			Gate:       "HAI_EVAL_HUB_REAL=1",
			Status:     "skipped",
			SkipReason: reason,
			Scenarios:  []*hubRealScenarioReport{},
			Notes:      []string{"门控未开启：未创建 manager，未发任何真实请求。"},
		}
		hubRealWriteReport(t, rep, reportPath, true, nil, "")
		t.Skip(reason)
	}

	providerName := hubRealEnvOr("HAI_EVAL_HUB_PROVIDER", hubRealDefaultProvider)
	model := hubRealEnvOr("HAI_EVAL_HUB_MODEL", hubRealDefaultModel)

	rep := &hubRealReport{
		Protocol:  hubRealProtocol,
		StartedAt: time.Now().UTC(),
		Provider:  providerName,
		Model:     model,
		Gate:      "HAI_EVAL_HUB_REAL=1",
		Status:    "fail", // 默认 fail：任何提前返回都不能写成假的 pass
		Scenarios: []*hubRealScenarioReport{},
		Notes: []string{
			"真实 provider 端到端评测：仅在本测试的 gated 路径里发真实请求。",
			"报告与事件不对凭证字段落盘；命中的敏感值已替换为 [REDACTED]。",
		},
	}
	var buf *syncBuf
	var redactKey string
	// 失败/跳过也要留下报告：defer 先于 testManager 的 cleanup 执行，事件流仍在。
	// skip 写 <report>.skip.json，避免把上一次的 pass 报告覆盖成 skipped。
	defer func() {
		hubRealWriteReport(t, rep, reportPath, rep.Status == "skipped", buf, redactKey)
	}()

	// ONLY 必须在读真实配置/建 manager/发请求之前解析：未知场景或漏选依赖 4 的
	// 5/6 直接 fail（错误里带全部合法名），不能跑 0 个场景还报 pass。
	only, onlyErr := hubRealParseOnly(os.Getenv("HAI_EVAL_HUB_ONLY"))
	if onlyErr != nil {
		rep.Status = "fail"
		t.Fatalf("%v", onlyErr)
	}
	if raw := strings.TrimSpace(os.Getenv("HAI_EVAL_HUB_ONLY")); raw != "" {
		rep.Notes = append(rep.Notes, "HAI_EVAL_HUB_ONLY="+raw)
	}

	cfg, skipReason := hubRealResolveProvider(t, providerName, model)
	if cfg != nil {
		redactKey = cfg.key // 报告脱敏用；key 本身绝不进报告
	}
	if skipReason != "" {
		rep.Status = "skipped"
		rep.SkipReason = skipReason
		rep.FinishedAt = time.Now().UTC()
		t.Skip(skipReason)
	}
	// 读真实 settings 里的价表会写全局 userPriceTable：快照并在 cleanup 还原，
	// 否则同包其它按内置价断言的测试会顺序性失败（memory_baseline_test.go 同款隔离）。
	isolateUserPrices(t)

	m, b := testManager(t) // HOME/USERPROFILE 指向临时目录，之后所有读写都在隔离环境里
	buf = b
	// settings.json 只写无 key 的配置（磁盘上没有任何明文 key）；loadSettings 后
	// 再把 key 注入内存（setKey 会标记 keyInjected，后续 loadSettings 不会清掉）。
	hubRealInstallSettings(t, cfg)
	m.provCfg.loadSettings()
	m.provCfg.setKey(providerName, cfg.key)
	if err := m.provCfg.setActive(providerName); err != nil {
		rep.Status = "fail"
		rep.SkipReason = "setActive: " + err.Error()
		t.Fatalf("设 active provider 失败: %v", err)
	}
	// 场景 7 的 session_create 走「全局 active provider/model」：必须显式钉住，
	// 否则项目会话会用默认模型而不是本次评测的 model。
	m.mu.Lock()
	m.lastModel = model
	m.mu.Unlock()
	if got := m.provCfg.providerName(); got != providerName {
		t.Fatalf("active provider = %q, want %q", got, providerName)
	}
	if got := m.provCfg.keyLocked(providerName); strings.TrimSpace(got) == "" {
		rep.Status = "skipped"
		rep.SkipReason = "loadSettings + setKey 后 key 仍为空"
		t.Skip(rep.SkipReason)
	}
	// Hub 派发（workspace_create/register/session_send）需要工作区登记表。
	m.wsReg = newWorkspaceRegistry(filepath.Join(t.TempDir(), "workspaces.json"))

	e := &hubRealEval{t: t, m: m, buf: buf, cfg: cfg, rep: rep}
	t.Logf("[hub-real] provider=%s model=%s report=%s", providerName, model, reportPath)

	for _, def := range hubRealScenarioDefs {
		if len(only) > 0 && !only[def.name] {
			continue
		}
		e.run(def.name, def.prompt, def.run)
	}
	// 选了 ONLY 却一个场景都没跑（例如别名全部被忽略/过滤逻辑回归）必须响亮失败：
	// 0 场景没有资格报 pass。
	if len(rep.Scenarios) == 0 {
		rep.Status = "fail"
		t.Fatalf("HAI_EVAL_HUB_ONLY=%q 没有执行任何场景（scenarios=0），不能报 pass",
			os.Getenv("HAI_EVAL_HUB_ONLY"))
	}

	// 场景全部跑完：状态由场景结果 + 顶层失败共同决定（write 里兜底）。
	rep.FinishedAt = time.Now().UTC()
	rep.Status = "pass"
	for _, sc := range rep.Scenarios {
		if sc.Status == "fail" {
			rep.Status = "fail"
			break
		}
	}
	if t.Failed() {
		rep.Status = "fail"
	}
}

func hubRealEnvOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// —— 真实 provider 配置：只读真实 HOME，写入临时 HOME ——

type hubRealProviderCfg struct {
	name  string
	model string
	key   string
	block map[string]any
}

// hubRealResolveProvider 在 testManager 改 HOME **之前**读真实 settings.json 的
// provider 配置块。返回 skipReason 时调用方 skip（并写进报告），不制造假失败。
func hubRealResolveProvider(t *testing.T, name, model string) (*hubRealProviderCfg, string) {
	t.Helper()
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil, "无法解析真实 HOME: " + err.Error()
	}
	settingsPath := filepath.Join(realHome, ".go-code", "settings.json")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, fmt.Sprintf("读真实 settings.json 失败（%s）: %v", settingsPath, err)
	}
	var doc struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, "真实 settings.json 不是合法 JSON: " + err.Error()
	}
	rawBlock, ok := doc.Provider[name]
	if !ok || len(rawBlock) == 0 {
		return nil, fmt.Sprintf("真实 settings.json 里没有 provider %q 配置（可用 HAI_EVAL_HUB_PROVIDER 覆盖）", name)
	}
	var block map[string]any
	if err := json.Unmarshal(rawBlock, &block); err != nil {
		return nil, fmt.Sprintf("provider %q 配置块不可解析: %v", name, err)
	}
	if deleted, _ := block["deleted"].(bool); deleted {
		return nil, fmt.Sprintf("provider %q 在真实 settings.json 里是已删除状态", name)
	}
	if base, _ := block["base_url"].(string); strings.TrimSpace(base) == "" {
		return nil, fmt.Sprintf("provider %q 没有 base_url（settings.json 未配置）", name)
	}
	key, _ := block["api_key"].(string)
	key = strings.TrimSpace(key)
	if env := strings.TrimSpace(os.Getenv("HAI_EVAL_HUB_API_KEY")); env != "" {
		key = env // 显式覆盖优先（key 可能已经迁到 safeStorage/keys.json）
	}
	if key == "" {
		return nil, fmt.Sprintf("provider %q 没有可用 api_key（settings.json 无 api_key 且 HAI_EVAL_HUB_API_KEY 未设置）", name)
	}
	return &hubRealProviderCfg{name: name, model: model, key: key, block: block}, ""
}

// hubRealInstallSettings 把选中的 provider 配置块写进**临时 HOME** 的 settings.json。
//
// **不写 api_key**：磁盘上不出现任何明文 key，SIGKILL / 测试超时 / 进程崩溃都不会
// 把 key 留在临时文件里。key 由调用方在 loadSettings 之后用 setKey 只注入内存。
// 这里也**不注册删除 Cleanup**：settings 在 t.TempDir 里，t.Cleanup 是 LIFO，testManager
// 注册的 shutdownHubRuntimes 先跑，TempDir 的删除后跑（N6：不能先删 home 再收尾）。
func hubRealInstallSettings(t *testing.T, cfg *hubRealProviderCfg) {
	t.Helper()
	block := map[string]any{}
	for k, v := range cfg.block {
		block[k] = v
	}
	delete(block, "api_key") // 关键：配置块可能来自真实 settings，key 不落临时盘
	doc := map[string]any{
		"provider": map[string]any{
			"active": cfg.name,
			cfg.name: block,
		},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("构造临时 settings.json 失败: %v", err)
	}
	path := homeCfgPath("settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建临时 HOME/.go-code 失败: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写临时 settings.json 失败: %v", err)
	}
	// 写后自检：文件里不能出现 api_key 字段，也不能出现 key 原文。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("回读临时 settings.json 失败: %v", err)
	}
	var check struct {
		Provider map[string]json.RawMessage `json:"provider"`
	}
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("临时 settings.json 不可解析: %v", err)
	}
	var checkBlock map[string]any
	if err := json.Unmarshal(check.Provider[cfg.name], &checkBlock); err != nil {
		t.Fatalf("临时 settings.json 的 provider %q 不是对象: %v", cfg.name, err)
	}
	if _, has := checkBlock["api_key"]; has {
		t.Fatalf("临时 settings.json 仍含 api_key 字段（key 不能落盘）")
	}
	if cfg.key != "" && strings.Contains(string(raw), cfg.key) {
		t.Fatalf("临时 settings.json 含明文 api_key（key 不能落盘）")
	}
	t.Logf("[hub-real] 已把 provider %q 的无 key 配置写入临时 HOME（key 仅内存注入；临时目录随 testManager cleanup 删除）", cfg.name)
}

// —— 评测驱动 ——

type hubRealEval struct {
	t     *testing.T
	m     *manager
	buf   *syncBuf
	cfg   *hubRealProviderCfg
	rep   *hubRealReport
	cmdID int
}

func (e *hubRealEval) nextID() int {
	e.cmdID++
	return e.cmdID
}

func (e *hubRealEval) run(name, prompt string, fn func(*hubRealScenario)) {
	e.t.Run(name, func(t *testing.T) {
		start := e.buf.Len()
		prior := hubRealRunIDs(hubRealParseEvents(e.buf.String()[:start]))
		sc := &hubRealScenario{
			t:         t,
			e:         e,
			name:      name,
			prompt:    prompt,
			start:     start,
			started:   time.Now(),
			priorRuns: prior,
			decisions: map[string]string{},
		}
		sc.rep = &hubRealScenarioReport{
			Name:      name,
			Status:    "fail", // 默认 fail：只有函数正常返回才改成 pass
			StartedAt: sc.started,
			Prompt:    prompt,
		}
		defer func() {
			sc.sync()
			sc.rep.DurationMS = time.Since(sc.started).Milliseconds()
			switch {
			case t.Skipped():
				sc.rep.Status = "skip"
			case t.Failed():
				sc.rep.Status = "fail"
			default:
				sc.rep.Status = "pass"
			}
			if sc.rep.LLM.Calls > 0 && sc.rep.LLM.CostUSD == 0 {
				sc.rep.Notes = append(sc.rep.Notes,
					"llm cost_usd=0：上游 usage.Cost.Total=0（不代表免费，可能是网关未上报成本）")
			}
			e.rep.Scenarios = append(e.rep.Scenarios, sc.rep)
			t.Logf("[hub-real] %s status=%s duration=%dms llm_calls=%d tools=%d approvals=%d",
				name, sc.rep.Status, sc.rep.DurationMS, sc.rep.LLM.Calls, len(sc.rep.ToolCalls), len(sc.rep.Approvals))
		}()
		fn(sc)
	})
}

// hubRealScenario 收集单个场景的事件切片、报告与失败诊断。
type hubRealScenario struct {
	t       *testing.T
	e       *hubRealEval
	name    string
	prompt  string
	start   int
	started time.Time
	rep     *hubRealScenarioReport

	// messageSource 由场景设置（DM/群日志）：失败时据此输出最近 5 条消息。
	messageSource func() []agenthub.Message
	// priorRuns 是场景开始前已存在的 run_id 集合：异步事件可能晚于上一场景收尾才落盘，
	// 终止判据必须忽略这些旧 run 的 agent_end，否则会把上一场景的收尾误判成本场景结束。
	priorRuns map[string]bool
	// decisions 审批 id → 决策（报告证据）。
	decisions map[string]string
	// finalText 场景主回复（报告 FinalText）。
	finalText string
}

func (sc *hubRealScenario) cmd(typ string, payload map[string]any) (bool, string, map[string]any) {
	return hubCmd(sc.t, sc.e.m, sc.e.buf, sc.e.nextID(), typ, payload)
}

func (sc *hubRealScenario) store() agenthub.Store {
	sc.t.Helper()
	st, err := sc.e.m.hubStore()
	if err != nil {
		sc.failf("hubStore: %v", err)
	}
	return st
}

func (sc *hubRealScenario) watchDM(agentID agenthub.AgentID) {
	sc.messageSource = func() []agenthub.Message {
		st, err := sc.e.m.hubStore()
		if err != nil {
			return nil
		}
		msgs, err := st.ReadDirectMessages(context.Background(), agentID, 0, 500)
		if err != nil {
			return nil
		}
		return msgs
	}
}

func (sc *hubRealScenario) watchGroup(gid agenthub.GroupID) {
	sc.messageSource = func() []agenthub.Message {
		st, err := sc.e.m.hubStore()
		if err != nil {
			return nil
		}
		msgs, err := st.ReadMessages(context.Background(), gid, 0, 500)
		if err != nil {
			return nil
		}
		return msgs
	}
}

func (sc *hubRealScenario) messages() []agenthub.Message {
	if sc.messageSource == nil {
		return nil
	}
	return sc.messageSource()
}

// events 取本场景开始之后的全部事件（按 JSONL 逐行解析；半行/坏行跳过）。
func (sc *hubRealScenario) events() []map[string]any {
	raw := sc.e.buf.String()
	if sc.start > len(raw) {
		return nil
	}
	return hubRealParseEvents(raw[sc.start:])
}

func hubRealParseEvents(raw string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func hubRealRunIDs(events []map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, ev := range events {
		if rid := str(ev, "run_id"); rid != "" {
			out[rid] = true
		}
	}
	return out
}

func (sc *hubRealScenario) createAgent(name, role, instructions string, policy map[string]any) agenthub.AgentProfile {
	profile := map[string]any{
		"name":         name,
		"provider":     sc.e.cfg.name,
		"model":        sc.e.cfg.model,
		"role":         role,
		"instructions": instructions,
	}
	if policy != nil {
		profile["policy"] = policy
	}
	ok, errMsg, data := sc.cmd("hub_agent_create", map[string]any{"profile": profile})
	if !ok {
		sc.failf("创建智能体 %s 失败: %s", name, errMsg)
	}
	return agentFromData(sc.t, data)
}

func (sc *hubRealScenario) sendDM(agentID agenthub.AgentID, text, clientMsgID string) map[string]any {
	ok, errMsg, data := sc.cmd("hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": string(agentID)},
		"text":   text, "client_msg_id": clientMsgID,
	})
	if !ok {
		sc.failf("hub_send(%s) 失败: %s", agentID, errMsg)
	}
	return data
}

func (sc *hubRealScenario) sendGroup(gid agenthub.GroupID, text string, mentionAll bool) map[string]any {
	payload := map[string]any{"group_id": string(gid), "text": text}
	if mentionAll {
		payload["mention_all"] = true
	}
	ok, errMsg, data := sc.cmd("hub_group_send", payload)
	if !ok {
		sc.failf("hub_group_send 失败: %s", errMsg)
	}
	// 人 @ 了目标却唤醒失败 = 这一轮根本没跑，必须响亮失败（不能只看到 round 就继续）。
	if raw, present := data["wake_errors"]; present && raw != nil {
		sc.failf("hub_group_send 唤醒失败: %v", raw)
	}
	return data
}

// waitFor 轮询直到 check 返回 ok；check 返回非空 terminal 时立即响亮失败
// （典型：agent_end 已到达但模型没调用要求的工具，不再静默等到超时）。
func (sc *hubRealScenario) waitFor(timeout time.Duration, what string, check func() (ok bool, terminal string)) {
	sc.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, terminal := check()
		if ok {
			return
		}
		if terminal != "" {
			sc.failf("%s: %s", what, terminal)
		}
		if time.Now().After(deadline) {
			sc.failf("%s: 等待超时（%s）", what, timeout)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// failf 记录结构化错误 + 诊断（最近消息/工具/审批/LLM），再 t.Fatalf。
func (sc *hubRealScenario) failf(format string, args ...any) {
	msg := sc.redactKey(fmt.Sprintf(format, args...))
	sc.rep.Errors = append(sc.rep.Errors, msg)
	for _, line := range sc.diagLines() {
		sc.rep.Diagnostics = append(sc.rep.Diagnostics, line)
		sc.t.Log("[diag] " + line)
	}
	sc.sync()
	sc.t.Fatalf("%s", msg)
}

// sync 从事件流重建报告字段（每次都重建，失败路径也能拿到已发生的事实）。
func (sc *hubRealScenario) sync() {
	rep := sc.rep
	rep.AgentReplies = nil
	rep.ToolCalls = nil
	rep.Approvals = nil
	rep.Handoffs = nil
	rep.LLMErrors = nil
	rep.LLM = hubRealLLM{}
	models := map[string]bool{}
	var approvals []hubRealApproval

	for _, ev := range sc.events() {
		// 只统计本场景的 run：上一场景的迟到事件（llm_end/tool_run_end/agent_end）
		// 即使落在本场景的 buffer 区间里也不能串进来（场景 6→7 的群会话尤其明显）。
		if rid := str(ev, "run_id"); rid != "" && sc.priorRuns[rid] {
			continue
		}
		switch str(ev, "event_type") {
		case "llm_end":
			in, out, cr, cw, reas, cost, priced := hubRealUsage(ev)
			rep.LLM.Calls++
			rep.LLM.InputTokens += in
			rep.LLM.OutputTokens += out
			rep.LLM.CacheRead += cr
			rep.LLM.CacheWrite += cw
			rep.LLM.Reasoning += reas
			rep.LLM.CostUSD += cost
			if priced {
				rep.LLM.PricedCalls++
			} else {
				rep.LLM.UnpricedCalls++
			}
			if m := str(ev, "model"); m != "" {
				models[m] = true
			}
		case "llm_error":
			if len(rep.LLMErrors) < 10 {
				rep.LLMErrors = append(rep.LLMErrors, fmt.Sprintf("attempt=%v will_retry=%v kind=%s message=%s",
					ev["attempt"], ev["will_retry"], str(ev, "error_kind"), hubRealClip(str(ev, "message"), 500)))
			}
		case "tool_run_end":
			rep.ToolCalls = append(rep.ToolCalls, hubRealToolCall{
				Name:       str(ev, "name"),
				Arguments:  hubRealClip(str(ev, "arguments"), 2000),
				Result:     hubRealClip(str(ev, "result"), 4000),
				IsError:    hubRealBool(ev, "is_error"),
				SessionID:  str(ev, "session_id"),
				ToolCallID: str(ev, "id"),
			})
		case "tool_approval_requested":
			approvals = append(approvals, hubRealApproval{
				ToolCallID: str(ev, "id"),
				Name:       str(ev, "name"),
				Arguments:  hubRealClip(str(ev, "arguments"), 2000),
				SessionID:  str(ev, "session_id"),
			})
		case "hub_handoff":
			h, _ := ev["handoff"].(map[string]any)
			if h != nil {
				rep.Handoffs = append(rep.Handoffs, hubRealHandoff{
					ID:          str(h, "id"),
					GroupID:     str(h, "group_id"),
					FromAgentID: str(h, "from_agent_id"),
					ToAgentID:   str(h, "to_agent_id"),
					Instruction: hubRealClip(str(h, "instruction"), 500),
					Status:      str(h, "status"),
					MessageSeq:  int64(numOr(ev, "message_seq", 0)),
				})
			}
		case "agent_end":
			text := strings.TrimSpace(str(ev, "content"))
			if text != "" && len(rep.AgentReplies) < 20 {
				rep.AgentReplies = append(rep.AgentReplies, fmt.Sprintf("session=%s model=%s text=%s",
					str(ev, "session_id"), str(ev, "model"), hubRealClip(text, 1000)))
			}
		}
	}
	// 审批决策在事件上没有（是测试注入的），按 id 合并。
	for i := range approvals {
		approvals[i].Decision = sc.decisions[approvals[i].ToolCallID]
		rep.Approvals = append(rep.Approvals, approvals[i])
	}
	for m := range models {
		rep.LLM.Models = append(rep.LLM.Models, m)
	}
	if sc.finalText != "" {
		rep.FinalText = hubRealClip(sc.finalText, 4000)
	}
	// 最近 20 条消息作为证据（失败路径同样可用）。
	var msgs []hubRealMessage
	all := sc.messages()
	if len(all) > 20 {
		all = all[len(all)-20:]
	}
	for _, m := range all {
		msgs = append(msgs, hubRealMessage{
			Seq: m.Seq, Sender: string(m.Sender.Kind), AgentID: string(m.Sender.AgentID),
			Kind: string(m.Kind), RoundID: m.RoundID, Text: hubRealClip(m.Text, 1000),
		})
	}
	rep.Messages = msgs
}

// redactKey 把凭证原文从诊断/日志里抹掉（真实模型可能把 key 回显进工具参数/正文）。
func (sc *hubRealScenario) redactKey(s string) string {
	if sc.e != nil && sc.e.cfg != nil && sc.e.cfg.key != "" {
		return strings.ReplaceAll(s, sc.e.cfg.key, "[REDACTED]")
	}
	return s
}

// diagLines 失败诊断：最近 5 条消息 + 最近 tool/approval/LLM 事件摘要（不含 key）。
func (sc *hubRealScenario) diagLines() []string {
	var lines []string
	all := sc.messages()
	if start := len(all) - 5; start > 0 {
		all = all[start:]
	}
	if len(all) == 0 {
		lines = append(lines, "最近消息: （无）")
	} else {
		lines = append(lines, fmt.Sprintf("最近 %d 条消息:", len(all)))
		for _, m := range all {
			lines = append(lines, fmt.Sprintf("  seq=%d sender=%s agent=%s kind=%s round=%s text=%q",
				m.Seq, m.Sender.Kind, m.Sender.AgentID, m.Kind, m.RoundID, hubRealClip(m.Text, 300)))
		}
	}
	var toolLines, approvalLines, llmLines []string
	for _, ev := range sc.events() {
		// 上一场景的迟到事件不进本场景诊断（与 sync 同一口径）。
		if rid := str(ev, "run_id"); rid != "" && sc.priorRuns[rid] {
			continue
		}
		switch str(ev, "event_type") {
		case "tool_run_end":
			toolLines = append(toolLines, fmt.Sprintf("  tool=%s is_error=%v args=%s result=%s",
				str(ev, "name"), hubRealBool(ev, "is_error"),
				hubRealClip(str(ev, "arguments"), 300), hubRealClip(str(ev, "result"), 500)))
			if len(toolLines) > 8 {
				toolLines = toolLines[1:]
			}
		case "tool_approval_requested":
			approvalLines = append(approvalLines, fmt.Sprintf("  approval=%s id=%s args=%s",
				str(ev, "name"), str(ev, "id"), hubRealClip(str(ev, "arguments"), 300)))
		case "llm_end":
			llmLines = append(llmLines, fmt.Sprintf("  llm_end session=%s model=%s content=%q",
				str(ev, "session_id"), str(ev, "model"), hubRealClip(str(ev, "content"), 300)))
			if len(llmLines) > 5 {
				llmLines = llmLines[1:]
			}
		case "llm_error":
			llmLines = append(llmLines, fmt.Sprintf("  llm_error session=%s kind=%s will_retry=%v message=%s",
				str(ev, "session_id"), str(ev, "error_kind"), ev["will_retry"], hubRealClip(str(ev, "message"), 300)))
			if len(llmLines) > 5 {
				llmLines = llmLines[1:]
			}
		}
	}
	lines = append(lines, "最近 tool 事件:")
	lines = append(lines, toolLines...)
	if len(toolLines) == 0 {
		lines = append(lines, "  （无 tool_run_end）")
	}
	lines = append(lines, "最近 approval 事件:")
	lines = append(lines, approvalLines...)
	if len(approvalLines) == 0 {
		lines = append(lines, "  （无 tool_approval_requested）")
	}
	lines = append(lines, "最近 LLM 事件:")
	lines = append(lines, llmLines...)
	if len(llmLines) == 0 {
		lines = append(lines, "  （无 llm_end/llm_error）")
	}
	for i := range lines {
		lines[i] = sc.redactKey(lines[i])
	}
	return lines
}

// —— 场景 1：单智能体 DM ——

func (sc *hubRealScenario) scenarioDM() {
	const token = "HAI_DM_OK"
	a := sc.createAgent("RealDM", "回复助手", "你只做被要求的事，回复尽量短，不要调用工具。", nil)
	sc.watchDM(a.ID)
	sid := "hub." + string(a.ID) + ".dm"
	sc.sendDM(a.ID, sc.prompt, "real-e2e-dm-1")

	var msgs []agenthub.Message
	sc.waitFor(hubRealTimeout, "DM 回复进入索引", func() (bool, string) {
		msgs = sc.messages()
		for _, m := range msgs {
			if m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == a.ID && strings.Contains(m.Text, token) {
				return true, ""
			}
		}
		if sc.hasNewAgentEnd(sid) {
			return false, "agent_end 已到达，但 DM 索引里没有含 " + token + " 的 agent 回复（模型没照做或回复被丢弃）"
		}
		return false, ""
	})
	if len(msgs) != 2 {
		sc.failf("DM 索引条数 = %d，want 2（human + agent）: %+v", len(msgs), hubRealMessageBriefs(msgs))
	}
	if msgs[0].Sender.Kind != agenthub.SenderHuman || !strings.Contains(msgs[0].Text, token) {
		sc.failf("DM 第 1 条不是人类 prompt: %+v", msgs[0])
	}
	if msgs[1].Sender.Kind != agenthub.SenderAgent || msgs[1].Sender.AgentID != a.ID {
		sc.failf("DM 第 2 条不是该 agent 的回复: %+v", msgs[1])
	}
	sc.finalText = msgs[1].Text
	sc.rep.Notes = append(sc.rep.Notes, fmt.Sprintf("agent_id=%s session=%s dm_messages=%d", a.ID, sid, len(msgs)))
}

// —— 场景 2：工具调用（auto，无审批） ——

func (sc *hubRealScenario) scenarioToolAuto() {
	const token = "HAI_TOOL_OK"
	a := sc.createAgent("RealTool", "工具测试", "严格按用户要求调用工具，不要解释。", nil)
	sc.watchDM(a.ID)
	sid := "hub." + string(a.ID) + ".dm"
	sc.sendDM(a.ID, sc.prompt, "real-e2e-tool-1")

	var toolEnd map[string]any
	sc.waitFor(hubRealTimeout, "bash 工具调用完成", func() (bool, string) {
		for _, ev := range sc.events() {
			if str(ev, "event_type") != "tool_run_end" || str(ev, "name") != "bash" {
				continue
			}
			if hubRealBool(ev, "is_error") {
				return false, "bash 工具返回 is_error=true: " + hubRealClip(str(ev, "result"), 800)
			}
			if strings.Contains(str(ev, "result"), token) {
				toolEnd = ev
				return true, ""
			}
		}
		if sc.hasNewAgentEnd(sid) {
			return false, "agent_end 已到达，但没有含 " + token + " 的 bash tool_run_end（模型没调用工具）"
		}
		return false, ""
	})
	if !strings.Contains(str(toolEnd, "arguments"), "printf") {
		sc.failf("bash 工具参数里没有 printf: %s", hubRealClip(str(toolEnd, "arguments"), 800))
	}
	var reply string
	sc.waitFor(hubRealTimeout, "最终回复含工具输出 token", func() (bool, string) {
		for _, m := range sc.messages() {
			if m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == a.ID && strings.Contains(m.Text, token) {
				reply = m.Text
				return true, ""
			}
		}
		if sc.hasNewAgentEnd(sid) {
			return false, "agent_end 已到达，但最终回复不含 " + token
		}
		return false, ""
	})
	sc.finalText = reply
	sc.rep.Notes = append(sc.rep.Notes,
		"bash arguments="+hubRealClip(str(toolEnd, "arguments"), 400),
		"bash result="+hubRealClip(str(toolEnd, "result"), 400))
}

// —— 场景 3：审批（hitl） ——

func (sc *hubRealScenario) scenarioApprovalHITL() {
	const token = "HAI_APPROVAL_OK"
	a := sc.createAgent("RealApproval", "审批测试", "严格按用户要求调用工具，不要解释。", nil)
	sc.watchDM(a.ID)
	bs, err := sc.e.m.hubDMSession(string(a.ID))
	if err != nil {
		sc.failf("hubDMSession: %v", err)
	}
	mode := parseMode("hitl")
	bs.mode = mode
	bs.s.SetMode(mode) // 快照语义：下一次 Run 生效
	sc.watchDM(a.ID)
	sid := "hub." + string(a.ID) + ".dm"
	sc.sendDM(a.ID, sc.prompt, "real-e2e-approval-1")

	var approval map[string]any
	sc.waitFor(hubRealTimeout, "等待 tool_approval_requested", func() (bool, string) {
		for _, ev := range sc.events() {
			if str(ev, "event_type") != "tool_approval_requested" {
				continue
			}
			approval = ev
			return true, ""
		}
		if sc.hasNewAgentEnd(sid) {
			return false, "agent_end 已到达却没有 tool_approval_requested（模型没调用 bash，审批路径没被走到）"
		}
		return false, ""
	})
	if got := str(approval, "name"); got != "bash" {
		sc.failf("审批工具名 = %q，want bash（arguments=%s）", got, hubRealClip(str(approval, "arguments"), 500))
	}
	if !strings.Contains(str(approval, "arguments"), token) {
		sc.failf("审批参数不含 %s: %s", token, hubRealClip(str(approval, "arguments"), 500))
	}
	id := str(approval, "id")
	if id == "" {
		sc.failf("审批事件缺 id（无法注入决策）: %+v", approval)
	}
	if err := bs.s.Approve(id, true); err != nil {
		sc.failf("Approve(%s,true) 失败: %v", id, err)
	}
	sc.decisions[id] = "approved"

	var toolEnd map[string]any
	sc.waitFor(hubRealTimeout, "审批后 bash 执行完成", func() (bool, string) {
		for _, ev := range sc.events() {
			if str(ev, "event_type") != "tool_run_end" || str(ev, "name") != "bash" {
				continue
			}
			if hubRealBool(ev, "is_error") {
				return false, "审批通过后 bash 仍失败: " + hubRealClip(str(ev, "result"), 800)
			}
			if strings.Contains(str(ev, "result"), token) {
				toolEnd = ev
				return true, ""
			}
		}
		return false, ""
	})
	var reply string
	sc.waitFor(hubRealTimeout, "审批后的最终回复", func() (bool, string) {
		for _, m := range sc.messages() {
			if m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == a.ID && strings.Contains(m.Text, token) {
				reply = m.Text
				return true, ""
			}
		}
		if sc.hasNewAgentEnd(sid) {
			return false, "agent_end 已到达，但最终回复不含 " + token
		}
		return false, ""
	})
	sc.finalText = reply
	sc.rep.Notes = append(sc.rep.Notes,
		"approval id="+id+" name=bash decision=approved",
		"bash result="+hubRealClip(str(toolEnd, "result"), 400))
}

// —— 场景 4：群 @all ——

func (sc *hubRealScenario) scenarioGroupMentionAll() {
	const token = "HAI_ALL_OK"
	a := sc.createAgent("RealAllA", "群成员 A", "你是群成员，只在被 @ 时回复，回复简短。", nil)
	b := sc.createAgent("RealAllB", "群成员 B", "你是群成员，只在被 @ 时回复，回复简短。", nil)
	g := groupWithMembers(sc.t, sc.e.m, sc.e.buf, sc.e.nextID(), "RealAllGroup",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"})
	sc.watchGroup(g.ID)
	// 人类消息用 @all 触发；同时明确禁止回复里再出现 @ —— 否则真实模型的回复
	// 会被 Router 当成新一轮 mention，自动接龙把后续场景的基线搅乱（隔离要求）。
	prompt := "@all 每人只回复自己的名字 + HAI_ALL_OK。回复正文里不要出现 @ 符号（不要 @all、不要 @任何人）。"
	sc.rep.Prompt = prompt
	data := sc.sendGroup(g.ID, prompt, true)
	roundID := str(data, "round_id")
	if r, ok := data["round"].(map[string]any); ok {
		roundID = str(r, "id")
	}
	if roundID == "" {
		roundID = hubRealRoundIDFromEvent(sc.events(), g.ID)
	}
	if roundID == "" {
		sc.failf("@all 的 hub_group_send 响应/事件里没有 round id: %+v", data)
	}
	triggerSeq := int64(numOr(data, "seq", 0))
	if triggerSeq == 0 {
		sc.failf("@all 的 hub_group_send 响应里没有 seq: %+v", data)
	}
	sc.rep.Notes = append(sc.rep.Notes, fmt.Sprintf("round=%s trigger_seq=%d targets=%v mention_all=%v",
		roundID, triggerSeq, data["targets"], data["round"]))

	// 真实模型可能用 group_say 工具发言（消息 RoundID 为空，finishHubRound 会按同文去重），
	// 所以判据是「触发 seq 之后该成员有一条含 token 的 agent 消息」，而不是强求 RoundID。
	sc.waitFor(hubRealTimeout, "@all 每个成员各写一条回复", func() (bool, string) {
		got := map[agenthub.AgentID]int{}
		for _, m := range sc.messages() {
			if m.Seq > triggerSeq && m.Sender.Kind == agenthub.SenderAgent && strings.Contains(m.Text, token) {
				got[m.Sender.AgentID]++
			}
		}
		if got[a.ID] > 0 && got[b.ID] > 0 {
			return true, ""
		}
		if sc.hasNewAgentEnd("hub."+string(a.ID)+".g."+string(g.ID)) &&
			sc.hasNewAgentEnd("hub."+string(b.ID)+".g."+string(g.ID)) {
			return false, fmt.Sprintf("两个成员都已 agent_end，但触发后含 %s 的回复数 = %v", token, got)
		}
		return false, ""
	})
	for _, id := range []agenthub.AgentID{a.ID, b.ID} {
		var texts []string
		for _, m := range sc.messages() {
			if m.Seq > triggerSeq && m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == id {
				texts = append(texts, m.Text)
			}
		}
		if len(texts) == 0 {
			sc.failf("%s 在 @all 触发后没有 agent 回复", id)
		}
		hasToken := false
		seen := map[string]bool{}
		for _, text := range texts {
			if strings.Contains(text, token) {
				hasToken = true
				sc.finalText = text
			}
			if seen[text] {
				sc.failf("%s 在 @all 同一轮重复写了同一段正文: %q", id, hubRealClip(text, 300))
			}
			seen[text] = true
		}
		if !hasToken {
			sc.failf("%s 的 @all 回复不含 %s: %v", id, token, hubRealMessageBriefs(sc.messages()))
		}
		for _, text := range texts {
			if strings.Contains(text, "@") {
				sc.failf("%s 的回复出现 @（会触发新一轮群唤醒，破坏场景隔离）: %q", id, hubRealClip(text, 300))
			}
		}
		sc.rep.Notes = append(sc.rep.Notes, fmt.Sprintf("agent %s replies=%d texts=%v", id, len(texts), hubRealBriefTexts(texts, 200)))
	}
	// 两个成员的 run 必须真正收口（AgentEnd），再等群轮闩释放。
	for _, id := range []agenthub.AgentID{a.ID, b.ID} {
		sid := "hub." + string(id) + ".g." + string(g.ID)
		sc.waitFor(hubRealTimeout, string(id)+" agent_end", func() (bool, string) {
			return sc.hasNewAgentEnd(sid), ""
		})
	}
	for _, id := range []string{string(a.ID), string(b.ID)} {
		bs, err := sc.e.m.hubGroupSession(id, string(g.ID))
		if err == nil {
			sc.waitFor(60*time.Second, id+" 群会话空闲", func() (bool, string) {
				return !bs.hubGroupBusy.Load(), ""
			})
		}
	}
	// 只有人类这条 @all 应该产生 hub_round：模型回复不得再触发自动接龙。
	rounds := hubRealRoundEvents(sc.events(), g.ID)
	if len(rounds) != 1 {
		sc.failf("hub_round 事件 = %d 条，want 1（回复里出现 @ 会触发额外轮次）: %+v", len(rounds), rounds)
	}
}

// —— 场景 5：群 @指定 ——

func (sc *hubRealScenario) scenarioGroupMentionOne() {
	const token = "HAI_ONE_OK"
	gid, a, b, sessA, sessB := sc.groupFixtureFromStore()
	baseSeq := hubRealMaxSeq(sc.messages())
	seqA, seqB := sessA.hubGroupSeq.Load(), sessB.hubGroupSeq.Load()

	data := sc.sendGroup(gid, "@"+b.Name+" 只回复 "+token, false)
	roundID := str(data, "round_id")
	if r, ok := data["round"].(map[string]any); ok {
		roundID = str(r, "id")
	}
	targets := hubRealTargets(data)
	if len(targets) != 1 || targets[0] != b.ID {
		sc.failf("@%s targets = %v，want 只有 %s（round=%+v）", b.Name, targets, b.ID, data["round"])
	}
	// 唤醒发号在 hub_group_send 返回前同步完成（P3a-10）：A 不动 = 从未被唤醒。
	if got := sessA.hubGroupSeq.Load(); got != seqA {
		sc.failf("%s 未被 @，唤醒发号却从 %d 变成 %d", a.ID, seqA, got)
	}
	if got := sessB.hubGroupSeq.Load(); got != seqB+1 {
		sc.failf("%s 被 @，唤醒发号 %d → %d，want +1", b.ID, seqB, got)
	}
	triggerSeq := int64(numOr(data, "seq", 0))
	if triggerSeq == 0 {
		sc.failf("@%s 的 hub_group_send 响应里没有 seq: %+v", b.Name, data)
	}
	sc.rep.Notes = append(sc.rep.Notes,
		fmt.Sprintf("round=%s trigger_seq=%d targets=%v", roundID, triggerSeq, targets),
		fmt.Sprintf("baseline_seq=%d wake_seq A=%d→%d B=%d→%d", baseSeq, seqA, sessA.hubGroupSeq.Load(), seqB, sessB.hubGroupSeq.Load()))

	sc.waitFor(hubRealTimeout, "被 @ 的成员写回复", func() (bool, string) {
		for _, m := range sc.messages() {
			if m.Seq > triggerSeq && m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == b.ID &&
				strings.Contains(m.Text, token) {
				sc.finalText = m.Text
				return true, ""
			}
		}
		if sc.hasNewAgentEnd("hub." + string(b.ID) + ".g." + string(gid)) {
			return false, "agent_end 已到达，但触发后没有含 " + token + " 的 agent 回复"
		}
		return false, ""
	})
	// 等这个 run 真正收尾（agent_end）：busy 闩可能先于收尾事件释放，
	// 不等它会把「上一场景的迟到 agent_end」带进下一场景。
	sc.waitFor(hubRealTimeout, "B 的 run 收尾", func() (bool, string) {
		return sc.hasNewAgentEnd("hub." + string(b.ID) + ".g." + string(gid)), ""
	})
	sc.waitFor(60*time.Second, "B 群会话空闲", func() (bool, string) {
		return !sessB.hubGroupBusy.Load(), ""
	})
	// 只有人类这条 @B 应该产生 hub_round：B 的回复不得再触发自动接龙。
	if rounds := hubRealRoundEvents(sc.events(), gid); len(rounds) != 1 {
		sc.failf("hub_round 事件 = %d 条，want 1（只应有人的 @%s）: %+v", len(rounds), b.Name, rounds)
	}
	// 未被 @ 的 A：触发之后没有新 agent 消息，游标也不动。
	for _, m := range sc.messages() {
		if m.Seq <= triggerSeq {
			continue
		}
		if m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == a.ID {
			sc.failf("%s 未被 @，却写了新 agent 消息: seq=%d text=%q", a.ID, m.Seq, hubRealClip(m.Text, 300))
		}
	}
	if got := sessA.hubGroupSeq.Load(); got != seqA {
		sc.failf("%s 未被 @，等待结束后唤醒发号 = %d（曾被唤醒）", a.ID, got)
	}
}

// groupFixtureFromStore 复用场景 4 建的群与两个成员（@all/@指定/handoff 同群）。
func (sc *hubRealScenario) groupFixtureFromStore() (agenthub.GroupID, agenthub.AgentProfile, agenthub.AgentProfile, *bridgeSession, *bridgeSession) {
	sc.t.Helper()
	st := sc.store()
	ctx := context.Background()
	groups, err := st.ListGroups(ctx)
	if err != nil {
		sc.failf("ListGroups: %v", err)
	}
	var gid agenthub.GroupID
	for _, g := range groups {
		if g != nil && g.Name == "RealAllGroup" && !g.Deleted {
			gid = g.ID
			break
		}
	}
	if gid == "" {
		sc.failf("找不到场景 4 建的 RealAllGroup（场景 5/6 依赖场景 4 的同群与成员；只选 5/6 时请显式同时选择 4_group_mention_all）")
	}
	var a, b agenthub.AgentProfile
	agents, err := st.ListAgents(ctx)
	if err != nil {
		sc.failf("ListAgents: %v", err)
	}
	for _, p := range agents {
		if p == nil {
			continue
		}
		switch p.Name {
		case "RealAllA":
			a = *p
		case "RealAllB":
			b = *p
		}
	}
	if a.ID == "" || b.ID == "" {
		sc.failf("找不到 RealAllA/RealAllB（场景 4 创建；场景 5/6 依赖）")
	}
	sessA, err := sc.e.m.hubGroupSession(string(a.ID), string(gid))
	if err != nil {
		sc.failf("hubGroupSession(A): %v", err)
	}
	sessB, err := sc.e.m.hubGroupSession(string(b.ID), string(gid))
	if err != nil {
		sc.failf("hubGroupSession(B): %v", err)
	}
	sc.watchGroup(gid)
	return gid, a, b, sessA, sessB
}

// —— 场景 6：@A → @B handoff ——

func (sc *hubRealScenario) scenarioHandoff() {
	const token = "HANDOFF_OK"
	gid, a, b, sessA, sessB := sc.groupFixtureFromStore()
	sidA := "hub." + string(a.ID) + ".g." + string(gid)
	sidB := "hub." + string(b.ID) + ".g." + string(gid)
	sc.rep.Notes = append(sc.rep.Notes, "from="+string(a.ID)+" to="+string(b.ID))

	// —— 阶段 1：A 创建 handoff（instruction 是 token 的唯一来源） ——
	// 人类 prompt 只描述动作；token 只出现在 instruction 里，B 的 prompt 全程不出现。
	promptA := fmt.Sprintf("@%s 调用 handoff 工具：group_id=%s，to_agent_id=%s，instruction 写「只回复 %s」。不要自己完成，不要回复我。",
		a.Name, gid, b.ID, token)
	sc.rep.Prompt = promptA
	sc.sendGroup(gid, promptA, false)
	var handoffID string
	sc.waitFor(hubRealTimeout, "handoff 创建事件", func() (bool, string) {
		for _, ev := range sc.events() {
			if str(ev, "event_type") != "hub_handoff" || str(ev, "group_id") != string(gid) {
				continue
			}
			h, _ := ev["handoff"].(map[string]any)
			if h == nil {
				continue
			}
			if str(h, "from_agent_id") != string(a.ID) || str(h, "to_agent_id") != string(b.ID) {
				return false, fmt.Sprintf("hub_handoff from/to = %s/%s，want %s/%s",
					str(h, "from_agent_id"), str(h, "to_agent_id"), a.ID, b.ID)
			}
			status := str(h, "status")
			if status != string(agenthub.HandoffPendingAck) {
				return false, fmt.Sprintf("新建 handoff status = %q，want %q", status, agenthub.HandoffPendingAck)
			}
			handoffID = str(h, "id")
			return true, ""
		}
		if sc.hasNewAgentEnd(sidA) {
			return false, "A 的 agent_end 已到达却没有 hub_handoff 事件（模型没调用 handoff 工具）"
		}
		return false, ""
	})
	if handoffID == "" {
		sc.failf("hub_handoff 事件缺 handoff id")
	}
	h0, err := sc.store().LoadHandoff(context.Background(), handoffID)
	if err != nil || h0 == nil {
		sc.failf("LoadHandoff(%s): %v", handoffID, err)
	} else if h0.Status != agenthub.HandoffPendingAck {
		sc.failf("handoff %s 落盘状态 = %q，want pending_ack", handoffID, h0.Status)
	}
	// A 的 handoff run：后面用来证明 B 的回复不是 A 的同一条 run。
	var aRunID string
	for _, ev := range sc.events() {
		// handoff 工具的参数里没有 handoff_id（id 是调用后分配的），按工具名取本次
		// handoff run 的 run_id 即可；取最后一个成功调用，兼容模型重试。
		if str(ev, "event_type") == "tool_run_end" && str(ev, "name") == "handoff" &&
			!hubRealBool(ev, "is_error") && str(ev, "run_id") != "" {
			aRunID = str(ev, "run_id")
		}
	}
	sc.waitFor(hubRealTimeout, "A 的 handoff run 收尾", func() (bool, string) {
		return sc.hasNewAgentEnd(sidA), ""
	})
	sc.waitFor(60*time.Second, "A 群会话空闲（handoff 工具后模型可能还要再收尾一轮）", func() (bool, string) {
		return !sessA.hubGroupBusy.Load(), ""
	})

	// —— 阶段 2：B 实际调用 handoff_ack 接受（prompt 不含 token） ——
	// 明确要求「必须实际调用工具」：只回文字不算完成验收（真实模型会偷懒直接用正文回复）。
	promptB1 := fmt.Sprintf("@%s 你必须**实际调用 handoff_ack 工具**（不要用文字代替工具调用）："+
		"handoff_id 必须是 %q，accept 必须是 true。本轮只回复「已接受」，"+
		"不要猜测交接内容，不要执行 instruction。",
		b.Name, handoffID)
	sc.rep.Prompt = promptA + "\n---\n" + promptB1
	sc.sendGroup(gid, promptB1, false)

	var ack1 map[string]any
	ack1Idx := -1
	sc.waitFor(hubRealTimeout, "handoff_ack 工具调用", func() (bool, string) {
		for i, ev := range sc.events() {
			if str(ev, "event_type") != "tool_run_end" || str(ev, "name") != "handoff_ack" {
				continue
			}
			// 只认 B 的 ack：A 可能在 handoff 后自己误调 handoff_ack（accept=false，
			// 这是模型的真实行为），不能把发起方的失败调用当成本场景的验收对象。
			if str(ev, "session_id") != sidB {
				continue
			}
			if rid := str(ev, "run_id"); rid != "" && sc.priorRuns[rid] {
				continue
			}
			if hubRealBool(ev, "is_error") {
				return false, "handoff_ack 返回 is_error=true: " + hubRealClip(str(ev, "result"), 800)
			}
			if strings.Contains(str(ev, "arguments"), handoffID) && strings.Contains(str(ev, "result"), "accepted") {
				ack1 = ev
				ack1Idx = i
				return true, ""
			}
		}
		if sc.hasNewAgentEnd(sidB) {
			return false, "B 的 agent_end 已到达但没有成功的 handoff_ack 调用"
		}
		return false, ""
	})
	h1, err := sc.store().LoadHandoff(context.Background(), handoffID)
	if err != nil || h1 == nil {
		sc.failf("LoadHandoff(%s) after ack: %v", handoffID, err)
	} else if h1.Status != agenthub.HandoffAccepted {
		sc.failf("handoff %s ack 后状态 = %q，want accepted", handoffID, h1.Status)
	}
	// 阶段 3：instruction 确实随 ack 工具结果回到了模型（不是人类 prompt 喂的答案）。
	ack1Result := str(ack1, "result")
	if !strings.Contains(ack1Result, token) {
		sc.failf("handoff_ack 结果里没有 instruction/token（B 无法从工具结果得知该回什么）: %s",
			hubRealClip(ack1Result, 800))
	}
	if h1.Instruction == "" || !strings.Contains(h1.Instruction, token) {
		sc.failf("handoff %s 落盘 instruction 不含 token: %q", handoffID, hubRealClip(h1.Instruction, 300))
	}
	// accepted 卡片（ack 触发的消息）的 seq：B 的最终回复必须排在它之后。
	var ackCardSeq int64
	for _, ev := range sc.events() {
		if str(ev, "event_type") != "hub_handoff" || str(ev, "group_id") != string(gid) {
			continue
		}
		h, _ := ev["handoff"].(map[string]any)
		if h == nil || str(h, "id") != handoffID || str(h, "status") != string(agenthub.HandoffAccepted) {
			continue
		}
		if !strings.Contains(str(h, "instruction"), token) {
			sc.failf("accepted hub_handoff 事件不含 instruction/token: %q", hubRealClip(str(h, "instruction"), 300))
		}
		ackCardSeq = int64(numOr(ev, "message_seq", 0))
	}
	if ackCardSeq == 0 {
		sc.failf("找不到 handoff %s 的 accepted 事件/message_seq（无法证明回复在 ack 之后）", handoffID)
	}
	sc.waitFor(hubRealTimeout, "B 的 ack run 收尾", func() (bool, string) {
		return sc.hasNewAgentEnd(sidB), ""
	})
	sc.waitFor(60*time.Second, "B 群会话空闲（ack run）", func() (bool, string) {
		return !sessB.hubGroupBusy.Load(), ""
	})

	// —— 阶段 4：新 run 里按 instruction 回复（token 仍然不在人类 prompt 里） ——
	// 第二个 prompt 发生在 ack 之后，因此产生的 run_id 必然是 ack 之后的新 run；
	// 要求再调一次幂等 handoff_ack，让 instruction 再次经由工具结果进入本 run
	// （若模型没再调工具、但确实按上下文回复，仍用第一轮 ack 结果 + LoadHandoff 断言）。
	promptB2 := fmt.Sprintf("@%s 再次实际调用 handoff_ack 工具（幂等，重复接受不会改变状态）："+
		"handoff_id 必须是 %q，accept=true。然后严格按工具返回的 handoff.instruction 回复："+
		"不要猜测、不要额外解释、不要提工具名。",
		b.Name, handoffID)
	if strings.Contains(promptB1, token) || strings.Contains(promptB2, token) {
		sc.failf("promptB 不得含 token（否则无法证明 instruction 被投递）")
	}
	sc.rep.Prompt = promptA + "\n---\n" + promptB1 + "\n---\n" + promptB2
	eventsBeforeB2 := len(sc.events())
	dataB2 := sc.sendGroup(gid, promptB2, false)
	triggerB2 := int64(numOr(dataB2, "seq", 0))
	if triggerB2 == 0 {
		sc.failf("第二个 prompt 的 hub_group_send 响应里没有 seq: %+v", dataB2)
	}

	var replyMsg agenthub.Message
	sc.waitFor(hubRealTimeout, "B 按 instruction 回复", func() (bool, string) {
		for _, m := range sc.messages() {
			if m.Seq > triggerB2 && m.Sender.Kind == agenthub.SenderAgent &&
				m.Sender.AgentID == b.ID && strings.Contains(m.Text, token) {
				replyMsg = m
				return true, ""
			}
		}
		// 本轮 run 已收尾仍没有 token 回复 → 立即失败，不等满 120s。
		for i, ev := range sc.events() {
			if i < eventsBeforeB2 || str(ev, "event_type") != "agent_end" || str(ev, "session_id") != sidB ||
				str(ev, "parent_run_id") != "" {
				continue
			}
			return false, "B 的第二个 run 已 agent_end，但没有含 " + token + " 的回复（instruction 没被采纳？）: " +
				hubRealClip(str(ev, "content"), 500)
		}
		return false, ""
	})
	if replyMsg.Seq <= ackCardSeq {
		sc.failf("B 的回复 seq=%d 不在 accepted 消息 seq=%d 之后", replyMsg.Seq, ackCardSeq)
	}

	// 回复所属 run：必须是 eventsBeforeB2 之后启动的新 run，且不是 A 的 handoff run。
	evs := sc.events()
	var endEv map[string]any
	endIdx := -1
	for i := eventsBeforeB2; i < len(evs); i++ {
		ev := evs[i]
		if str(ev, "event_type") == "agent_end" && str(ev, "session_id") == sidB &&
			str(ev, "parent_run_id") == "" {
			endEv = ev
			endIdx = i
		}
	}
	if endEv == nil {
		sc.waitFor(hubRealTimeout, "B 的第二个 run 收尾", func() (bool, string) {
			evs = sc.events()
			for i := eventsBeforeB2; i < len(evs); i++ {
				if str(evs[i], "event_type") == "agent_end" && str(evs[i], "session_id") == sidB &&
					str(evs[i], "parent_run_id") == "" {
					endEv = evs[i]
					endIdx = i
					return true, ""
				}
			}
			return false, ""
		})
	}
	rid2 := str(endEv, "run_id")
	if rid2 == "" {
		sc.failf("B 第二个 run 的 agent_end 缺 run_id")
	}
	if sc.priorRuns[rid2] {
		sc.failf("B 的回复来自上一场景的旧 run_id=%s", rid2)
	}
	if aRunID != "" && rid2 == aRunID {
		sc.failf("B 的回复 run_id=%s 与 A 的 handoff run 相同（没有在 ack 之后新起 run）", rid2)
	}
	startIdx := -1
	for i := eventsBeforeB2; i < len(evs); i++ {
		if str(evs[i], "event_type") == "agent_start" && str(evs[i], "session_id") == sidB &&
			str(evs[i], "run_id") == rid2 && str(evs[i], "parent_run_id") == "" {
			startIdx = i
			break
		}
	}
	if startIdx < 0 {
		sc.failf("找不到 B 第二个 run（run_id=%s）的 agent_start：无法证明回复来自新 run", rid2)
	}
	if startIdx <= ack1Idx {
		sc.failf("B 第二个 run 的 agent_start（idx=%d）不在 ack（idx=%d）之后", startIdx, ack1Idx)
	}
	if endIdx <= ack1Idx {
		sc.failf("B 第二个 run 的 agent_end（idx=%d）不在 ack（idx=%d）之后", endIdx, ack1Idx)
	}

	// 若第二个 run 里也调了 handoff_ack：它的结果必须带 instruction，且属于同一个 run。
	ack2Seen := false
	for i := eventsBeforeB2; i < len(evs); i++ {
		ev := evs[i]
		if str(ev, "event_type") != "tool_run_end" || str(ev, "name") != "handoff_ack" ||
			str(ev, "session_id") != sidB {
			continue
		}
		if hubRealBool(ev, "is_error") {
			sc.failf("第二个 run 的 handoff_ack 失败: %s", hubRealClip(str(ev, "result"), 800))
		}
		if str(ev, "run_id") != rid2 {
			sc.failf("第二个 run 里的 handoff_ack run_id=%s，want %s", str(ev, "run_id"), rid2)
		}
		if !strings.Contains(str(ev, "result"), token) {
			sc.failf("第二个 run 的 handoff_ack 结果不含 instruction/token: %s", hubRealClip(str(ev, "result"), 800))
		}
		ack2Seen = true
	}
	if ack2Seen {
		sc.rep.Notes = append(sc.rep.Notes, "第二个 run 内 handoff_ack 结果携带 instruction；回复来自该 run")
	} else {
		sc.rep.Notes = append(sc.rep.Notes, "第二个 run 未重复 handoff_ack；instruction 证据取自第一轮 ack 结果 + LoadHandoff")
	}

	sc.finalText = replyMsg.Text
	sc.rep.Notes = append(sc.rep.Notes,
		fmt.Sprintf("handoff_id=%s ack_message_seq=%d reply_seq=%d trigger_b2_seq=%d reply_run=%s",
			handoffID, ackCardSeq, replyMsg.Seq, triggerB2, rid2),
		"promptB 不含 token；B 的回复 seq 在 accepted 消息之后，run 在 ack 之后新起")

	// 场景 6 收尾：等 B 真正空闲再交给场景 7，避免迟到的 llm_end/tool_run_end
	// 落进场景 7 的统计窗口（sync 还会按 run_id 过滤，这里是第二道保险）。
	sc.waitFor(60*time.Second, "B 群会话空闲（场景 6 收尾，避免串入场景 7）", func() (bool, string) {
		return !sessB.hubGroupBusy.Load(), ""
	})
}

// —— 场景 7：人类 @ 后自主「建项目 + 建会话 + 派任务 + 项目会话真跑」 ——

func (sc *hubRealScenario) scenarioProjectSession() {
	const token = "HAI_PROJECT_SESSION_OK"
	tempHome, err := os.UserHomeDir()
	if err != nil {
		sc.failf("解析临时 HOME 失败: %v", err)
	}
	root := filepath.Join(tempHome, "hub-eval-project-roots")
	if err := os.MkdirAll(root, 0o755); err != nil {
		sc.failf("建项目根失败（%s）: %v", root, err)
	}
	projectName := fmt.Sprintf("hai-eval-%d", time.Now().UnixNano()%1e9)
	projectName = strings.ReplaceAll(projectName, "-", "") // 单段目录名，无分隔符
	a := sc.createAgent("ProjectRunner", "项目执行者",
		"你能自主创建项目、创建会话并把任务派到项目会话；严格按用户给的顺序调用工具，不要用 bash 代替 workspace/session 工具。",
		map[string]any{
			"can_create_workspace": true,
			"can_dispatch":         true,
			"can_handoff":          true,
			"can_share_artifact":   true,
			"workspace_roots":      []string{root},
			"max_auto_turns":       12,
		})
	g := groupWithMembers(sc.t, sc.e.m, sc.e.buf, sc.e.nextID(), "RealProjectGroup",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"})
	sc.watchGroup(g.ID)
	hubSID := "hub." + string(a.ID) + ".g." + string(g.ID)
	sc.rep.Project = &hubRealProject{Root: root, RequestedName: projectName}

	prompt := fmt.Sprintf("@%s 请依次完成，不要跳步、不要用 bash 代替：\n"+
		"1) 调用 workspace_create 创建项目，name 必须是 %q（单段目录名）；\n"+
		"2) 调用 workspace_list 确认它已登记；\n"+
		"3) 调用 session_create 在该 workspace 下创建会话；\n"+
		"4) 调用 session_list 确认该会话存在；\n"+
		"5) 调用 session_send 给该会话派任务，text 必须是「只回复 %s」；\n"+
		"6) 最后把 workspace 路径、session_id、task_id 回给我。",
		a.Name, projectName, token)
	sc.rep.Prompt = prompt
	sc.sendGroup(g.ID, prompt, false)

	// 1) workspace_create
	wsEvidence := sc.waitToolEnd(hubSID, "workspace_create", func(ev map[string]any) (bool, string) {
		return !hubRealBool(ev, "is_error"), ""
	})
	var wsRes struct {
		Workspace struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal([]byte(str(wsEvidence, "result")), &wsRes); err != nil || wsRes.Workspace.Path == "" {
		sc.failf("workspace_create 结果不可解析: %s (err=%v)", hubRealClip(str(wsEvidence, "result"), 1000), err)
	}
	wsPath := wsRes.Workspace.Path
	sc.rep.Project.WorkspaceCreate = hubRealToolEvidence(wsEvidence)
	sc.rep.Project.WorkspacePath = wsPath
	// 断言**参数**：模型传的项目名就是人要求的那个（不是自己编的另一个）。
	if args := str(wsEvidence, "arguments"); !strings.Contains(args, projectName) {
		sc.failf("workspace_create 参数里没有要求的项目名 %q: %s", projectName, hubRealClip(args, 800))
	}
	if st, err := os.Stat(wsPath); err != nil || !st.IsDir() {
		sc.failf("workspace_create 声称的目录不存在: %s (err=%v)", wsPath, err)
	}
	if !hubRealPathWithin(wsPath, root) {
		sc.failf("workspace_create 建到了 policy.workspace_roots 之外: %s (root=%s)", wsPath, root)
	}

	// 2) workspace_list 必须能看到它
	listEvidence := sc.waitToolEnd(hubSID, "workspace_list", func(ev map[string]any) (bool, string) {
		return !hubRealBool(ev, "is_error") && strings.Contains(str(ev, "result"), wsPath), ""
	})
	sc.rep.Project.WorkspaceList = hubRealToolEvidence(listEvidence)

	// 3) session_create
	createEvidence := sc.waitToolEnd(hubSID, "session_create", func(ev map[string]any) (bool, string) {
		return !hubRealBool(ev, "is_error"), ""
	})
	var sessRes struct {
		Session struct {
			ID        string `json:"id"`
			Workspace string `json:"workspace"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(str(createEvidence, "result")), &sessRes); err != nil || sessRes.Session.ID == "" {
		sc.failf("session_create 结果不可解析: %s (err=%v)", hubRealClip(str(createEvidence, "result"), 1000), err)
	}
	sid := sessRes.Session.ID
	sc.rep.Project.SessionCreate = hubRealToolEvidence(createEvidence)
	sc.rep.Project.SessionID = sid
	// 断言参数：session_create 用的就是 workspace_create 返回的路径。
	if args := str(createEvidence, "arguments"); !strings.Contains(args, wsPath) {
		sc.failf("session_create 参数里没有 workspace_create 返回的路径 %q: %s", wsPath, hubRealClip(args, 800))
	}
	if bs := sc.e.m.get(wsPath, sid); bs == nil {
		sc.failf("session_create 返回 %s，但 m.get(%s,%s) == nil", sid, wsPath, sid)
	}
	found := false
	for _, row := range sc.e.m.listSessions(wsPath) {
		if str(row, "id") == sid {
			found = true
			break
		}
	}
	if !found {
		sc.failf("listSessions(%s) 里没有 session_create 返回的 %s", wsPath, sid)
	}

	// 4) session_list 必须能看到它
	sessListEvidence := sc.waitToolEnd(hubSID, "session_list", func(ev map[string]any) (bool, string) {
		return !hubRealBool(ev, "is_error") && strings.Contains(str(ev, "result"), sid), ""
	})
	sc.rep.Project.SessionList = hubRealToolEvidence(sessListEvidence)

	// 5) session_send
	sendEvidence := sc.waitToolEnd(hubSID, "session_send", func(ev map[string]any) (bool, string) {
		return !hubRealBool(ev, "is_error"), ""
	})
	var sendRes struct {
		TaskID    string `json:"task_id"`
		SessionID string `json:"session_id"`
		Accepted  bool   `json:"accepted"`
		Status    string `json:"status"`
		Queued    bool   `json:"queued"`
	}
	if err := json.Unmarshal([]byte(str(sendEvidence, "result")), &sendRes); err != nil {
		sc.failf("session_send 结果不可解析: %s (err=%v)", hubRealClip(str(sendEvidence, "result"), 1000), err)
	}
	sc.rep.Project.SessionSend = hubRealToolEvidence(sendEvidence)
	sc.rep.Project.TaskID = sendRes.TaskID
	sc.rep.Project.TaskStatus = sendRes.Status
	sc.rep.Project.Accepted = sendRes.Accepted
	sc.rep.Project.Queued = sendRes.Queued
	if !sendRes.Accepted || sendRes.TaskID == "" {
		sc.failf("session_send accepted=%v task_id=%q，want accepted=true 且有 task_id（status=%q queued=%v）",
			sendRes.Accepted, sendRes.TaskID, sendRes.Status, sendRes.Queued)
	}
	if sendRes.SessionID != "" && sendRes.SessionID != sid {
		sc.failf("session_send 回的 session_id=%s，want %s", sendRes.SessionID, sid)
	}
	// 断言参数：派给的是刚建的那个会话，任务文本就是要求的 token 提示。
	if args := str(sendEvidence, "arguments"); !strings.Contains(args, sid) || !strings.Contains(args, token) {
		sc.failf("session_send 参数不符（需要 session_id=%s 且 text 含 %s）: %s", sid, token, hubRealClip(args, 800))
	}
	if sendRes.Queued || strings.EqualFold(sendRes.Status, "queued") {
		sc.waitFor(60*time.Second, "排队任务被派发", func() (bool, string) {
			task, err := agenthub.NewLedger(sc.store()).Replay(context.Background(), agenthub.TaskID(sendRes.TaskID))
			if err != nil {
				return false, ""
			}
			switch task.Status {
			case agenthub.TaskDispatched, agenthub.TaskRunning, agenthub.TaskCompleted, agenthub.TaskDelivered:
				sc.rep.Project.TaskStatus = string(task.Status)
				return true, ""
			case agenthub.TaskCanceled, agenthub.TaskFailed, agenthub.TaskDeclined, agenthub.TaskBlockedBudget:
				return false, fmt.Sprintf("排队任务进入终态 %s: %s", task.Status, task.Error)
			}
			return false, ""
		})
	}

	// 6) 项目会话必须**真的跑**：按 session_id 过滤 llm_end/agent_end，且正文含 token。
	var projectEnd map[string]any
	sc.waitFor(hubRealTimeout, "项目会话 agent_end", func() (bool, string) {
		for _, ev := range sc.events() {
			if str(ev, "event_type") == "agent_end" && str(ev, "session_id") == sid {
				projectEnd = ev
				if strings.Contains(str(ev, "content"), token) {
					return true, ""
				}
				return false, "项目会话 agent_end 不含 " + token + "：" + hubRealClip(str(ev, "content"), 500)
			}
			if str(ev, "event_type") == "llm_error" && str(ev, "session_id") == sid {
				return false, "项目会话 LLM 失败: " + hubRealClip(str(ev, "message"), 500)
			}
		}
		return false, ""
	})
	var projectLLM hubRealLLM
	projectLLMSeen := false
	for _, ev := range sc.events() {
		if str(ev, "event_type") != "llm_end" || str(ev, "session_id") != sid {
			continue
		}
		in, out, cr, cw, reas, cost, priced := hubRealUsage(ev)
		projectLLM.Calls++
		projectLLM.InputTokens += in
		projectLLM.OutputTokens += out
		projectLLM.CacheRead += cr
		projectLLM.CacheWrite += cw
		projectLLM.Reasoning += reas
		projectLLM.CostUSD += cost
		if priced {
			projectLLM.PricedCalls++
		} else {
			projectLLM.UnpricedCalls++
		}
		if strings.Contains(str(ev, "content"), token) {
			projectLLMSeen = true
		}
	}
	sc.rep.Project.SessionLLM = projectLLM
	if projectLLM.Calls == 0 {
		sc.failf("项目会话 %s 没有 llm_end（会话被创建但没有真跑）", sid)
	}
	if !projectLLMSeen {
		sc.failf("项目会话 %s 的 llm_end 正文都不含 %s（agent_end 有 token 也不够：llm 事件是真实调用的证据）", sid, token)
	}
	sc.rep.Project.SessionAgentEnd = hubRealClip(str(projectEnd, "content"), 2000)
	sc.rep.Project.SessionFinalText = hubRealClip(str(projectEnd, "content"), 4000)

	// 6b) 账本证据：派出的任务必须真的被**结算**（completed/delivered），
	// 且 Result.Summary 来自项目会话的最终答复（含 token）——不是只拿到 task_id 就结束。
	sc.waitFor(60*time.Second, "session_send 任务结算", func() (bool, string) {
		task, err := agenthub.NewLedger(sc.store()).Replay(context.Background(), agenthub.TaskID(sendRes.TaskID))
		if err != nil {
			return false, ""
		}
		sc.rep.Project.TaskResultStatus = string(task.Status)
		if task.Status == agenthub.TaskCompleted || task.Status == agenthub.TaskDelivered {
			if task.Result != nil {
				sc.rep.Project.TaskResultFinishReason = task.Result.FinishReason
				sc.rep.Project.TaskResultSummary = hubRealClip(task.Result.Summary, 4000)
				if !strings.Contains(task.Result.Summary, token) {
					return false, fmt.Sprintf("任务 %s 已 %s，但 Result.Summary 不含 %s: %q",
						sendRes.TaskID, task.Status, token, hubRealClip(task.Result.Summary, 500))
				}
			}
			return true, ""
		}
		switch task.Status {
		case agenthub.TaskFailed, agenthub.TaskInterrupted, agenthub.TaskCanceled,
			agenthub.TaskDeclined, agenthub.TaskBlockedBudget:
			return false, fmt.Sprintf("派出的任务进入终态 %s: %s", task.Status, task.Error)
		}
		return false, ""
	})

	// 会话必须回到空闲（不是只断言工具返回 JSON）。
	sc.waitFor(60*time.Second, "项目会话 running=false", func() (bool, string) {
		bs := sc.e.m.get(wsPath, sid)
		if bs == nil {
			return false, "项目会话从 m.get 里消失"
		}
		if !bs.running.Load() && !bs.hubDispatchPending.Load() {
			return true, ""
		}
		return false, ""
	})

	// 7) Hub agent 的最终群回复应带 workspace/session_id（任务要求回给人类）。
	sc.waitFor(hubRealTimeout, "Hub agent 回报 workspace/session_id", func() (bool, string) {
		for _, m := range sc.messages() {
			if m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == a.ID &&
				strings.Contains(m.Text, sid) {
				sc.finalText = m.Text
				return true, ""
			}
		}
		if sc.hasNewAgentEnd(hubSID) {
			return false, "Hub agent 已 agent_end，但群回复里没有 session_id " + sid
		}
		return false, ""
	})
	if sc.rep.Project.TaskResultStatus != "" {
		sc.rep.Project.TaskStatus = sc.rep.Project.TaskResultStatus
	} else {
		sc.rep.Project.TaskStatus = sendRes.Status
		if sc.rep.Project.TaskStatus == "" {
			sc.rep.Project.TaskStatus = "dispatched"
		}
	}
	sc.rep.Notes = append(sc.rep.Notes,
		"workspace_create name="+projectName,
		"workspace_path="+wsPath,
		"session_id="+sid,
		"task_id="+sendRes.TaskID,
		fmt.Sprintf("project_session_llm calls=%d in=%d out=%d cost_usd=%.6f",
			projectLLM.Calls, projectLLM.InputTokens, projectLLM.OutputTokens, projectLLM.CostUSD),
		fmt.Sprintf("task_result status=%s finish_reason=%s summary=%q",
			sc.rep.Project.TaskResultStatus, sc.rep.Project.TaskResultFinishReason,
			hubRealClip(sc.rep.Project.TaskResultSummary, 300)),
		fmt.Sprintf("project_agent_end content=%q", hubRealClip(str(projectEnd, "content"), 300)))
}

// waitToolEnd 等某个工具成功结束；失败/未调用都要响亮失败（不静默等到超时）。
// accept 返回 true = 找到符合预期的调用；返回 terminal 非空 = 立即判失败。
func (sc *hubRealScenario) waitToolEnd(hubSID, name string, accept func(ev map[string]any) (bool, string)) map[string]any {
	sc.t.Helper()
	var found map[string]any
	var lastErr string
	sc.waitFor(hubRealTimeout, "工具 "+name+" 成功完成", func() (bool, string) {
		for _, ev := range sc.events() {
			if str(ev, "event_type") != "tool_run_end" || str(ev, "name") != name {
				continue
			}
			if hubRealBool(ev, "is_error") {
				lastErr = hubRealClip(str(ev, "result"), 800)
				continue
			}
			ok, terminal := accept(ev)
			if terminal != "" {
				return false, terminal
			}
			if ok {
				found = ev
				return true, ""
			}
		}
		if sc.hasNewAgentEnd(hubSID) {
			msg := "agent_end 已到达但没有成功的 " + name + " 调用"
			if lastErr != "" {
				msg += "；最近一次工具错误: " + lastErr
			}
			// 把该场景已发生的工具调用摘要塞进诊断，而不是只说"没调用"。
			for _, ev := range sc.events() {
				if str(ev, "event_type") == "tool_run_end" {
					sc.rep.Notes = append(sc.rep.Notes, fmt.Sprintf("tool %s is_error=%v result=%s",
						str(ev, "name"), hubRealBool(ev, "is_error"), hubRealClip(str(ev, "result"), 300)))
				}
			}
			return false, msg
		}
		return false, ""
	})
	return found
}

// —— 事件/报告小工具 ——

// hasNewAgentEnd 本场景内是否出现了**新 run** 的 agent_end（忽略场景开始前就存在的 run）。
func (sc *hubRealScenario) hasNewAgentEnd(sessionID string) bool {
	for _, ev := range sc.events() {
		if str(ev, "event_type") != "agent_end" {
			continue
		}
		if sessionID != "" && str(ev, "session_id") != sessionID {
			continue
		}
		if rid := str(ev, "run_id"); rid != "" && sc.priorRuns[rid] {
			continue // 上一场景的迟到收尾
		}
		return true
	}
	return false
}

func hubRealRoundEvents(events []map[string]any, gid agenthub.GroupID) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if str(ev, "event_type") == "hub_round" && str(ev, "group_id") == string(gid) {
			out = append(out, ev)
		}
	}
	return out
}

func hubRealRoundIDFromEvent(events []map[string]any, gid agenthub.GroupID) string {
	for _, ev := range events {
		if str(ev, "event_type") != "hub_round" || str(ev, "group_id") != string(gid) {
			continue
		}
		if r, ok := ev["round"].(map[string]any); ok {
			return str(r, "id")
		}
	}
	return ""
}

func hubRealTargets(data map[string]any) []agenthub.AgentID {
	var out []agenthub.AgentID
	raw, ok := data["targets"].([]any)
	if !ok {
		if r, ok := data["round"].(map[string]any); ok {
			raw, _ = r["targets"].([]any)
		}
	}
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, agenthub.AgentID(s))
		}
	}
	return out
}

func hubRealMaxSeq(msgs []agenthub.Message) int64 {
	var max int64
	for _, m := range msgs {
		if m.Seq > max {
			max = m.Seq
		}
	}
	return max
}

func hubRealBriefTexts(texts []string, n int) []string {
	out := make([]string, 0, len(texts))
	for _, t := range texts {
		out = append(out, hubRealClip(t, n))
	}
	return out
}

func hubRealMessageBriefs(msgs []agenthub.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, fmt.Sprintf("seq=%d sender=%s agent=%s text=%q", m.Seq, m.Sender.Kind, m.Sender.AgentID, hubRealClip(m.Text, 200)))
	}
	return out
}

func hubRealToolEvidence(ev map[string]any) *hubRealToolCall {
	return &hubRealToolCall{
		Name:       str(ev, "name"),
		Arguments:  hubRealClip(str(ev, "arguments"), 2000),
		Result:     hubRealClip(str(ev, "result"), 4000),
		IsError:    hubRealBool(ev, "is_error"),
		SessionID:  str(ev, "session_id"),
		ToolCallID: str(ev, "id"),
	}
}

func hubRealUsage(ev map[string]any) (in, out, cacheRead, cacheWrite, reasoning int64, costUSD float64, priced bool) {
	u, ok := ev["usage"].(map[string]any)
	if !ok || u == nil {
		return
	}
	in = int64(hubRealNum(u, "Input"))
	out = int64(hubRealNum(u, "Output"))
	cacheRead = int64(hubRealNum(u, "CacheRead"))
	cacheWrite = int64(hubRealNum(u, "CacheWrite"))
	reasoning = int64(hubRealNum(u, "Reasoning"))
	if c, ok := u["Cost"].(map[string]any); ok {
		priced, _ = c["Priced"].(bool)
		costUSD = hubRealNum(c, "Total") / 1e6 // core.Cost.Total 是 µUSD
	}
	return
}

func hubRealNum(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func hubRealBool(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func hubRealClip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…(truncated)"
}

func hubRealPathWithin(path, root string) bool {
	// macOS 的 /tmp 是 /private/tmp 的符号链接：两边都要先解析真实路径再比，
	// 否则 workspace_root 明明包含它，前缀比较却判成「跑到 root 之外」。
	rel, err := filepath.Rel(hubCanonicalPath(root), hubCanonicalPath(path))
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

// hubRealWriteReport 写报告与事件流；失败也写（defer 调用）。
// skip=true 时写 <report>.skip.json + events.skip.jsonl（不覆盖真实报告，N4）。
// api_key 用字符串替换抹掉；报告本身不含 base_url。
func hubRealWriteReport(t *testing.T, rep *hubRealReport, reportPath string, skip bool, buf *syncBuf, key string) {
	if skip {
		reportPath += ".skip.json"
	}
	rep.FinishedAt = time.Now().UTC()
	if rep.StartedAt.IsZero() {
		rep.StartedAt = rep.FinishedAt
	}
	rep.DurationMS = rep.FinishedAt.Sub(rep.StartedAt).Milliseconds()
	if rep.Status == "" {
		rep.Status = "fail"
	}
	// totals
	rep.Totals = hubRealTotals{Scenarios: len(rep.Scenarios)}
	for _, sc := range rep.Scenarios {
		switch sc.Status {
		case "pass":
			rep.Totals.Passed++
		case "skip":
			rep.Totals.Skipped++
		default:
			rep.Totals.Failed++
		}
		rep.Totals.LLMCalls += sc.LLM.Calls
		rep.Totals.InputTokens += sc.LLM.InputTokens
		rep.Totals.OutputTokens += sc.LLM.OutputTokens
		rep.Totals.CostUSD += sc.LLM.CostUSD
	}
	dir := filepath.Dir(reportPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("写报告前建目录失败 %s: %v", dir, err)
		return
	}
	rep.ReportPath = reportPath
	// 没有事件流（门控 skip / ONLY 解析失败）时不写 events 文件：否则会用空文件
	// 覆盖同目录上一次真实运行的 events.jsonl。
	if buf == nil {
		rep.EventsPath = ""
	} else {
		eventsName := "events.jsonl"
		if skip {
			eventsName = "events.skip.jsonl"
		}
		rep.EventsPath = filepath.Join(dir, eventsName)
	}

	reportJSON, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Errorf("报告序列化失败: %v", err)
		return
	}
	reportText := string(reportJSON)
	if key != "" {
		reportText = strings.ReplaceAll(reportText, key, "[REDACTED]")
	}
	if err := os.WriteFile(reportPath, []byte(reportText+"\n"), 0o644); err != nil {
		t.Errorf("写报告失败 %s: %v", reportPath, err)
	}
	if buf != nil {
		events := buf.String()
		if key != "" {
			events = strings.ReplaceAll(events, key, "[REDACTED]")
		}
		if err := os.WriteFile(rep.EventsPath, []byte(events), 0o644); err != nil {
			t.Errorf("写原始事件失败 %s: %v", rep.EventsPath, err)
		}
	}
	t.Logf("[hub-real] report=%s events=%s status=%s scenarios=%d pass=%d fail=%d skip=%d llm_calls=%d cost_usd=%.6f",
		reportPath, rep.EventsPath, rep.Status, rep.Totals.Scenarios, rep.Totals.Passed,
		rep.Totals.Failed, rep.Totals.Skipped, rep.Totals.LLMCalls, rep.Totals.CostUSD)
}

// TestHubRealOnlySelection 无网络自检：HAI_EVAL_HUB_ONLY 的未知名 / 只选 5、6
// 必须在解析阶段失败（不会创建 manager、不会发真实请求）；别名与空项规则也在这里锁定。
func TestHubRealOnlySelection(t *testing.T) {
	invalid := []string{"__none__", "5", "6", "5,6", "0", "1,__none__", ","}
	for _, raw := range invalid {
		if _, err := hubRealParseOnly(raw); err == nil {
			t.Fatalf("hubRealParseOnly(%q) 应当失败，实际成功（会静默跑 0 场景/缺依赖）", raw)
		}
	}
	var legal []string
	for _, def := range hubRealScenarioDefs {
		legal = append(legal, def.name)
	}
	_, err := hubRealParseOnly("__none__")
	if err == nil || !strings.Contains(err.Error(), "合法名") {
		t.Fatalf("未知场景错误应列出合法名，实际: %v", err)
	}
	for _, name := range legal {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("未知场景错误缺少合法名 %s: %v", name, err)
		}
	}
	_, err = hubRealParseOnly("5")
	if err == nil || !strings.Contains(err.Error(), "4_group_mention_all") || !strings.Contains(err.Error(), "场景 4") {
		t.Fatalf("只选 5 的错误应明确依赖场景 4，实际: %v", err)
	}

	if only, err := hubRealParseOnly("   "); err != nil || only != nil {
		t.Fatalf("空 ONLY 应表示全跑（nil），实际 only=%v err=%v", only, err)
	}
	only, err := hubRealParseOnly(" 1 , dm ,, 4,5 ")
	if err != nil {
		t.Fatalf("hubRealParseOnly: %v", err)
	}
	if len(only) != 3 || !only["1_dm"] || !only["4_group_mention_all"] || !only["5_group_mention_one"] {
		t.Fatalf("别名/空项解析 = %v，want 1_dm+4+5", only)
	}
	// 每个场景名的全部别名都能单独选中（5/6 带上依赖 4）。
	for _, def := range hubRealScenarioDefs {
		for _, alias := range append([]string{def.name}, def.aliases...) {
			raw := alias
			if len(def.deps) > 0 {
				raw += "," + def.deps[0]
			}
			got, err := hubRealParseOnly(raw)
			if err != nil || !got[def.name] {
				t.Fatalf("别名 %q 解析失败: only=%v err=%v", raw, got, err)
			}
		}
	}
}

// TestHubRealReportPathValidation 无网络自检：相对报告路径必须在写盘之前被拒绝。
func TestHubRealReportPathValidation(t *testing.T) {
	if err := hubRealValidateReportPath("report.json"); err == nil {
		t.Fatal("相对报告路径必须被拒绝（否则会写进仓库）")
	}
	if err := hubRealValidateReportPath(filepath.Join(t.TempDir(), "report.json")); err != nil {
		t.Fatalf("绝对报告路径被误拒: %v", err)
	}
	if !filepath.IsAbs(hubRealDefaultReport()) {
		t.Fatalf("默认报告路径必须是绝对路径: %q", hubRealDefaultReport())
	}
}
