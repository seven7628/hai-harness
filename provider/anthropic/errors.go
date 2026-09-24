package anthropic

import (
	"errors"

	"github.com/seven7628/hai-harness/provider"

	"github.com/anthropics/anthropic-sdk-go"
)

// classifyAnthropicError 错误分类：
//   - 4xx → 公共分类器（429 限流 / 408/409/425 瞬时 / 其余永久 + 上下文超限识别）
//   - 非 APIError → 文本识别：限流（流中段 200+error frame 的 429）→ 上下文超限
func classifyAnthropicError(err error) error {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 {
		return provider.ClassifyHTTPError(apiErr.StatusCode, provider.UnwrapErrorBody(err))
	}
	// SSE 帧 / 非 JSON 错误体 → 可读文案（与 openai/responses 同一归一，见 UnwrapErrorBody）
	err = provider.UnwrapErrorBody(err)
	if provider.IsRateLimitText(err.Error()) {
		return &provider.RateLimitError{StatusCode: 0, Err: err}
	}
	return provider.DetectContextExceeded(err)
}
