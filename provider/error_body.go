package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 「错误体不是纯 JSON」时的可读化（三协议共用）。
//
// 背景（2026-09-22 实测）：部分网关（OpenCode Go / zen）在**流式**请求失败时，把上游错误
// 以 SSE 帧形态塞进 4xx 响应的 body：
//
//	HTTP/1.1 400
//	data: {"error":{"param":"","type":"server_error","message":"Streaming response failed: [400] Invalid request parameters"}}
//
// go-openai 只按纯 JSON 解析错误体，解析失败就把**解析错误**当文案拼进错误：
//
//	error, status code: 400, status: 400 Bad Request, message: invalid character 'd' looking
//	for beginning of value, body: data: {"error":{...}}
//
// 用户看到的是一句 JSON 解析报错，真正的上游信息（message / param）埋在里面 —— 既没法
// 排查也没法让分类器（限流/上下文超限/供应商抖动）识别。这里剥掉 SSE 帧后取 JSON，
// 重建为 `error, status code: 400, status: 400 Bad Request, message: <上游 message>`
// （保留 status code 前缀：既有文案与分类器都按它工作）。
//
// 只处理「文案里带 `, body: ` 尾巴」的错误（go-openai RequestError 形态）；其余错误原样返回。
// 解析不出 JSON 时也原样返回 —— 宁可粗，不可把未知错误改写成错的话。
func UnwrapErrorBody(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	bodyAt := strings.Index(msg, ", body: ")
	if bodyAt < 0 {
		return err
	}
	body := msg[bodyAt+len(", body: "):]
	upstream := parseErrorBodyMessage(body)
	if upstream == "" {
		return err
	}
	prefix := msg[:bodyAt]
	// 保留 "..., message: " 之前的原文（status code / status 等），只换 message 段
	if mAt := strings.Index(prefix, ", message: "); mAt >= 0 {
		prefix = prefix[:mAt]
	}
	rebuilt := prefix + ", message: " + upstream
	if rebuilt == msg {
		return err
	}
	return fmt.Errorf("%s", rebuilt)
}

// parseErrorBodyMessage 从错误体原文里取上游自身的信息（SSE 帧 / 纯 JSON / 带前后噪声都试）：
// 命中返回可读文本（含 param 时附在末尾），否则返回空串。
func parseErrorBodyMessage(body string) string {
	payload := firstJSONValue(body)
	if payload == "" {
		return ""
	}
	var envelope struct {
		Error struct {
			Message string          `json:"message"`
			Param   json.RawMessage `json:"param"`
			Code    string          `json:"code"`
			Type    string          `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(payload), &envelope) != nil {
		return ""
	}
	msg := strings.TrimSpace(envelope.Error.Message)
	if msg == "" {
		msg = strings.TrimSpace(envelope.Message)
	}
	if msg == "" {
		return ""
	}
	// param 可能是字符串（OpenAI 形态）或对象；只在消息里没提到、且它像「参数标识」
	// （短、单行）时补上 —— 像 "messages[0] role is not supported" 这种信息量很大，
	// 而一整句错误描述则与 message 重复，追加只会变噪音
	if param := rawJSONText(envelope.Error.Param); param != "" && !strings.Contains(msg, param) &&
		len(param) <= 64 && !strings.ContainsAny(param, "\n\r") {
		msg += " (param: " + param + ")"
	}
	return msg
}

// rawJSONText 把 JSON 原始值还原成短文本（字符串去引号；空值/空串返回空）。
func rawJSONText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	t := strings.TrimSpace(string(raw))
	if t == "null" {
		return ""
	}
	if len(t) > 120 {
		t = t[:120] + "…"
	}
	return t
}

// firstJSONValue 从 SSE 帧 / 混合文本里取出第一个完整 JSON 值（对象或数组）。
// 逐字符扫描做括号配对（字符串内的括号不计数），因此 data: 前缀、event: 行、
// 尾部的空行/多余帧都不会干扰；取不到返回空串。
func firstJSONValue(s string) string {
	i := strings.IndexAny(s, "{[")
	if i < 0 {
		return ""
	}
	open := s[i]
	closeCh := byte('}')
	if open == '[' {
		closeCh = ']'
	}
	depth := 0
	inStr := false
	escaped := false
	for j := i; j < len(s); j++ {
		c := s[j]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return s[i : j+1]
			}
		}
	}
	return ""
}
