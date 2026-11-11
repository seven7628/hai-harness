package session

// 通用「顶层数组切分器」：把一个 JSON 对象切成「前缀 + 数组元素片段 + 后缀」，
// 使得**拼接结果与原字节逐字节相同**。
//
// 为什么需要（2026-11-11，CHECKPOINT_V3_DESIGN §6.2）：
//
// sidecar 是 append-only 的「一个 id → 一个 payload（最后写的赢）」。块内容一变，
// 整个 payload 被重新追加一遍 —— 追加字节 = Σ(每个版本的大小)，而不是增量。
// 实测：running 卡片每个事件整块重写（单张最大 1.2MB）→ 写放大 150–275×，
// 一个会话能攒出 1.1GB 纯垃圾。
//
// 解法：让「只增」的块（agent 卡片的 items 数组）以**片段**形式落盘。
// 同一 id 的多个片段在索引里各自占一格；读时按 key 顺序拼接 → 与今天读出的
// payload 逐字节等价 → **上层（reducer / bridge / 前端）一行都不用改**。
//
// 关键约束：**逐字节还原**。因为 sidecar 的完整性校验是「读出来的字节的摘要
// 与记录里的摘要比对」，重组后的字节必须与写入时完全一致。
//
// 因此切分必须：
//  1. 只在**顶层**数组字段上切（不碰嵌套）；
//  2. 保留每个元素的**原始字节**（不重新序列化 —— 重新序列化会改变字段顺序，
//     摘要就变了）；
//  3. 元素之间原样保留分隔符。

import (
	"bytes"
	"encoding/json"
	"errors"
)

// splitErr 表示 payload 不是可切分的 JSON 对象。
var splitErr = errors.New("payload is not a JSON object")

// splitTopLevelArray 把 payload 切成片段。
//
// 返回值：
//   - frags 非 nil：按顺序拼接得到与 payload **逐字节相同**的结果；
//     frags[0] 是「到数组左括号为止」的前缀，frags[1..n-2] 是数组元素
//     （元素自带其后的逗号，除最后一个外），frags[n-1] 是「右括号及其后」的后缀。
//   - frags == nil：不该切（不是对象 / 没有顶层数组 / 数组太短 / 数组元素太碎）。
//
// minArrayBytes 是「数组内容多小才值得切」的下限：太短的数组切了反而碎片化
// （每个片段都要占一条索引 + 一次 IO）。
func splitTopLevelArray(payload []byte, minArrayBytes int) [][]byte {
	elems, ok := topLevelArrayElements(payload)
	if !ok {
		return nil
	}
	// 数组总长（不含左右括号）小于门槛 → 不切。
	inner := elems.innerBytes
	if len(inner) < minArrayBytes {
		return nil
	}
	// 元素太少 → 切了收益不大（每片都要一条索引 + 一次读）。
	if len(elems.starts) < 2 {
		return nil
	}
	// 前缀：payload[: 数组左括号+1]
	prefix := payload[:elems.arrayStart+1]
	// 片段：每个元素 + 其后的逗号（按原始字节保留）
	frags := make([][]byte, 0, len(elems.starts)+2)
	frags = append(frags, prefix)
	for i, s := range elems.starts {
		end := elems.ends[i]
		if i+1 < len(elems.starts) {
			// 元素后面紧跟的逗号（可能夹着空白）也归这一片，保证拼接逐字节相同。
			end = elems.commaEnd[i]
		}
		frags = append(frags, payload[s:end])
	}
	// 后缀：数组右括号及其后的一切
	frags = append(frags, payload[elems.arrayEnd:])
	return frags
}

// topLevelArrayElements 定位 payload 里**最长的顶层数组字段**，并给出它的
// 元素边界。
type arrayElements struct {
	arrayStart int    // '[' 的位置
	arrayEnd   int    // ']' 的位置（不含）
	innerBytes []byte // 去掉左右括号后的内容
	starts     []int  // 各元素的起始偏移
	ends       []int  // 各元素的结束偏移（不含尾随逗号）
	commaEnd   []int  // 各元素「连同其后逗号」的结束偏移（最后一个与 ends 相同）
}

// topLevelArrayElements 扫描顶层对象，找出最长的顶层数组值。
//
// 用 json.Decoder + InputOffset 定位每个字段值的**原始字节区间**：
// json.Unmarshal 到 map 会丢字段顺序（map 无序），而字段顺序影响字节摘要，
// 所以必须保留原始字节。
func topLevelArrayElements(payload []byte) (arrayElements, bool) {
	trimmed := bytes.TrimLeft(payload, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return arrayElements{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	// 读掉 '{'
	if _, err := dec.Token(); err != nil {
		return arrayElements{}, false
	}
	var best arrayElements
	bestLen := -1
	for dec.More() {
		// 键
		tok, err := dec.Token()
		if err != nil {
			return arrayElements{}, false
		}
		if _, ok := tok.(string); !ok {
			return arrayElements{}, false
		}
		keyEnd := int(dec.InputOffset())
		// 值：Decode 到 RawMessage 保留原始字节
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return arrayElements{}, false
		}
		valEnd := int(dec.InputOffset())
		// 值的原始区间：keyEnd 之后是 ':'（可能夹空白），再是值
		valStart := skipColonSpace(payload, keyEnd, valEnd)
		if valStart >= valEnd {
			continue
		}
		if payload[valStart] != '[' {
			continue
		}
		// 找到配对的 ']'（顶层数组内部只可能有字符串/数字/对象/数组）
		closeIdx, ok := matchArrayEnd(payload, valStart, valEnd)
		if !ok {
			continue
		}
		inner := payload[valStart+1 : closeIdx]
		if len(inner) <= bestLen {
			continue
		}
		starts, ends, commaEnd, ok := splitArrayElements(inner, valStart+1)
		if !ok {
			continue
		}
		bestLen = len(inner)
		best = arrayElements{
			arrayStart: valStart,
			arrayEnd:   closeIdx,
			innerBytes: inner,
			starts:     starts,
			ends:       ends,
			commaEnd:   commaEnd,
		}
	}
	if bestLen < 0 {
		return arrayElements{}, false
	}
	return best, true
}

// skipColonSpace 从 keyEnd 起跳过 ':' 与空白，返回值的起始偏移。
func skipColonSpace(b []byte, from, limit int) int {
	i := from
	for i < limit {
		switch b[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case ':':
			i++
			for i < limit && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
				i++
			}
			return i
		default:
			return i
		}
	}
	return i
}

// matchArrayEnd 找到与 payload[start]=='[' 配对的 ']'，返回其位置。
func matchArrayEnd(b []byte, start, limit int) (int, bool) {
	depth := 0
	inStr := false
	esc := false
	for i := start; i < limit; i++ {
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
		case '[', '{':
			depth++
		case ']', '}':
			depth--
			if depth == 0 && c == ']' {
				return i, true
			}
			if depth < 0 {
				return 0, false
			}
		}
	}
	return 0, false
}

// splitArrayElements 把数组内容（不含左右括号）按**顶层逗号**切开。
// 返回各元素的 [start,end) 绝对偏移，以及「连同尾随逗号」的结束偏移。
func splitArrayElements(inner []byte, base int) (starts, ends, commaEnd []int, ok bool) {
	// 预分配：一张 running 卡片有几百条 item，三个切片各 append 几百次 ——
	// 不预分配会反复扩容+拷贝（Go 的 append 扩容是 2 倍，拷贝量 O(n)）。
	// 这里按「内容长度 / 最小元素尺寸」估一个上界，避免扩容抖动。
	capHint := len(inner)/8 + 8
	starts = make([]int, 0, capHint)
	ends = make([]int, 0, capHint)
	commaEnd = make([]int, 0, capHint)
	depth := 0
	inStr := false
	esc := false
	elemStart := 0
	hasElem := false
	for i := 0; i < len(inner); i++ {
		c := inner[i]
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
			if !hasElem {
				elemStart = i
				hasElem = true
			}
		case '[', '{':
			depth++
			if !hasElem {
				elemStart = i
				hasElem = true
			}
		case ']', '}':
			depth--
			if !hasElem {
				elemStart = i
				hasElem = true
			}
		case ',':
			if depth == 0 {
				// 顶层逗号 → 一个元素结束
				if !hasElem {
					elemStart = i
				}
				starts = append(starts, base+elemStart)
				ends = append(ends, base+i)
				commaEnd = append(commaEnd, base+i+1)
				hasElem = false
			}
		default:
			if !hasElem && c != ' ' && c != '\t' && c != '\r' && c != '\n' {
				elemStart = i
				hasElem = true
			}
		}
	}
	// 最后一个元素
	trimmedEnd := len(inner)
	for trimmedEnd > 0 {
		c := inner[trimmedEnd-1]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			trimmedEnd--
			continue
		}
		break
	}
	if hasElem || trimmedEnd > elemStart {
		starts = append(starts, base+elemStart)
		ends = append(ends, base+trimmedEnd)
		commaEnd = append(commaEnd, base+trimmedEnd)
	}
	if len(starts) == 0 {
		return nil, nil, nil, false
	}
	return starts, ends, commaEnd, true
}
