package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/desktop/bridge/conversion"
	"github.com/seven7628/hai-harness/internal/officekit"
	"github.com/seven7628/hai-harness/tools"
)

// fakeOfficeKit 造一套「能通过校验」的 kit（假 soffice + KIT.json），返回探测选项。
//
// 与 internal/officekit 的测试同一手法：**离线**、假二进制、真校验。bridge 侧要证明的
// 是「装配与降级正确」，不是「soffice 能转文档」（后者本机无 soffice，见报告）。
func fakeOfficeKit(t *testing.T, script string) officekit.ProbeOptions {
	t.Helper()
	root := t.TempDir()
	platform := officekit.CurrentPlatform()
	dir := filepath.Join(root, officekit.PinnedVersion, platform)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "soffice")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	manifest := map[string]any{
		"version": officekit.PinnedVersion, "platform": platform,
		"sha256": hex.EncodeToString(sum[:]), "buildAt": "test",
		"license": "MPL-2.0 / LGPL-3.0", "licenseUrl": "https://example.invalid/license",
		"sourceUrl": "https://example.invalid/source",
	}
	b, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "KIT.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return officekit.ProbeOptions{
		KitRoot:     root,
		LookPath:    func(string) (string, error) { return "", os.ErrNotExist },
		SystemPaths: []string{filepath.Join(root, "no-system-soffice")},
	}
}

// noKitOptions 让「kit 不可用」成为确定事实（不受本机是否装了 LibreOffice 影响）。
func noKitOptions(t *testing.T) officekit.ProbeOptions {
	t.Helper()
	return officekit.ProbeOptions{
		KitRoot:     filepath.Join(t.TempDir(), "absent"),
		LookPath:    func(string) (string, error) { return "", os.ErrNotExist },
		SystemPaths: []string{filepath.Join(t.TempDir(), "none")},
	}
}

// installOfficeState 把 manager 的办公组件状态换成可注入的版本（测试用）。
func installOfficeState(m *manager, opts officekit.ProbeOptions, render func(ctx context.Context, py officekit.PythonRuntime, pdf, outDir string, opt officekit.RenderOptions) (officekit.RenderResult, error)) {
	m.officeMu.Lock()
	defer m.officeMu.Unlock()
	m.office = &officeKitState{
		prober: officekit.NewProber(opts),
		sem:    make(chan struct{}, officeConvertConcurrency),
		render: render,
	}
}

// pdfScript 假 soffice：把输入「转成」一个 20 字节的假 PDF。
const pdfScript = `OUT=''
SRC=''
while [ $# -gt 0 ]; do
  case "$1" in
    --outdir) shift; OUT="$1" ;;
    *) SRC="$1" ;;
  esac
  shift
done
b=$(basename "$SRC")
b=${b%.*}
printf '%%PDF-1.4\n%% fake\n' > "$OUT/$b.pdf"
`

// 状态命令：kit 缺失是 **ok:true + available:false**，并带上平台矩阵（设置页要展示
// 「怎么装、多大、什么许可」）。不可用绝不能变成命令错误 —— 前端据此走降级。
func TestOfficeKitStatusCommand(t *testing.T) {
	m, buf := testManager(t)
	installOfficeState(m, noKitOptions(t), officekit.RenderImages)
	m.dispatch(cmd(3, "office_kit_status", nil))
	resp := waitResponse(t, buf, 3)
	if resp["ok"] != true {
		t.Fatalf("不可用不应是命令错误: %+v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	kit, _ := data["kit"].(map[string]any)
	if kit["available"] != false || kit["state"] != string(officekit.StateMissing) {
		t.Fatalf("期望 available=false/state=missing，得到 %+v", kit)
	}
	if kit["reason"] == "" {
		t.Fatal("缺 reason：横幅/设置页要给人话原因")
	}
	plats, _ := kit["platforms"].([]any)
	if len(plats) != len(officekit.Platforms) {
		t.Fatalf("平台矩阵没带上：%d", len(plats))
	}
}

// 预览装配：kit 可用 → soffice→PDF，响应带 renderer=libreoffice 且 html_path 就是 PDF。
func TestPreviewConvertUsesKitPDFWhenAvailable(t *testing.T) {
	ws := t.TempDir()
	src := filepath.Join(ws, "report.docx")
	if err := os.WriteFile(src, []byte("fake docx"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)
	m.runtime(ws)
	installOfficeState(m, fakeOfficeKit(t, pdfScript), officekit.RenderImages)

	m.dispatch(cmd(4, "docx_preview", map[string]any{"workspace": ws, "path": "report.docx"}))
	resp := waitResponse(t, buf, 4)
	if resp["ok"] != true {
		t.Fatalf("kit 路径失败: %+v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data["renderer"] != "libreoffice" {
		t.Fatalf("renderer = %v，期望 libreoffice", data["renderer"])
	}
	pdf, _ := data["pdf_path"].(string)
	if pdf == "" || !strings.HasSuffix(pdf, ".pdf") {
		t.Fatalf("pdf_path = %q", pdf)
	}
	if data["html_path"] != pdf {
		t.Fatalf("html_path(%v) 应是交给浏览器打开的同一个 PDF(%v)", data["html_path"], pdf)
	}
	if st, err := os.Stat(pdf); err != nil || st.Size() == 0 {
		t.Fatalf("PDF 没落进缓存: %v", err)
	}
	// 第二次同一份文件：命中缓存（不再起 soffice —— 用产物 mtime 证明没重写）。
	before, _ := os.Stat(pdf)
	m.dispatch(cmd(5, "docx_preview", map[string]any{"workspace": ws, "path": "report.docx"}))
	resp2 := waitResponse(t, buf, 5)
	d2, _ := resp2["data"].(map[string]any)
	if d2["pdf_path"] != pdf {
		t.Fatalf("第二次路径不同（没命中缓存）: %v vs %v", d2["pdf_path"], pdf)
	}
	after, _ := os.Stat(pdf)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("第二次重写了产物 mtime：缓存没命中")
	}
}

// kit 转不动这份文件 → 落回近似路径，响应 renderer=approximate（横幅依据），
// 而且**必须仍然成功**（用户至少还能看到内容）。
func TestPreviewConvertFallsBackWhenKitFails(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "broken.docx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)
	m.runtime(ws)
	// 假 soffice 不产出 PDF 且退出码 0 → 走 officekit 的「0 退出 ≠ 有产物」判据。
	installOfficeState(m, fakeOfficeKit(t, "exit 0\n"), officekit.RenderImages)
	// 近似路径的转换器换成假实现（不依赖受管 Python），但要证明它真的被调到了。
	fake := &stubConverter{html: "<html>approximate</html>"}
	m.conv = convertSvc(t, fake)

	m.dispatch(cmd(6, "docx_preview", map[string]any{"workspace": ws, "path": "broken.docx"}))
	resp := waitResponse(t, buf, 6)
	if resp["ok"] != true {
		t.Fatalf("降级后应成功: %+v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data["renderer"] != "approximate" {
		t.Fatalf("renderer = %v，期望 approximate", data["renderer"])
	}
	if fake.calls.Load() == 0 {
		t.Fatal("没有落回近似路径")
	}
}

// 面板渲染器（store 的 loadOfficePreview）只要**近似 HTML**：approximate=true 时必须
// 跳过 kit。否则 html_path 是 PDF，前端用 file_preview 把它当文本读回 → iframe 空白
// （R08d 回归；e2e:preview-native 的 docx 一条抓到）。原生视图不传这个标志，照旧拿 PDF。
func TestPreviewConvertApproximateFlagSkipsKit(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "panel.docx"), []byte("fake docx"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)
	m.runtime(ws)
	installOfficeState(m, fakeOfficeKit(t, pdfScript), officekit.RenderImages)
	fake := &stubConverter{html: "<html>approximate</html>"}
	m.conv = convertSvc(t, fake)

	m.dispatch(cmd(8, "docx_preview", map[string]any{"workspace": ws, "path": "panel.docx", "approximate": true}))
	resp := waitResponse(t, buf, 8)
	if resp["ok"] != true {
		t.Fatalf("近似预览失败: %+v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data["renderer"] != "approximate" {
		t.Fatalf("renderer = %v，期望 approximate（面板读不了 PDF）", data["renderer"])
	}
	if html, _ := data["html_path"].(string); !strings.HasSuffix(html, ".html") {
		t.Fatalf("html_path = %q，面板要的是 HTML 产物", html)
	}
	if fake.calls.Load() == 0 {
		t.Fatal("近似转换器没被调用（说明还是走了 kit）")
	}
}

// kit 卸载（bin 被删）后状态必须**自己**失效（R08d 复评 P1）：转换回落近似路径，
// **并且** bridge 推一条 office_kit_status(state=missing) —— 前端横幅据此消失。
//
// 为什么这条必须有：Prober 的 Kit() 是进程级缓存，文件被删不会让它变 nil。修之前
// 这里只打一行 stderr 就回落，横幅会一直写着「可用」，直到用户自己去点「重新探测」
// —— 用户卸载后看到的应该是「已缺失 + 怎么装」。
func TestOfficeKitUninstallInvalidatesStatusAndFallsBack(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "gone.docx"), []byte("fake docx"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, buf := testManager(t)
	m.runtime(ws)
	opts := fakeOfficeKit(t, pdfScript)
	installOfficeState(m, opts, officekit.RenderImages)
	// 生产里 App 启动就会查一次状态（App.tsx 的 fetchOfficeKit）→ 缓存「可用」。
	if st := m.officeKitStatus(false); !st.Available {
		t.Fatalf("前置条件不成立：kit 应可用，得到 %+v", st)
	}
	fake := &stubConverter{html: "<html>approximate</html>"}
	m.conv = convertSvc(t, fake)

	// 卸载：kit 目录整块删掉（bin 跟着消失，KIT.json 也没了 → 重探结论是 missing 而不是 invalid）。
	if err := os.RemoveAll(opts.KitRoot); err != nil {
		t.Fatal(err)
	}

	m.dispatch(cmd(7, "docx_preview", map[string]any{"workspace": ws, "path": "gone.docx"}))
	resp := waitResponse(t, buf, 7)
	if resp["ok"] != true {
		t.Fatalf("kit 没了应回落成功（用户至少还能看近似版），得到 %+v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data["renderer"] != "approximate" {
		t.Fatalf("renderer = %v，期望 approximate", data["renderer"])
	}
	if fake.calls.Load() == 0 {
		t.Fatal("没有落回近似路径")
	}

	// 状态推送：事件本身，而不是「下次被问到时才重新算」。
	ev := waitEvent(t, buf, "office_kit_status", 5*time.Second)
	kit, _ := ev["data"].(map[string]any)["kit"].(map[string]any)
	if kit["available"] != false || kit["state"] != string(officekit.StateMissing) {
		t.Fatalf("推送的状态应是 missing/不可用，得到 %+v", kit)
	}
	// 缓存也真的作废了（事件之外，后续命令读到的必须是新结论）。
	if st := m.officeKitStatus(false); st.Available || st.State != officekit.StateMissing {
		t.Fatalf("探测缓存没失效：%+v", st)
	}
}

// 渲染缓存：第二次同一份 PDF/同参数必须命中（不再调 render），产物路径相同。
func TestOfficeRenderCacheHit(t *testing.T) {
	m, _ := testManager(t)
	var calls atomic.Int32
	render := func(_ context.Context, _ officekit.PythonRuntime, _, outDir string, opt officekit.RenderOptions) (officekit.RenderResult, error) {
		calls.Add(1)
		var pages []officekit.RenderedPage
		for _, n := range opt.Pages {
			p := filepath.Join(outDir, "page-000"+string(rune('0'+n))+".png")
			if err := os.WriteFile(p, []byte("PNG"+string(rune('0'+n))), 0o600); err != nil {
				return officekit.RenderResult{}, err
			}
			pages = append(pages, officekit.RenderedPage{Number: n, Path: p, Width: 400, Height: 200})
		}
		return officekit.RenderResult{Pages: pages, TotalPages: 2}, nil
	}
	installOfficeState(m, fakeOfficeKit(t, pdfScript), render)
	pdf := filepath.Join(t.TempDir(), "two.pdf")
	if err := os.WriteFile(pdf, []byte("%PDF-1.4 fake body"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := m.renderOfficePages(context.Background(), pdf, []int{1, 2}, 0, 144)
	if err != nil {
		t.Fatalf("首次渲染: %v", err)
	}
	if first.Cached || len(first.Pages) != 2 || first.Total != 2 {
		t.Fatalf("首次结果异常: %+v", first)
	}
	for _, p := range first.Pages {
		if !strings.Contains(p.Path, string(os.PathSeparator)+"previews"+string(os.PathSeparator)) {
			t.Fatalf("产物不在预览缓存里: %s", p.Path)
		}
	}
	afterFirst := calls.Load()
	second, err := m.renderOfficePages(context.Background(), pdf, []int{1, 2}, 0, 144)
	if err != nil {
		t.Fatalf("二次渲染: %v", err)
	}
	if !second.Cached {
		t.Fatal("第二次没有命中缓存")
	}
	if calls.Load() != 1 {
		t.Fatalf("render 被调 %d 次，期望 1（第二次应命中缓存）", calls.Load())
	}
	if second.Pages[0].Path != first.Pages[0].Path {
		t.Fatalf("缓存路径漂移: %s vs %s", second.Pages[0].Path, first.Pages[0].Path)
	}
	t.Logf("首次 cached=%v（render 调用 %d 次）；二次 cached=%v（render 调用仍是 %d 次 = 没重跑）；产物 %s",
		first.Cached, afterFirst, second.Cached, calls.Load(), filepath.Base(first.Pages[0].Path))
	// 换 dpi = 换参数 = 另一个条目（缓存键必须带参数）。
	if _, err := m.renderOfficePages(context.Background(), pdf, []int{1, 2}, 0, 72); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("换 dpi 后应重新渲染，render 调用数 = %d", calls.Load())
	}
}

// 工具注册：kit 不可用不注册（不给模型一个必然失败的工具）；可用则两个都在。
func TestRegisterOfficeToolsOnlyWhenKitAvailable(t *testing.T) {
	m, _ := testManager(t)
	ws := t.TempDir()

	installOfficeState(m, noKitOptions(t), officekit.RenderImages)
	eng := tools.NewToolEngine()
	m.registerOfficeTools(eng, ws)
	if _, err := eng.GetTool(context.Background(), "office_convert"); err == nil {
		t.Fatal("kit 不可用时不该注册 office_convert")
	}

	installOfficeState(m, fakeOfficeKit(t, pdfScript), officekit.RenderImages)
	m.registerOfficeTools(eng, ws)
	for _, name := range []string{"office_convert", "office_render"} {
		if _, err := eng.GetTool(context.Background(), name); err != nil {
			t.Fatalf("kit 可用时应注册 %s: %v", name, err)
		}
	}
}

// CodeMode 契约（00-conventions §2）：输出 schema 非空、脚本路径写结构化结果、
// 失败必须 reject（Go error，不是 {"error":...}）。
func TestOfficeToolsCodeModeContract(t *testing.T) {
	host := &stubOfficeHost{pdf: "/cache/abc/preview.pdf", pages: []officekit.RenderedPage{{Number: 1, Path: "/cache/abc/page-0001.png", Width: 400, Height: 200}}, total: 2, dpi: 144}
	ws := t.TempDir()
	src := filepath.Join(ws, "deck.pptx")
	if err := os.WriteFile(src, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		tool  tools.Tool
		name  string
		args  string
		check func(t *testing.T, raw []byte)
	}{
		{
			tool: newOfficeConvertTool(host, ws), name: "office_convert",
			args: `{"path":"deck.pptx"}`,
			check: func(t *testing.T, raw []byte) {
				var out officeConvertOutput
				if err := json.Unmarshal(raw, &out); err != nil {
					t.Fatalf("结构化结果不是 JSON: %v", err)
				}
				if out.PDFPath != host.pdf || out.Kind != "pptx" {
					t.Fatalf("结构化字段不对: %+v", out)
				}
			},
		},
		{
			tool: newOfficeRenderTool(host, ws), name: "office_render",
			args: `{"path":"deck.pptx","pages":[1],"dpi":72}`,
			check: func(t *testing.T, raw []byte) {
				var out officeRenderToolOutput
				if err := json.Unmarshal(raw, &out); err != nil {
					t.Fatalf("结构化结果不是 JSON: %v", err)
				}
				if len(out.Pages) != 1 || out.DPI != 72 || out.Total != 2 {
					t.Fatalf("结构化字段不对: %+v", out)
				}
			},
		},
	} {
		schema, ok := tc.tool.(interface{ OutputSchema() any })
		if !ok || schema.OutputSchema() == nil {
			t.Fatalf("%s 没实现 OutputSchemaProvider（C1）", tc.name)
		}
		// 脚本路径：结构化结果必须写进 ctx 槽。
		sink := &tools.StructuredSink{}
		ctx := tools.WithStructuredSink(tools.WithScriptCall(context.Background()), sink)
		if _, err := tc.tool.Call(ctx, "", tc.args); err != nil {
			t.Fatalf("%s 正常路径失败: %v", tc.name, err)
		}
		if len(sink.Content()) == 0 {
			t.Fatalf("%s 在脚本路径没有写结构化结果（C2/C3）", tc.name)
		}
		tc.check(t, sink.Content())

		// 模型路径（非脚本调用）：不写结构化结果（避免同一份数据占两份 token）。
		sink2 := &tools.StructuredSink{}
		if _, err := tc.tool.Call(tools.WithStructuredSink(context.Background(), sink2), "", tc.args); err != nil {
			t.Fatal(err)
		}
		if len(sink2.Content()) != 0 {
			t.Fatalf("%s 在模型路径也写了结构化结果（C3）", tc.name)
		}

		// 默认页：不传 pages 时由**工具**给「前 3 页」的意图（Limit=3），而不是写死页号
		//（写死 [1,2,3] 会让 1 页的文档在 renderImages 里报越界）。
		if tc.name == "office_render" {
			host.gotPages, host.gotLimit = nil, -1
			if _, err := tc.tool.Call(context.Background(), "", `{"path":"deck.pptx"}`); err != nil {
				t.Fatal(err)
			}
			if host.gotLimit != 3 || len(host.gotPages) != 0 {
				t.Fatalf("默认页应是 Limit=3 + 空 Pages，得到 limit=%d pages=%v", host.gotLimit, host.gotPages)
			}
			host.gotPages, host.gotLimit = nil, -1
			if _, err := tc.tool.Call(context.Background(), "", `{"path":"deck.pptx","pages":[2]}`); err != nil {
				t.Fatal(err)
			}
			if host.gotLimit != 3 || len(host.gotPages) != 1 || host.gotPages[0] != 2 {
				t.Fatalf("显式页应透传，得到 limit=%d pages=%v", host.gotLimit, host.gotPages)
			}
		}

		// 失败必须 reject（C4）。
		if _, err := tc.tool.Call(context.Background(), "", `{"path":"missing.pptx"}`); err == nil {
			t.Fatalf("%s 对不存在的文件应返回 error", tc.name)
		}
	}
}

// stubConverter 假近似转换器（counts 调用次数；不走 Python）。
type stubConverter struct {
	html  string
	calls atomic.Int32
}

func (c *stubConverter) Convert(_ context.Context, src, _ string) (conversion.Output, error) {
	c.calls.Add(1)
	p := filepath.Join(filepath.Dir(src), "approximate.html")
	if err := os.WriteFile(p, []byte(c.html), 0o600); err != nil {
		return conversion.Output{}, err
	}
	return conversion.Output{HTMLPath: p}, nil
}

// stubOfficeHost 假宿主（工具契约测试用；不碰 kit/Python）。
type stubOfficeHost struct {
	pdf   string
	pages []officekit.RenderedPage
	total int
	dpi   int
	err   error
	// 最近一次 officeRender 的入参（验「默认前 3 页」与「显式页透传」）
	gotPages []int
	gotLimit int
}

func (h *stubOfficeHost) officeKitReady() bool { return true }

func (h *stubOfficeHost) officePDF(context.Context, string, string) (string, error) {
	if h.err != nil {
		return "", h.err
	}
	return h.pdf, nil
}

func (h *stubOfficeHost) officeRender(_ context.Context, _ string, pages []int, limit, dpi int) (officeRenderOutput, error) {
	if h.err != nil {
		return officeRenderOutput{}, h.err
	}
	h.gotPages, h.gotLimit = pages, limit
	return officeRenderOutput{Pages: h.pages, Total: h.total, DPI: dpi, PDF: h.pdf}, nil
}
