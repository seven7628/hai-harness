// office_tools.go —— 两个给 agent「看图验收」的办公组件工具（R08d）。
//
// 为什么需要工具（而不是只让面板自己转）：agent 生成 pptx/docx/xlsx 之后，**没有**任何
// 手段确认自己交付的版式 —— 它只能读 XML 猜。office_convert 给 PDF（分页/字体/版式），
// office_render 给逐页 PNG（可直接交给 read_file 看图）。这条链是「产出 → 自己验收」
// 的闭环里唯一缺的一环。
//
// CodeMode 契约（00-conventions §2）：两个工具都实现 OutputSchemaProvider，脚本路径
// （tools.IsScriptCall）写结构化结果，失败一律返回 Go error（不是 {"error":...}）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/seven7628/hai-harness/internal/officekit"
	"github.com/seven7628/hai-harness/tools"
)

// officeHost 办公工具需要的宿主能力（*manager 满足；测试注入假实现即可离线验契约）。
type officeHost interface {
	officeKitReady() bool
	officePDF(ctx context.Context, abs, kind string) (string, error)
	officeRender(ctx context.Context, abs string, pages []int, limit, dpi int) (officeRenderOutput, error)
}

// officeKitReady / officePDF / officeRender 把 manager 的既有通路适配成工具要的形状。
func (m *manager) officeKitReady() bool { return m.officeKitState().prober.Kit() != nil }

func (m *manager) officePDF(ctx context.Context, abs, kind string) (string, error) {
	return m.convertOfficePDF(ctx, abs, kind)
}

func (m *manager) officeRender(ctx context.Context, abs string, pages []int, limit, dpi int) (officeRenderOutput, error) {
	return m.renderOfficePages(ctx, abs, pages, limit, dpi)
}

// registerOfficeTools 注册两个办公工具（kit 不可用则**不注册**）。
//
// 为什么不注册一个「会明确报错」的版本：注册一个必然失败的工具只会让模型白耗轮次
// （与 run_python 未装配时的取舍一致，见 main.go 的注册点）。
func (m *manager) registerOfficeTools(eng *tools.Engine, workspace string) {
	if !m.officeKitReady() {
		return
	}
	eng.RegisterTool(context.Background(), newOfficeConvertTool(m, workspace))
	eng.RegisterTool(context.Background(), newOfficeRenderTool(m, workspace))
}

// resolveOfficeInput 工具输入 → 绝对路径 + kind（路径口径与 file_preview 完全一致）。
func resolveOfficeInput(workspace, raw string) (string, string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", "", errors.New("path 为空")
	}
	abs, err := resolveWorkspacePath(workspace, raw)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", "", fmt.Errorf("无法读取文件: %v", err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("path 是目录，不是文件: %s", abs)
	}
	kind := officeKindOf(abs)
	if kind == "" && !strings.EqualFold(filepath.Ext(abs), ".pdf") {
		return "", "", fmt.Errorf("不支持的文件类型 %q（office_convert 接受 docx/pptx/xlsx；office_render 另接受 pdf）", filepath.Ext(abs))
	}
	return abs, kind, nil
}

// —— office_convert ——

func newOfficeConvertTool(h officeHost, workspace string) tools.Tool {
	return &officeConvertTool{
		BaseTool: tools.BaseTool{
			Name_: "office_convert",
			Description_: "把 docx/pptx/xlsx 转成 PDF（需 LibreOffice kit），返回 PDF 路径。" +
				"交付前想确认分页/字体/版式，或需要一份 PDF 时用。",
			Params_: tools.Obj(map[string]any{
				"path": tools.Str("工作区内的文件路径（相对或绝对，绝对路径可指向工作区外）"),
			}, "path"),
			ReadOnly_: false, // 会写预览缓存（不在工作区产生文件，但确有副作用）
		},
		host: h, workspace: workspace,
	}
}

type officeConvertTool struct {
	tools.BaseTool
	host      officeHost
	workspace string
}

// officeConvertOutput office_convert 的结构化结果。
type officeConvertOutput struct {
	PDFPath string `json:"pdf_path"`
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Bytes   int64  `json:"bytes"`
}

func (t *officeConvertTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if strings.TrimSpace(a.Path) == "" {
		return errors.New("office_convert: path 不能为空")
	}
	return nil
}

func (t *officeConvertTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	abs, kind, err := resolveOfficeInput(t.workspace, a.Path)
	if err != nil {
		return "", err
	}
	if kind == "" {
		return "", fmt.Errorf("office_convert 不接受 PDF（已经是目标格式）: %s", filepath.Base(abs))
	}
	pdf, err := t.host.officePDF(ctx, abs, kind)
	if err != nil {
		return "", err // C4：失败必须 reject（脚本的 catch 才抓得到）
	}
	out := officeConvertOutput{PDFPath: pdf, Kind: kind, Source: abs}
	if st, serr := os.Stat(pdf); serr == nil {
		out.Bytes = st.Size()
	}
	if tools.IsScriptCall(ctx) { // C3：只有脚本路径写结构化结果
		if sink := tools.StructuredSinkFrom(ctx); sink != nil {
			if b, merr := json.Marshal(out); merr == nil {
				sink.Set(b)
			}
		}
	}
	return fmt.Sprintf("已转成 PDF（%s，%s）：%s\n要看版式就用 office_render 渲染成 PNG。",
		kind, humanBytes(int(out.Bytes)), pdf), nil
}

// OutputSchema 实现 tools.OutputSchemaProvider（C1）。
func (t *officeConvertTool) OutputSchema() any {
	return tools.Obj(map[string]any{
		"pdf_path": tools.Str("PDF 的绝对路径（在预览缓存里，按内容寻址；同一份文件重复调用命中同一条）"),
		"kind":     tools.Str("源文件的转换器类型：docx|pptx|xlsx"),
		"source":   tools.Str("源文件绝对路径"),
		"bytes":    tools.Int("PDF 字节数（拿不到时为 0）"),
	}, "pdf_path", "kind", "source")
}

// —— office_render ——

func newOfficeRenderTool(h officeHost, workspace string) tools.Tool {
	return &officeRenderTool{
		BaseTool: tools.BaseTool{
			Name_: "office_render",
			Description_: "把办公文档或 PDF 逐页渲染成 PNG，返回图片路径（用 read_file 打开即可看图）。" +
				"验收版式时用；pages 指定页码（默认前 3 页），dpi 默认 144。",
			Params_: tools.Obj(map[string]any{
				"path": tools.Str("工作区内的文件路径（docx/pptx/xlsx/pdf）"),
				"pages": tools.Map{
					"type":        "array",
					"description": "要渲染的页码（1 起，如 [1,3]）；省略 = 前 3 页（PDF 不足 3 页则为全部）",
					"items":       tools.Map{"type": "integer"},
				},
				"dpi": tools.Int("渲染分辨率，默认 144（范围 36–600；越大越清晰也越占缓存）"),
			}, "path"),
			ReadOnly_: false, // 写渲染缓存
		},
		host: h, workspace: workspace,
	}
}

type officeRenderTool struct {
	tools.BaseTool
	host      officeHost
	workspace string
}

// officeRenderToolOutput office_render 的结构化结果（顶层是对象，C7）。
type officeRenderToolOutput struct {
	Pages  []officekit.RenderedPage `json:"pages"`
	Total  int                      `json:"total"`
	DPI    int                      `json:"dpi"`
	Cached bool                     `json:"cached"`
	PDF    string                   `json:"pdf_path"`
}

func (t *officeRenderTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		Path  string `json:"path"`
		Pages []int  `json:"pages"`
		DPI   int    `json:"dpi"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if strings.TrimSpace(a.Path) == "" {
		return errors.New("office_render: path 不能为空")
	}
	for _, p := range a.Pages {
		if p < 1 {
			return fmt.Errorf("office_render: 页码从 1 开始，收到 %d", p)
		}
	}
	if a.DPI != 0 && (a.DPI < 36 || a.DPI > 600) {
		return fmt.Errorf("office_render: dpi 需在 36–600，收到 %d", a.DPI)
	}
	return nil
}

func (t *officeRenderTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a struct {
		Path  string `json:"path"`
		Pages []int  `json:"pages"`
		DPI   int    `json:"dpi"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	abs, _, err := resolveOfficeInput(t.workspace, a.Path)
	if err != nil {
		return "", err
	}
	// 默认前 3 页（Limit）：模型第一次看一份文档时不该被迫一次渲染 40 页（缓存与时间都贵）。
	// 用 Limit 而不是 Pages=[1,2,3]：渲染前不知道总页数，写死页号会让 1 页的文档直接报越界。
	res, err := t.host.officeRender(ctx, abs, a.Pages, 3, a.DPI)
	if err != nil {
		return "", err
	}
	out := officeRenderToolOutput{Pages: res.Pages, Total: res.Total, DPI: res.DPI, Cached: res.Cached, PDF: res.PDF}
	if tools.IsScriptCall(ctx) {
		if sink := tools.StructuredSinkFrom(ctx); sink != nil {
			if b, merr := json.Marshal(out); merr == nil {
				sink.Set(b)
			}
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "已渲染 %d 页（共 %d 页，dpi=%d%s）：\n", len(res.Pages), res.Total, res.DPI, cachedNote(res.Cached))
	for _, p := range res.Pages {
		fmt.Fprintf(&b, "  第 %d 页 %dx%d —— %s\n", p.Number, p.Width, p.Height, p.Path)
	}
	b.WriteString("用 read_file 打开上面的 PNG 即可看图验收版式。")
	return b.String(), nil
}

// OutputSchema 实现 tools.OutputSchemaProvider（C1）。
func (t *officeRenderTool) OutputSchema() any {
	return tools.Obj(map[string]any{
		"pages": tools.Map{
			"type":        "array",
			"description": "本次渲染出的页（按页码升序）",
			"items": tools.Obj(map[string]any{
				"page":   tools.Int("页码（1 起）"),
				"path":   tools.Str("PNG 绝对路径（在预览缓存里）"),
				"width":  tools.Int("像素宽"),
				"height": tools.Int("像素高"),
			}, "page", "path", "width", "height"),
		},
		"total":    tools.Int("PDF 实际页数（不是本次渲染的页数）"),
		"dpi":      tools.Int("本次渲染分辨率"),
		"cached":   tools.Bool("true = 命中缓存，没有重新渲染"),
		"pdf_path": tools.Str("被渲染的 PDF 绝对路径（office 文档会先经 kit 转成 PDF）"),
	}, "pages", "total", "dpi", "cached")
}

// cachedNote 文本路径的缓存说明（模型据此判断「这次没花时间」）。
func cachedNote(cached bool) string {
	if cached {
		return "，命中缓存"
	}
	return ""
}
