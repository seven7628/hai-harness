package events

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// 默认提问等待时限：超时 = 用户未答（工具回明确提示「用户未作答」，模型按最佳判断继续）。
// 2026-09-20（用户报障）：5 分钟 → 30 分钟 —— 提问是「模型停下来等人」，而人可能正在翻
// 对话历史/离开工位；5 分钟太短，容易在用户还没读完上下文时就被判定为未答。
const defaultQuestionTimeout = 30 * time.Minute

// MaxQuestionTimeout 等待窗口上限：NewQuestionWaiter 把「> 上限」的显式正值钳到上限
// （用户拍板：最长 30 分钟）。<0 = 无时间限制的显式声明不参与钳制（见 NewQuestionWaiter）。
const MaxQuestionTimeout = 30 * time.Minute

// MaxQuestionsPerBatch 单次 ask_user 提问的最大题数（UI 列表可控，防止模型一次问爆屏）。
const MaxQuestionsPerBatch = 8

// ErrNoPendingQuestion 无待应答的提问批次（Session.AnswerQuestions 匹配不到）。
var ErrNoPendingQuestion = errors.New("no pending question batch for this id")

// QuestionItem 单个问题（batch 内每项独立锚，回答可逐题注入）。
type QuestionItem struct {
	Id       string           `json:"id"` // 单题锚（QuestionAnswer.QuestionId 匹配）
	Question string           `json:"question"`
	Options  []QuestionOption `json:"options,omitempty"`
	Multi    bool             `json:"multi,omitempty"` // true = 可多选：用户可同时勾选多个选项（提交时拼接为一条 answer）
}

// QuestionOption 单个快捷选项：
//   - Label 选项文本 —— 用户点选后它**原样成为回答**（模型不必回看 Preview 就知道用户选了什么）；
//   - Preview 可选预览（markdown 文本）—— 「必须看见才能选」的备选才有意义（ASCII 版面/代码
//     或 diff 片段/配置或 schema 变体/小表格），UI 与选项并排展示。仅单选问题可用（多选的
//     勾选项无法并排预览，工具校验层直接拒绝，见 tools/question）。
//
// 只允许单选 + 预览，是「预览边界」的一半；另一半在工具描述里（不要写长文/不要用预览
// 承载论证）。校验在 tools/question，展示在 AskUserQuestionPanel。
type QuestionOption struct {
	Label   string `json:"label"`
	Preview string `json:"preview,omitempty"`
}

// UnmarshalJSON 容忍两种写法：纯字符串 "A"（等价 {"label":"A"}）与对象
// {"label":"A","preview":"…"}。
//
// 为什么必须容忍字符串：模型最常见的写法就是 ["A","B"]；且**历史会话事件**（落盘 JSONL）
// 与前端 mock 里 options 都是字符串，重放（replay）时事件要还能解析——事件格式的兼容
// 是回放正确性的一部分，不能靠「新版本只会写新格式」的假设。
func (o *QuestionOption) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*o = QuestionOption{Label: s}
		return nil
	}
	type alias QuestionOption // 别名避免递归调用本方法
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*o = QuestionOption(a)
	return nil
}

// QuestionAnswer 单个问题的用户回答（answer_question 命令逐题携带）。
type QuestionAnswer struct {
	QuestionId string `json:"question_id"`
	Answer     string `json:"answer"`
}

// QuestionWaiter 模型→用户提问等待器（HITL 反方向）：
// ask_user 工具调用时经 Begin 注册回答通道并发 AskUserQuestion 事件（UI 展示全部问题），
// 用户经 Session.AnswerQuestions(batchId, answers) 一次性注入整批回答；等待期间工具同步阻塞
// （与 approvalWaiter 同构：注册先行、超时、ctx 取消）。
type QuestionWaiter struct {
	timeout time.Duration

	mu    sync.Mutex
	waits map[string]chan []QuestionAnswer // 批次 id → 回答通道（决策锚 = 工具生成的唯一 id）
}

// NewQuestionWaiter 创建提问等待器。timeout 语义：
//   - > 0：指定等待时限（超时 = 整批未答，工具回明确提示「用户未作答」，模型继续）；
//     超过 MaxQuestionTimeout（30 分钟）的值钳到上限；
//   - == 0：默认 30 分钟（defaultQuestionTimeout）；
//   - < 0：无时间限制（不自动关闭；放弃只能靠用户中断 / 关会话）。
func NewQuestionWaiter(timeout time.Duration) *QuestionWaiter {
	if timeout == 0 {
		timeout = defaultQuestionTimeout
	}
	if timeout > MaxQuestionTimeout {
		timeout = MaxQuestionTimeout
	}
	return &QuestionWaiter{timeout: timeout, waits: map[string]chan []QuestionAnswer{}}
}

// Timeout 等待窗口（<0 = 无时间限制）。工具把它写进 AskUserQuestion.TimeoutMs，
// UI 据此显示「剩余决策时间」倒计时 —— 倒计时口径只此一处，不复制常量。
func (w *QuestionWaiter) Timeout() time.Duration { return w.timeout }

// Begin 注册单个提问批次的回答通道并返回等待函数（阻塞直到用户整批回答 / 超时 / ctx 取消）。
// 注册先行语义（与 approvalWaiter 一致）：工具先注册再发事件——事件消费者收到
// AskUserQuestion 即可 AnswerBatch，无「事件已发但通道未注册」的竞态窗口。
func (w *QuestionWaiter) Begin(id string) func(ctx context.Context) ([]QuestionAnswer, error) {
	ch := make(chan []QuestionAnswer, 1)
	w.mu.Lock()
	w.waits[id] = ch
	w.mu.Unlock()
	return func(ctx context.Context) ([]QuestionAnswer, error) {
		defer func() {
			w.mu.Lock()
			delete(w.waits, id)
			w.mu.Unlock()
		}()
		// 无时间限制（timeout < 0）：timer 为 nil，select 的 nil channel 永远阻塞，
		// 等待只能由用户回答或打断（abort）结束。
		var timer <-chan time.Time
		if w.timeout > 0 {
			timer = time.After(w.timeout)
		}
		select {
		case ans := <-ch:
			return ans, nil // 用户整批回答
		case <-ctx.Done():
			return nil, ctx.Err() // 打断（abort）：提问中止
		case <-timer:
			return nil, nil // 超时 = 整批未答（工具回「用户未作答」明确提示，模型继续）
		}
	}
}

// AnswerBatch 注入整批用户回答（Session.AnswerQuestions 调用）。
func (w *QuestionWaiter) AnswerBatch(id string, answers []QuestionAnswer) error {
	w.mu.Lock()
	ch, ok := w.waits[id]
	w.mu.Unlock()
	if !ok {
		return ErrNoPendingQuestion
	}
	ch <- answers
	return nil
}

// AskUserQuestion 模型→用户提问批次事件（UI 一次性展示全部问题与选项，用户经
// Session.AnswerQuestions(batchId, answers) 注入整批回答）。
type AskUserQuestion struct {
	RunId     string         `json:"run_id,omitempty"` // 所属运行（事件自包含）
	Id        string         `json:"id"`               // 批次 id（回答锚：Session.AnswerQuestions 按此匹配）
	Questions []QuestionItem `json:"questions"`
	// TimeoutMs 等待窗口（毫秒）；0/缺省 = 无时间限制。UI 据此显示「剩余决策时间」倒计时，
	// 与 QuestionWaiter 的 timeout 同源（工具写入），倒计时口径不另设常量。
	TimeoutMs int64     `json:"timeout_ms,omitempty"`
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (q *AskUserQuestion) Type() EventType { return q.EventType }
