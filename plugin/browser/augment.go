package browser

import "strings"

// 工具描述增强（移植 @amaster.ai/pi-browser-use tool-augment.ts）。
// 给关键工具追加用法提示，从源头减少 LLM 误用（uid 失效、press_key 只接受单键等）。
var descriptionHints = map[string]string{
	"click":         " 使用 a11y 快照里的元素 uid（如 uid=\"87_4\"）。动作后 uid 失效——用下一个 uid 前先 take_snapshot。",
	"hover":         " 使用 a11y 快照里的元素 uid 悬停。",
	"fill":          " 按 uid 填标准 HTML 表单（<input>/<textarea>/<select>）。canvas/自定义组件（如 Sheets 单元格、Notion 块）不可用；失败时先点元素再用 press_key 逐字符输入。",
	"fill_form":     " 一次填多个表单字段。优先于多次 fill（更快、更省轮次）。",
	"click_at":      " 在精确像素坐标 (x, y) 点击。用于有坐标但 a11y 树无视觉信息的场景。",
	"take_snapshot": " 返回带 uid 的 a11y 树。FIRST 调用，且每个改变状态的动作之后、使用任何 uid 之前都要调用。",
	"navigate_page": " 导航到指定 URL。导航后调用 take_snapshot 看新页面。",
	"new_page":      " 用指定 URL 打开新页/标签。打开后调用 take_snapshot。",
	"press_key":     " 只按一个键名（如 \"Enter\"、\"Tab\"、\"Escape\"、\"ArrowDown\"、\"a\"、\"8\"）。不要传多字符串——输入文本用 type_text。",
}

// augmentToolDescription 按工具名关键词追加用法提示。
func augmentToolDescription(toolName, description string) string {
	for key, hint := range descriptionHints {
		if strings.Contains(strings.ToLower(toolName), key) {
			return description + hint
		}
	}
	return description
}

// overlayPatterns 点击被遮挡的征兆（a11y 错误文案子串）。
var overlayPatterns = []string{
	"not interactable",
	"obscured",
	"intercept",
	"blocked",
	"element is not visible",
	"element not found",
}

// stalePatterns 元素引用失效的征兆（§15.3）：上游 chrome-devtools-mcp 真实错误文案
// 是 "no longer exists"/"not found on page"/"Element with uid"，原模式只匹配
// stale/detached（英文通用）→ 模型拿不到「重新 take_snapshot」引导。补齐上游文案。
var stalePatterns = []string{
	"stale",
	"detached",
	"no longer exists",
	"not found on page",
	"element with uid",
	"elementhandle",
}

// postProcessToolResult 结果后处理（token 控制 + 失败引导）：
//   - 非 take_snapshot 响应剥离内嵌快照块（防 token 膨胀）；
//   - click 类被 overlay/弹窗遮挡 → 追加「先关掉」提示；
//   - 元素引用过期（stale/detached/uid 失效）→ 追加「重新 take_snapshot」。
func postProcessToolResult(toolName, text string) string {
	processed := text
	// 剥离内嵌快照（除 take_snapshot 本身）
	if toolName != "take_snapshot" {
		if idx := strings.Index(processed, "## Latest page snapshot"); idx >= 0 {
			processed = strings.TrimSpace(processed[:idx])
		}
	}
	// 剥离模型面 structuredContent JSON（chrome-devtools-mcp 每个工具都带 structured 等价内容；
	// 模型只要文本即可——web tab 走 CallRaw 拿原文）。serializeResult 追加形式为
	// "\nstructuredContent: {...}"（前缀形式=仅结构化内容，保留）。
	if idx := strings.Index(processed, "\nstructuredContent: "); idx >= 0 {
		processed = strings.TrimSpace(processed[:idx])
	}
	// overlay/遮挡检测（click 类）
	if strings.Contains(toolName, "click") {
		lower := strings.ToLower(processed)
		for _, p := range overlayPatterns {
			if strings.Contains(lower, p) {
				processed += "\n\n此操作可能被 overlay/弹窗/提示遮罩。请在 a11y 树中找关闭/取消按钮先点掉。"
				break
			}
		}
	}
	// 过期元素检测（含上游 uid 失效文案）
	lower := strings.ToLower(processed)
	for _, p := range stalePatterns {
		if strings.Contains(lower, p) {
			processed += "\n\n元素 uid 已失效（页面已变化）。请先 browser_take_snapshot 获取最新 uid，再用新 uid 操作。"
			break
		}
	}
	return processed
}
