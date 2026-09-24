package computer

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// 看图工具（档 V：仅视觉模型注册，见 docs/COMPUTER_USE_DESIGN.md §4.2）：
// 截图 → image 内容块进模型上下文（模型真正「看见」画面）。
//
// 与语义族的边界（设计决策）：语义工具（a11y 树/OCR/按压）是主通道，截图是
// **补位**——用于 a11y 树给不出答案的场景（颜色/布局/画布内容/视觉校验）。
// 因此工具描述明确要求「先 snapshot，只在需要看的时候截图」，避免模型退化成
// 纯像素猜测（官方也承认纯视觉点按有误差）。

type screenshotTool struct {
	*Executor
	tools.BaseTool
	toolBase
}

func newScreenshotTool(ex *Executor) *screenshotTool {
	return &screenshotTool{Executor: ex, BaseTool: tools.BaseTool{
		Name_:        "computer_screenshot",
		Description_: `Capture the screen as an image you can see directly. Use it when the accessibility tree cannot answer the question: colors, layout/alignment, canvas or custom-drawn content, images, or visual verification that a change looks right. Do NOT use it to locate buttons — computer_snapshot gives uids, which are precise; guessing pixel coordinates from a screenshot is unreliable. Prefer snapshot first, then screenshot only if needed.`,
		Params_: tools.Obj(map[string]any{
			"note": tools.Str("What you are looking for in the image (optional; helps you stay focused)"),
		}),
		CanParallel_: false,
		ReadOnly_:    true,
	}}
}

func (t *screenshotTool) Call(ctx context.Context, _ string, arguments string) (string, error) {
	var a struct {
		Note string `json:"note"`
	}
	if msg, ok := requireArgs(arguments, &a); !ok {
		return msg, nil
	}
	img, err := t.Executor.Screenshot(ctx)
	if err != nil {
		return "", err
	}
	if img == nil || len(img.Data) == 0 {
		return "screenshot failed: no image data returned (check Screen Recording permission)", nil
	}
	mime := "image/png"
	if img.Format == "jpeg" || img.Format == "jpg" {
		mime = "image/jpeg"
	}
	block := core.Content{
		Type:     core.ContentTypeImage,
		Content:  "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img.Data),
		MimeType: mime,
	}
	tools.ImageSinkFrom(ctx).Add(block)
	note := ""
	if a.Note != "" {
		note = " (looking for: " + a.Note + ")"
	}
	return fmt.Sprintf("[computer_screenshot: %dx%d %s, %d bytes — the image is attached; read it directly%s]",
		img.Width, img.Height, mime, len(img.Data), note), nil
}

// VisionTools 返回视觉档工具族（仅当当前模型支持图片输入时注册，见
// docs/COMPUTER_USE_DESIGN.md §2.1：档 V = 语义工具 + 看图工具）。
func VisionTools(ex *Executor) []tools.Tool {
	return []tools.Tool{newScreenshotTool(ex)}
}
