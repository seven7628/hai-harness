package main

// llm_replayer.go —— 用真实模型做记忆 Replay 的宿主实现（evolution.Replayer）。
//
// # 为什么默认不接
//
// evolution 包的默认 Replayer 是确定性的结构回放，零 LLM 调用。结构回放能抓住
// 「写进文件后读回来变了」这类错误，但**抓不到事实错误**——一条把旧版本号写成
// 当前版本号的记忆，渲染/解析往返完全一致，结构回放会放行。
//
// 事实错误只能由真的读过仓库的模型来判。这就是本文件的职责，且**必须显式注入**：
// 每次 Replay 都是一次真实模型调用，会花钱、有延迟、会抖动。启动路径上默认关掉它，
// 是为了让「没有接真实验证」这件事是显式配置的结果，而不是默认碰巧没花。
//
// # 判定协议
//
// 让模型只回答一个词（SUPPORTED / CONTRADICTED / UNVERIFIABLE），解析失败一律
// 判 inconclusive 而不是 pass。理由与 evolution 包一致：解析不了不等于内容有问题，
// 但也绝不能当成验证通过——未验证即不可召回。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/evolution"
	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// llmReplayer 用真实 provider 验证一条候选记忆是否与仓库事实相符。
type llmReplayer struct {
	name     string
	prov     provider.Provider
	provName string
	model    string
	// repoRoot 判定时可读的工作区根（工具锚点）；空 = 不给工具。
	repoRoot string
	// tools 只读工具引擎（read_file / grep / glob）。nil = 无工具，模型只能凭题作答。
	tools *tools.Engine
	// budget 单次 Replay 的时限（0 = 不限）。
	budget time.Duration
	// maxRounds 工具轮上限（防"验证"变成"改代码"或死循环）。
	maxRounds int
}

// NewLLMReplayer 构造真实 LLM Replayer。
//
// provName / model 传当前会话用的那一对，理由是判定必须和被验证的 Agent 在同一个
// 模型能力档上——用一个更强的模型判「这条记忆对不对」，判出来的结论对
// space-bunny-free 这类较弱模型的适用性不一定成立。
//
// repoRoot 传工作区根时，Replayer 会得到**只读**工具（read_file/grep/glob），
// 让模型真能去核对仓库事实。给只读面是刻意的：验证者的职责是读，写能力会让
// "验证"有机会变成"顺手改一下"，而那条改动会绕过记忆门禁直接进工作区。
func NewLLMReplayer(prov provider.Provider, provName, model, repoRoot string) evolution.Replayer {
	r := &llmReplayer{
		name: "llm:" + model, prov: prov, provName: provName, model: model,
		repoRoot: repoRoot, maxRounds: 6,
	}
	if repoRoot != "" {
		eng := tools.NewToolEngine()
		for _, t := range readOnlyTools(repoRoot) {
			eng.RegisterTool(context.Background(), t)
		}
		r.tools = eng
	}
	return r
}

// readOnlyTools 取工作区只读工具。写工具（write_file/edit_file/bash）**不**进这条链。
func readOnlyTools(root string) []tools.Tool {
	var out []tools.Tool
	for _, t := range builtin.NewFileTools(root).Tools() {
		switch t.Name() {
		case "read_file", "grep", "glob":
			out = append(out, t)
		}
	}
	return out
}

// runJudgeLoop 跑「模型 + 只读工具」的判定循环，返回模型最后一条文本。
//
// 为什么需要循环而不是一次调用：题干要求模型 "using your tools" 去核对仓库，
// 而一次 Stream 调用只能拿到首轮回复——模型若先调 read_file，那一轮的 content
// 是空的、判定词在工具结果回传之后的下一轮才出现。少跑一轮就会把「还没判定」
// 误当成「UNVERIFIABLE」。
//
// 循环边界：maxRounds 轮工具调用后强制收尾（让模型在**没有**新证据的情况下
// 给出判定，而不是无限探查）。这不是信任模型，是限制一次验证的成本。
func (r *llmReplayer) runJudgeLoop(ctx context.Context, req evolution.ReplayRequest, topic memory.Topic) (string, error) {
	messages := []core.Message{
		core.NewUserMessage(core.Content{
			Type:    core.ContentTypeText,
			Content: fmt.Sprintf(replayPrompt, topic.Title, topic.Body),
		}),
	}
	var schemas []core.ToolSchema
	if r.tools != nil {
		if s, err := r.tools.ToolParams(ctx); err == nil {
			schemas = s
		}
	}
	maxRounds := r.maxRounds
	if maxRounds <= 0 {
		maxRounds = 1
	}

	for round := 0; ; round++ {
		var answer string
		var toolCalls []core.ToolCall
		err := r.prov.Stream(ctx, &provider.StreamRequest{
			Model:    r.model,
			Provider: r.provName,
			Messages: messages,
			Tools:    schemas,
		}, func(e provider.StreamEvent) error {
			switch ev := e.(type) {
			case provider.LLMEndEvent:
				answer, toolCalls = ev.Content, ev.ToolCalls
			}
			return nil
		})
		if err != nil {
			return "", err
		}
		if len(toolCalls) == 0 {
			return answer, nil
		}
		// 工具轮用尽：让模型基于已有证据收尾。下一轮撤掉工具 schema ——
		// 留着它模型会继续探查，"收尾"就成了可选项。
		if round+1 >= maxRounds {
			messages = append(messages,
				core.NewAssistantMessage(nil, "", toolCalls))
			results, err := r.tools.RunBatch(ctx, toolCalls, nil, nil, nil, 0)
			if err != nil {
				return "", err
			}
			messages = append(messages, toolResultsMessage(toolCalls, results)...)
			schemas = nil
			continue
		}
		if r.tools == nil {
			// 没给工具却要了工具调用：回一条说明，避免模型空转。
			// 用 Tool 角色回执（不是 user）——见 toolResultsMessage 的注释。
			messages = append(messages, core.NewAssistantMessage(nil, "", toolCalls))
			messages = append(messages, toolResultsMessage(toolCalls, nil)...)
			continue
		}

		messages = append(messages, core.NewAssistantMessage(nil, "", toolCalls))
		results, err := r.tools.RunBatch(ctx, toolCalls, nil, nil, nil, 0)
		if err != nil {
			return "", err
		}
		if len(results) == 0 {
			results = []core.ToolResult{{Id: toolCalls[0].Id, IsError: true, Result: "tool execution produced no result"}}
		}
		messages = append(messages, toolResultsMessage(toolCalls, results)...)
	}
}

// toolResultsMessage 把工具结果拼成 core.Tool 消息序列。
//
// 消息角色必须是 Tool 而不是 User —— 这是踩过的坑：opencode 网关对
// "assistant 带 tool_calls + 紧随一条 user 消息"的组合直接 400
// （invalid_request_error）。AgentLoop 走的是 NewToolMessageWithImages，本函数
// 与它同构，只是没有图片块（只读工具不产生图片）。
//
// 配对完整性同样重要：漏掉任何一条 call 的结果，上游会因 tool_call_id 找不到
// 对应的 tool 消息而报错，而这在真实 provider 上表现成一个看不出原因的 Replay
// 失败。因此这里按 calls 顺序遍历，而不是按 results 顺序。
func toolResultsMessage(calls []core.ToolCall, results []core.ToolResult) []core.Message {
	byID := make(map[string]core.ToolResult, len(results))
	for _, r := range results {
		byID[r.Id] = r
	}
	out := make([]core.Message, 0, len(calls))
	for _, c := range calls {
		text := "tool produced no result"
		if r, ok := byID[c.Id]; ok {
			text = r.Result
		}
		msg := core.NewToolMessage(c.Id, text)
		msg.ToolName = c.Name
		out = append(out, msg)
	}
	return out
}

func (r *llmReplayer) Name() string { return r.name }

// replayPrompt 是判定提示词。
//
// 三条设计约束：
//   - 要求模型**只**回答一个词：自由文本答案无法确定性解析，而「解析不了就当通过」
//     正是这类系统最危险的失败模式。
//   - 明确给出 UNVERIFIABLE 这条出路：强制二选一会让模型在证据不足时瞎猜
//     SUPPORTED，而瞎猜出来的 pass 会让错误记忆进入可召回索引。
//   - 把「记忆可能比仓库旧」写进题干：仓库演进了但记忆没更新是很常见的真实情形，
//     那是 stale 而非 contradicted，两者在 Phase C 的处置完全不同。
const replayPrompt = `You are verifying a project memory entry against the actual repository state.

Memory title: %s
Memory body:
%s

Check the memory against the repository using your tools. Then answer with EXACTLY ONE of these words and nothing else:

SUPPORTED      - the memory matches the current repository
CONTRADICTED   - the memory states something the repository actively disagrees with
UNVERIFIABLE   - you cannot determine this (insufficient evidence, or the memory describes
                 something that may simply be outdated)

The memory may be older than the repository. If it describes a concrete thing (a command,
a path, a version) that plausibly changed, answer UNVERIFIABLE rather than SUPPORTED.

Answer with one word only.`

func (r *llmReplayer) Replay(ctx context.Context, req evolution.ReplayRequest) (*evolution.ReplayResult, error) {
	if r.prov == nil {
		return nil, errors.New("llm replayer: no provider")
	}
	if req.Candidate == nil {
		return &evolution.ReplayResult{Replayer: r.name, Verdict: evolution.ReplayInconclusive, Reason: "no candidate", At: time.Now().UTC()}, nil
	}
	if r.budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.budget)
		defer cancel()
	}

	topic := req.Candidate.Topic
	// 技能候选不进这条通道：技能的真实验证需要跑技能定义的流程（联合 Replay），
	// 拿"这段指令是否与仓库相符"去判它是没有意义的。
	if req.Candidate.Kind == "skill" {
		return &evolution.ReplayResult{Replayer: r.name, Verdict: evolution.ReplayInconclusive, At: time.Now().UTC(),
			Reason: "skill candidates need joint replay, not fact replay"}, nil
	}

	start := time.Now()
	answer, err := r.runJudgeLoop(ctx, req, topic)
	res := &evolution.ReplayResult{
		Replayer:         r.name,
		At:               start.UTC().Truncate(time.Second),
		DurationMs:       time.Since(start).Milliseconds(),
		UsedRealProvider: true,
	}
	if err != nil {
		// 基础设施错误不是内容失败：返回 error 让 evolution 记 inconclusive。
		return nil, err
	}

	switch verdict := parseReplayVerdict(answer); verdict {
	case evolution.ReplayPass:
		res.Verdict = evolution.ReplayPass
		res.Reason = "model confirmed the memory matches the repository"
	case evolution.ReplayFail:
		res.Verdict = evolution.ReplayFail
		res.Reason = "model found the memory contradicts the repository"
	default:
		res.Verdict = evolution.ReplayInconclusive
		res.Reason = "model could not determine (unverifiable or unparseable answer)"
	}
	return res, nil
}

// parseReplayVerdict 从自由文本回复里取判定词。
//
// 只接受**恰好一个**判定词：回复里同时出现多个判定词说明模型没按格式答
// （常见于它把题干复述了一遍），这种情况无法判定，必须走 inconclusive 而不是
// 挑第一个命中——"SUPPORTED ... CONTRADICTED" 里取到哪个都是掷骰子。
//
// 三个判定词的映射不对称，这是有意的：
//   - SUPPORTED → pass：唯一放行的；
//   - CONTRADICTED → fail：仓库明确反对这条记忆，是负证据；
//   - UNVERIFIABLE → inconclusive：**不放行**。证据不足不是「看起来没问题」，
//     当成 pass 会让一条根本没被验证过的记忆进入可召回索引。
func parseReplayVerdict(answer string) string {
	s := strings.ToUpper(strings.TrimSpace(answer))
	// 只看第一行：模型常在答案后附解释，解释里可能复述其它判定词。
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	// 长词在前：CONTRADICTED 必须先于任何可能成为其子串的短词被检查。
	var found []string
	for _, v := range []string{"UNVERIFIABLE", "CONTRADICTED", "SUPPORTED"} {
		if strings.Contains(s, v) {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		return evolution.ReplayInconclusive
	}
	switch found[0] {
	case "SUPPORTED":
		return evolution.ReplayPass
	case "CONTRADICTED":
		return evolution.ReplayFail
	default: // UNVERIFIABLE
		return evolution.ReplayInconclusive
	}
}
