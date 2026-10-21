// reasoning_stream.go —— 「思考文本叫什么」的兼容层（2026-10 真机实证）。
//
// 触发场景：opendesign 网关（deepseek-v4.1-flash 等）把流式思考文本放在
// choices[].delta.**reasoning**（+ reasoning_details），而 SDK 结构体只认
// **reasoning_content**，于是 json.Unmarshal 阶段字段被直接丢弃 —— 表现为
// 「reasoning delta 事件 0 个、LLMEnd.Reasoning 恒空」，但 usage 里的
// reasoning_tokens 照常有值（计费口径在另一个字段上，所以不会报错，只是文本没了）。
//
// 为什么做成**传输层**（与 upstream_cost.go 同一层、同一理由）：SDK 的
// streamReader.Recv() 内部完成 unmarshal，调用方拿不到原始 JSON，也没有注入点；
// 两个发送通道（SDK 通道 / minimal 自建通道）唯一共同的交汇处就是 resp.Body。
// 在这一层把帧体规范化后，两条通道一次覆盖，且不改 SDK。
//
// 归一化方向固定为 reasoning_content（SDK 已建模的字段），其余形态只做**输入别名**：
//
//	reasoning_content（原样，优先级最高）> reasoning > reasoning_details[].text > thinking
//
// 只增不改：原字段全部保留（SDK 忽略未知键），审计/排查仍能看到上游真实形态。
// 且仅当帧里**没有** reasoning_content 时才重写 —— 已是标准形态的端点一个字节都不动。
package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// reasoningSSEMaxLine SSE 单行上限（思考帧可能整段下发；超长帧按行截断会破坏 JSON，
// 这里只作为 bufio 的初始容量，实际由 ReadBytes 按需增长，不会截断）。
const reasoningSSEMaxLine = 64 << 10

// reasoningStreamTransport 在响应体上挂「思考字段名归一化」。
// 只处理 text/event-stream；其余响应（模型列表、非流式 JSON）原样透传。
type reasoningStreamTransport struct{ base http.RoundTripper }

// Unwrap 暴露被包装的 transport（BaseTransport 据此穿透，见 upstream_cost.go）。
func (t *reasoningStreamTransport) Unwrap() http.RoundTripper { return t.base }

func (t *reasoningStreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	if !isEventStreamResponse(resp) {
		return resp, nil
	}
	resp.Body = &reasoningStreamBody{ReadCloser: resp.Body, br: bufio.NewReaderSize(resp.Body, reasoningSSEMaxLine)}
	return resp, nil
}

func isEventStreamResponse(resp *http.Response) bool {
	if resp.Header == nil {
		return false
	}
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "event-stream")
}

// WrapReasoningStreamClient 就地把客户端包上思考字段归一化（保留 Transport / Timeout / Jar）。
// nil 进 nil 出；已包过则原样返回（幂等，便于构造点重复调用）。
func WrapReasoningStreamClient(c *http.Client) *http.Client {
	if c == nil {
		return nil
	}
	if _, done := c.Transport.(*reasoningStreamTransport); done {
		return c
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out := *c
	out.Transport = &reasoningStreamTransport{base: base}
	return &out
}

// reasoningStreamBody 逐行改写 SSE 帧：攒够一行就尝试归一化，其余原样输出。
// 语义上只是 Read 的搬运工（不缓冲整条流、不等 EOF），首字延迟不受影响。
type reasoningStreamBody struct {
	io.ReadCloser

	br   *bufio.Reader
	out  []byte // 已产出但尚未被调用方读走的字节
	done bool
	err  error // 底层非 EOF 错误，待 out 读空后返回
}

func (b *reasoningStreamBody) Read(p []byte) (int, error) {
	for len(b.out) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		if b.done {
			return 0, io.EOF
		}
		line, err := b.br.ReadBytes('\n')
		if len(line) > 0 {
			// 流末尾的残留行（readErr=EOF 但确有字节）同样尝试解析：
			// 它常是携带 finish_reason/usage 的收尾帧，不能直接丢。
			b.out = append(b.out, rewriteSSELine(line)...)
		}
		if err != nil {
			b.done = true
			if err != io.EOF {
				b.err = err
			}
			if len(b.out) == 0 {
				continue // 交给循环顶部分派 EOF / err
			}
		}
	}
	n := copy(p, b.out)
	b.out = b.out[n:]
	return n, nil
}

// rewriteSSELine 归一化单个 SSE 行。非 data: 行、[DONE]、解析失败、无需改写 → 原样返回。
func rewriteSSELine(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 || payload[0] != '{' {
		return line // [DONE] 或空帧
	}
	// 快速路径（长回答的每帧都走这里，必须零成本跳过）：已经是标准形态 → 直接透传；
	// 连别名字样都没有 → 不必解析 JSON。`"reasoning` 带前引号做边界，可同时覆盖
	// reasoning / reasoning_details（后者是 `"reasoning"` 前缀，靠引号区分不开，故取前缀）。
	if bytes.Contains(payload, []byte(`"reasoning_content"`)) {
		return line
	}
	if !bytes.Contains(payload, []byte(`"reasoning`)) && !bytes.Contains(payload, []byte(`"thinking`)) {
		return line
	}
	rewritten, changed := normalizeReasoningPayload(payload)
	if !changed {
		return line
	}
	// 保留原始行尾（"\n" / "\r\n" / 无尾换行的残留行）
	tail := line[len(bytes.TrimRight(line, "\r\n")):]
	out := make([]byte, 0, len("data: ")+len(rewritten)+len(tail))
	out = append(out, "data: "...)
	out = append(out, rewritten...)
	out = append(out, tail...)
	return out
}

// normalizeReasoningPayload 把一帧里的思考文本别名补成 reasoning_content。
// 返回 changed=false 表示该帧不需要（或无法）改写，调用方据此原样透传。
//
// 用 map[string]json.RawMessage 逐层解析：未触碰的键保持**原始字节**
// （大整数、浮点精度、unknown 扩展字段都不会因重编码而变样）。
func normalizeReasoningPayload(payload []byte) ([]byte, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(payload, &top) != nil {
		return nil, false
	}
	choicesRaw, ok := top["choices"]
	if !ok {
		return nil, false
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(choicesRaw, &choices) != nil {
		return nil, false
	}

	changed := false
	for _, choice := range choices {
		deltaRaw, ok := choice["delta"]
		if !ok {
			continue // 非流式形态（message）不在本层职责内
		}
		var delta map[string]json.RawMessage
		if json.Unmarshal(deltaRaw, &delta) != nil {
			continue
		}
		if _, exists := delta["reasoning_content"]; exists {
			continue // 标准形态：一个字节都不动
		}
		text, ok := reasoningTextFromDelta(delta)
		if !ok {
			continue
		}
		encoded, err := json.Marshal(text)
		if err != nil {
			continue
		}
		delta["reasoning_content"] = encoded
		newDelta, err := json.Marshal(delta)
		if err != nil {
			continue
		}
		choice["delta"] = newDelta
		changed = true
	}
	if !changed {
		return nil, false
	}
	// 必须写回 top["choices"]：choices 是解析出的副本，只改元素不会影响原始顶层 map。
	encodedChoices, err := json.Marshal(choices)
	if err != nil {
		return nil, false
	}
	top["choices"] = encodedChoices
	out, err := json.Marshal(top)
	if err != nil {
		return nil, false
	}
	return out, true
}

// reasoningTextFromDelta 按优先级取出这一帧的思考文本（空串视为「本帧没有」）。
//
//   - reasoning         ：opendesign 系网关（本层的主要修复对象）
//   - reasoning_details ：openrouter 系；只取**明文**条目（type=reasoning.text），
//     type=reasoning.encrypted 只有密文 → 不伪造文本（gpt-6-luna 实测如此）
//   - thinking          ：部分自建端点的别名
func reasoningTextFromDelta(delta map[string]json.RawMessage) (string, bool) {
	for _, key := range []string{"reasoning", "thinking"} {
		raw, ok := delta[key]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s, true
		}
	}
	raw, ok := delta["reasoning_details"]
	if !ok {
		return "", false
	}
	var details []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &details) != nil {
		return "", false
	}
	var sb strings.Builder
	for _, d := range details {
		if d.Text == "" {
			continue
		}
		// 空 type 视为明文（宽松兼容）；显式非明文（encrypted/summary 等）跳过。
		if d.Type != "" && d.Type != "reasoning.text" && d.Type != "thinking" {
			continue
		}
		sb.WriteString(d.Text)
	}
	if sb.Len() == 0 {
		return "", false
	}
	return sb.String(), true
}
