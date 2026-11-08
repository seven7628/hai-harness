package agenthub

// P3b-06：群组左栏投影（未读 / 预览 / 任务结论）的领域口径。

import (
	"strings"
	"testing"
	"time"
)

func perceptionTime(seq int64) time.Time {
	return time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Minute)
}

// percMsg 构造一条群消息（seq 递增时间，方便断言 last_at）。
func percMsg(seq int64, kind MessageKind, sender Sender, text string) Message {
	return Message{Seq: seq, Kind: kind, Sender: sender, Text: text, CreatedAt: perceptionTime(seq)}
}

// percTaskCard 构造一张 bridge 同形的 task_result 卡。
func percTaskCard(seq int64, taskID TaskID, status TaskStatus, summary, errText string) Message {
	m := percMsg(seq, KindTaskResult, Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"}, summary)
	m.TaskID = taskID
	m.Refs = map[string]any{
		"task_id": string(taskID),
		"status":  string(status),
		"error":   errText,
	}
	return m
}

// 任务卡优先于普通消息：last_outcome 是确定性中文结论，UI-only 回执不参与。
func TestGroupPerceptionTaskResultPriority(t *testing.T) {
	msgs := []Message{
		// 故意倒序：投影必须与日志顺序无关。
		percMsg(6, KindText, Sender{Kind: SenderHuman}, "   "),
		percMsg(5, KindApprovalStub, Sender{Kind: SenderSystem}, "审批占位"),
		percMsg(4, KindRoundSummary, Sender{Kind: SenderSystem}, "本轮唤醒：A"),
		percMsg(3, KindRouting, Sender{Kind: SenderSystem}, "回执：唤醒 A"),
		percTaskCard(2, "task_1", TaskCompleted, "三家价格表已整理", ""),
		percMsg(1, KindText, Sender{Kind: SenderHuman}, "先看看价格表"),
	}
	got := BuildGroupPerception(msgs, 1, 0)
	if got.Unread != 1 {
		t.Fatalf("unread = %d, want 1（人自己的消息不算，回执/汇总不算）", got.Unread)
	}
	if got.LastSeq != 2 || got.LastSender != string(SenderAgent) {
		t.Fatalf("last = seq %d sender %q, want 2/agent", got.LastSeq, got.LastSender)
	}
	if got.LastAt == nil || !got.LastAt.Equal(perceptionTime(2)) {
		t.Fatalf("last_at = %v, want %v", got.LastAt, perceptionTime(2))
	}
	if got.LastText != "三家价格表已整理" {
		t.Fatalf("last_text = %q", got.LastText)
	}
	if got.LastOutcome != "任务完成：三家价格表已整理" {
		t.Fatalf("last_outcome = %q", got.LastOutcome)
	}
	if got.LastTaskID != "task_1" || got.LastTaskStatus != TaskCompleted ||
		got.LastTaskSummary != "三家价格表已整理" {
		t.Fatalf("task fields = %+v", got)
	}
	// 读过最新一条后未读归零。
	if again := BuildGroupPerception(msgs, 2, 0); again.Unread != 0 {
		t.Fatalf("unread after read=2: %d", again.Unread)
	}
	// 没有时间戳混进文本（前端自己格式化）。
	if strings.Contains(got.LastOutcome, "2026") || strings.Contains(got.LastText, ":") {
		t.Fatalf("text must not embed timestamps: %q / %q", got.LastOutcome, got.LastText)
	}
}

// 失败卡用错误原因，且「最近一张卡」优先（旧的成功卡不再顶到前面）。
func TestGroupPerceptionFailedCard(t *testing.T) {
	msgs := []Message{
		percMsg(1, KindText, Sender{Kind: SenderHuman}, "跑一下"),
		percTaskCard(2, "task_1", TaskCompleted, "旧结果", ""),
		percTaskCard(3, "task_2", TaskFailed, "只拿到两家", "工具超时"),
	}
	got := BuildGroupPerception(msgs, 0, 0)
	if got.LastTaskID != "task_2" || got.LastTaskStatus != TaskFailed {
		t.Fatalf("latest task = %s/%s, want task_2/failed", got.LastTaskID, got.LastTaskStatus)
	}
	if got.LastTaskSummary != "只拿到两家" {
		t.Fatalf("last_task_summary = %q", got.LastTaskSummary)
	}
	if got.LastOutcome != "任务失败：工具超时" {
		t.Fatalf("last_outcome = %q, want 任务失败：工具超时", got.LastOutcome)
	}
	// 最新的普通消息盖过任务卡：结论留空，预览用正文（前端决定怎么显示）。
	msgs = append(msgs, percMsg(4, KindText, Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"}, "我先补充一句"))
	got = BuildGroupPerception(msgs, 0, 0)
	if got.LastOutcome != "" {
		t.Fatalf("stale task card must not render outcome: %q", got.LastOutcome)
	}
	if got.LastText != "我先补充一句" || got.LastSeq != 4 {
		t.Fatalf("preview = %q seq %d", got.LastText, got.LastSeq)
	}
	// 任务字段仍然保留（「最近一次任务结果」不因后来聊天丢失）。
	if got.LastTaskID != "task_2" || got.LastTaskStatus != TaskFailed {
		t.Fatalf("task fields lost after newer chat: %+v", got)
	}
}

// 未读只数别人的：人自己发的略过，agent/system 的任务卡要数。
func TestGroupPerceptionUnreadExcludesHuman(t *testing.T) {
	msgs := []Message{
		percMsg(1, KindText, Sender{Kind: SenderHuman}, "我来"),
		percMsg(2, KindText, Sender{Kind: SenderHuman}, "再说一句"),
		percMsg(3, KindText, Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"}, "收到"),
		percMsg(4, KindSystem, Sender{Kind: SenderSystem}, "自动对话已停止"),
		percTaskCard(5, "task_1", TaskCompleted, "做完了", ""),
		percMsg(6, KindRouting, Sender{Kind: SenderSystem}, "回执不算"),
		percMsg(7, KindText, Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"}, "   "),
	}
	cases := []struct {
		readSeq int64
		want    int
	}{
		{0, 3}, // 3/4/5
		{2, 3},
		{3, 2},
		{5, 0},
		{7, 0},
	}
	for _, tc := range cases {
		if got := BuildGroupPerception(msgs, tc.readSeq, 0); got.Unread != tc.want {
			t.Fatalf("readSeq=%d unread=%d, want %d", tc.readSeq, got.Unread, tc.want)
		}
	}
	// 只有人自己的消息：永远没有未读点。
	humanOnly := []Message{
		percMsg(1, KindText, Sender{Kind: SenderHuman}, "a"),
		percMsg(2, KindText, Sender{Kind: SenderHuman}, "b"),
	}
	if got := BuildGroupPerception(humanOnly, 0, 0); got.Unread != 0 || got.LastSeq != 2 {
		t.Fatalf("human-only perception = %+v", got)
	}
}

// 没有任务卡时：预览 = 最后一条可用消息正文（空白折叠）。
func TestGroupPerceptionFallbackText(t *testing.T) {
	msgs := []Message{
		percMsg(1, KindText, Sender{Kind: SenderHuman}, "旧消息"),
		percMsg(2, KindText, Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"}, "  第一行\n\t第二行   第三行  "),
	}
	got := BuildGroupPerception(msgs, 1, 0)
	if got.LastText != "第一行 第二行 第三行" {
		t.Fatalf("last_text = %q", got.LastText)
	}
	if got.LastOutcome != "" || got.LastTaskID != "" || got.LastTaskStatus != "" {
		t.Fatalf("plain text must not render task fields: %+v", got)
	}
	if got.Unread != 1 {
		t.Fatalf("unread = %d, want 1", got.Unread)
	}
}

// 按 rune 截断（中文不切半字），默认 80；任务摘要同样截断。
func TestGroupPerceptionTruncation(t *testing.T) {
	long := strings.Repeat("长", 200)
	got := BuildGroupPerception([]Message{
		percMsg(1, KindText, Sender{Kind: SenderHuman}, long),
	}, 0, 10)
	if len([]rune(got.LastText)) != 11 || !strings.HasSuffix(got.LastText, "…") {
		t.Fatalf("last_text = %q (%d runes)", got.LastText, len([]rune(got.LastText)))
	}
	if !strings.HasPrefix(got.LastText, strings.Repeat("长", 10)) {
		t.Fatalf("truncation broke rune prefix: %q", got.LastText)
	}
	// previewRunes <= 0 → 默认 80。
	def := BuildGroupPerception([]Message{
		percMsg(1, KindText, Sender{Kind: SenderHuman}, long),
	}, 0, 0)
	if len([]rune(def.LastText)) != DefaultGroupPreviewRunes+1 {
		t.Fatalf("default truncation = %d runes, want %d", len([]rune(def.LastText)), DefaultGroupPreviewRunes+1)
	}
	// 任务结论里的摘要是截断后的摘要（前缀不能被截掉）。
	card := percTaskCard(1, "task_1", TaskCompleted, long, "")
	card.Refs["error"] = ""
	got = BuildGroupPerception([]Message{card}, 0, 10)
	if got.LastOutcome != "任务完成："+strings.Repeat("长", 10)+"…" {
		t.Fatalf("outcome = %q", got.LastOutcome)
	}
	if len([]rune(got.LastTaskSummary)) != 11 {
		t.Fatalf("last_task_summary = %q (%d)", got.LastTaskSummary, len([]rune(got.LastTaskSummary)))
	}
}

// 空日志/只有 UI 回执：零值（前端空态），不能凭空造最后一条。
func TestGroupPerceptionEmptyLog(t *testing.T) {
	for name, msgs := range map[string][]Message{
		"nil": nil,
		"ui-only": {
			percMsg(1, KindRouting, Sender{Kind: SenderSystem}, "回执"),
			percMsg(2, KindRoundSummary, Sender{Kind: SenderSystem}, "汇总"),
			percMsg(3, KindApprovalStub, Sender{Kind: SenderSystem}, "审批占位"),
			percMsg(4, KindText, Sender{Kind: SenderHuman}, "   "),
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := BuildGroupPerception(msgs, 0, 0)
			if got != (GroupPerception{}) {
				t.Fatalf("perception = %+v, want zero value", got)
			}
		})
	}
}

// 状态 → 文案是固定的（前端/测试都按这个口径断言）。
func TestGroupPerceptionOutcomeMapping(t *testing.T) {
	cases := []struct {
		status TaskStatus
		want   string
	}{
		{TaskCompleted, "任务完成：产出"},
		{TaskDelivered, "任务完成：产出"},
		{TaskFailed, "任务失败：原因"},
		{TaskInterrupted, "任务已中断：原因"},
		{TaskCanceled, "任务已取消：原因"},
		{TaskDeclined, "任务已被拒绝：原因"},
		{TaskBlockedBudget, "任务预算阻断：原因"},
		{TaskRunning, ""},
		{TaskQueued, ""},
	}
	for _, tc := range cases {
		got := BuildGroupPerception([]Message{
			percTaskCard(1, "task_x", tc.status, "产出", "原因"),
		}, 0, 0)
		if got.LastOutcome != tc.want {
			t.Fatalf("status %s outcome = %q, want %q", tc.status, got.LastOutcome, tc.want)
		}
	}
}
