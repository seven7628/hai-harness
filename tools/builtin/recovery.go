package builtin

import "fmt"

// recoveryError formats tool failures as actionable instructions for the model.
// Keep this text in the tool result so the next model turn can recover without
// guessing whether a side effect occurred or whether the same call is safe.
//
// 精简格式（4 字段）：tool 名模型已知（结果挂在对应 tool_call 下）、
// changed/retryable/retry_with_same_arguments 现网取值恒为 false/true/false
// （engine.parseToolError 缺省即此），不再逐条输出。
func recoveryError(code, next, detail, fix string) error {
	return fmt.Errorf("TOOL_ERROR\ncode: %s\nnext_action: %s\ndetail: %s\nfix: %s", code, next, detail, fix)
}

func invalidJSON(tool, detail string) error {
	return recoveryError("invalid_arguments", "correct_arguments", detail, "Send valid JSON matching the tool schema; do not repeat the same arguments.")
}

func requiredArgument(tool, name string) error {
	return recoveryError("missing_required_argument", "correct_arguments", name+" is required", "Provide the missing argument and retry.")
}
