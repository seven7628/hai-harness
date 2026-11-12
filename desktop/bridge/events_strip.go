package main

// 事件日志的**字段裁剪**（2026-11-11）。
//
// ## 为什么
//
// 事件日志（~/.go-code/events/<wsKey>/<sid>.jsonl）是会话的**唯一事实来源**，
// 实测一个会话 1126MB / 441,863 行：
//
//	tool_run_end   523.5 MB  ← 工具结果（真实内容，不可去）
//	llm_end        295.6 MB  ← 模型输出（真实内容，不可去）
//	tool_run_start  133.4 MB  ← 其中 arguments 82MB —— **与 tool_run_end 重复**
//	agent_end      114.5 MB  ← 子智能体的 ToolCalls —— **与子智能体 journal 重复**
//
// 两项冗余合计约 17%（全目录 ~1GB）。
//
// ## 为什么裁剪只发生在「落盘」这一侧
//
// stdout 那一份是**实时**数据流：前端在工具开始时就要显示参数（tool_run_start
// 的 arguments）；agent_end 的 tool_calls 前端也不用，但为了「文件与实时流
// 同形」不做特殊处理。落盘的那一份只服务于**冷恢复/重放**：
//
//   - tool_run_start 的 arguments：冷恢复时工具块/条目的参数改由
//     **tool_run_end 的 arguments** 回填（reducer 侧配套改动）；
//   - agent_end 的 tool_calls：子智能体的完整历史在**子智能体自己的 journal**
//     里（agent_history 命令按页取），事件流这份是纯重复，**无任何消费方**。
//
// ## 安全性
//
// 只裁**顶层**字段（不碰嵌套同名键）；逐字节扫描 JSON 字符串/转义；
// 找不到或结构不符时**原样返回**（绝不产出半截 JSON —— 坏行会被读侧跳过，
// 但那意味着丢事件）。

import "bytes"

// stripTopLevelField 从一个 JSON **对象**里移除顶层字段（连同它前面的逗号）。
//
// 返回的切片可能复用入参的底层数组（不复制）——调用方要长期持有就自行拷贝。
// 字段不存在 / 不是对象 / 结构异常 → 原样返回。
func stripTopLevelField(line []byte, key string) []byte {
	want := `"` + key + `"`
	// ① 定位顶层对象的键值序列
	i := 0
	// 跳过空白
	for i < len(line) && isJSONSpace(line[i]) {
		i++
	}
	if i >= len(line) || line[i] != '{' {
		return line // 不是对象 → 不动
	}
	depth := 0
	inStr := false
	esc := false
	keyStart := -1 // 当前顶层键的起始位置（含引号）
	valStart := -1
	for i < len(line) {
		c := line[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
				if depth == 1 && keyStart >= 0 && valStart < 0 {
					// 刚读完一个顶层键 → 检查是不是目标
					if bytes.Equal(line[keyStart:i+1], []byte(want)) {
						// 跳过冒号与空白，找值的起点
						j := i + 1
						for j < len(line) && isJSONSpace(line[j]) {
							j++
						}
						if j >= len(line) || line[j] != ':' {
							return line // 结构异常 → 不动
						}
						j++
						for j < len(line) && isJSONSpace(line[j]) {
							j++
						}
						valStart = j
					}
				}
			}
			i++
			continue
		}
		switch c {
		case '"':
			inStr = true
			if depth == 1 && valStart < 0 {
				keyStart = i
			}
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				// 顶层对象结束
				goto done
			}
		case ':':
			if depth == 1 && valStart < 0 && keyStart >= 0 {
				// 记住冒号位置（用于算值的起点）
				_ = valStart
			}
		}
		i++
	}
done:
	if valStart < 0 {
		return line // 没找到目标字段
	}
	// ② 找到值的结束位置
	valEnd, ok := jsonValueEnd(line, valStart)
	if !ok {
		return line // 结构异常 → 不动
	}
	// ③ 确定要删除的区间：字段 + 它的键，以及可能的前导/后随逗号
	delStart := keyStart
	delEnd := valEnd
	// 向前吃掉紧邻的逗号（优先吃前面的，避免留下尾逗号）
	pre := delStart - 1
	for pre >= 0 && isJSONSpace(line[pre]) {
		pre--
	}
	if pre >= 0 && line[pre] == ',' {
		delStart = pre
	} else {
		// 没有前导逗号 → 吃掉后面的逗号
		post := delEnd
		for post < len(line) && isJSONSpace(line[post]) {
			post++
		}
		if post < len(line) && line[post] == ',' {
			delEnd = post + 1
		}
	}
	out := make([]byte, 0, len(line)-(delEnd-delStart))
	out = append(out, line[:delStart]...)
	out = append(out, line[delEnd:]...)
	return out
}

// jsonValueEnd 返回从 start 开始的 JSON 值的结束位置（不含）。
func jsonValueEnd(b []byte, start int) (int, bool) {
	if start >= len(b) {
		return 0, false
	}
	switch b[start] {
	case '"':
		inStr := false
		esc := false
		for i := start; i < len(b); i++ {
			c := b[i]
			if inStr {
				switch {
				case esc:
					esc = false
				case c == '\\':
					esc = true
				case c == '"':
					return i + 1, true
				}
				continue
			}
			if c == '"' {
				inStr = true
			}
		}
		return 0, false
	case '{', '[':
		depth := 0
		inStr := false
		esc := false
		for i := start; i < len(b); i++ {
			c := b[i]
			if inStr {
				switch {
				case esc:
					esc = false
				case c == '\\':
					esc = true
				case c == '"':
					inStr = false
				}
				continue
			}
			switch c {
			case '"':
				inStr = true
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
		}
		return 0, false
	default: // 数字 / true / false / null
		i := start
		for i < len(b) && b[i] != ',' && b[i] != '}' && b[i] != ']' && !isJSONSpace(b[i]) {
			i++
		}
		if i == start {
			return 0, false
		}
		return i, true
	}
}

// isJSONSpace 报告是否是 JSON 空白字符。
func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}
