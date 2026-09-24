package responses

import (
	"errors"

	"github.com/seven7628/hai-harness/provider"

	"github.com/sashabaranov/go-openai"
)

// classifyResponsesError 错误分类：
//   - 4xx → 公共分类器（429 限流 / 408/409/425 瞬时 / 其余永久 + 上下文超限识别）
//   - 非 APIError → 文本识别：限流（流中段 200+error frame 的 429）→ 上下文超限
func classifyResponsesError(err error) error {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPStatusCode >= 400 {
		return provider.ClassifyHTTPError(apiErr.HTTPStatusCode, provider.UnwrapErrorBody(err))
	}
	// RequestError（错误体非纯 JSON：网关把上游错误塞进 data: {...} 时 go-openai 只报
	// JSON 解析错误）：状态码取自原错误分类（400 参数错误不该当 generic 反复重试），
	// 文案换成可读的上游信息（provider.UnwrapErrorBody）
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) && reqErr.HTTPStatusCode > 0 {
		return provider.ClassifyHTTPError(reqErr.HTTPStatusCode, provider.UnwrapErrorBody(err))
	}
	if provider.IsRateLimitText(err.Error()) {
		return &provider.RateLimitError{StatusCode: 0, Err: err}
	}
	// 流中错误帧 / 非 APIError：文本识别上下文超限
	return provider.DetectContextExceeded(provider.UnwrapErrorBody(err))
}
