package cron

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// 运行历史（sessions 快照）解析与本地化落盘。
//
// 设计约定（与 desktop/bridge/cron.go 配合）：
//   - 每次触发新建的派生会话，其事件日志被重定向到独立目录
//     ~/.go-code/cron_sessions/<wsKey>/<jobID>/<sid>.jsonl（不进入工作区
//     events/<wsKey>，因此不出现在工作区会话历史里——「独立记录、本地化存储」）。
//   - 该事件日志即执行历史的唯一数据源：桌面端「定时任务 → 某次执行」详情页
//     实时调用 ExtractMessages 解析，与工作区会话历史完全同构。
//   - 删除任务时宿主连带删除 ~/.go-code/cron_sessions/<wsKey>/<jobID>/ 整目录
//     （任务删除 = 其全部执行历史一并删除）。
//
// 本文件只负责纯解析与读写；路径拼接、事件重定向、调度在 bridge 层完成。

// RunMessage 详情页展示用的一条消息（user/assistant 文本轮；与主会话恢复
// restoredMessages 同构，保证「和当前对话页面展示的对话历史一致」）。
type RunMessage struct {
	Role     string `json:"role"` // user | assistant
	Text     string `json:"text"`
	HasImage bool   `json:"has_image,omitempty"` // 该轮含图片附件（详情页渲染占位）
}

// ExtractMessages 从派生会话的事件日志（独立目录下 <sid>.jsonl）提取
// user/assistant 文本轮（与主会话恢复一致：纯文本轮 + 图片标记；工具调用
// 轮跳过）。返回 nil 表示无有效消息（空会话/日志缺失）。
func ExtractMessages(eventsPath string) []RunMessage {
	f, err := os.Open(eventsPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []RunMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &raw) != nil {
			continue
		}
		var et string
		if json.Unmarshal(raw["event_type"], &et) != nil {
			continue
		}
		switch et {
		case "user_inputs_consumed":
			// 一批用户输入被消费：每条文本一条 user 消息；UserContents 里的图片块 → HasImage
			msgs := userMessagesFromConsumed(raw)
			out = append(out, msgs...)
		case "agent_end":
			// 助手回复（整次运行的最终回复）：content 字段。子 agent（ParentRunId 非空）
			// 不单独成轮（其内容已在工具调用卡片里展示，与主会话历史一致）。
			if parentRun(raw) {
				continue
			}
			var content string
			if json.Unmarshal(raw["content"], &content) != nil {
				continue
			}
			if strings.TrimSpace(content) == "" {
				continue
			}
			out = append(out, RunMessage{Role: "assistant", Text: strings.TrimSpace(content)})
		case "content_chunk":
			// 流式文本块：并入上一条 assistant（事件日志里 agent_end 之前有完整流式块；
			// 若 agent_end 已落，则此块属于下一轮，跳过）
			if len(out) > 0 && out[len(out)-1].Role == "assistant" {
				var c string
				if json.Unmarshal(raw["content"], &c) == nil && c != "" {
					out[len(out)-1].Text += c
				}
			}
		}
	}
	// 压缩空行（流式拼接可能产生多余空白）
	clean := out[:0]
	for _, m := range out {
		m.Text = strings.TrimSpace(m.Text)
		if m.Role == "assistant" && m.Text == "" && !m.HasImage {
			continue // 纯空 assistant 轮丢弃
		}
		clean = append(clean, m)
	}
	return clean
}

// userMessagesFromConsumed 从 user_inputs_consumed 事件提取用户消息列表：
// user_texts 为文本（顺序 = 入队顺序）；user_contents 为完整内容块（含图片）。
func userMessagesFromConsumed(raw map[string]json.RawMessage) []RunMessage {
	var texts []string
	if b, ok := raw["user_texts"]; ok {
		_ = json.Unmarshal(b, &texts)
	}
	hasImages := map[int]bool{}
	if b, ok := raw["user_contents"]; ok {
		var blocks [][]struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &blocks) == nil {
			for i, bs := range blocks {
				for _, bl := range bs {
					if bl.Type == "image" {
						hasImages[i] = true
						break
					}
				}
			}
		}
	}
	if len(texts) == 0 {
		// 无文本（可能只有图片）：仍给占位（有图片时）
		if len(hasImages) > 0 {
			return []RunMessage{{Role: "user", Text: "", HasImage: true}}
		}
		return nil
	}
	out := make([]RunMessage, 0, len(texts))
	for i, t := range texts {
		if strings.TrimSpace(t) == "" && !hasImages[i] {
			continue
		}
		out = append(out, RunMessage{Role: "user", Text: strings.TrimSpace(t), HasImage: hasImages[i]})
	}
	return out
}

// parentRun 判断事件是否属于子 agent 运行（agent_end 的 parent_run_id 非空）。
func parentRun(raw map[string]json.RawMessage) bool {
	var p string
	if b, ok := raw["parent_run_id"]; ok {
		_ = json.Unmarshal(b, &p)
	}
	return p != ""
}
