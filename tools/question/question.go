// Package question 模型→用户提问工具（HITL 反方向：模型问，用户答）。
//
// 复用审批的「工具阻塞 + 宿主注入」模式：工具调用时经 ToolContext.Handler 发
// AskUserQuestion 批次事件（UI 一次性展示全部问题与选项），同步阻塞在
// QuestionWaiter 上；宿主经 Session.AnswerQuestions(batchId, answers) 一次性注入
// 整批回答 → 工具返回逐题回答文本给 LLM。信息不足时模型显式暂停询问（可一次问多条），
// 而非瞎猜继续。
package question

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
)

type questionTool struct {
	tools.BaseTool
	w *events.QuestionWaiter
}

// NewTool 创建 ask_user 提问工具（需注册到引擎；w = 会话共享的等待器，
// 产品侧接线 Session.AnswerQuestions 注入回答）。
// w = nil 时创建独立等待器兜底（不接线 UI 则按默认窗口等待、超时告知模型「用户未作答」，不 hang）。
func NewTool(w *events.QuestionWaiter) *questionTool {
	if w == nil {
		w = events.NewQuestionWaiter(0)
	}
	// 等待窗口写进工具描述：模型在**调用前**就知道「用户最多多久必须作答、超时会怎样」，
	// 而不是等超时才发现（描述与 QuestionWaiter 同源，不复制常量）。
	waitNote := "Waits until the user answers (no time limit)."
	if d := w.Timeout(); d > 0 {
		waitNote = fmt.Sprintf("Waits up to %s for the user. If nobody answers in time, the tool result says so explicitly "+
			"(all questions unanswered) — never assume a choice was made; continue with your best judgment, and ask again later only if that answer is still blocking.",
			humanWait(d))
	}
	return &questionTool{
		BaseTool: tools.BaseTool{
			Name_: "ask_user",
			Description_: "Ask the user questions and wait for answers (HITL reverse direction: the answer is the user's to give — ask instead of guessing and continuing). " +
				"WHEN TO ASK: only when you cannot resolve the decision from the request, the code, or a sensible default, and the answer changes what you do next (product/UX trade-offs, irreversible or risky actions, scope you cannot infer, missing inputs or credentials). " +
				"Do NOT ask for facts you can verify yourself, for details you can decide, or when a conventional default applies — pick the default, say in your reply that you did, and continue; never re-ask what the user already answered. " +
				"questions is required (1-8 items), each with question required and options optional (quick choices); one call can ask multiple questions at once, " +
				"the UI shows them together and the user answers all at once. `multi` is a JSON boolean: use true to allow multiple selections or false for one selection; never use the strings \"true\" or \"false\". " +
				"Set multi:true only when multiple options may be selected; omit options for free-text questions. " +
				"STRUCTURE: each element of questions is {\"question\": string, \"options\": [{\"label\": string, \"preview\": string}, ...], \"multi\": bool} — question must be non-empty; " +
				"each option is an object with a non-empty label (a bare string \"A\" is accepted as shorthand for {\"label\":\"A\"}); never nest other objects. " +
				"PREVIEW: put `preview` on an option when the user must SEE the alternatives to choose between them (ASCII mockups or layouts, code or diff snippets, config or schema variants, small tables); " +
				"the UI lays the options and the selected option's preview out side by side. The label is what gets answered and what you read back, so keep it a one-line summary. " +
				"PREVIEW BOUNDARIES: single-select only — previews on a multi:true question are rejected, give that alternative its own question; each option that needs to be seen carries its own preview (never one shared block in the question text); " +
				"keep each preview short (a few dozen lines — it is a preview, not the deliverable, and not the place for reasoning or persuasion); cover what the user must see, not what you could just say in words; when a plain preference question needs no visual, use no previews at all. " +
				"Example: {\"questions\":[{\"question\":\"Which layout for the settings page?\",\"multi\":false,\"options\":[" +
				"{\"label\":\"Docked panel above the composer\",\"preview\":\"+-----------+\\n| history   |\\n| [panel]   |\\n+-----------+\"}," +
				"{\"label\":\"Full-screen modal\",\"preview\":\"+===========+\\n| scrim     |\\n| [modal]   |\\n+===========+\"}]},{\"question\":\"Any hard deadline?\"}]} " +
				"Returns per-question answer text; unanswered questions are marked as not answered. " + waitNote,
			Params_: tools.Obj(map[string]any{
				"questions": tools.Map{"type": "array",
					"items": tools.Obj(map[string]any{
						"question": tools.Str("Question for the user (required)"),
						"options": tools.Map{
							"type": "array",
							"items": tools.Obj(map[string]any{
								"label": tools.Str("Choice text — becomes the answer text when the user picks it (required, one line)"),
								"preview": tools.Str("Optional markdown preview of this alternative, shown side by side with the options " +
									"(ASCII mockup, code/diff snippet, config/schema variant, small table). Single-select questions only: " +
									"previews with multi:true are rejected. Keep it short (a few dozen lines); omit it when the choice needs no visual."),
							}, "label"),
							"description": "Optional quick choices (user clicks to answer; without them the user types freely). " +
								"A bare string \"A\" is accepted as shorthand for {\"label\":\"A\"}.",
						},
						"multi": tools.Bool("Optional JSON boolean. true = user may select multiple options (previews are not allowed then); false = single choice. Do not use string values \"true\" or \"false\"."),
					}, "question"),
					"minItems":    1,
					"maxItems":    events.MaxQuestionsPerBatch,
					"description": fmt.Sprintf("List of questions to ask (1-%d, each answered independently)", events.MaxQuestionsPerBatch)},
			}, "questions"),
			CanParallel_: false, // 一次一个批次（UI 聚焦；避免多问并存）
		},
		w: w,
	}
}

// askItem 单题入参。Options 用 events.QuestionOption：既能收纯字符串（兼容模型的
// 常见写法与前端的紧凑写法），也能收 {"label","preview"} 对象（见其 UnmarshalJSON）。
type askItem struct {
	Question string                  `json:"question"`
	Options  []events.QuestionOption `json:"options"`
	Multi    bool                    `json:"multi,omitempty"` // true = 可多选（选项可同时勾选多个）
}
type askArgs struct {
	Questions []askItem `json:"questions"`
}

func (t *questionTool) ValidParams(_ context.Context, _, arguments string) error {
	var a askArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		// 结构漂移（问题十五）：options 元素嵌套对象等 → 附结构模板让模型自我修正
		return fmt.Errorf("ask_user: %w; expected shape: {\"questions\":[{\"question\":string,\"options\":[{\"label\":string,\"preview\":string}],\"multi\":bool}]} — each option is an object with a non-empty label (a bare string \"A\" means {\"label\":\"A\"}), never another nested object", err)
	}
	if len(a.Questions) == 0 {
		return errors.New("ask_user: questions is required (1-8)")
	}
	if len(a.Questions) > events.MaxQuestionsPerBatch {
		return fmt.Errorf("ask_user: too many questions (%d, max %d)", len(a.Questions), events.MaxQuestionsPerBatch)
	}
	for i, q := range a.Questions {
		if q.Question == "" {
			return fmt.Errorf("ask_user: questions[%d].question is required (non-empty string)", i)
		}
		for j, o := range q.Options {
			// 问题十五：options 元素必须是选项文本——空串/空 label 在 unmarshal 或此处
			// 拦截，错误带结构模板让模型可自我修正。
			if strings.TrimSpace(o.Label) == "" {
				return fmt.Errorf("ask_user: questions[%d].options[%d].label is required (non-empty choice text; a bare string \"A\" means {\"label\":\"A\"})", i, j)
			}
			// 预览边界（描述里对模型讲，这里对模型兜底）：多选题的勾选项无法并排预览。
			// 拒绝而非静默丢弃 preview —— 丢掉就等于「模型以为用户看见了，而用户没看见」。
			if q.Multi && strings.TrimSpace(o.Preview) != "" {
				return fmt.Errorf("ask_user: questions[%d].options[%d].preview is not allowed on a multi-select question (multi:true) — give that alternative its own single-select question, or drop the preview", i, j)
			}
		}
	}
	return nil
}

// ToolTimeout 实现 tools.ToolTimeoutProvider：显式不受限（<0）。
// 为什么：ask_user 的等待时长由 QuestionWaiter 控制（bridge 装配 -1 = 无时间限制；
// 独立兜底 NewQuestionWaiter(0) 默认 5 分钟），引擎默认工具超时（30s）若包裹 Call，
// 会在用户决策前截断等待 —— 弹窗 30s 就关、用户"还没来得及决策"（2026-08-17 修复）。
// 打断/abort 仍有效：ctx 取消沿父级级联，不依赖工具超时。
func (t *questionTool) ToolTimeout() time.Duration { return -1 }

func (t *questionTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a askArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	// 事件转发通道：无 ToolContext（根直调/未注入）无法把问题发给宿主 → 直接失败
	tc := events.ToolContextFrom(ctx)
	if tc == nil || tc.Handler == nil {
		return "", errors.New("ask_user: no event handler available to surface the question")
	}
	// 批次 id + 单题锚：一次提问 = 一个批次（一次展示 / 一次回答注入）
	batchID := fmt.Sprintf("q-%d", time.Now().UnixNano())
	items := make([]events.QuestionItem, len(a.Questions))
	for i, q := range a.Questions {
		items[i] = events.QuestionItem{
			Id:       fmt.Sprintf("%s-%d", batchID, i+1),
			Question: q.Question,
			Options:  q.Options,
			Multi:    q.Multi,
		}
	}
	wait := t.w.Begin(batchID)
	// 注册先行（Begin 已注册通道）再发事件 —— 事件消费者收到即可 AnswerBatch，无竞态窗口。
	// RunId 取 tc.RunId（当前正在执行工具的运行）而非 tc.ParentRunId：与工具事件/审批
	// 事件的归属口径一致（events/toolctx.go:11）。用 ParentRunId 时根运行的提问
	// run_id 恒为空，trace 链路（docs/TRACE_CHAIN_REDESIGN.md）挂不上主 Agent。
	// TimeoutMs = 等待窗口：UI 据此显示「剩余决策时间」倒计时（<0 无时限 → 不带该字段）。
	tc.Handler(ctx, &events.AskUserQuestion{
		RunId:     tc.RunId,
		Id:        batchID,
		Questions: items,
		TimeoutMs: timeoutMsOf(t.w),
		Timestamp: time.Now(),
		EventType: events.AskUserQuestionType,
	})
	answers, err := wait(ctx)
	if err != nil {
		return "", err // 打断（abort）：提问中止
	}
	if answers == nil {
		// 超时（整批未答）：**必须在工具结果里明确告知「用户没有填写」** —— 否则模型会
		// 以为拿到了回答/以为面板还开着而困惑（用户报障：「超时之后需要让 LLM 在上下文中
		// 知道用户未填写，否则模型会很疑惑」）。措辞同时钉死「没有任何选项被选中」。
		if d := t.w.Timeout(); d > 0 {
			return fmt.Sprintf("No answer from the user: this batch timed out after %s and none of the %d question(s) were answered. "+
				"The user selected no option and typed nothing — treat every question as unanswered (do not assume any implicit choice); "+
				"continue with your best judgment, and ask again later only if that answer is still blocking.", humanWait(d), len(items)), nil
		}
		return fmt.Sprintf("No answer from the user: none of the %d question(s) were answered. "+
			"The user selected no option and typed nothing — treat every question as unanswered (do not assume any implicit choice); continue with your best judgment.", len(items)), nil
	}
	byID := make(map[string]string, len(answers))
	for _, an := range answers {
		byID[an.QuestionId] = an.Answer
	}
	return formatAnswers(items, byID), nil
}

// formatAnswers 把回答整理成模型可读文本：单问直接「用户回答：X」；
// 多问逐条「问题 → 回答」（缺答明确标「用户未作答」，不写含糊的「未回答」——
// 模型要能分清「用户说不知道」与「用户根本没填」）。
func formatAnswers(qs []events.QuestionItem, byID map[string]string) string {
	if len(qs) == 1 {
		a := byID[qs[0].Id]
		if a == "" {
			return "(no answer from the user — no option selected, no text typed; continue with your best judgment)"
		}
		return "User answer: " + a
	}
	var b strings.Builder
	b.WriteString("User answers:\n")
	for _, q := range qs {
		a := byID[q.Id]
		if a == "" {
			a = "(the user did not answer this question)"
		}
		fmt.Fprintf(&b, "- %s → %s\n", q.Question, a)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// humanWait 等待窗口的人类可读写法（模型读的是描述与工具结果，写 "30 minutes" 比 "30m0s" 清楚）。
func humanWait(d time.Duration) string {
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
	return d.String()
}

// timeoutMsOf 等待窗口的毫秒表示（<=0 / nil = 无时间限制 → 0，事件里不带该字段，UI 无倒计时）。
func timeoutMsOf(w *events.QuestionWaiter) int64 {
	if w == nil {
		return 0
	}
	d := w.Timeout()
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}

var _ tools.Tool = (*questionTool)(nil)
