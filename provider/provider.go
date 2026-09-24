package provider

import (
	"context"
	"errors"
	"github.com/seven7628/hai-harness/core"
)

type Provider interface {
	Stream(ctx context.Context, req *StreamRequest, handler func(e StreamEvent) error) error
}

// PermanentError 标记不可自动重试的错误（如参数校验失败、4xx 客户端错误）。
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// MarkPermanent 把错误包装为不可重试。
func MarkPermanent(err error) error {
	if err == nil || IsPermanent(err) {
		return err
	}
	return &PermanentError{Err: err}
}

// IsPermanent 判断错误是否不可重试。
func IsPermanent(err error) bool {
	var pe *PermanentError
	if errors.As(err, &pe) {
		return true
	}
	// 上下文超限同为不可重试（DetectContextExceeded 包装；重试只会再发同一份超限上下文）
	var ce *ContextExceededError
	return errors.As(err, &ce)
}

type StreamRequest struct {
	Model string

	// Provider 所属 provider 名（deepseek/openai/opencode/kimi/zhipu/anthropic/自定义）。
	// 供注册表 Lookup；零值 = 未知 provider，走注册表兜底默认。
	// 加字段不改签名 —— agents/session 的构造点零改动。
	Provider string

	// Config 请求级统一配置（思考/强度/输出上限/用量/推理回传）；nil = 全部厂商默认。
	Config *RequestConfig

	// SessionID 会话稳定标识（2026-09 新增）：Responses prompt cache key + session affinity
	// 头用（对齐 pi options.sessionId）。空 = 不启用（旧调用兼容）。bridge 装配时注入。
	SessionID string

	Messages []core.Message    // 抽象消息，由实现翻译为厂商 SDK 参数
	Tools    []core.ToolSchema // 厂商无关的工具描述，由实现翻译为 SDK 格式
}
