package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// maxEventLineBytes 事件日志单行读取上限（Scanner buffer）。
// 与 checkpoint 懒迁移路径同口径（必须一致）：两条路径读同一份 events.jsonl，
// 上限不一致会导致同一会话在「有 record」与「无 record」时恢复出不同结果
// （超限行被一条路径静默丢弃）。
// 取值需覆盖最大单行事件：含图片的 user_inputs_consumed / tool response
// （read_file 读图 / 截图）—— 图片 data URL 单张内嵌上限 3MiB（tools 引擎闸门），
// 一条消息多张图 + 转义开销 → 16MiB 留足余量。
const maxEventLineBytes = 16 * 1024 * 1024

// buildSnapshot 冷启动恢复：把事件日志聚合为单行 command_response 内的紧凑快照
// （替代逐行重放，修复大量事件行造成的 stdout 洪峰/响应延迟与前端逐行 dispatch 的 O(N×M) 拷贝）。
// 事件保持文件顺序的单个有序数组；每元素保留 event_type 与全部字段
// （仅去掉 session_id 省空间——前端已知 sid）。无日志/无有效事件 → (nil, false)。
//
// 性能：避免每行 Unmarshal→Marshal 两次序列化——直接在原始行字节上删除
// `"session_id":"..."` 字段（字符串扫描），仅坏行才回退 Unmarshal 校验。
func (m *manager) buildSnapshot(wsPath, id string) (map[string]any, bool) {
	return buildSnapshotFromPath(filepath.Join(wsEventsDir(wsPath), id+".jsonl"))
}

// buildSnapshotFromPath 从任意事件日志文件聚合 snapshot（buildSnapshot 的底层实现；
// cron 独立历史目录复用同一管线，保证详情页与主对话页恢复同构）。
func buildSnapshotFromPath(evPath string) (map[string]any, bool) {
	f, err := os.Open(evPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLineBytes)
	var events []json.RawMessage
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		raw := []byte(line)
		// 快速路径：原始字节删 session_id（省 Unmarshal/Marshal）
		cleaned := removeJSONField(raw, "session_id")
		if cleaned != nil && !bytes.Contains(cleaned, []byte(`"event_type"`)) {
			// 非事件行（缺 event_type）→ 跳过（与旧逻辑一致）
			continue
		}
		if cleaned == nil {
			// 慢路径：坏行/异常结构 → 校验 event_type，能恢复则保留
			var rec map[string]json.RawMessage
			if json.Unmarshal(raw, &rec) != nil || string(rec["event_type"]) == "" {
				continue
			}
			delete(rec, "session_id")
			cleaned, _ = json.Marshal(rec)
			if cleaned == nil {
				continue
			}
		}
		events = append(events, cleaned)
	}
	// 扫描错误（超长行 / IO 失败）：已读前缀不完整 —— 不能当作「会话就这一段」返回，
	// 否则前端会把截断后的历史当成全部（图片/工具结果整段消失且无提示）。
	// 与 migrateLegacyToCheckpoint 的 sc.Err() 检查同语义（两条路径同口径）。
	if err := sc.Err(); err != nil {
		slog.Warn("event snapshot truncated: scanner stopped early",
			"path", evPath, "events", len(events), "max_line_bytes", maxEventLineBytes, "error", err)
		return nil, false
	}
	if len(events) == 0 {
		return nil, false
	}
	return map[string]any{"events": events}, true
}

// removeJSONField 从 JSON 对象字节中删除指定键（顶层）。返回 nil 表示无法安全处理
// （非对象/键不存在/结构异常——调用方回退 Unmarshal 路径）。
// 实现：扫描 `"key":` 的键值对并跳过其值（支持字符串/数字/对象/数组/布尔/null），
// 拼接剩余部分。仅处理顶层（不递归），值内嵌套同名键不影响（按引号/括号配对跳过）。
func removeJSONField(raw []byte, key string) []byte {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") {
		return nil
	}
	keyPat := `"` + key + `"`
	out := make([]byte, 0, len(raw))
	i := 0
	// 跳过首 {
	for i < len(trimmed) && trimmed[i] != '{' {
		i++
	}
	out = append(out, trimmed[i]) // {
	i++
	first := true
	for i < len(trimmed) {
		// 跳过空白与逗号
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n' || trimmed[i] == '\r') {
			i++
		}
		if i >= len(trimmed) {
			break
		}
		if trimmed[i] == ',' {
			i++
			continue
		}
		if trimmed[i] == '}' {
			break
		}
		// 读键名
		if trimmed[i] != '"' {
			return nil // 异常结构
		}
		keyStart := i
		i++
		for i < len(trimmed) && trimmed[i] != '"' {
			if trimmed[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(trimmed) {
			return nil
		}
		i++ // 跳过闭合引号
		keyEnd := i
		// 跳过空白，检查冒号
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n' || trimmed[i] == '\r') {
			i++
		}
		if i >= len(trimmed) || trimmed[i] != ':' {
			return nil
		}
		i++ // 跳过冒号
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n' || trimmed[i] == '\r') {
			i++
		}
		// 读值（支持嵌套）
		valEnd, ok := skipJSONValue(trimmed, i)
		if !ok {
			return nil
		}
		// 判断是否目标键
		isTarget := string(trimmed[keyStart:keyEnd]) == keyPat
		if !isTarget {
			if !first {
				out = append(out, ',')
			}
			first = false
			out = append(out, trimmed[keyStart:valEnd]...)
		}
		i = valEnd
		// 跳过值后的空白与逗号（逗号在下一轮处理）
	}
	out = append(out, '}')
	return out
}

// skipJSONValue 跳过 JSON 值（字符串/数字/对象/数组/布尔/null），返回结束下标。
func skipJSONValue(s string, i int) (int, bool) {
	if i >= len(s) {
		return i, false
	}
	switch s[i] {
	case '"': // 字符串
		i++
		for i < len(s) {
			if s[i] == '\\' {
				i += 2
				continue
			}
			if s[i] == '"' {
				return i + 1, true
			}
			i++
		}
		return i, false
	case '{', '[': // 对象/数组
		open := s[i]
		close := byte('}')
		if open == '[' {
			close = ']'
		}
		depth := 0
		inStr := false
		for i < len(s) {
			c := s[i]
			if inStr {
				if c == '\\' {
					i += 2
					continue
				}
				if c == '"' {
					inStr = false
				}
				i++
				continue
			}
			switch c {
			case '"':
				inStr = true
			case open:
				depth++
			case close:
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
			i++
		}
		return i, false
	default: // 数字/布尔/null（到逗号或 } 结束）
		start := i
		for i < len(s) && s[i] != ',' && s[i] != '}' && s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
			i++
		}
		if i == start {
			return i, false
		}
		return i, true
	}
}
