package agenthub

// perception.go：群组左栏投影的领域口径（P3b-06，§6.5.7 / §7.1.3 / §7.5）。
//
// 左栏一行需要三件事：未读点、最后一条可用消息的预览、最近一次任务结果的结论。
// 它们都从**已落盘的群日志**算出来（不是 bridge 的进程内状态），所以刷新、重连、
// 重启后仍然成立 —— 否则任务完成就只剩「最后一句闲聊」（D6 的入口）。
//
// 口径：
//   - 可用消息 = 非空正文 + 非 UI-only 类型（routing_receipt / round_summary /
//     approval_stub，与 BuildGroupSnapshot 共用 AgentContextKind）。task_result
//     恰恰是可用的：它就是「人在群外也要知道」的信号；
//   - 未读 = seq > readSeq 的可用消息，**不数人自己发的**（人已经看过自己的话），
//     agent/system 的任务卡要数；
//   - 预览 = 最后一条可用消息正文，空白折叠 + 按 rune 截断（默认 80）；
//   - 结论 = 最近一张 task_result 是最后一条可用消息时，渲染确定性中文；
//   - 时间戳只放字段，不拼进文本（前端自己格式化，避免时区/相对时间漂移）。
//
// 纯函数：同输入同输出，不读时钟、不读 store；单测在 perception_test.go。

import (
	"sort"
	"strings"
	"time"
)

// DefaultGroupPreviewRunes 是左栏预览的默认字数（一条摘要，不整段塞给列表）。
const DefaultGroupPreviewRunes = 80

// GroupPerception 是一个群的左栏投影。字段名即 hub_group_list 的 progress 载荷。
type GroupPerception struct {
	// Unread 是 seq > human_read_seq 的可用消息数（不含人自己发的）。
	Unread int `json:"unread"`
	// Last* 描述最后一条可用消息（任何发送者），供预览与排序。
	LastSeq    int64      `json:"last_seq,omitempty"`
	LastSender string     `json:"last_sender,omitempty"` // human | agent | system
	LastAt     *time.Time `json:"last_at,omitempty"`
	LastText   string     `json:"last_text,omitempty"`
	// LastOutcome 是任务结论行（例如「任务完成：三家价格表出来了」）。
	// 只有最新可用消息就是 task_result 时才渲染；否则前端用 LastText 兜底。
	LastOutcome string `json:"last_outcome,omitempty"`
	// 最近一张 task_result（不要求它是最后一条消息）：任务 id/状态/摘要。
	LastTaskID      TaskID     `json:"last_task_id,omitempty"`
	LastTaskStatus  TaskStatus `json:"last_task_status,omitempty"`
	LastTaskSummary string     `json:"last_task_summary,omitempty"`
}

// BuildGroupPerception 计算一个群的左栏投影。
// readSeq 是群级人类已读游标（Group.HumanReadSeq）；previewRunes <= 0 用默认 80。
func BuildGroupPerception(messages []Message, readSeq int64, previewRunes int) GroupPerception {
	if previewRunes <= 0 {
		previewRunes = DefaultGroupPreviewRunes
	}
	var out GroupPerception

	// 调用方可能给任何顺序（分页拼装、并发追加）；投影必须与日志顺序无关。
	ordered := append([]Message(nil), messages...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Seq < ordered[j].Seq })

	var last *Message
	var lastTask *Message
	for i := range ordered {
		m := &ordered[i]
		if !groupPerceptionUsable(*m) {
			continue
		}
		if m.Seq > readSeq && m.Sender.Kind != SenderHuman {
			out.Unread++
		}
		last = m
		if m.Kind == KindTaskResult {
			lastTask = m
		}
	}
	if last == nil {
		return out // 空日志/只有 UI 回执：零值，前端显示空态
	}
	out.LastSeq = last.Seq
	out.LastSender = string(last.Sender.Kind)
	if !last.CreatedAt.IsZero() {
		at := last.CreatedAt.UTC()
		out.LastAt = &at
	}
	out.LastText = groupPreviewText(last.Text, previewRunes)
	if lastTask == nil {
		return out
	}

	id, status, summary, reason := taskResultCardFields(*lastTask)
	out.LastTaskID = id
	out.LastTaskStatus = status
	out.LastTaskSummary = groupPreviewText(summary, previewRunes)
	if lastTask.Seq == last.Seq {
		// 最新可用消息就是这张卡：渲染确定性结论（文案见 §7.5.5 的任务卡状态表）。
		out.LastOutcome = groupTaskOutcome(status,
			groupPreviewText(summary, previewRunes),
			groupPreviewText(reason, previewRunes))
	}
	return out
}

// groupPerceptionUsable 与快照同一口径：UI-only 回执 / 空正文不算「可用消息」。
func groupPerceptionUsable(m Message) bool {
	if !m.AgentContextKind() {
		return false
	}
	return strings.TrimSpace(m.Text) != ""
}

// taskResultCardFields 从结果卡提取 (task_id, status, summary, error)。
// 卡由 bridge 写（Refs 反规范化）；直接构造的 Message 可能只填 TaskID，
// 两种形态都要能读，避免投影依赖某一种写法。
func taskResultCardFields(m Message) (TaskID, TaskStatus, string, string) {
	id := m.TaskID
	if id == "" {
		id = TaskID(refString(m.Refs["task_id"]))
	}
	status := TaskStatus(refString(m.Refs["status"]))
	return id, status, strings.TrimSpace(m.Text), strings.TrimSpace(refString(m.Refs["error"]))
}

// refString 宽松读 Refs 的字符串字段（JSON 反序列化/直接构造都可能是 string 或命名类型）。
func refString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case TaskID:
		return string(s)
	case TaskStatus:
		return string(s)
	case MessageKind:
		return string(s)
	default:
		return ""
	}
}

// groupTaskOutcome 把终态渲染成左栏结论行（§6.5.7 / §7.5.5）。
// 失败优先用错误原因；其余状态摘要优先（摘要为空时退回错误/状态词）。
func groupTaskOutcome(status TaskStatus, summary, reason string) string {
	var head, detail string
	switch status {
	case TaskCompleted, TaskDelivered:
		head, detail = "任务完成：", firstNonEmpty(summary, reason)
	case TaskFailed:
		head, detail = "任务失败：", firstNonEmpty(reason, summary)
	case TaskInterrupted:
		head, detail = "任务已中断：", firstNonEmpty(reason, summary)
	case TaskCanceled:
		head, detail = "任务已取消：", firstNonEmpty(reason, summary)
	case TaskDeclined:
		head, detail = "任务已被拒绝：", firstNonEmpty(reason, summary)
	case TaskBlockedBudget:
		head, detail = "任务预算阻断：", firstNonEmpty(reason, summary)
	default:
		return "" // 非终态/未知状态：不编结论，前端用正文兜底
	}
	if detail == "" {
		return strings.TrimSuffix(head, "：")
	}
	return head + detail
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// groupPreviewText 把正文压成单行并按 rune 截断（中文不能切半个字）。
func groupPreviewText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
