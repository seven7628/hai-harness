package main

// 场景 8–12：真实 Provider 的 Hub 第二轮端到端（round2）。
//
// 与 base 场景（1–7）共用同一套门控/报告/等待辅助（hub_eval_real_test.go），这里只补
// 本轮要覆盖的六件事里「适合真实模型」的五件：
//
//	8  DM/群上下文与请求互不串：同一智能体两个作用域各记一个哨兵（DM_ONLY_SENT /
//	   GRP_ONLY_SENT），断言①各自落盘的消息不含对方哨兵；②模型请求（真实 StreamRequest
//	   的记录）不含对方哨兵；③hub_agent_prompt_preview 的作用域分叉（dm 无群工具、
//	   group 有名册层）。
//	9  Project 会话的审批/Ask/ToDo 数据源：bash 审批 + todo_add + ask_user 全部发生在
//	   **项目会话**里，Hub 侧只应看到 need / tool_run_end，正文不得变成群或 DM 消息；
//	   断言 tool_run_end.todos 含哨兵、hub_need 形状（origin/target/anchor/task_id）、
//	   resolve 之后该 Run 继续跑到终态。
//	10 DM 多气泡：一次工具调用强制两轮 LLM，断言气泡数 == 非空 llm_end 数、message_id
//	   互异、final 晚于 delta。首轮不带正文时允许 bounded 1 次纠正重发并记 note。
//	11 任务结果回推：agent 用 session_send 派 Project 任务，结果回推到群内发起者；
//	   断言账本 delivered/summary、恰好一张 task_result 卡、发起者新 run 且落一条总结
//	   气泡、旁人无新 run/无路由、重放不产生第二张卡。
//	12 session_send 同批合并：模型一次 session_send 带 command=compact + text + files +
//	   images；断言参数四要素、回执、账本附件、项目侧事件序
//	   agent_start→compress_start→compress_end(error 空, before>0)→llm_end→agent_end
//	   且整批只有一个 agent_start。预热历史由 harness 预置（fake loop），不让模型生成 PNG。
//
// 关键手法：recordingProvider。真实 provider 用 m.provCfg.newProvider 构造，外面包一层
// 只观测的壳（记录 StreamRequest 快照后原样透传）；装进会话 loop 时走生产的
// hubLoopOptions（L1/L2/L4/压缩阈值/工具引擎全部原样），所以记录到的就是模型真正看到
// 的请求，而不是测试自己拼的一份。项目会话（9/11/12）用生产 m.create 装配，不重建
// loop —— 它的证据来自事件流与账本，不需要请求级快照。
//
// 全部证据都由 sync() 从事件/消息/账本重建（derive* 是纯函数，不在 defer 里 Fail）：
// 失败报告也能看出是哪条断言不成立。真实模型不按提示词时做 bounded 1 次纠正并记
// note，绝不静默降级成 PASS。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
)

// ---------- 场景表（8–12） ----------

// hubRealRound2Defs 是本轮新增的五个场景。deps 全空：每个场景自己建智能体/群/工作区，
// 互不依赖（也不依赖 1–7 留下的现场）。
var hubRealRound2Defs = []hubRealScenarioDef{
	{
		name: "8_dm_group_isolation", aliases: []string{"8", "isolation", "dm_group_isolation"},
		prompt: "同一智能体先在 DM 里记 DM_ONLY_SENT，再在群里记 GRP_ONLY_SENT，然后分别在两个作用域里复述自己记住的哨兵。",
		run:    (*hubRealScenario).scenarioDMGroupIsolation,
	},
	{
		name: "9_inline_needs_todos", aliases: []string{"9", "needs", "inline_needs_todos"},
		prompt: "让 Project 会话按顺序调用 bash（需审批）、todo_add、ask_user，最后回复 SC9-DONE；Hub 侧只经 need 与 tool_run_end 观测。",
		run:    (*hubRealScenario).scenarioInlineNeedsTodos,
	},
	{
		name: "10_multi_bubble", aliases: []string{"10", "bubble", "multi_bubble"},
		prompt: "DM 里分两轮：第一轮先输出正文 SC10-BUBBLE-ONE 再调用 bash，第二轮只输出正文 SC10-BUBBLE-TWO。",
		run:    (*hubRealScenario).scenarioMultiBubble,
	},
	{
		name: "11_task_result_push", aliases: []string{"11", "push", "task_result_push"},
		prompt: "群内智能体用 session_send 把任务派给 Project 会话，结果回推到群内发起者（一张 task_result 卡 + 发起者一条总结气泡）。",
		run:    (*hubRealScenario).scenarioTaskResultPush,
	},
	{
		name: "12_session_send_merge", aliases: []string{"12", "merge", "session_send_merge"},
		prompt: "一次 session_send 同时带 command=compact + text + files + images，断言参数四要素、回执、账本附件与项目侧事件序。",
		run:    (*hubRealScenario).scenarioSessionSendMerge,
	},
}

// ---------- 记录型 provider（只观测，不改行为） ----------

// hubRealRecordedRequest 是一次真实模型请求的快照（证据用；不含 key/headers）。
// Prompt 是 isolationPrompt 摊平后的全文：所有角色 + 内容块（含 hidden 快照块）+
// 工具名/参数 —— 隔离断言要看模型真正读到的全部字节。
type hubRealRecordedRequest struct {
	At        time.Time `json:"at"`
	Scope     string    `json:"scope"` // dm | group
	SessionID string    `json:"session_id"`
	AgentID   string    `json:"agent_id"`
	Model     string    `json:"model"`
	Provider  string    `json:"provider"`
	Messages  int       `json:"messages"`
	Tools     int       `json:"tools"`
	Prompt    string    `json:"prompt"`
}

// hubRealRecorder 收集某会话的全部模型请求（按到达顺序）。
type hubRealRecorder struct {
	mu   sync.Mutex
	reqs []hubRealRecordedRequest
}

func (r *hubRealRecorder) add(q hubRealRecordedRequest) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, q)
	r.mu.Unlock()
}

func (r *hubRealRecorder) snapshot() []hubRealRecordedRequest {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]hubRealRecordedRequest, len(r.reqs))
	copy(out, r.reqs)
	return out
}

func (r *hubRealRecorder) count() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *hubRealRecorder) last() (hubRealRecordedRequest, bool) {
	if r == nil {
		return hubRealRecordedRequest{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.reqs) == 0 {
		return hubRealRecordedRequest{}, false
	}
	return r.reqs[len(r.reqs)-1], true
}

// recordingProvider 是真实 provider 的只观测外壳：Stream 之前记一份请求快照，然后
// 原样透传（事件、错误、重试语义完全由内层 provider 决定）。
type recordingProvider struct {
	inner provider.Provider
	sink  *hubRealRecorder
	scope string
	sid   string
	agent string
}

func (p *recordingProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	if p != nil && p.sink != nil && req != nil {
		p.sink.add(hubRealRecordedRequest{
			At: time.Now().UTC(), Scope: p.scope, SessionID: p.sid, AgentID: p.agent,
			Model: req.Model, Provider: req.Provider,
			Messages: len(req.Messages), Tools: len(req.Tools),
			Prompt: isolationPrompt(req),
		})
	}
	if p == nil || p.inner == nil {
		return fmt.Errorf("recordingProvider: inner provider 为空")
	}
	return p.inner.Stream(ctx, req, h)
}

// useRecorder 把「真 provider（m.provCfg.newProvider）+ 记录壳」装进会话 loop。
//
// 走生产的 hubLoopOptions（L1/L2 提示词、名册、L4 记忆、压缩阈值、工具引擎原样），
// 只在末尾追加 agents.WithProvider(记录壳) —— 后面的 Option 覆盖前面的 provider，
// 其余装配逐字不变。返回 recorder 与安装前的 loop（调用方需要时可以还原）。
func (sc *hubRealScenario) useRecorder(bs *bridgeSession, agentID string, scope agenthub.Scope) (*hubRealRecorder, *agents.AgentLoop) {
	sc.t.Helper()
	if bs == nil || bs.s == nil {
		sc.failf("useRecorder: 会话为空（agent=%s scope=%s）", agentID, scope.Kind)
	}
	m := sc.e.m
	loaded, st, err := m.hubLoadAgent(agentID)
	if err != nil {
		sc.failf("useRecorder: 读智能体 %s 失败: %v", agentID, err)
	}
	ws := m.hubRuntime(agentID)
	if ws == nil {
		sc.failf("useRecorder: 智能体 %s 没有运行态（Hub 正在关停？）", agentID)
	}
	spec := m.hubSpecFor(loaded, scope, ws.path)
	if scope.Kind == agenthub.ScopeGroup && scope.GroupID != "" {
		// 与生产 applyHubSessionLoop 同口径：群名册（L2）必须补全，否则记录到的
		// 系统提示词缺 roster，群/DM 的隔离断言就变成「测一份假提示词」。
		if err := m.enrichHubGroupSpec(context.Background(), st, spec, scope.GroupID); err != nil {
			sc.failf("useRecorder: 群 %s 名册补全失败: %v", scope.GroupID, err)
		}
	}
	inner := m.provCfg.newProvider(loaded.Provider)
	if inner == nil {
		sc.failf("useRecorder: newProvider(%s) == nil", loaded.Provider)
	}
	rec := &hubRealRecorder{}
	opts := m.hubLoopOptions(ws, bs.id, spec, bs.hubEngine)
	opts = append(opts, agents.WithProvider(&recordingProvider{
		inner: inner, sink: rec,
		scope: string(scope.Kind), sid: bs.id, agent: agentID,
	}))
	prev := bs.s.Loop()
	bs.s.SetLoop(agents.NewAgentLoop(opts...))
	bs.loop = bs.s.Loop()
	return rec, prev
}

// ---------- 报告证据结构（8–12） ----------

// hubRealIsoSide 一个作用域的落盘/回复证据。
type hubRealIsoSide struct {
	SessionID string `json:"session_id,omitempty"`
	Messages  int    `json:"messages"`
	HasDM     bool   `json:"has_dm_sentinel"`
	HasGroup  bool   `json:"has_group_sentinel"`
	Reply     string `json:"reply,omitempty"`
}

// hubRealIsoRequest 一个作用域真实模型请求的证据（Calls 次请求的并集）。
type hubRealIsoRequest struct {
	Calls     int  `json:"calls"`
	HasDM     bool `json:"has_dm_sentinel"`
	HasGroup  bool `json:"has_group_sentinel"`
	PromptLen int  `json:"prompt_len"`
}

type hubRealIsolation struct {
	DM                 hubRealIsoSide    `json:"dm"`
	Group              hubRealIsoSide    `json:"group"`
	DMRequest          hubRealIsoRequest `json:"dm_request"`
	GroupRequest       hubRealIsoRequest `json:"group_request"`
	PreviewDMScope     string            `json:"preview_dm_scope,omitempty"`
	PreviewGroupScope  string            `json:"preview_group_scope,omitempty"`
	PreviewDMLayers    []string          `json:"preview_dm_layers,omitempty"`
	PreviewGroupLayers []string          `json:"preview_group_layers,omitempty"`
	Attempts           int               `json:"attempts"`
	Pass               bool              `json:"pass"`
	Note               string            `json:"note,omitempty"`
}

// hubRealNeedEvidence 一条 need 的关键形状（不复制正文全文，只留剪裁后的 body）。
type hubRealNeedEvidence struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Status          string `json:"status"`
	Anchor          string `json:"anchor,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	Workspace       string `json:"workspace,omitempty"`
	TaskID          string `json:"task_id,omitempty"`
	AgentID         string `json:"agent_id,omitempty"`
	GroupID         string `json:"group_id,omitempty"`
	OriginAgentID   string `json:"origin_agent_id,omitempty"`
	OriginAgentName string `json:"origin_agent_name,omitempty"`
	OriginKind      string `json:"origin_kind,omitempty"`
	TargetKind      string `json:"target_kind,omitempty"`
	TargetWorkspace string `json:"target_workspace,omitempty"`
	TargetSessionID string `json:"target_session_id,omitempty"`
	RunID           string `json:"run_id,omitempty"`
	Depth           int    `json:"depth,omitempty"`
	Title           string `json:"title,omitempty"`
	Body            string `json:"body,omitempty"`
}

type hubRealNeeds struct {
	ProjectSessionID string               `json:"project_session_id,omitempty"`
	ProjectWorkspace string               `json:"project_workspace,omitempty"`
	TaskID           string               `json:"task_id,omitempty"`
	Approval         *hubRealNeedEvidence `json:"approval,omitempty"`
	Question         *hubRealNeedEvidence `json:"question,omitempty"`
	ApprovalStatus   string               `json:"approval_status,omitempty"`
	QuestionStatus   string               `json:"question_status,omitempty"`
	AskQuestionID    string               `json:"ask_question_id,omitempty"`
	TodoRunEnd       *hubRealToolCall     `json:"todo_run_end,omitempty"`
	TodoHasSentinel  bool                 `json:"todo_has_sentinel"`
	AskRunEnd        *hubRealToolCall     `json:"ask_run_end,omitempty"`
	AskAnswerEchoed  bool                 `json:"ask_answer_echoed"`
	BashRunEnd       *hubRealToolCall     `json:"bash_run_end,omitempty"`
	ProjectAgentEnd  string               `json:"project_agent_end,omitempty"`
	ProjectFinalText string               `json:"project_final_text,omitempty"`
	ProjectDone      bool                 `json:"project_done"`
	GroupAgentLeak   []string             `json:"group_agent_leak,omitempty"`
	DMAgentLeak      []string             `json:"dm_agent_leak,omitempty"`
	Pass             bool                 `json:"pass"`
	Note             string               `json:"note,omitempty"`
}

// hubRealBubble 一条 DM 气泡（= 一轮非空 LLM 正文）。
type hubRealBubble struct {
	Seq              int64   `json:"seq"`
	MessageID        string  `json:"message_id"`
	Text             string  `json:"text"`
	DeltaEvents      int     `json:"delta_events"`
	DeltaIndexes     []int64 `json:"delta_indexes,omitempty"`
	FinalSeen        bool    `json:"final_seen"`
	DeltaBeforeFinal bool    `json:"delta_before_final"`

	// firstDelta/hasDeltaAt 是 derive 期记的事件下标（final 与 delta 的先后判据），
	// 不进报告（报告里只有布尔结论）。
	firstDelta int
	hasDeltaAt bool
}

type hubRealBubbles struct {
	SessionID       string          `json:"session_id,omitempty"`
	AgentID         string          `json:"agent_id,omitempty"`
	Attempts        int             `json:"attempts"`
	TotalLLMEnds    int             `json:"total_llm_ends"`
	NonEmptyLLMEnds int             `json:"non_empty_llm_ends"`
	Bubbles         []hubRealBubble `json:"bubbles,omitempty"`
	DistinctIDs     bool            `json:"distinct_ids"`
	ToolRunEnds     int             `json:"tool_run_ends"`
	Pass            bool            `json:"pass"`
	Note            string          `json:"note,omitempty"`
}

type hubRealPush struct {
	TaskID                 string `json:"task_id,omitempty"`
	GroupID                string `json:"group_id,omitempty"`
	Status                 string `json:"status,omitempty"`
	Delivered              bool   `json:"delivered"`
	Summary                string `json:"summary,omitempty"`
	Cards                  int    `json:"cards"`
	CardSeq                int64  `json:"card_seq,omitempty"`
	CardSender             string `json:"card_sender,omitempty"`
	CardText               string `json:"card_text,omitempty"`
	OriginNewRun           bool   `json:"origin_new_run"`
	OriginAgentEnds        int    `json:"origin_agent_ends"`
	OriginBubbles          int    `json:"origin_bubbles"`
	OriginBubbleText       string `json:"origin_bubble_text,omitempty"`
	OriginRequestHasResult bool   `json:"origin_request_has_result"`
	BystanderLLMEnds       int    `json:"bystander_llm_ends"`
	BystanderTaskResult    bool   `json:"bystander_task_result"`
	RoutingRounds          int    `json:"routing_rounds"`
	DeliveredEvents        int    `json:"delivered_events"`
	ReplayCards            int    `json:"replay_cards"`
	ReplayDeliveredEvents  int    `json:"replay_delivered_events"`
	Pass                   bool   `json:"pass"`
	Note                   string `json:"note,omitempty"`
}

type hubRealMerge struct {
	GroupSessionID    string   `json:"group_session_id,omitempty"`
	ProjectSessionID  string   `json:"project_session_id,omitempty"`
	ProjectWorkspace  string   `json:"project_workspace,omitempty"`
	TaskID            string   `json:"task_id,omitempty"`
	SessionSendCalls  int      `json:"session_send_calls"`
	ArgsCommand       string   `json:"args_command,omitempty"`
	ArgsTextHasToken  bool     `json:"args_text_has_token"`
	ArgsFiles         []string `json:"args_files,omitempty"`
	ArgsImages        []string `json:"args_images,omitempty"`
	Accepted          bool     `json:"accepted"`
	Queued            bool     `json:"queued"`
	ReceiptName       string   `json:"receipt_name,omitempty"`
	ReceiptDelivered  bool     `json:"receipt_delivered"`
	LedgerCommand     string   `json:"ledger_command,omitempty"`
	LedgerAttachments []string `json:"ledger_attachments,omitempty"`
	EventOrder        []string `json:"event_order,omitempty"`
	AgentStarts       int      `json:"agent_starts"`
	CompressReason    string   `json:"compress_reason,omitempty"`
	CompressError     string   `json:"compress_error,omitempty"`
	CompressBefore    int      `json:"compress_before"`
	CompressAfter     int      `json:"compress_after"`
	CompressSummary   int      `json:"compress_summary_len"`
	LLMEndText        string   `json:"llm_end_text,omitempty"`
	AnswerEchoesToken bool     `json:"answer_echoes_token"`
	WarmupRuns        int      `json:"warmup_runs"`
	Pass              bool     `json:"pass"`
	Note              string   `json:"note,omitempty"`
}

// hubRealRound2State 是 8–12 的原始事实（会话 id / recorder / 账本 id / 事件起点）。
// derive* 只读它 + 事件流 + 消息 + 账本，所以 sync() 在 defer 里也能重建全部证据。
type hubRealRound2State struct {
	Name string

	// 8
	DMAgent            agenthub.AgentID
	GroupAgent         agenthub.AgentID
	GroupID            agenthub.GroupID
	DMSessionID        string
	GroupSessionID     string
	DMRecorder         *hubRealRecorder
	GroupRecorder      *hubRealRecorder
	DMSentinel         string
	GroupSentinel      string
	PreviewDMScope     string
	PreviewGroupScope  string
	PreviewDMLayers    []string
	PreviewGroupLayers []string
	IsoAttempts        int

	// 9
	ProjectAgent  agenthub.AgentID
	ProjectGroup  agenthub.GroupID
	ProjectWS     string
	ProjectSID    string
	ProjectTaskID agenthub.TaskID
	ApprovalToken string
	TodoToken     string
	AskToken      string
	AnswerToken   string
	DoneToken     string
	AskQuestionID string

	// 10
	BubbleAgent    agenthub.AgentID
	BubbleSID      string
	BubbleFrom     int   // 事件起点（bounded 纠正时前移）
	BubbleMinSeq   int64 // 消息起点（同一纠正口径）
	BubbleAttempts int
	BubbleNote     string

	// 11
	PushAgent     agenthub.AgentID
	PushBystander agenthub.AgentID
	PushGroupID   agenthub.GroupID
	PushGroupSID  string
	PushPeerSID   string
	PushTaskID    agenthub.TaskID
	PushRecorder  *hubRealRecorder
	PushCardSeq   int64
	PushEndBase   int // 派发轮收尾时的 agent_end 条数：回推轮必须在此之后新起
	PushReplay    bool

	// 12
	MergeAgent     agenthub.AgentID
	MergeGroupID   agenthub.GroupID
	MergeGroupSID  string
	MergeWS        string
	MergeSID       string
	MergeFile      string
	MergeImage     string
	MergeTaskID    agenthub.TaskID
	MergeEventFrom int
	MergeTextToken string
	MergeWarmups   int
}

func (sc *hubRealScenario) round2State() *hubRealRound2State {
	if sc.round2 == nil {
		sc.round2 = &hubRealRound2State{Name: sc.name}
	}
	return sc.round2
}

// syncRound2 由 sync() 调用：按场景名重建对应的证据块（纯函数，不 Fail）。
func (sc *hubRealScenario) syncRound2() {
	if sc.rep == nil || sc.round2 == nil {
		return
	}
	switch sc.round2.Name {
	case "8_dm_group_isolation":
		sc.rep.Isolation = sc.deriveIsolation()
	case "9_inline_needs_todos":
		sc.rep.Needs = sc.deriveNeeds()
	case "10_multi_bubble":
		sc.rep.Bubbles = sc.deriveBubbles()
	case "11_task_result_push":
		sc.rep.Push = sc.derivePush()
	case "12_session_send_merge":
		sc.rep.Merge = sc.deriveMerge()
	}
}

// ---------- 通用小工具 ----------

// eventsFrom 取本场景第 i 条之后的事件（i 由 len(sc.events()) 在某个时间点捕获）。
func (sc *hubRealScenario) eventsFrom(i int) []map[string]any {
	evs := sc.events()
	if i <= 0 {
		return evs
	}
	if i >= len(evs) {
		return nil
	}
	return evs[i:]
}

// dmMessagesOf 读某智能体的 DM 索引（读失败返回 nil：derive* 必须是无害的）。
func (sc *hubRealScenario) dmMessagesOf(agentID agenthub.AgentID) []agenthub.Message {
	if agentID == "" || sc.e == nil || sc.e.m == nil {
		return nil
	}
	st, err := sc.e.m.hubStore()
	if err != nil || st == nil {
		return nil
	}
	msgs, err := st.ReadDirectMessages(context.Background(), agentID, 0, 500)
	if err != nil {
		return nil
	}
	return msgs
}

// groupMessagesOf 读某群日志（读失败返回 nil）。
func (sc *hubRealScenario) groupMessagesOf(gid agenthub.GroupID) []agenthub.Message {
	if gid == "" || sc.e == nil || sc.e.m == nil {
		return nil
	}
	st, err := sc.e.m.hubStore()
	if err != nil || st == nil {
		return nil
	}
	msgs, err := st.ReadMessages(context.Background(), gid, 0, 500)
	if err != nil {
		return nil
	}
	return msgs
}

// needsOf 读落库的全部 need（空过滤 = 全部；读失败返回 nil）。
func (sc *hubRealScenario) needsOf() []*agenthub.Need {
	if sc.e == nil || sc.e.m == nil {
		return nil
	}
	st, err := sc.e.m.hubStore()
	if err != nil || st == nil {
		return nil
	}
	needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{})
	if err != nil {
		return nil
	}
	return needs
}

// toolEndsOf 取某会话某工具的全部 tool_run_end 事件（本场景内）。
func (sc *hubRealScenario) toolEndsOf(sessionID, name string) []map[string]any {
	var out []map[string]any
	for _, ev := range sc.events() {
		if str(ev, "event_type") != "tool_run_end" || str(ev, "name") != name {
			continue
		}
		if sessionID != "" && str(ev, "session_id") != sessionID {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// agentEndCount 本场景内某会话已到达的 agent_end 条数（同一会话多轮时做基线）。
func (sc *hubRealScenario) agentEndCount(sessionID string) int {
	n := 0
	for _, ev := range sc.events() {
		if str(ev, "event_type") == "agent_end" && str(ev, "session_id") == sessionID {
			if rid := str(ev, "run_id"); rid != "" && sc.priorRuns[rid] {
				continue
			}
			n++
		}
	}
	return n
}

// waitReply 等一条符合预期的回复；agent_end 已到但消息还没落账时给 2s 宽限
// （AgentEnd 处理与消息落账是紧邻两步，不保证先后），之后判失败并带诊断。
func (sc *hubRealScenario) waitReply(sessionID string, endBase int, what string, fetch func() []agenthub.Message, pred func(agenthub.Message) bool) agenthub.Message {
	sc.t.Helper()
	var got agenthub.Message
	var endAt time.Time
	sc.waitFor(hubRealTimeout, what, func() (bool, string) {
		for _, m := range fetch() {
			if pred(m) {
				got = m
				return true, ""
			}
		}
		if sc.agentEndCount(sessionID) > endBase {
			if endAt.IsZero() {
				endAt = time.Now()
			}
			if time.Since(endAt) > 2*time.Second {
				return false, what + "：agent_end 已到 2s 仍没有符合预期的消息"
			}
		}
		return false, ""
	})
	return got
}

// waitReplySoft 与 waitReply 同形，但 agent_end 之后**不判失败**：返回零值消息，
// 让调用方决定是否做 bounded 1 次纠正（真实模型不按提示词时不能直接判死）。
func (sc *hubRealScenario) waitReplySoft(sessionID string, endBase int, what string, fetch func() []agenthub.Message, pred func(agenthub.Message) bool) agenthub.Message {
	sc.t.Helper()
	var got agenthub.Message
	var endAt time.Time
	sc.waitFor(hubRealTimeout, what, func() (bool, string) {
		for _, m := range fetch() {
			if pred(m) {
				got = m
				return true, ""
			}
		}
		if sc.agentEndCount(sessionID) > endBase {
			if endAt.IsZero() {
				endAt = time.Now()
			}
			if time.Since(endAt) > 2*time.Second {
				return true, ""
			}
		}
		return false, ""
	})
	return got
}

// waitRunIdle 等某会话本轮收尾（新 agent_end + 不在跑），再给 300ms 让落账跟上。
func (sc *hubRealScenario) waitRunIdle(bs *bridgeSession, sessionID string, endBase int, what string) {
	sc.t.Helper()
	sc.waitFor(hubRealTimeout, what, func() (bool, string) {
		if sc.agentEndCount(sessionID) <= endBase {
			return false, ""
		}
		if bs != nil && bs.s != nil && bs.s.IsRunning() {
			return false, ""
		}
		return true, ""
	})
	// hub_message / DM 索引落账与 agent_end 同一条处理链，收尾后再让出一次调度。
	time.Sleep(300 * time.Millisecond)
}

// tempProjectRoot 在测试临时 HOME 下建一个「项目根」（单段目录名，无分隔符）。
func (sc *hubRealScenario) tempProjectRoot(prefix string) string {
	sc.t.Helper()
	tempHome, err := os.UserHomeDir()
	if err != nil {
		sc.failf("解析临时 HOME 失败: %v", err)
	}
	root := filepath.Join(tempHome, fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano()%1e9))
	if err := os.MkdirAll(root, 0o755); err != nil {
		sc.failf("建项目根失败（%s）: %v", root, err)
	}
	return root
}

// newProjectSession 用生产 m.create 装配一个项目会话（真 provider/model），并登记工作区。
// 项目会话的 loop 不重建：工具引擎（bash/todo_add/ask_user）、模式、压缩器都是生产装配。
func (sc *hubRealScenario) newProjectSession(root, name string) (string, string, *bridgeSession) {
	sc.t.Helper()
	m := sc.e.m
	ws := filepath.Join(root, name)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		sc.failf("建项目目录失败（%s）: %v", ws, err)
	}
	if m.wsReg == nil {
		sc.failf("工作区登记表未初始化（TestHubRealE2E 应当已建）")
	}
	if _, err := m.wsReg.add(ws); err != nil {
		sc.failf("登记工作区 %s 失败: %v", ws, err)
	}
	sid := "s-" + name
	bs, err := m.create(ws, sid, "")
	if err != nil {
		sc.failf("装配项目会话 %s/%s 失败: %v", ws, sid, err)
	}
	return ws, sid, bs
}

// groupReplyFrom 取群内某智能体最后一条 text 消息。
func groupReplyFrom(msgs []agenthub.Message, agentID agenthub.AgentID) (agenthub.Message, bool) {
	var out agenthub.Message
	found := false
	for _, m := range msgs {
		if m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == agentID && m.Kind == agenthub.KindText {
			out = m
			found = true
		}
	}
	return out, found
}

// dmReplyFrom 取 DM 索引里某智能体最后一条 text 消息。
func dmReplyFrom(msgs []agenthub.Message, agentID agenthub.AgentID) (agenthub.Message, bool) {
	return groupReplyFrom(msgs, agentID)
}

// ---------- 场景 8：DM/群隔离 ----------

// scenarioDMGroupIsolation 覆盖「同一智能体的 DM 与群上下文/请求互不串」。
func (sc *hubRealScenario) scenarioDMGroupIsolation() {
	const (
		dmSentinel  = "DM_ONLY_SENT"
		grpSentinel = "GRP_ONLY_SENT"
	)
	st := sc.round2State()
	st.Name = sc.name
	st.DMSentinel = dmSentinel
	st.GroupSentinel = grpSentinel

	a := sc.createAgent("RealIso", "隔离验证者",
		"你只执行用户明确要求的动作，回复尽量短。不要调用任何工具（不要 bash、不要读写文件、不要记忆工具），只回复文本。用户会让你记住一个哨兵字符串；被问起时只复述那个 token，不要编造、不要解释。",
		map[string]any{"max_auto_turns": 4})
	b := sc.createAgent("RealIsoPeer", "群旁观者",
		"你只执行用户明确要求的动作，回复尽量短；没有被 @ 时不要说话。", nil)
	g := groupWithMembers(sc.t, sc.e.m, sc.e.buf, sc.e.nextID(), "RealIsolationGroup",
		memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	sc.watchGroup(g.ID)
	st.DMAgent, st.GroupAgent, st.GroupID = a.ID, a.ID, g.ID

	// 两个作用域都在第一条消息之前装配好并装上记录器：第一次请求就要被记录。
	bsDM, err := sc.e.m.hubDMSession(string(a.ID))
	if err != nil {
		sc.failf("装配 DM 会话失败: %v", err)
	}
	dmRec, _ := sc.useRecorder(bsDM, string(a.ID), agenthub.Scope{Kind: agenthub.ScopeDM})
	bsGrp, err := sc.e.m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		sc.failf("装配群会话失败: %v", err)
	}
	grpRec, _ := sc.useRecorder(bsGrp, string(a.ID),
		agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	st.DMSessionID, st.GroupSessionID = bsDM.id, bsGrp.id
	st.DMRecorder, st.GroupRecorder = dmRec, grpRec

	// 1) DM 里记住 DM 哨兵。
	dmBase := sc.agentEndCount(bsDM.id)
	sc.sendDM(a.ID, "请记住这个哨兵（只记，不用复述）："+dmSentinel+"。收到只回复 OK。", "real-e2e-iso-dm-1")
	sc.waitReply(bsDM.id, dmBase, "DM 第一轮收尾", func() []agenthub.Message {
		return sc.dmMessagesOf(a.ID)
	}, func(m agenthub.Message) bool {
		return m.Sender.AgentID == a.ID && strings.TrimSpace(m.Text) != ""
	})

	// 2) 群里记住群哨兵（回复里不要出现 @，避免自动接龙）。
	grpBase := sc.agentEndCount(bsGrp.id)
	sc.sendGroup(g.ID, fmt.Sprintf("@%s 请记住这个哨兵（只记，不用复述）：%s。收到只回复 OK。回复正文里不要出现 @ 符号。",
		a.Name, grpSentinel), false)
	sc.waitReply(bsGrp.id, grpBase, "群第一轮收尾", func() []agenthub.Message {
		return sc.groupMessagesOf(g.ID)
	}, func(m agenthub.Message) bool {
		return m.Sender.AgentID == a.ID && strings.TrimSpace(m.Text) != ""
	})

	// 3) 分别在两个作用域里复述：真实模型可能不肯只回 token → bounded 1 次纠正。
	st.IsoAttempts = 1
	dmReply := sc.askIsolationDM(bsDM, a.ID, dmSentinel)
	grpReply := sc.askIsolationGroup(bsGrp, a.ID, g.ID, grpSentinel)

	// 4) 落盘 + 请求 + prompt preview 三条证据（derive* 与报告同源）。
	iso := sc.deriveIsolation()
	iso.PreviewDMScope, iso.PreviewDMLayers = sc.promptPreviewScope(string(a.ID), "")
	iso.PreviewGroupScope, iso.PreviewGroupLayers = sc.promptPreviewScope(string(a.ID), string(g.ID))
	iso.Attempts = st.IsoAttempts
	iso.DM.Reply = hubRealClip(dmReply, 200)
	iso.Group.Reply = hubRealClip(grpReply, 200)
	st.PreviewDMScope, st.PreviewGroupScope = iso.PreviewDMScope, iso.PreviewGroupScope
	st.PreviewDMLayers, st.PreviewGroupLayers = iso.PreviewDMLayers, iso.PreviewGroupLayers
	iso.Pass = isoEvidencePass(iso)

	// 断言：DM 侧不串群、群侧不串 DM（落盘 + 请求）。
	if !iso.DM.HasDM || iso.DM.HasGroup {
		sc.failf("DM 落盘隔离不成立：dm_messages=%d has_dm=%v has_group=%v（群哨兵漏进 DM 或 DM 哨兵没落盘）",
			iso.DM.Messages, iso.DM.HasDM, iso.DM.HasGroup)
	}
	if !iso.Group.HasGroup || iso.Group.HasDM {
		sc.failf("群落盘隔离不成立：group_messages=%d has_group=%v has_dm=%v（DM 哨兵漏进群或群哨兵没落盘）",
			iso.Group.Messages, iso.Group.HasGroup, iso.Group.HasDM)
	}
	if !iso.DMRequest.HasDM || iso.DMRequest.HasGroup {
		sc.failf("DM 模型请求隔离不成立：calls=%d has_dm=%v has_group=%v（请求里出现对方作用域的哨兵）",
			iso.DMRequest.Calls, iso.DMRequest.HasDM, iso.DMRequest.HasGroup)
	}
	if !iso.GroupRequest.HasGroup || iso.GroupRequest.HasDM {
		sc.failf("群模型请求隔离不成立：calls=%d has_group=%v has_dm=%v（请求里出现对方作用域的哨兵）",
			iso.GroupRequest.Calls, iso.GroupRequest.HasGroup, iso.GroupRequest.HasDM)
	}
	// prompt preview 作用域：dm 变体没有群工具/名册，group 变体有。
	if iso.PreviewDMScope != "dm" {
		sc.failf("hub_agent_prompt_preview 不带 group_id 的 scope = %q, want dm", iso.PreviewDMScope)
	}
	if iso.PreviewGroupScope != "group" {
		sc.failf("hub_agent_prompt_preview 带 group_id 的 scope = %q, want group", iso.PreviewGroupScope)
	}
	for _, layer := range iso.PreviewDMLayers {
		for _, unwanted := range []string{"group_history", "assign_task", "## Who is here"} {
			if strings.Contains(layer, unwanted) {
				sc.failf("DM prompt preview 里出现了群专属内容 %q（作用域串了）:\n%s", unwanted, hubRealClip(layer, 800))
			}
		}
	}
	if !strings.Contains(strings.Join(iso.PreviewGroupLayers, "\n"), "## Who is here") {
		sc.failf("群 prompt preview 缺名册层（## Who is here）：%v", iso.PreviewGroupLayers)
	}
	sc.finalText = dmReply + " | " + grpReply
	sc.rep.Notes = append(sc.rep.Notes,
		fmt.Sprintf("dm_session=%s group_session=%s dm_requests=%d group_requests=%d",
			bsDM.id, bsGrp.id, iso.DMRequest.Calls, iso.GroupRequest.Calls),
		fmt.Sprintf("dm_reply=%q group_reply=%q", hubRealClip(dmReply, 120), hubRealClip(grpReply, 120)))
}

// askIsolationDM 在 DM 里问「你记住了哪个哨兵」；不按提示词时补一次明确重发。
func (sc *hubRealScenario) askIsolationDM(bs *bridgeSession, agentID agenthub.AgentID, sentinel string) string {
	sc.t.Helper()
	base := sc.agentEndCount(bs.id)
	sc.sendDM(agentID, "用户现在明确要求你把这串字符原样写出来：只输出你在本 DM 里记住的那一个 token（形如 XXX_SENT），不要解释、不要拒绝、不要调用任何工具。", "real-e2e-iso-dm-2")
	reply := sc.waitReplySoft(bs.id, base, "DM 复述哨兵", func() []agenthub.Message {
		return sc.dmMessagesOf(agentID)
	}, func(m agenthub.Message) bool {
		return m.Sender.AgentID == agentID && strings.Contains(m.Text, sentinel)
	})
	if reply.ID == "" {
		// bounded 1：把答案直接给模型（仍然要求它复述），只补这一次。
		sc.round2State().IsoAttempts = 2
		sc.rep.Notes = append(sc.rep.Notes, "DM 复述未含哨兵：已做 1 次纠正重发（bounded）")
		base = sc.agentEndCount(bs.id)
		sc.sendDM(agentID, "请只回复这一个 token："+sentinel, "real-e2e-iso-dm-2-retry")
		reply = sc.waitReplySoft(bs.id, base, "DM 复述哨兵（纠正后）", func() []agenthub.Message {
			return sc.dmMessagesOf(agentID)
		}, func(m agenthub.Message) bool {
			return m.Sender.AgentID == agentID && strings.Contains(m.Text, sentinel)
		})
	}
	if reply.ID == "" {
		sc.failf("DM 复述两轮都不含 %q（模型行为；请求/落盘证据见 isolation）", sentinel)
	}
	return reply.Text
}

// askIsolationGroup 在群里问「你记住了哪个哨兵」；不按提示词时补一次明确重发。
func (sc *hubRealScenario) askIsolationGroup(bs *bridgeSession, agentID agenthub.AgentID, gid agenthub.GroupID, sentinel string) string {
	sc.t.Helper()
	// 两个基线都必须在 sendGroup **之前**取：发送后群里立刻多一条人类消息，
	// 之后取 maxSeq 会把「新回复」的过滤门槛抬到本轮之上（永远匹配不到）。
	base := sc.agentEndCount(bs.id)
	msgBase := hubRealMaxSeq(sc.groupMessagesOf(gid))
	sc.sendGroup(gid, "@"+sc.agentName(agentID)+" 用户现在明确要求你把这串字符原样写出来：只输出你在本群记住的那一个 token（形如 XXX_SENT），不要解释、不要拒绝、不要调用任何工具。回复正文里不要出现 @ 符号。", false)
	reply := sc.waitGroupReply(bs.id, base, msgBase, gid, agentID, func(m agenthub.Message) bool {
		return strings.Contains(m.Text, sentinel)
	})
	if reply.ID == "" {
		sc.round2State().IsoAttempts = 2
		sc.rep.Notes = append(sc.rep.Notes, "群复述未含哨兵：已做 1 次纠正重发（bounded）")
		base = sc.agentEndCount(bs.id)
		msgBase = hubRealMaxSeq(sc.groupMessagesOf(gid))
		sc.sendGroup(gid, "@"+sc.agentName(agentID)+" 请只回复这一个 token："+sentinel+"（正文里不要出现 @ 符号）", false)
		reply = sc.waitGroupReply(bs.id, base, msgBase, gid, agentID, func(m agenthub.Message) bool {
			return strings.Contains(m.Text, sentinel)
		})
	}
	if reply.ID == "" {
		sc.failf("群复述两轮都不含 %q（模型行为；请求/落盘证据见 isolation）", sentinel)
	}
	return reply.Text
}

// waitGroupReply 等群内某智能体的下一条符合预期的 text 消息（等的是「新的」那条：
// 通过调用方在 sendGroup **之前**取到的 seq 基线过滤，避免命中上一轮回复）。
// 走 waitReplySoft：模型不按提示词时返回零值，让调用方走 bounded 1 次纠正分支。
func (sc *hubRealScenario) waitGroupReply(sessionID string, endBase int, msgBase int64, gid agenthub.GroupID, agentID agenthub.AgentID, pred func(agenthub.Message) bool) agenthub.Message {
	sc.t.Helper()
	base := msgBase
	return sc.waitReplySoft(sessionID, endBase, "群内 "+string(agentID)+" 的新回复", func() []agenthub.Message {
		var out []agenthub.Message
		for _, m := range sc.groupMessagesOf(gid) {
			if m.Seq > base {
				out = append(out, m)
			}
		}
		return out
	}, pred)
}

// agentName 取智能体名字（读失败回落到 id）。
func (sc *hubRealScenario) agentName(agentID agenthub.AgentID) string {
	if p, _, err := sc.e.m.hubLoadAgent(string(agentID)); err == nil && p != nil && p.Name != "" {
		return p.Name
	}
	return string(agentID)
}

// promptPreviewScope 调 hub_agent_prompt_preview 拿作用域与各层文本。
func (sc *hubRealScenario) promptPreviewScope(agentID, groupID string) (string, []string) {
	sc.t.Helper()
	payload := map[string]any{"agent_id": agentID}
	if groupID != "" {
		payload["group_id"] = groupID
	}
	ok, errMsg, data := sc.cmd("hub_agent_prompt_preview", payload)
	if !ok {
		sc.failf("hub_agent_prompt_preview(%s, %s) 失败: %s", agentID, groupID, errMsg)
	}
	var layers []string
	if raw, ok := data["layers"].([]any); ok {
		for _, item := range raw {
			layer, _ := item.(map[string]any)
			if layer != nil {
				layers = append(layers, str(layer, "text"))
			}
		}
	}
	return str(data, "scope"), layers
}

func isoEvidencePass(iso *hubRealIsolation) bool {
	if iso == nil {
		return false
	}
	return iso.DM.HasDM && !iso.DM.HasGroup &&
		iso.Group.HasGroup && !iso.Group.HasDM &&
		iso.DMRequest.HasDM && !iso.DMRequest.HasGroup &&
		iso.GroupRequest.HasGroup && !iso.GroupRequest.HasDM
}

// deriveIsolation 从落盘消息 + recorder 快照重建隔离证据。
func (sc *hubRealScenario) deriveIsolation() *hubRealIsolation {
	st := sc.round2State()
	iso := &hubRealIsolation{Attempts: st.IsoAttempts}
	iso.DM.SessionID, iso.Group.SessionID = st.DMSessionID, st.GroupSessionID
	iso.PreviewDMScope, iso.PreviewDMLayers = st.PreviewDMScope, st.PreviewDMLayers
	iso.PreviewGroupScope, iso.PreviewGroupLayers = st.PreviewGroupScope, st.PreviewGroupLayers

	dmMsgs := sc.dmMessagesOf(st.DMAgent)
	iso.DM.Messages = len(dmMsgs)
	for _, m := range dmMsgs {
		if strings.Contains(m.Text, st.DMSentinel) {
			iso.DM.HasDM = true
		}
		if strings.Contains(m.Text, st.GroupSentinel) {
			iso.DM.HasGroup = true
		}
	}
	grpMsgs := sc.groupMessagesOf(st.GroupID)
	iso.Group.Messages = len(grpMsgs)
	for _, m := range grpMsgs {
		if strings.Contains(m.Text, st.GroupSentinel) {
			iso.Group.HasGroup = true
		}
		if strings.Contains(m.Text, st.DMSentinel) {
			iso.Group.HasDM = true
		}
	}
	for _, req := range st.DMRecorder.snapshot() {
		iso.DMRequest.Calls++
		if len(req.Prompt) > iso.DMRequest.PromptLen {
			iso.DMRequest.PromptLen = len(req.Prompt)
		}
		if strings.Contains(req.Prompt, st.DMSentinel) {
			iso.DMRequest.HasDM = true
		}
		if strings.Contains(req.Prompt, st.GroupSentinel) {
			iso.DMRequest.HasGroup = true
		}
	}
	for _, req := range st.GroupRecorder.snapshot() {
		iso.GroupRequest.Calls++
		if len(req.Prompt) > iso.GroupRequest.PromptLen {
			iso.GroupRequest.PromptLen = len(req.Prompt)
		}
		if strings.Contains(req.Prompt, st.GroupSentinel) {
			iso.GroupRequest.HasGroup = true
		}
		if strings.Contains(req.Prompt, st.DMSentinel) {
			iso.GroupRequest.HasDM = true
		}
	}
	iso.Pass = isoEvidencePass(iso)
	return iso
}

// ---------- 场景 9：Project 会话内联 need / todo ----------

// scenarioInlineNeedsTodos 覆盖「Project 会话的审批/Ask/ToDo 只走 need 与事件通道」。
func (sc *hubRealScenario) scenarioInlineNeedsTodos() {
	const (
		approvalToken = "SC9-APPROVAL-SENTINEL"
		todoToken     = "SC9-TODO-SENTINEL"
		askToken      = "SC9-ASK-SENTINEL"
		answerToken   = "SC9-ANSWER-SENTINEL"
		doneToken     = "SC9-DONE"
	)
	st := sc.round2State()
	st.Name = sc.name
	st.ApprovalToken, st.TodoToken, st.AskToken = approvalToken, todoToken, askToken
	st.AnswerToken, st.DoneToken = answerToken, doneToken

	root := sc.tempProjectRoot("hub-eval-inline-roots")
	ws, sid, target := sc.newProjectSession(root, "proj")
	st.ProjectWS, st.ProjectSID = ws, sid
	// 审批必须真的被要求：项目会话切 hitl（与场景 3 同口径，快照语义：下一次 Run 生效）。
	mode := parseMode("hitl")
	target.mode = mode
	target.s.SetMode(mode)

	a := sc.createAgent("RealNeeds", "内联观测者",
		"严格按用户要求调用工具；把用户给的任务正文原样作为 session_send 的 text 参数传递，不要改写、不要自己执行任务。回复简短，不要复述任务正文，不要出现 @ 符号。",
		map[string]any{"can_dispatch": true, "workspace_roots": []string{root}, "max_auto_turns": 6})
	g := groupWithMembers(sc.t, sc.e.m, sc.e.buf, sc.e.nextID(), "RealNeedsGroup", memberRef{a.ID, "owner"})
	sc.watchGroup(g.ID)
	st.ProjectAgent, st.ProjectGroup = a.ID, g.ID
	hubSID := "hub." + string(a.ID) + ".g." + string(g.ID)

	taskText := fmt.Sprintf("请严格按顺序完成三步，不要跳步、不要合并：\n"+
		"1) 调用 bash 工具一次，command 必须是 `printf %s`；\n"+
		"2) 调用 todo_add 工具一次，title 必须是 `%s`；\n"+
		"3) 调用 ask_user 工具一次，questions 只给一条，question 必须是 `%s`（不要 options）；\n"+
		"最后只回复：%s", approvalToken, todoToken, askToken, doneToken)
	prompt := fmt.Sprintf("@%s 请调用 session_send **一次**，把下面这段文本原样作为 text 参数派给项目会话：\n"+
		"workspace=%q\nsession_id=%q\n---\n%s\n---\n"+
		"派发成功后只回复 task_id，不要在群里复述任务正文，不要出现 @ 符号，不要自己执行这些步骤。",
		a.Name, ws, sid, taskText)
	sc.rep.Prompt = prompt
	sc.sendGroup(g.ID, prompt, false)

	send := sc.waitToolEnd(hubSID, "session_send", func(ev map[string]any) (bool, string) {
		args := str(ev, "arguments")
		if !strings.Contains(args, ws) || !strings.Contains(args, sid) {
			return false, ""
		}
		if !strings.Contains(args, approvalToken) || !strings.Contains(args, doneToken) {
			// 模型改写了正文：哨兵没派过去，后续断言就没有意义，直接判失败。
			return false, "session_send 的 text 没有原样带上任务正文（缺哨兵）: " + hubRealClip(args, 800)
		}
		return true, ""
	})
	var sendRes struct {
		TaskID    string `json:"task_id"`
		Accepted  bool   `json:"accepted"`
		Queued    bool   `json:"queued"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(str(send, "result")), &sendRes); err != nil || sendRes.TaskID == "" {
		sc.failf("session_send 结果不可解析: %s (err=%v)", hubRealClip(str(send, "result"), 1000), err)
	}
	st.ProjectTaskID = agenthub.TaskID(sendRes.TaskID)
	sc.rep.Notes = append(sc.rep.Notes,
		fmt.Sprintf("session_send task_id=%s accepted=%v queued=%v workspace=%s session=%s",
			sendRes.TaskID, sendRes.Accepted, sendRes.Queued, ws, sid))

	// 1) 审批 need：等它落库（bash 被 hitl 挡住 → recordHubNeed）。
	approval := sc.waitProjectNeed(st.ProjectTaskID, "approval", approvalToken)
	if approval.SessionID != sid || approval.Workspace != ws {
		sc.failf("审批 need 的阻塞会话 = %q/%q, want 项目会话 %q/%q", approval.SessionID, approval.Workspace, sid, ws)
	}
	if approval.Anchor == "" || approval.RunID == "" {
		sc.failf("审批 need 缺 anchor/run_id: %+v", approval)
	}
	if ok, errMsg, _ := sc.cmd("hub_need_resolve", map[string]any{
		"need_id": string(approval.ID), "resolution": "approve",
	}); !ok {
		sc.failf("hub_need_resolve(approve) 失败: %s", errMsg)
	}

	// 2) todo_add：事件里的 todos 快照必须含哨兵（Hub 侧唯一数据源）。
	var todoEnd map[string]any
	sc.waitFor(hubRealTimeout, "todo_add 的 tool_run_end", func() (bool, string) {
		for _, ev := range sc.toolEndsOf(sid, "todo_add") {
			if hubRealBool(ev, "is_error") {
				continue
			}
			raw, _ := json.Marshal(ev["todos"])
			if strings.Contains(string(raw), todoToken) {
				todoEnd = ev
				return true, ""
			}
		}
		if sc.hasNewAgentEnd(sid) {
			return false, "项目会话 agent_end 已到，但没有含 " + todoToken + " 的 todo_add tool_run_end"
		}
		return false, ""
	})

	// 3) 提问 need：等它落库，按 need body 里的单题 id 注入回答。
	question := sc.waitProjectNeed(st.ProjectTaskID, "question", askToken)
	qid := needQuestionID(question.Body)
	if qid == "" {
		sc.failf("提问 need 的 body 里没有单题 id: %s", hubRealClip(question.Body, 600))
	}
	st.AskQuestionID = qid
	if ok, errMsg, _ := sc.cmd("hub_need_resolve", map[string]any{
		"need_id": string(question.ID),
		"answers": []map[string]any{{"question_id": qid, "answer": answerToken}},
	}); !ok {
		sc.failf("hub_need_resolve(answer) 失败: %s", errMsg)
	}

	// 4) 之后 Run 必须继续跑到终态（审批/提问都是阻塞点，不是终点）。
	sc.waitRunIdle(target, sid, 0, "Project 会话在 resolve 之后收尾")

	needs := sc.deriveNeeds()
	if needs.ApprovalStatus != "resolved" || needs.QuestionStatus != "resolved" {
		sc.failf("need resolve 后状态 = approval %q / question %q, want resolved",
			needs.ApprovalStatus, needs.QuestionStatus)
	}
	if !needs.ProjectDone {
		sc.failf("Project 会话没有跑到终态（agent_end 内容=%q）", hubRealClip(needs.ProjectAgentEnd, 400))
	}
	if !needs.AskAnswerEchoed {
		sc.failf("ask_user 的 tool_run_end 里没有回答 %q（resolve 没把回答送到模型）", answerToken)
	}
	// 群/DM 的 **智能体** 消息不得含 Project 侧哨兵（人类 prompt 里有任务正文，不算泄漏）。
	if len(needs.GroupAgentLeak) > 0 || len(needs.DMAgentLeak) > 0 {
		sc.failf("Project 内联正文漏进了 Hub 智能体消息: group=%v dm=%v", needs.GroupAgentLeak, needs.DMAgentLeak)
	}
	if todoEnd == nil {
		sc.failf("todo_add 的 tool_run_end 没抓到（todos 快照缺失）")
	}
	sc.finalText = needs.ProjectFinalText
	sc.rep.Notes = append(sc.rep.Notes,
		fmt.Sprintf("approval_need=%s anchor=%s", needs.Approval.ID, needs.Approval.Anchor),
		fmt.Sprintf("question_need=%s qid=%s", needs.Question.ID, qid),
		fmt.Sprintf("project_final=%q", hubRealClip(needs.ProjectFinalText, 200)))
}

// waitProjectNeed 等某任务的一条指定 kind 的 pending need 落库（真实模型的审批/提问
// 事件是异步的；hub_need_list 是前端同一数据源，这里直接走它，顺带锁住命令路径）。
func (sc *hubRealScenario) waitProjectNeed(taskID agenthub.TaskID, kind, sentinel string) *agenthub.Need {
	sc.t.Helper()
	var found *agenthub.Need
	sc.waitFor(hubRealTimeout, "落库的 "+kind+" need", func() (bool, string) {
		for _, n := range sc.needsOf() {
			if n.Kind != kind || n.TaskID != taskID {
				continue
			}
			if !strings.Contains(n.Body, sentinel) {
				continue
			}
			found = n
			return true, ""
		}
		if sc.hasNewAgentEnd(sc.round2State().ProjectSID) {
			// agent_end 已到：可能这条 need 根本没产生（模型没调对应工具）。
			return false, "项目会话 agent_end 已到，但没有落库的 " + kind + " need（模型没调用对应工具？）"
		}
		return false, ""
	})
	// 再用 hub_need_list 复核一次（命令路径与落库同源，顺带验证 origin 名字回落）。
	ok, errMsg, data := sc.cmd("hub_need_list", map[string]any{
		"group_id": string(sc.round2State().ProjectGroup),
	})
	if !ok {
		sc.failf("hub_need_list 失败: %s", errMsg)
	}
	var listed struct {
		Needs []*agenthub.Need `json:"needs"`
	}
	raw, _ := json.Marshal(data)
	if err := json.Unmarshal(raw, &listed); err != nil {
		sc.failf("hub_need_list 结果不可解析: %s", hubRealClip(string(raw), 600))
	}
	for _, n := range listed.Needs {
		if n.Kind == kind && n.TaskID == taskID && strings.Contains(n.Body, sentinel) {
			return n
		}
	}
	// 列表默认只看 pending：刚被 resolve 的 need 不会出现（同一份落库数据，直接返回）。
	if found != nil {
		return found
	}
	sc.failf("hub_need_list 里没有刚落库的 %s need（task=%s）", kind, taskID)
	return nil
}

// needQuestionID 从 need body（AskUserQuestion.Questions 的 JSON）取单题 id。
func needQuestionID(body string) string {
	var items []struct {
		ID       string `json:"id"`
		Question string `json:"question"`
	}
	if err := json.Unmarshal([]byte(body), &items); err != nil || len(items) == 0 {
		return ""
	}
	return items[0].ID
}

// deriveNeeds 从落库 need + 事件流 + 消息重建场景 9 的证据。
func (sc *hubRealScenario) deriveNeeds() *hubRealNeeds {
	st := sc.round2State()
	out := &hubRealNeeds{
		ProjectSessionID: st.ProjectSID,
		ProjectWorkspace: st.ProjectWS,
		TaskID:           string(st.ProjectTaskID),
		AskQuestionID:    st.AskQuestionID,
	}
	for _, n := range sc.needsOf() {
		if n.TaskID != st.ProjectTaskID {
			continue
		}
		ev := needEvidence(n)
		switch n.Kind {
		case "approval":
			out.Approval = ev
			out.ApprovalStatus = n.Status
		case "question":
			out.Question = ev
			out.QuestionStatus = n.Status
		}
	}
	for _, ev := range sc.toolEndsOf(st.ProjectSID, "todo_add") {
		raw, _ := json.Marshal(ev["todos"])
		out.TodoRunEnd = hubRealToolEvidence(ev)
		if strings.Contains(string(raw), st.TodoToken) {
			out.TodoHasSentinel = true
		}
	}
	for _, ev := range sc.toolEndsOf(st.ProjectSID, "ask_user") {
		out.AskRunEnd = hubRealToolEvidence(ev)
		if strings.Contains(str(ev, "result"), st.AnswerToken) {
			out.AskAnswerEchoed = true
		}
	}
	for _, ev := range sc.toolEndsOf(st.ProjectSID, "bash") {
		if strings.Contains(str(ev, "arguments"), st.ApprovalToken) {
			out.BashRunEnd = hubRealToolEvidence(ev)
		}
	}
	for _, ev := range sc.events() {
		if str(ev, "event_type") != "agent_end" || str(ev, "session_id") != st.ProjectSID {
			continue
		}
		if rid := str(ev, "run_id"); rid != "" && sc.priorRuns[rid] {
			continue
		}
		out.ProjectAgentEnd = hubRealClip(str(ev, "content"), 4000)
		if strings.Contains(str(ev, "content"), st.DoneToken) {
			out.ProjectDone = true
		}
		out.ProjectFinalText = hubRealClip(str(ev, "content"), 4000)
	}
	tokens := []string{st.ApprovalToken, st.TodoToken, st.AskToken, st.AnswerToken}
	collectLeak := func(msgs []agenthub.Message, agentID agenthub.AgentID) []string {
		var leaks []string
		for _, m := range msgs {
			if m.Sender.Kind != agenthub.SenderAgent || m.Sender.AgentID != agentID {
				continue
			}
			for _, tok := range tokens {
				if tok != "" && strings.Contains(m.Text, tok) {
					leaks = append(leaks, fmt.Sprintf("seq=%d token=%s text=%q", m.Seq, tok, hubRealClip(m.Text, 200)))
				}
			}
		}
		return leaks
	}
	out.GroupAgentLeak = collectLeak(sc.groupMessagesOf(st.ProjectGroup), st.ProjectAgent)
	out.DMAgentLeak = collectLeak(sc.dmMessagesOf(st.ProjectAgent), st.ProjectAgent)
	out.Pass = out.Approval != nil && out.Question != nil && out.TodoHasSentinel &&
		out.AskAnswerEchoed && out.ProjectDone &&
		len(out.GroupAgentLeak) == 0 && len(out.DMAgentLeak) == 0
	return out
}

func needEvidence(n *agenthub.Need) *hubRealNeedEvidence {
	if n == nil {
		return nil
	}
	return &hubRealNeedEvidence{
		ID: string(n.ID), Kind: n.Kind, Status: n.Status, Anchor: n.Anchor,
		SessionID: n.SessionID, Workspace: n.Workspace, TaskID: string(n.TaskID),
		AgentID: string(n.AgentID), GroupID: string(n.GroupID),
		OriginAgentID: string(n.OriginAgentID), OriginAgentName: n.OriginAgentName,
		OriginKind: string(n.OriginKind), TargetKind: string(n.TargetKind),
		TargetWorkspace: n.TargetWorkspace, TargetSessionID: n.TargetSessionID,
		RunID: n.RunID, Depth: n.Depth, Title: n.Title, Body: hubRealClip(n.Body, 1000),
	}
}

// ---------- 场景 10：DM 多气泡 ----------

// scenarioMultiBubble 覆盖「一次工具调用强制两轮 LLM → 两条气泡」。
func (sc *hubRealScenario) scenarioMultiBubble() {
	const (
		round1    = "SC10-BUBBLE-ONE"
		round2    = "SC10-BUBBLE-TWO"
		toolToken = "SC10-TOOL-OK"
	)
	st := sc.round2State()
	st.Name = sc.name

	a := sc.createAgent("RealBubble", "分段回复",
		"严格按用户要求分轮输出正文并调用工具，不要合并成一条回复，不要省略任何一轮正文。",
		map[string]any{"max_auto_turns": 6})
	bsDM, err := sc.e.m.hubDMSession(string(a.ID))
	if err != nil {
		sc.failf("装配 DM 会话失败: %v", err)
	}
	rec, _ := sc.useRecorder(bsDM, string(a.ID), agenthub.Scope{Kind: agenthub.ScopeDM})
	st.BubbleAgent, st.BubbleSID = a.ID, bsDM.id
	_ = rec
	sc.watchDM(a.ID)

	prompt := fmt.Sprintf("分两轮完成，不要合并成一条回复：\n"+
		"第一轮：先输出一行正文 `%s`，然后调用 bash 工具一次，command 必须是 `printf %s`；\n"+
		"第二轮：拿到工具输出后，只输出一行正文 `%s`，不要再调用任何工具。",
		round1, toolToken, round2)
	sc.rep.Prompt = prompt

	st.BubbleFrom = len(sc.events())
	st.BubbleMinSeq = hubRealMaxSeq(sc.dmMessagesOf(a.ID))
	st.BubbleAttempts = 1
	bubbleBase := sc.agentEndCount(bsDM.id)
	sc.sendDM(a.ID, prompt, "real-e2e-bubble-1")

	sc.waitFor(hubRealTimeout, "bash 工具调用（第一轮之后）", func() (bool, string) {
		for _, ev := range sc.toolEndsOf(bsDM.id, "bash") {
			if strings.Contains(str(ev, "arguments"), toolToken) || strings.Contains(str(ev, "result"), toolToken) {
				return true, ""
			}
		}
		if sc.hasNewAgentEnd(bsDM.id) {
			return false, "agent_end 已到，但没有带 " + toolToken + " 的 bash tool_run_end（模型没调用工具，只有一轮 LLM）"
		}
		return false, ""
	})
	sc.waitRunIdle(bsDM, bsDM.id, 0, "DM 两轮收尾")

	// 气泡数 == 非空 llm_end 数；首轮没正文时做 bounded 1 次纠正重发（记 note）。
	der := sc.waitBubbleInvariant(bsDM, a.ID)
	if !bubbleHasToken(der, round1) || !bubbleHasToken(der, round2) {
		st.BubbleAttempts = 2
		st.BubbleNote = "首轮/次轮正文缺失：已做 1 次纠正重发（bounded）"
		sc.rep.Notes = append(sc.rep.Notes, st.BubbleNote)
		st.BubbleFrom = len(sc.events())
		st.BubbleMinSeq = hubRealMaxSeq(sc.dmMessagesOf(a.ID))
		correct := fmt.Sprintf("上一轮没有按要求分两轮输出正文。请重做一次，仍然分两轮：\n"+
			"第一轮：先输出一行正文 `%s`，然后调用 bash 工具一次，command 必须是 `printf %s`；\n"+
			"第二轮：拿到工具输出后，只输出一行正文 `%s`，不要再调用任何工具。",
			round1, toolToken, round2)
		sc.sendDM(a.ID, correct, "real-e2e-bubble-1-retry")
		sc.waitRunIdle(bsDM, bsDM.id, bubbleBase, "DM 纠正轮收尾")
		der = sc.waitBubbleInvariant(bsDM, a.ID)
	}

	// 硬不变量：气泡数必须等于非空 llm_end 数（bounded 纠正之后仍然不成立就是 FAIL，
	// 不能因为「模型没配合」把这条断言降级成 note）。
	if !der.Pass {
		sc.failf("气泡数 != 非空 llm_end 数：bubbles=%d non_empty_llm_ends=%d total_llm_ends=%d distinct_ids=%v texts=%v",
			len(der.Bubbles), der.NonEmptyLLMEnds, der.TotalLLMEnds, der.DistinctIDs, bubbleTexts(der))
	}
	if !bubbleHasToken(der, round1) || !bubbleHasToken(der, round2) {
		sc.failf("两轮正文没落成两条气泡（bubbles=%d non_empty_llm_ends=%d texts=%v；模型行为，已 bounded 纠正 1 次）",
			len(der.Bubbles), der.NonEmptyLLMEnds, bubbleTexts(der))
	}
	// message_id 互异（前端按 id 替换草稿：复用同一个 id 会把两条并成一条）。
	seen := map[string]bool{}
	for _, b := range der.Bubbles {
		if b.MessageID == "" {
			sc.failf("气泡缺 message_id: %+v", b)
		}
		if seen[b.MessageID] {
			sc.failf("两条气泡共用了同一个 message_id %q", b.MessageID)
		}
		seen[b.MessageID] = true
	}
	// final 必须晚于 delta（前端按 message_id 用 final 替换草稿）。
	for _, b := range der.Bubbles {
		if !b.FinalSeen {
			sc.failf("气泡 %s 没有对应的 hub_message final 事件（seq=%d）", b.MessageID, b.Seq)
		}
		if b.DeltaEvents == 0 {
			sc.failf("气泡 %s 没有任何 hub_message_delta（真实 provider 应当流式；final 无草稿可替换）", b.MessageID)
		}
		if !b.DeltaBeforeFinal {
			sc.failf("气泡 %s 的顺序反了：final 早于 delta", b.MessageID)
		}
		for i, idx := range b.DeltaIndexes {
			if idx != int64(i) {
				sc.failf("气泡 %s 的 delta index[%d] = %d，want 从 0 连续递增: %v",
					b.MessageID, i, idx, b.DeltaIndexes)
			}
		}
	}
	sc.finalText = strings.Join(bubbleTexts(der), " | ")
	sc.rep.Notes = append(sc.rep.Notes,
		fmt.Sprintf("bubbles=%d non_empty_llm_ends=%d total_llm_ends=%d",
			len(der.Bubbles), der.NonEmptyLLMEnds, der.TotalLLMEnds))
}

// waitBubbleInvariant 等「气泡数 == 非空 llm_end 数」稳定下来（真实模型最后一轮落账
// 与 agent_end 紧邻，给一小段有界轮询；超过 10s 不再等，交给断言报错）。
func (sc *hubRealScenario) waitBubbleInvariant(bs *bridgeSession, agentID agenthub.AgentID) *hubRealBubbles {
	sc.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		der := sc.deriveBubbles()
		if der.NonEmptyLLMEnds > 0 && len(der.Bubbles) == der.NonEmptyLLMEnds {
			return der
		}
		if time.Now().After(deadline) || (bs != nil && bs.s != nil && !bs.s.IsRunning() && der.TotalLLMEnds > 0) {
			return der
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func bubbleTexts(der *hubRealBubbles) []string {
	if der == nil {
		return nil
	}
	out := make([]string, 0, len(der.Bubbles))
	for _, b := range der.Bubbles {
		out = append(out, hubRealClip(b.Text, 200))
	}
	return out
}

func bubbleHasToken(der *hubRealBubbles, token string) bool {
	if der == nil {
		return false
	}
	for _, b := range der.Bubbles {
		if strings.Contains(b.Text, token) {
			return true
		}
	}
	return false
}

// deriveBubbles 从事件流（llm_end / hub_message_delta / hub_message）与 DM 索引重建。
func (sc *hubRealScenario) deriveBubbles() *hubRealBubbles {
	st := sc.round2State()
	out := &hubRealBubbles{
		SessionID: st.BubbleSID, AgentID: string(st.BubbleAgent),
		Attempts: st.BubbleAttempts, Note: st.BubbleNote,
	}
	evs := sc.eventsFrom(st.BubbleFrom)
	deltaByID := map[string]*hubRealBubble{}
	finalAt := map[string]int{}
	for i, ev := range evs {
		switch str(ev, "event_type") {
		case "llm_end":
			if str(ev, "session_id") != st.BubbleSID {
				continue
			}
			if rid := str(ev, "run_id"); rid != "" && sc.priorRuns[rid] {
				continue
			}
			out.TotalLLMEnds++
			if strings.TrimSpace(str(ev, "content")) != "" {
				out.NonEmptyLLMEnds++
			}
		case "hub_message_delta":
			id := str(ev, "message_id")
			if id == "" {
				continue
			}
			b := deltaByID[id]
			if b == nil {
				b = &hubRealBubble{MessageID: id}
				deltaByID[id] = b
			}
			b.DeltaEvents++
			if idx, ok := ev["index"].(float64); ok {
				b.DeltaIndexes = append(b.DeltaIndexes, int64(idx))
			}
			if _, seen := b.seqDeltaAt(); !seen {
				b.markDeltaAt(i)
			}
		case "hub_message":
			body, _ := ev["message"].(map[string]any)
			if body == nil {
				continue
			}
			finalAt[str(body, "id")] = i
		case "tool_run_end":
			if str(ev, "session_id") == st.BubbleSID {
				out.ToolRunEnds++
			}
		}
	}
	for _, m := range sc.dmMessagesOf(st.BubbleAgent) {
		if m.Sender.Kind != agenthub.SenderAgent || m.Sender.AgentID != st.BubbleAgent ||
			m.Kind != agenthub.KindText || m.Seq <= st.BubbleMinSeq {
			continue
		}
		b := hubRealBubble{Seq: m.Seq, MessageID: m.ID, Text: hubRealClip(m.Text, 1000)}
		if d := deltaByID[m.ID]; d != nil {
			b.DeltaEvents = d.DeltaEvents
			b.DeltaIndexes = d.DeltaIndexes
		}
		if at, ok := finalAt[m.ID]; ok {
			b.FinalSeen = true
			if d := deltaByID[m.ID]; d != nil {
				b.DeltaBeforeFinal = d.deltaAtIndex() < at
			}
		}
		out.Bubbles = append(out.Bubbles, b)
	}
	out.DistinctIDs = true
	seen := map[string]bool{}
	for _, b := range out.Bubbles {
		if seen[b.MessageID] {
			out.DistinctIDs = false
		}
		seen[b.MessageID] = true
	}
	out.Pass = out.NonEmptyLLMEnds > 0 && len(out.Bubbles) == out.NonEmptyLLMEnds && out.DistinctIDs
	return out
}

// ---------- 场景 11：任务结果回推 ----------

// scenarioTaskResultPush 覆盖「Project 任务结算 → 回推群内发起者」。
func (sc *hubRealScenario) scenarioTaskResultPush() {
	const projectToken = "SC11-PROJECT-OK"
	st := sc.round2State()
	st.Name = sc.name

	root := sc.tempProjectRoot("hub-eval-push-roots")
	ws, sid, _ := sc.newProjectSession(root, "proj")
	st.ProjectWS, st.ProjectSID = ws, sid

	a := sc.createAgent("RealPushOrigin", "派发者",
		"严格按用户要求调用工具；派发成功后简短回复，不要出现 @ 符号，不要自己执行项目任务。",
		map[string]any{"can_dispatch": true, "workspace_roots": []string{root}, "max_auto_turns": 6})
	b := sc.createAgent("RealPushPeer", "旁观者",
		"你只执行用户明确要求的动作，回复简短；没有被 @ 时不要说话。", nil)
	g := groupWithMembers(sc.t, sc.e.m, sc.e.buf, sc.e.nextID(), "RealPushGroup",
		memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	sc.watchGroup(g.ID)
	st.PushAgent, st.PushBystander, st.PushGroupID = a.ID, b.ID, g.ID

	bsA := mustGroupSession(sc.t, sc.e.m, string(a.ID), string(g.ID))
	rec, _ := sc.useRecorder(bsA, string(a.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	bsB := mustGroupSession(sc.t, sc.e.m, string(b.ID), string(g.ID))
	st.PushGroupSID, st.PushPeerSID = bsA.id, bsB.id
	st.PushRecorder = rec

	prompt := fmt.Sprintf("@%s 请调用 session_send **一次**，把下面的任务派给项目会话：\n"+
		"workspace=%q\nsession_id=%q\ntext=%q\n"+
		"派发成功后只回复 task_id；不要出现 @ 符号，不要自己执行项目任务。",
		a.Name, ws, sid, "只回复 "+projectToken)
	sc.rep.Prompt = prompt
	sc.sendGroup(g.ID, prompt, false)

	send := sc.waitToolEnd(bsA.id, "session_send", func(ev map[string]any) (bool, string) {
		args := str(ev, "arguments")
		return strings.Contains(args, ws) && strings.Contains(args, sid), ""
	})
	var sendRes struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(str(send, "result")), &sendRes); err != nil || sendRes.TaskID == "" {
		sc.failf("session_send 结果不可解析: %s (err=%v)", hubRealClip(str(send, "result"), 1000), err)
	}
	taskID := agenthub.TaskID(sendRes.TaskID)
	st.PushTaskID = taskID

	// 派发轮先收尾，再取「回推前」的 agent_end 基线：否则「发起者新 run」会被
	// 派发轮自己那个 agent_end 平凡满足（等于没断言）。
	sc.waitRunIdle(bsA, bsA.id, 0, "发起者派发轮收尾")
	st.PushEndBase = sc.agentEndCount(bsA.id)

	// 结算 + 回推（delivered = MarkNotified 已落账，回推已完成）。
	sc.waitFor(hubRealTimeout, "Project 任务结算并回推完成", func() (bool, string) {
		task := p3bLoadTask(sc.t, sc.e.m, taskID)
		if task.Status == agenthub.TaskDelivered {
			return true, ""
		}
		return false, ""
	})
	waitResultNotifyDrained(sc.t, sc.e.m)
	sc.waitRunIdle(bsA, bsA.id, 1, "发起者总结轮收尾")

	push := sc.derivePush()
	// 1) 账本：delivered + 项目会话摘要（真被模型回答，不是空摘要）。
	if !push.Delivered {
		sc.failf("账本状态 = %q, want delivered", push.Status)
	}
	if !strings.Contains(push.Summary, projectToken) {
		sc.failf("账本 summary 缺项目哨兵 %q: %q", projectToken, hubRealClip(push.Summary, 600))
	}
	// 2) 群日志：恰好一张 task_result 卡，署名发起者，正文含摘要。
	if push.Cards != 1 {
		sc.failf("群内 task_result 卡 = %d, want 1（msgs 见报告 messages）", push.Cards)
	}
	if push.CardSender != string(a.ID) {
		sc.failf("结果卡署名 = %q, want 发起者 %q", push.CardSender, a.ID)
	}
	if !strings.Contains(push.CardText, projectToken) {
		sc.failf("结果卡正文缺项目摘要: %q", hubRealClip(push.CardText, 600))
	}
	// 3) 发起者：新 run + 一条总结气泡（结果卡之后），且模型请求里真的有结果正文。
	if !push.OriginNewRun || push.OriginAgentEnds == 0 {
		sc.failf("发起者没有被回推唤醒（agent_end=%d）", push.OriginAgentEnds)
	}
	if push.OriginBubbles != 1 {
		sc.failf("发起者在结果卡之后的总结气泡 = %d, want 1", push.OriginBubbles)
	}
	if !push.OriginRequestHasResult {
		sc.failf("发起者的模型请求里没有项目摘要（task_result 没进上下文）")
	}
	// 4) 旁人：无新 run、无 task_result、无 @ 路由。
	if push.BystanderLLMEnds != 0 {
		sc.failf("旁观者被叫醒 %d 次（结果卡不能路由）", push.BystanderLLMEnds)
	}
	if push.BystanderTaskResult {
		sc.failf("旁观者收到了 task_result：只应唤醒发起者")
	}
	if push.RoutingRounds != 0 {
		sc.failf("结果卡产生了 %d 条 @ 路由（hub_round），want 0", push.RoutingRounds)
	}
	// 5) 重放同一结算 + 再排一次通知：不能写第二张卡、不能推第二份结果。
	task := p3bLoadTask(sc.t, sc.e.m, taskID)
	runID := task.Dispatch.RunID
	if runID == "" {
		sc.failf("结算后账本里没有 run_id：无法复现重放")
	}
	for i := 0; i < 2; i++ {
		sc.e.m.finishHubTask(&bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{taskID}},
			&events.AgentEnd{RunId: runID, Content: push.Summary, FinishReason: core.FinishReasonStop})
	}
	sc.e.m.scheduleTaskResultNotification(taskID)
	waitResultNotifyDrained(sc.t, sc.e.m)
	st.PushReplay = true
	replay := sc.derivePush()
	if replay.ReplayCards != 1 {
		sc.failf("重放后 task_result 卡 = %d, want 1（幂等键失效）", replay.ReplayCards)
	}
	if replay.ReplayDeliveredEvents != 1 {
		sc.failf("重放后 task_result_delivered = %d, want 1（重复回推）", replay.ReplayDeliveredEvents)
	}
	if replay.BystanderLLMEnds != 0 {
		sc.failf("重放后旁观者被叫醒 %d 次", replay.BystanderLLMEnds)
	}
	sc.finalText = push.OriginBubbleText
	sc.rep.Notes = append(sc.rep.Notes,
		fmt.Sprintf("task=%s status=%s cards=%d card_seq=%d origin_bubbles=%d delivered_events=%d",
			taskID, push.Status, push.Cards, push.CardSeq, push.OriginBubbles, push.DeliveredEvents),
		"origin_summary="+hubRealClip(push.OriginBubbleText, 300))
}

// derivePush 从账本 + 群日志 + 事件流重建场景 11 的证据。
func (sc *hubRealScenario) derivePush() *hubRealPush {
	st := sc.round2State()
	out := &hubRealPush{TaskID: string(st.PushTaskID), GroupID: string(st.PushGroupID)}
	if task := sc.ledgerTaskOf(st.PushTaskID); task != nil {
		out.Status = string(task.Status)
		out.Delivered = task.Status == agenthub.TaskDelivered
		if task.Result != nil {
			out.Summary = hubRealClip(task.Result.Summary, 1000)
		}
	}
	msgs := sc.groupMessagesOf(st.PushGroupID)
	for _, m := range msgs {
		if m.Kind != agenthub.KindTaskResult || m.TaskID != st.PushTaskID {
			continue
		}
		out.Cards++
		out.CardSeq = m.Seq
		out.CardSender = string(m.Sender.AgentID)
		out.CardText = hubRealClip(m.Text, 1000)
	}
	for _, m := range msgs {
		if m.Sender.Kind != agenthub.SenderAgent || m.Sender.AgentID != st.PushAgent ||
			m.Kind != agenthub.KindText || m.Seq <= out.CardSeq {
			continue
		}
		out.OriginBubbles++
		out.OriginBubbleText = hubRealClip(m.Text, 1000)
	}
	cardSeq := out.CardSeq
	total := 0
	for _, ev := range sc.events() {
		typ := str(ev, "event_type")
		if typ == "hub_round" && str(ev, "group_id") == string(st.PushGroupID) {
			round, _ := ev["round"].(map[string]any)
			trigger := int64(0)
			if round != nil {
				trigger = int64(hubRealNum(round, "trigger_seq"))
			}
			if trigger == 0 {
				trigger = int64(hubRealNum(ev, "trigger_seq"))
			}
			if cardSeq > 0 && trigger == cardSeq {
				out.RoutingRounds++
			}
		}
		if typ == "task_result_delivered" && str(ev, "task_id") == string(st.PushTaskID) {
			out.DeliveredEvents++
			if !st.PushReplay {
				out.ReplayDeliveredEvents++
			}
		}
		if typ == "agent_end" && str(ev, "session_id") == st.PushGroupSID {
			if rid := str(ev, "run_id"); rid != "" && sc.priorRuns[rid] {
				continue
			}
			total++
		}
		if typ == "llm_end" && str(ev, "session_id") == st.PushPeerSID {
			out.BystanderLLMEnds++
		}
	}
	// 只算派发轮收尾之后新起的 run（基线见 PushEndBase）。
	out.OriginAgentEnds = total - st.PushEndBase
	if out.OriginAgentEnds < 0 {
		out.OriginAgentEnds = 0
	}
	out.OriginNewRun = total > st.PushEndBase
	if st.PushRecorder != nil {
		for _, req := range st.PushRecorder.snapshot() {
			if out.Summary != "" && strings.Contains(req.Prompt, strings.TrimSpace(out.Summary)) {
				out.OriginRequestHasResult = true
			}
		}
	}
	if st.PushPeerSID != "" && st.PushTaskID != "" && sc.e != nil && sc.e.m != nil {
		if bs := sc.e.m.sessionByID(st.PushPeerSID); bs != nil && bs.s != nil {
			out.BystanderTaskResult = bs.s.HasTaskResult(string(st.PushTaskID))
		}
	}
	out.ReplayCards = out.Cards
	if st.PushReplay {
		out.ReplayDeliveredEvents = out.DeliveredEvents
	}
	out.Pass = out.Delivered && out.Cards == 1 && out.OriginNewRun && out.OriginBubbles == 1 &&
		out.BystanderLLMEnds == 0 && out.RoutingRounds == 0
	return out
}

// ---------- 场景 12：session_send 同批合并 ----------

// scenarioSessionSendMerge 覆盖「一次 session_send 带 command=compact + text + files +
// images」的参数/回执/账本/项目侧事件序。预热历史由 harness 预置（fake loop 跑三轮），
// 不让模型生成 PNG。
func (sc *hubRealScenario) scenarioSessionSendMerge() {
	const textToken = "SC12-TEXT-OK"
	st := sc.round2State()
	st.Name = sc.name
	st.MergeTextToken = textToken

	root := sc.tempProjectRoot("hub-eval-merge-roots")
	ws, sid, target := sc.newProjectSession(root, "proj")
	st.MergeWS, st.MergeSID = ws, sid

	// 0) 预热历史：短历史会合法返回 "nothing to compress"，compress_end 就没有
	//    before>0 可断言。用 fake loop 跑三轮真 Run（Ask + wakeRun）产生 user/assistant
	//    历史，然后还原生产 loop（真 provider + 真压缩器），批次的 compact 才走真实路径。
	orig := target.s.Loop()
	warm := &fixedEvalProvider{reply: "SC12-WARMUP：项目背景已记录（第一轮回答）"}
	target.s.SetLoop(agents.NewAgentLoop(
		agents.WithProvider(warm),
		agents.WithModel("sc12-warm-fake"),
		agents.WithMaxIterations(1),
	))
	for i := 0; i < 3; i++ {
		msg := fmt.Sprintf("SC12-WARMUP-%d：项目背景第 %d 段（历史，仅供压缩使用）", i+1, i+1)
		if err := target.s.Ask(context.Background(), core.NewUserMessage(
			core.Content{Type: core.ContentTypeText, Content: msg})); err != nil {
			sc.failf("预热 Ask 失败: %v", err)
		}
		target.wakeRun()
		sc.waitFor(hubRealTimeout, fmt.Sprintf("预热 Run %d 完成", i+1), func() (bool, string) {
			if target.s.IsRunning() {
				return false, ""
			}
			if n := sc.warmupRunCount(sid); n >= i+1 {
				return true, ""
			}
			return false, ""
		})
	}
	st.MergeWarmups = sc.warmupRunCount(sid)
	target.s.SetLoop(orig)
	if target.s.Loop() != orig {
		sc.failf("预热后没还原生产 loop（批次会用 fake provider 跑）")
	}

	// 附件：文本 + 8x8 PNG（远低于内联闸门，data URL 就是文件字节本身）。
	mdPath := filepath.Join(ws, "sc12-notes.md")
	if err := os.WriteFile(mdPath, []byte("# notes\nSC12 同批合并：命令 + 正文 + 文件 + 图片\n"), 0o644); err != nil {
		sc.failf("写附件文本失败: %v", err)
	}
	pngPath := filepath.Join(ws, "sc12-tiny.png")
	writeTestPNG(sc.t, pngPath, 8, 8)
	st.MergeFile, st.MergeImage = mdPath, pngPath

	a := sc.createAgent("RealMerge", "同批派发者",
		"严格按用户要求**一次**调用 session_send，把 workspace/session_id/command/text/files/images 全部放在同一次调用里；不要用 bash，不要分多次调用，不要自己执行项目任务。回复简短，不要出现 @ 符号。",
		map[string]any{"can_dispatch": true, "workspace_roots": []string{root}, "max_auto_turns": 6})
	g := groupWithMembers(sc.t, sc.e.m, sc.e.buf, sc.e.nextID(), "RealMergeGroup", memberRef{a.ID, "owner"})
	sc.watchGroup(g.ID)
	st.MergeAgent, st.MergeGroupID = a.ID, g.ID
	bsA := mustGroupSession(sc.t, sc.e.m, string(a.ID), string(g.ID))
	st.MergeGroupSID = bsA.id

	prompt := fmt.Sprintf("@%s 只调用 session_send **一次**，参数必须同时包含（缺一不可，不要分两次调用）：\n"+
		"workspace=%q\nsession_id=%q\ncommand=\"compact\"\ntext=\"只回复 %s\"\nfiles=[%q]\nimages=[%q]\n"+
		"调用成功后只回复 task_id；不要用 bash，不要自己执行项目任务，不要出现 @ 符号。",
		a.Name, ws, sid, textToken, mdPath, pngPath)
	sc.rep.Prompt = prompt
	st.MergeEventFrom = len(sc.events())
	sc.sendGroup(g.ID, prompt, false)

	send := sc.waitToolEnd(bsA.id, "session_send", func(ev map[string]any) (bool, string) {
		args := str(ev, "arguments")
		if !strings.Contains(args, ws) || !strings.Contains(args, sid) {
			return false, ""
		}
		if !strings.Contains(args, "compact") || !strings.Contains(args, textToken) ||
			!strings.Contains(args, filepath.Base(mdPath)) || !strings.Contains(args, filepath.Base(pngPath)) {
			// 四要素没进同一次调用：直接判失败（分两次调用会退化成场景 6 的旧覆盖）。
			return false, "session_send 参数缺四要素之一（command/text/files/images）: " + hubRealClip(args, 1000)
		}
		return true, ""
	})
	// 一次调用：只统计成功（is_error=false）的 session_send。模型先失败重试一次是
	// 可接受的（记 note），但成功调用必须恰好一次 —— 分两次成功就测不到「同批」。
	success, failed := 0, 0
	for _, ev := range sc.toolEndsOf(bsA.id, "session_send") {
		if hubRealBool(ev, "is_error") {
			failed++
			continue
		}
		success++
	}
	if failed > 0 {
		sc.rep.Notes = append(sc.rep.Notes,
			fmt.Sprintf("session_send 有 %d 次失败调用（已忽略，只对成功调用断言一次）", failed))
	}
	if success != 1 {
		sc.failf("成功的 session_send 调用 = %d 次, want 1（必须一次带齐四要素，不能拆成多次）", success)
	}

	var sendRes struct {
		TaskID   string `json:"task_id"`
		Accepted bool   `json:"accepted"`
		Queued   bool   `json:"queued"`
		Command  *struct {
			Name      string `json:"name"`
			Delivered bool   `json:"delivered"`
		} `json:"command"`
	}
	if err := json.Unmarshal([]byte(str(send, "result")), &sendRes); err != nil || sendRes.TaskID == "" {
		sc.failf("session_send 结果不可解析: %s (err=%v)", hubRealClip(str(send, "result"), 1000), err)
	}
	if !sendRes.Accepted || sendRes.Queued {
		sc.failf("批次没被接收: accepted=%v queued=%v", sendRes.Accepted, sendRes.Queued)
	}
	if sendRes.Command == nil || sendRes.Command.Name != "compact" || !sendRes.Command.Delivered {
		sc.failf("命令回执 = %+v, want compact/delivered", sendRes.Command)
	}
	taskID := agenthub.TaskID(sendRes.TaskID)
	st.MergeTaskID = taskID

	// 账本：command + 两个附件（归一化真实路径/名字/字节数）。
	task := p3bLoadTask(sc.t, sc.e.m, taskID)
	if task.Command != "compact" {
		sc.failf("账本 command = %q, want compact", task.Command)
	}
	sc6AssertAttachment(sc.t, task, agenthub.AttachmentKindFile, mdPath)
	sc6AssertAttachment(sc.t, task, agenthub.AttachmentKindImage, pngPath)
	if raw, err := json.Marshal(task); err == nil && strings.Contains(string(raw), "data:image") {
		sc.failf("账本里出现了图片 data URL（附件只该存引用）: %s", hubRealClip(string(raw), 800))
	}

	// 项目侧事件序 + 结算：压缩在同一个 Run 里先跑，随后正文那轮回答。
	sc.waitFor(hubRealTimeout, "Project 任务结算并回推完成", func() (bool, string) {
		got := p3bLoadTask(sc.t, sc.e.m, taskID)
		if got.Status == agenthub.TaskDelivered || got.Status == agenthub.TaskCompleted {
			return true, ""
		}
		return false, ""
	})
	sc.waitRunIdle(target, sid, st.MergeWarmups, "Project 同批 Run 收尾")
	waitResultNotifyDrained(sc.t, sc.e.m)

	merge := sc.deriveMerge()
	want := []string{"agent_start", "compress_start", "compress_end", "llm_end", "agent_end"}
	if strings.Join(merge.EventOrder, ">") != strings.Join(want, ">") {
		sc.failf("项目侧事件序 = %v, want %v（压缩必须在同一个 Run 里先于正文那轮）",
			merge.EventOrder, want)
	}
	if merge.AgentStarts != 1 {
		sc.failf("同批 agent_start = %d, want 1（压缩后必须在同一 Run 继续，不能重开）", merge.AgentStarts)
	}
	if merge.CompressReason != "manual" {
		sc.failf("compress_end.reason = %q, want manual", merge.CompressReason)
	}
	if merge.CompressError != "" {
		sc.failf("compress_end.error 非空（压缩失败降级）: %q", merge.CompressError)
	}
	if merge.CompressBefore <= 0 {
		sc.failf("compress_end.before = %d, want >0（预热历史没进压缩输入）", merge.CompressBefore)
	}
	if merge.CompressSummary <= 0 {
		sc.failf("compress_end 没有摘要（summary_len=0）")
	}
	if !merge.AnswerEchoesToken {
		sc.failf("正文那轮 llm_end 没有回答 %q（同批正文没被消费）: %q",
			textToken, hubRealClip(merge.LLMEndText, 600))
	}
	sc.finalText = merge.LLMEndText
	sc.rep.Notes = append(sc.rep.Notes,
		fmt.Sprintf("task=%s attachments=%v warmup_runs=%d compress before=%d after=%d summary_len=%d",
			taskID, merge.LedgerAttachments, merge.WarmupRuns,
			merge.CompressBefore, merge.CompressAfter, merge.CompressSummary))
}

// warmupRunCount 统计项目会话已完成的预热 Run 数（按 agent_end 计）。
func (sc *hubRealScenario) warmupRunCount(sid string) int {
	n := 0
	for _, ev := range sc.events() {
		if str(ev, "event_type") == "agent_end" && str(ev, "session_id") == sid {
			n++
		}
	}
	return n
}

// deriveMerge 从事件流 + 账本重建场景 12 的证据。
func (sc *hubRealScenario) deriveMerge() *hubRealMerge {
	st := sc.round2State()
	out := &hubRealMerge{
		GroupSessionID: st.MergeGroupSID, ProjectSessionID: st.MergeSID,
		ProjectWorkspace: st.MergeWS, TaskID: string(st.MergeTaskID),
		WarmupRuns: st.MergeWarmups,
	}
	// 1) session_send 的参数四要素（取最后一次成功调用）。
	for _, ev := range sc.toolEndsOf(st.MergeGroupSID, "session_send") {
		if hubRealBool(ev, "is_error") {
			continue
		}
		out.SessionSendCalls++
		var args struct {
			Command string   `json:"command"`
			Text    string   `json:"text"`
			Files   []string `json:"files"`
			Images  []string `json:"images"`
		}
		if err := json.Unmarshal([]byte(str(ev, "arguments")), &args); err != nil {
			continue
		}
		out.ArgsCommand = args.Command
		out.ArgsTextHasToken = strings.Contains(args.Text, st.MergeTextToken)
		out.ArgsFiles = args.Files
		out.ArgsImages = args.Images
	}
	// 2) 回执
	for _, ev := range sc.toolEndsOf(st.MergeGroupSID, "session_send") {
		if hubRealBool(ev, "is_error") {
			continue
		}
		var res struct {
			Accepted bool   `json:"accepted"`
			Queued   bool   `json:"queued"`
			TaskID   string `json:"task_id"`
			Command  *struct {
				Name      string `json:"name"`
				Delivered bool   `json:"delivered"`
			} `json:"command"`
		}
		if json.Unmarshal([]byte(str(ev, "result")), &res) != nil {
			continue
		}
		out.Accepted, out.Queued = res.Accepted, res.Queued
		if res.Command != nil {
			out.ReceiptName, out.ReceiptDelivered = res.Command.Name, res.Command.Delivered
		}
		if out.TaskID == "" && res.TaskID != "" {
			out.TaskID = res.TaskID
		}
	}
	// 3) 账本：command + 附件引用
	if st.MergeTaskID != "" {
		if task := sc.ledgerTaskOf(st.MergeTaskID); task != nil {
			out.LedgerCommand = task.Command
			for _, att := range task.Attachments {
				out.LedgerAttachments = append(out.LedgerAttachments,
					fmt.Sprintf("%s:%s:%d", att.Kind, att.Path, att.Bytes))
			}
		}
	}
	// 4) 项目侧事件序（只认本场景、本项目会话、本批次之后的事件）
	for _, ev := range sc.eventsFrom(st.MergeEventFrom) {
		if str(ev, "session_id") != st.MergeSID {
			continue
		}
		if rid := str(ev, "run_id"); rid != "" && sc.priorRuns[rid] {
			continue
		}
		switch str(ev, "event_type") {
		case "agent_start":
			out.AgentStarts++
			out.EventOrder = append(out.EventOrder, "agent_start")
		case "compress_start":
			out.EventOrder = append(out.EventOrder, "compress_start")
		case "compress_end":
			out.EventOrder = append(out.EventOrder, "compress_end")
			out.CompressReason = str(ev, "reason")
			out.CompressError = str(ev, "error")
			out.CompressBefore = int(hubRealNum(ev, "before"))
			out.CompressAfter = int(hubRealNum(ev, "after"))
			out.CompressSummary = len(str(ev, "summary"))
		case "llm_end":
			out.EventOrder = append(out.EventOrder, "llm_end")
			out.LLMEndText = hubRealClip(str(ev, "content"), 4000)
			if strings.Contains(str(ev, "content"), st.MergeTextToken) {
				out.AnswerEchoesToken = true
			}
		case "agent_end":
			out.EventOrder = append(out.EventOrder, "agent_end")
		}
	}
	out.Pass = out.SessionSendCalls == 1 && out.ArgsCommand == "compact" && out.ArgsTextHasToken &&
		out.ReceiptName == "compact" && out.ReceiptDelivered && out.LedgerCommand == "compact" &&
		out.AgentStarts == 1 && out.CompressReason == "manual" && out.CompressError == "" &&
		out.CompressBefore > 0 && out.AnswerEchoesToken
	return out
}

// ---------- 报告证据辅助 ----------

// hubRealBubble 的 delta 位置（derive 时记录，避免再扫一遍事件）。
func (b *hubRealBubble) markDeltaAt(i int) {
	if b == nil {
		return
	}
	b.firstDelta = i
}

func (b *hubRealBubble) seqDeltaAt() (int, bool) {
	if b == nil || !b.hasDeltaAt {
		return 0, false
	}
	return b.firstDelta, true
}

func (b *hubRealBubble) deltaAtIndex() int {
	if b == nil {
		return 0
	}
	return b.firstDelta
}

// ledgerTaskOf 读账本任务（纯函数：读失败返回 nil，derive 里不能 Fail）。
func (sc *hubRealScenario) ledgerTaskOf(id agenthub.TaskID) *agenthub.Task {
	if id == "" || sc.e == nil || sc.e.m == nil {
		return nil
	}
	st, err := sc.e.m.hubStore()
	if err != nil || st == nil {
		return nil
	}
	task, err := agenthub.NewLedger(st).Replay(context.Background(), id)
	if err != nil {
		return nil
	}
	return task
}
