package officekit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	goruntime "github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
)

// DefaultDPI 逐页栅格化的默认分辨率（144 = 2×，屏幕上缩放一档仍清晰）。
const DefaultDPI = 144

// 栅格化时限：30 页 PDF 在 144dpi 下约 2–4s（pypdfium2 是 C++ 绑定），
// 60s 是「明显不正常」的阈值，与 DefaultTimeout 同口径。
const renderTimeout = 60 * time.Second

// PythonRuntime 受管 Python 运行时（runtime.Runtime 满足）。
//
// 为什么不 import tools/builtin：那是**工具层**（背 model/schema 概念），本包是环境层，
// 反向依赖会让 officekit 拖进整个工具栈。这里只声明用到的一个方法。
type PythonRuntime interface {
	Ensure(ctx context.Context) (goruntime.Python, error)
}

// RenderOptions 栅格化选项（零值 = 生产默认）。
type RenderOptions struct {
	// DPI 分辨率；<= 0 → DefaultDPI。
	DPI int
	// Pages 要渲染的页（**1 起**，与用户/模型看到的页码一致）；空 = 全部（受 Limit 约束）。
	Pages []int
	// Limit 只取前 N 页（仅当 Pages 为空时生效；0 = 不限）。
	//
	// 为什么要有它（而不是让调用方先查页数再填 Pages）：调用方在渲染前不知道 PDF 有几页，
	// 「默认前 3 页」若写成 Pages=[1,2,3]，一份 1 页的文档就会因为「第 2 页越界」直接失败。
	Limit int
	// Timeout 单次时限；<= 0 → 60s。
	Timeout time.Duration
}

// RenderedPage 一页 PNG。
type RenderedPage struct {
	Number int    `json:"page"`
	Path   string `json:"path"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// RenderResult 栅格化结果。
type RenderResult struct {
	Pages      []RenderedPage
	TotalPages int // PDF 实际页数（请求的页可能只是其中几页）
}

// RenderImages 用受管 Python 的 pypdfium2 把 PDF 逐页栅格成 PNG（05 §8.3 的 renderImages）。
//
// 为什么是 pypdfium2 而不是嵌一个 poppler/mutool：受管运行时里**已经**装了它
// （runtime/python.go 的依赖清单钉了 5.9.0，含 macOS 11 wheel），另引一个二进制
// 等于再背一份平台矩阵与许可 —— 而它做的正是同一件事（PDFium 是 Chromium 的 PDF 引擎，
// 与预览面板里看到的渲染同一个血缘）。
//
// 产物写进 outDir（调用方负责内容寻址缓存与清理）；本函数不删任何东西。
func RenderImages(ctx context.Context, py PythonRuntime, pdfPath, outDir string, opt RenderOptions) (RenderResult, error) {
	if py == nil {
		return RenderResult{}, fmt.Errorf("%w: 未装配受管 Python 运行时，无法栅格化 PDF", ErrConvertFailed)
	}
	info, err := os.Stat(pdfPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return RenderResult{}, fmt.Errorf("%w: PDF 不可读: %s", ErrConvertFailed, filepath.Base(pdfPath))
	}
	if outDir == "" {
		return RenderResult{}, fmt.Errorf("%w: outDir 为空", ErrConvertFailed)
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return RenderResult{}, fmt.Errorf("%w: 建输出目录: %v", ErrConvertFailed, err)
	}
	dpi := opt.DPI
	if dpi <= 0 {
		dpi = DefaultDPI
	}
	if dpi < 36 || dpi > 600 {
		// 上下界不是审美：36dpi 以下页码不可辨（等于没渲染），600dpi 以上单页 PNG
		// 能到几十 MB，会把缓存预算一次性吃光。
		return RenderResult{}, fmt.Errorf("%w: dpi 超出范围 [36,600]: %d", ErrConvertFailed, dpi)
	}
	pages := append([]int(nil), opt.Pages...)
	sort.Ints(pages)
	limit := opt.Limit
	if limit < 0 {
		limit = 0
	}

	rt, err := py.Ensure(ctx)
	if err != nil {
		return RenderResult{}, fmt.Errorf("%w: 受管 Python 运行时不可用: %v", ErrConvertFailed, err)
	}
	scriptPath, err := writeScript(outDir)
	if err != nil {
		return RenderResult{}, err
	}
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = renderTimeout
	}
	pagesJSON, _ := json.Marshal(pages)
	var exitCode int
	var timedOut, canceled bool
	out, runErr := sandbox.RunArgs(ctx, rt.VenvPython,
		[]string{scriptPath, pdfPath, outDir, strconv.Itoa(dpi), string(pagesJSON), strconv.Itoa(limit)},
		sandbox.ExecSpec{Command: rt.VenvPython, Cwd: outDir, Timeout: timeout,
			ExitCode: &exitCode, TimedOut: &timedOut, Canceled: &canceled,
			// 输出封顶（第 5 轮爬山第 5 步）：改造前无界缓冲。栅格化脚本的 stdout
			// 只用于错误文案（firstLine 取前 400 字节）⇒ 64KB 足够。
			OutputLimit: officekitOutputLimit})
	if runErr != nil {
		return RenderResult{}, fmt.Errorf("%w: 启动栅格化脚本: %v", ErrConvertFailed, runErr)
	}
	switch {
	case canceled:
		return RenderResult{}, ErrCanceled
	case timedOut:
		return RenderResult{}, fmt.Errorf("%w: 栅格化超过 %s", ErrTimeout, timeout)
	case exitCode != 0:
		return RenderResult{}, fmt.Errorf("%w: 栅格化脚本退出码 %d: %s", ErrConvertFailed, exitCode, firstLine(out, 400))
	}
	return readRenderManifest(outDir)
}

// renderManifest 脚本写出的清单（比解析 stdout 稳：pdfium 的告警会混进 stdout）。
type renderManifest struct {
	Total int            `json:"total"`
	Pages []RenderedPage `json:"pages"`
	Error string         `json:"error,omitempty"`
}

// readManifest 读回脚本产出的清单。
func readRenderManifest(outDir string) (RenderResult, error) {
	b, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		return RenderResult{}, fmt.Errorf("%w: 脚本未产出 manifest.json: %v", ErrConvertFailed, err)
	}
	var m renderManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return RenderResult{}, fmt.Errorf("%w: manifest.json 解析失败: %v", ErrConvertFailed, err)
	}
	if m.Error != "" {
		return RenderResult{}, fmt.Errorf("%w: %s", ErrConvertFailed, m.Error)
	}
	if len(m.Pages) == 0 {
		return RenderResult{}, errors.New("officekit: 渲染结果为空（PDF 没有页？）")
	}
	return RenderResult{Pages: m.Pages, TotalPages: m.Total}, nil
}

// writeScript 把栅格化脚本落到 outDir（0600；内容是我们自己的常量，不含用户数据）。
func writeScript(dir string) (string, error) {
	p := filepath.Join(dir, "render_pages.py")
	if err := os.WriteFile(p, []byte(renderScript), 0o600); err != nil {
		return "", fmt.Errorf("%w: 写栅格化脚本: %v", ErrConvertFailed, err)
	}
	return p, nil
}

// renderScript 逐页栅格化脚本（pypdfium2）。
//
// 三个刻意的选择：
//   - **清单落文件**（manifest.json）而不是 stdout：pdfium/Pillow 的告警会混进 stdout，
//     按行解析 JSON 是「日志一多就偶发解析失败」的经典坑；
//   - 页号 **1 起**：与模型/用户的「第 3 页」一致；越界在这里就报错，而不是静默跳过
//     （「我要第 9 页，返回 3 张图」是最难察觉的错）；
//   - scale = dpi/72：PDF 的点是 1/72 英寸，这是 dpi 的唯一正确换算。
const renderScript = `import json, os, sys
import pypdfium2 as pdfium

pdf_path, out_dir, dpi = sys.argv[1], sys.argv[2], int(sys.argv[3])
want = json.loads(sys.argv[4])
limit = int(sys.argv[5])
manifest = {"total": 0, "pages": []}
try:
    doc = pdfium.PdfDocument(pdf_path)
    total = len(doc)
    manifest["total"] = total
    if not want:
        last = total if limit <= 0 else min(total, limit)
        want = list(range(1, last + 1))
    for n in want:
        if n < 1 or n > total:
            raise ValueError("page %d out of range 1..%d" % (n, total))
        page = doc[n - 1]
        pil = page.render(scale=dpi / 72.0).to_pil()
        name = "page-%04d.png" % n
        pil.save(os.path.join(out_dir, name))
        manifest["pages"].append({"page": n, "path": os.path.join(out_dir, name),
                                  "width": pil.width, "height": pil.height})
except Exception as exc:  # noqa: BLE001 —— 任何异常都要变成清单里的 error 字段
    manifest["error"] = "%s: %s" % (type(exc).__name__, exc)

with open(os.path.join(out_dir, "manifest.json"), "w", encoding="utf-8") as f:
    json.dump(manifest, f, ensure_ascii=False)
sys.exit(1 if manifest.get("error") else 0)
`
