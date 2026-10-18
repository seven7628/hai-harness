package main

// 子智能体完整历史的分页读取（agent_history 命令的数据层）。
//
// 为什么需要它（2026-10-18 用户决策）：
// checkpoint 里的 `blocks[kind=agent].items` 曾经把子智能体的完整 tool/thinking
// 流水整份塞进 SessionRecord —— 实测占 record 体积的 98%（128MB 里 126MB），
// 直接导致每次 commit 要序列化近百 MB（6s+），12 个会话就把机器压满。
//
// 直接截断（只留最近 N 条）能到 10x，但用户明确指出「看不到全量历史不合理」。
// 所以改成：**checkpoint 只留首屏**（快速渲染），全量从子任务 journal 按页取。
//
// 数据源为什么可靠：子任务的 StateStore 每轮落一条 `state` 行，其中 `messages`
// 是完整上下文（含每次 tool 调用的 args 与 result，未截断）。实测一个 23MB 的
// journal 里，最后一条 state 含 453 条消息 / 233 个 tool 调用。
// 注意：journal 的 `turn` 行是**有损**旁路（只记工具名、正文截断 4000 runes），
// 不能用来做历史展示；必须读 `state.messages`。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// agentHistoryEntry 是分页返回给 UI 的一条历史条目。
type agentHistoryEntry struct {
	// Kind: assistant（模型输出）| tool（工具调用）| user（任务输入）
	Kind string `json:"kind"`
	// Text: assistant 的思考/正文；tool 的 result；user 的任务原文。
	Text string `json:"text,omitempty"`
	// 仅 tool 有：
	ToolID string `json:"tool_id,omitempty"`
	Name   string `json:"name,omitempty"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	// 该条目在全量历史中的序号（0-based）；分页游标就是它。
	Index int `json:"index"`
}

// agentHistoryPage 是一页历史 + 分页元信息。
type agentHistoryPage struct {
	Entries []agentHistoryEntry `json:"entries"`
	// Total 是全量条目数（UI 据此显示「共 N 条」与分页器总页数）。
	Total int `json:"total"`
	// Offset/Limit 回显本次请求的窗口，便于 UI 校验乱序响应。
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	// HasMore 表示还有更老的条目（offset+len(entries) < total）。
	HasMore bool `json:"has_more"`
}

// defaultAgentHistoryPage 默认页大小（首屏一次拉这么多够看完一屏）。
const defaultAgentHistoryPage = 50

// maxAgentHistoryPage 单页上限，防止一次拉爆 stdout。
const maxAgentHistoryPage = 500

// readAgentHistoryPage 从子任务 journal 读出**完整**历史并按页切片。
//
// 实现要点：journal 是 append-only 的，越靠后的 `state` 行上下文越完整；
// 因此从文件尾部往前找第一条可解析的 state 行（不读整个 23MB 文件）。
// 这一步是 O(尾部窗口)，与 journal 总大小无关 —— 长任务（几十 MB）也能快速响应。
func readAgentHistoryPage(path string, offset, limit int) (*agentHistoryPage, error) {
	if limit <= 0 {
		limit = defaultAgentHistoryPage
	}
	if limit > maxAgentHistoryPage {
		limit = maxAgentHistoryPage
	}
	if offset < 0 {
		offset = 0
	}

	msgs, err := readLatestAgentMessages(path)
	if err != nil {
		return nil, err
	}
	entries := agentItemsFromMessages(msgs)
	total := len(entries)

	// offset 从「最新」往「最老」数更符合用户预期（先看最近的），
	// 但返回顺序仍按时间正序，便于 UI 直接拼接。
	// 这里把 offset 定义为「从最新往回的偏移」，窗口取 [total-offset-limit, total-offset)。
	end := total - offset
	if end < 0 {
		end = 0
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	page := &agentHistoryPage{
		Entries: entries[start:end],
		Total:   total,
		Offset:  offset,
		Limit:   limit,
		HasMore: start > 0,
	}
	return page, nil
}

// readLatestAgentMessages 从 journal 尾部找出最后一条含 messages 的 state 行。
//
// 为什么从尾部扫：state 行每轮都落，越后的越完整；且文件可能几十 MB，
// 全量 ReadAll 再取最后一条会浪费内存与时间。倒序扫描用一个行缓冲区，
// 命中即停（通常只读最后几百 KB）。
func readLatestAgentMessages(path string) ([]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}

	// 从尾部按块倒着读，收集完整行（倒序）。命中 state 即停。
	// 单条 state 行可能很大（实测 ~141KB），块大小取 1MB 覆盖绝大多数情况。
	const chunk = 1 << 20
	var (
		tail   []byte
		pos    = size
		result []json.RawMessage
	)
	for pos > 0 && result == nil {
		readSize := int64(chunk)
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		buf := make([]byte, readSize)
		if _, err := f.ReadAt(buf, pos); err != nil {
			return nil, err
		}
		tail = append(buf, tail...)

		// 从 tail 尾部往前找 state 行。注意第一条可能被截断（在块边界），
		// 所以跳过 tail 的**最前面**那一段不完整行。
		for {
			idx := lastIndexByte(tail, '\n')
			if idx < 0 {
				break
			}
			line := tail[idx+1:]
			tail = tail[:idx]
			// 跳过空行与不含 "state" 的行（快速过滤，避免整行 json 解析）
			if len(line) == 0 || !containsStateType(line) {
				continue
			}
			msgs, ok := extractStateMessages(line)
			if ok {
				result = msgs
				break
			}
		}
		// 防止无限增长：tail 保留最后 4MB 足够容纳一条超大 state 行。
		if len(tail) > 4*chunk {
			tail = tail[len(tail)-4*chunk:]
		}
	}
	// 若整个文件是一条行（理论上不会），回退到全量扫。
	if result == nil {
		result = scanWholeFileForState(path)
	}
	return result, nil
}

// scanWholeFileForState 是兜底路径：逐行扫整个文件，保留最后一条 state。
func scanWholeFileForState(path string) []json.RawMessage {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var last []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // state 行可达数百 KB
	for sc.Scan() {
		if msgs, ok := extractStateMessages(sc.Bytes()); ok {
			last = msgs
		}
	}
	return last
}

// containsStateType 判断一行是否可能是 state 记录（避免对每行做完整 JSON 解析）。
func containsStateType(line []byte) bool {
	return strings.Contains(string(line), `"type":"state"`)
}

// extractStateMessages 从一条 journal 记录里取出 state.messages。
func extractStateMessages(line []byte) ([]json.RawMessage, bool) {
	var rec struct {
		Type  string `json:"type"`
		State *struct {
			Messages []json.RawMessage `json:"messages"`
		} `json:"state"`
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return nil, false
	}
	if rec.Type != "state" || rec.State == nil || len(rec.State.Messages) == 0 {
		return nil, false
	}
	return rec.State.Messages, true
}

// agentItemsFromMessages 把 LLM 消息序列转成 UI 可展示的线性历史。
//
// 映射规则（与 reducer 侧 agent items 的语义保持一致，便于 UI 复用渲染）：
//   - role=user        → 一条 user 条目（任务原文）
//   - role=assistant   → 一条 assistant 条目（正文），其 tool_calls 各展开成一条 tool 条目
//   - role=tool        → 不回填（结果已在对应 tool_calls 条目上，见下）
//
// 工具结果按 tool_call_id 关联回对应的 tool 条目，这样 UI 一次拿到「调用+结果」。
func agentItemsFromMessages(msgs []json.RawMessage) []agentHistoryEntry {
	// 先收集 tool 结果：call_id → result 文本
	results := make(map[string]string, len(msgs))
	for _, raw := range msgs {
		var m struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		}
		if err := json.Unmarshal(raw, &m); err != nil || m.Role != "tool" {
			continue
		}
		results[m.ToolCallID] = contentToText(m.Content)
	}

	out := make([]agentHistoryEntry, 0, len(msgs))
	for _, raw := range msgs {
		var m struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID string `json:"id"`
				// 两种形态并存（踩过）：hai-harness 的 core.ToolCall 是**扁平**的
				//（name/arguments 直接在对象上），而 OpenAI SDK 兼容格式是
				// 嵌套的 function.name/function.arguments。两种都要认。
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Function  struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		switch m.Role {
		case "user":
			out = append(out, agentHistoryEntry{Kind: "user", Text: contentToText(m.Content), Index: len(out)})
		case "assistant":
			if txt := contentToText(m.Content); strings.TrimSpace(txt) != "" {
				out = append(out, agentHistoryEntry{Kind: "assistant", Text: txt, Index: len(out)})
			}
			for _, tc := range m.ToolCalls {
				name, args := tc.Name, tc.Arguments
				if name == "" {
					name = tc.Function.Name
				}
				if args == "" {
					args = tc.Function.Arguments
				}
				out = append(out, agentHistoryEntry{
					Kind:   "tool",
					ToolID: tc.ID,
					Name:   name,
					Args:   args,
					Result: results[tc.ID],
					Index:  len(out),
				})
			}
		}
	}
	return out
}

// contentToText 把 core.Message 的 content（可能是字符串、或 [{type:text,content}]）
// 归一成纯文本。多模态内容里非文本块（图片等）以占位符表示，避免丢信息又撑大 payload。
func contentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// 形式 1：纯字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// 形式 2：[{type,content}]
	var blocks []struct {
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Content != "" {
				b.WriteString(blk.Content)
				continue
			}
			if blk.Type != "" && blk.Type != "text" {
				fmt.Fprintf(&b, "[%s]", blk.Type)
			}
		}
		return b.String()
	}
	return string(raw)
}

// lastIndexByte 返回 b 中最后一个 c 的下标（-1 表示没有）。
func lastIndexByte(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}
