package agenthub

// task_header.go：派发消息里的 hub_task header 块（方案 §6.6.2 / §6.7.2）。
//
// 为什么把 header 放在内容块而不是正文前缀：
//   - UserInputsConsumed 携带完整 UserContents，桥接层据此**确定**这个 AgentEnd
//     属于哪个任务（不靠时间/文本猜）；
//   - collectUserContents 只认 text/image，header 不污染会话标题、不算用户输入；
//   - 前端按块类型过滤后，用户气泡里只剩正文（不会看到 JSON）。
//
// header 必须能独立解析（ContentTypeHubTask + JSON），正文另起 text 块。

import (
	"encoding/json"
	"fmt"
	"strings"
)

// TaskHeader hub_task 块的 JSON 形状（保持小：每轮都会随上下文送给模型）。
type TaskHeader struct {
	TaskID     TaskID     `json:"task_id"`
	RootTaskID TaskID     `json:"root_task_id,omitempty"`
	Origin     TaskOrigin `json:"origin"`
	Callback   string     `json:"callback,omitempty"`
	Note       string     `json:"note,omitempty"`
}

// EncodeTaskHeader 把任务快照编码成 hub_task 块内容。
func EncodeTaskHeader(t *Task) (string, error) {
	if t == nil || t.ID == "" {
		return "", fmt.Errorf("%w: task header 需要 task_id", ErrInvalid)
	}
	h := TaskHeader{
		TaskID:     t.ID,
		RootTaskID: t.RootTaskID,
		Origin:     t.Origin,
		Callback:   t.Callback,
	}
	b, err := json.Marshal(h)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseTaskHeader 解析 hub_task 块内容；坏 JSON/缺 task_id → 错误（调用方据此告警）。
func ParseTaskHeader(content string) (*TaskHeader, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("%w: 空 hub_task 块", ErrInvalid)
	}
	var h TaskHeader
	if err := json.Unmarshal([]byte(content), &h); err != nil {
		return nil, fmt.Errorf("%w: hub_task header JSON: %v", ErrInvalid, err)
	}
	if h.TaskID == "" || !strings.HasPrefix(string(h.TaskID), PrefixTask+"_") {
		return nil, fmt.Errorf("%w: hub_task header 缺 task_id", ErrInvalid)
	}
	return &h, nil
}
