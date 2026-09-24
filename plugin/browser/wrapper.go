package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/mcp"
	"github.com/seven7628/hai-harness/plugin/browser/trace"
	"github.com/seven7628/hai-harness/tools"
)

// DefaultNavTimeoutMs 导航等待类工具的缺省超时（毫秒）。上游 timeoutSchema 默认 10s，
// 对重型站点（YouTube 等 load 事件 10s+）频繁误报失败——导航实际已提交，仅工具提前放弃。
// 60s 覆盖重站；快速失败场景（连接拒绝等）仍按浏览器底层超时先行报错，不受影响。
const DefaultNavTimeoutMs = 60000

// navTimeoutTools 需要注入缺省超时的导航等待类工具（上游带 timeoutSchema 的集合）。
var navTimeoutTools = map[string]bool{
	"new_page":      true,
	"navigate_page": true,
	"wait_for":      true,
}

// applyNavTimeout 为导航等待类工具补缺省超时（参数未显式携带 timeout 时）。
// 非目标工具 / 空参 / 已显式指定 / 非法 JSON（交由上游报错）→ 原样返回。
func applyNavTimeout(upstream, arguments string) string {
	if !navTimeoutTools[upstream] {
		return arguments
	}
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" || trimmed == "null" {
		arguments = "{}"
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return arguments
	}
	if _, ok := m["timeout"]; ok {
		return arguments
	}
	m["timeout"] = float64(DefaultNavTimeoutMs)
	out, err := json.Marshal(m)
	if err != nil {
		return arguments
	}
	return string(out)
}

// defaultExcludedTools 上游噪声/慢工具（注册时过滤；对齐 pi-browser-use EXCLUDED_TOOLS）。
func defaultExcludedTools() []string {
	return []string{
		"lighthouse_audit", // 慢（性能审计）
		"performance_analyze_insight",
		"performance_start_trace",
		"performance_stop_trace",
		"screencast_start",
		"screencast_stop",
		"install_extension", // 扩展管理（有头会话下用户自己装）
		"list_extensions",
		"reload_extension",
		"trigger_extension_action",
		"uninstall_extension",
	}
}

// WrapperConfig 包裹层配置。
type WrapperConfig struct {
	// ExcludeTools 追加排除的上游工具（默认集之外）。
	ExcludeTools []string
	// HealthInterval 健康探活间隔（默认 10s）。
	HealthInterval time.Duration
	// 测试注入（nil = 由 Wrap 从 mcpm 构建真探活）。
	HealthCheck func(ctx context.Context) error
	Refresh     func(ctx context.Context) error
	// Rebind 连接整体重建（自愈终极手段）：Refresh 无法恢复（manager 已被释放，
	// 报「mcp 服务器 不存在」）时调用——重建私有连接并返回新工具面；wrapper 换绑
	// 同名适配器（工具名不变，引擎持有的 wrappedTool 引用无需重建）。
	Rebind func(ctx context.Context) ([]tools.Tool, error)
}

// Wrap 把私有 mcp.Manager 的工具包裹为 browser_<tool>：
//   - 排除噪声工具（默认集 + ExcludeTools）；
//   - 命名 browser_<upstream>；
//   - 描述增强 + 结果后处理；
//   - 懒式健康守卫（每次 Call 前 > HealthInterval 未检则探活，失败触发 Refresh 重连一次）。
func Wrap(mcpm *mcp.Manager, cfg WrapperConfig) []tools.Tool {
	if mcpm == nil {
		return nil
	}
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 10 * time.Second
	}
	if cfg.HealthCheck == nil {
		mcpmRef := mcpm
		cfg.HealthCheck = func(ctx context.Context) error { return mcpmRef.Ping(ctx, ID) }
		cfg.Refresh = func(ctx context.Context) error { return mcpmRef.Refresh(ctx, ID) }
	}
	excluded := map[string]bool{}
	for _, n := range append(defaultExcludedTools(), cfg.ExcludeTools...) {
		excluded[n] = true
	}

	raw := mcpm.Tools(context.Background()) // 惰性连接（connectTimeout 内）
	out := make([]tools.Tool, 0, len(raw))
	for _, t := range raw {
		upstream := strings.TrimPrefix(t.Name(), "mcp__"+ID+"__")
		if upstream == t.Name() {
			continue // 非本插件服务器工具（防御）
		}
		if excluded[upstream] {
			continue
		}
		out = append(out, &wrappedTool{
			inner:    t,
			upstream: upstream,
			desc:     augmentToolDescription(upstream, t.Description()),
			cfg:      cfg,
		})
	}
	return out
}

// wrappedTool 包裹的浏览器工具（实现 tools.Tool；能力委托 inner，外挂后处理与健康守卫）。
type wrappedTool struct {
	inner     tools.Tool
	upstream  string // 上游工具名（去 mcp__browser__ 前缀）
	desc      string // 描述（增强后）
	cfg       WrapperConfig
	mu        sync.Mutex
	lastCheck time.Time
}

func (w *wrappedTool) Name() string        { return "browser_" + w.upstream }
func (w *wrappedTool) Description() string { return w.desc }
func (w *wrappedTool) Parameters() any     { return w.inner.Parameters() }
func (w *wrappedTool) CanParallel() bool   { return w.inner.CanParallel() }

func (w *wrappedTool) ValidParams(ctx context.Context, name, arguments string) error {
	return w.inner.ValidParams(ctx, name, arguments)
}
func (w *wrappedTool) BeforeCall(ctx context.Context, call core.ToolCall) tools.BeforeToolCallResponse {
	return w.inner.BeforeCall(ctx, call)
}
func (w *wrappedTool) AfterCall(ctx context.Context, call core.ToolCall) {
	w.inner.AfterCall(ctx, call)
}

// ReadOnly / RequiresApproval 提升 inner 的只读/审批标记（引擎按此分桶/审批）。
func (w *wrappedTool) ReadOnly() bool {
	if ro, ok := w.inner.(tools.ReadOnlyTool); ok {
		return ro.ReadOnly()
	}
	return false
}
func (w *wrappedTool) RequiresApproval(ctx context.Context, call core.ToolCall) bool {
	if ar, ok := w.inner.(tools.ApprovalRequired); ok {
		return ar.RequiresApproval(ctx, call)
	}
	return false
}

// Call 先健康守卫再执行，结果经后处理（快照剥离/overlay/stale 引导）；
// 导航等待类工具缺省注入 60s 超时（applyNavTimeout）。
// transport-closed 错误（§15.1）：调用中连接断开 → Refresh 一次再重试（上限 1 次）。
func (w *wrappedTool) Call(ctx context.Context, name, arguments string) (string, error) {
	trStart := time.Now()
	defer func() { trace.Phase("wrapped.Call("+w.upstream+")", trStart) }()
	trace.Log("wrapped.Call.begin", w.upstream, "args="+arguments)
	if err := w.ensureReady(ctx); err != nil {
		return "", err
	}
	args := applyNavTimeout(w.upstream, arguments)
	text, err := w.inner.Call(ctx, name, args)
	if err != nil && isTransportClosed(err) && w.cfg.Refresh != nil {
		// 调用中连接断开：先重连一次再重试
		if rerr := w.cfg.Refresh(ctx); rerr == nil {
			text, err = w.inner.Call(ctx, name, args)
		}
	}
	if err != nil {
		return text, err
	}
	return postProcessToolResult(w.upstream, text), nil
}

// isTransportClosed 判断是否为 MCP 传输层断开错误（§15.1）。
func isTransportClosed(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "transport closed") ||
		strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "context canceled")
}

// ensureReady 懒式健康探活：间隔内探活失败 → Refresh 重连一次；仍失败（manager 已被
// 释放，如工作区关闭/重启后引擎仍持旧工具面）→ Rebind 整体重建并换绑同名适配器。
func (w *wrappedTool) ensureReady(ctx context.Context) error {
	if w.cfg.HealthCheck == nil {
		return nil // 无探针（测试注入场景）→ 不守卫
	}
	w.mu.Lock()
	stale := time.Since(w.lastCheck) >= w.cfg.HealthInterval
	if stale {
		w.lastCheck = time.Now()
	}
	w.mu.Unlock()
	if !stale {
		return nil
	}
	if err := w.cfg.HealthCheck(ctx); err != nil {
		if w.cfg.Refresh != nil {
			if rerr := w.cfg.Refresh(ctx); rerr == nil {
				return nil
			}
		}
		// Refresh 无法恢复（连接/manager 已释放）→ 整体重建（自愈终极手段）
		if w.cfg.Rebind != nil {
			newTools, berr := w.cfg.Rebind(ctx)
			if berr != nil {
				return fmt.Errorf("浏览器连接恢复失败：%v", berr)
			}
			if serr := w.swapInner(newTools); serr != nil {
				return fmt.Errorf("浏览器连接恢复失败：%v", serr)
			}
			return nil
		}
		return fmt.Errorf("浏览器连接恢复失败：%v", err)
	}
	return nil
}

// swapInner 换绑 Rebuild 后的同名适配器（wrappedTool 与新工具面同包，取其 inner；
// 工具名不变 → 引擎侧无需任何重建）。
func (w *wrappedTool) swapInner(newTools []tools.Tool) error {
	for _, t := range newTools {
		if t.Name() != w.Name() {
			continue
		}
		nt, ok := t.(*wrappedTool)
		if !ok {
			return fmt.Errorf("重建工具集类型不匹配")
		}
		w.inner = nt.inner
		return nil
	}
	return fmt.Errorf("重建工具集缺少 %s", w.Name())
}
