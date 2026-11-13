package officekit

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	goruntime "github.com/seven7628/hai-harness/runtime"
)

// realRuntime 取真实受管运行时；未就绪则 skip（CI 不带运行时，与既有慢测试同口径）。
//
// 为什么不注入假 PythonRuntime：pypdfium2 才是被验的对象（真 PDF → 真 PNG），
// 换个假解释器等于把这条用例变成「我调了我的函数」。
func realRuntime(t *testing.T) *goruntime.Runtime {
	t.Helper()
	if testing.Short() {
		t.Skip("慢测试：需真实受管 Python 运行时（pypdfium2）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rt := goruntime.New(goruntime.Config{})
	st, err := rt.Status(ctx)
	if err != nil || !st.Ready {
		if st.Stale {
			t.Fatalf("受管运行时与依赖清单不匹配（%s）：先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime", st.Reason)
		}
		t.Skipf("受管 Python 运行时未就绪（%s）", st.Reason)
	}
	return rt
}

// twoPagePDF 用受管解释器造一张 2 页 PDF（200×100pt / 300×150pt），返回路径。
//
// 为什么用 pypdf 而不是手拼 PDF 字节：手写 xref 偏移在测试里是一层额外的失败源，
// 而 pypdf 已在依赖清单里（抽文本层用的就是它）。
func twoPagePDF(t *testing.T, rt *goruntime.Runtime) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	py, err := rt.Ensure(ctx)
	if err != nil {
		t.Fatalf("runtime.Ensure: %v", err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "two.pdf")
	script := `from pypdf import PdfWriter
w = PdfWriter()
w.add_blank_page(width=200, height=100)
w.add_blank_page(width=300, height=150)
w.write("` + out + `")
`
	cmd := exec.CommandContext(ctx, py.VenvPython, "-c", script)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造 PDF 失败: %v\n%s", err, b)
	}
	return out
}

// 2 页 PDF → 2 张 PNG，尺寸符合 dpi（144dpi = 2× 点尺寸）。
func TestRenderImagesPagesAndDPI(t *testing.T) {
	rt := realRuntime(t)
	pdf := twoPagePDF(t, rt)
	out := t.TempDir()

	res, err := RenderImages(context.Background(), rt, pdf, out, RenderOptions{DPI: 144})
	if err != nil {
		t.Fatalf("RenderImages: %v", err)
	}
	if res.TotalPages != 2 || len(res.Pages) != 2 {
		t.Fatalf("期望 2 页，得到 total=%d pages=%d", res.TotalPages, len(res.Pages))
	}
	want := []struct{ w, h int }{{400, 200}, {600, 300}}
	for i, p := range res.Pages {
		if p.Number != i+1 {
			t.Fatalf("第 %d 项页号 = %d", i, p.Number)
		}
		if p.Width != want[i].w || p.Height != want[i].h {
			t.Fatalf("第 %d 页尺寸 %dx%d，期望 %dx%d（dpi=144）", p.Number, p.Width, p.Height, want[i].w, want[i].h)
		}
		b, rerr := os.ReadFile(p.Path)
		if rerr != nil {
			t.Fatalf("读 PNG 失败: %v", rerr)
		}
		if len(b) < 8 || string(b[1:4]) != "PNG" {
			t.Fatalf("产物不是 PNG: %s", p.Path)
		}
		t.Logf("page %d: %dx%d, %d bytes, %s", p.Number, p.Width, p.Height, len(b), filepath.Base(p.Path))
	}
}

// 指定页：只渲染要的那一页（office_render 的 pages 参数语义）。
func TestRenderImagesSelectedPages(t *testing.T) {
	rt := realRuntime(t)
	pdf := twoPagePDF(t, rt)
	out := t.TempDir()

	res, err := RenderImages(context.Background(), rt, pdf, out, RenderOptions{DPI: 72, Pages: []int{2}})
	if err != nil {
		t.Fatalf("RenderImages: %v", err)
	}
	if len(res.Pages) != 1 || res.Pages[0].Number != 2 {
		t.Fatalf("期望只渲染第 2 页，得到 %+v", res.Pages)
	}
	if res.Pages[0].Width != 300 || res.Pages[0].Height != 150 {
		t.Fatalf("72dpi 下第 2 页应为 300x150，得到 %dx%d", res.Pages[0].Width, res.Pages[0].Height)
	}
	if res.TotalPages != 2 {
		t.Fatalf("TotalPages = %d，期望 2（页数应反映 PDF 本身）", res.TotalPages)
	}
}

// Limit：默认「前 N 页」不能把不足 N 页的文档变成越界错误（1 页 PDF + Limit=3 → 1 张图）。
func TestRenderImagesLimitClampsToTotalPages(t *testing.T) {
	rt := realRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	py, err := rt.Ensure(ctx)
	if err != nil {
		t.Fatalf("runtime.Ensure: %v", err)
	}
	pdf := filepath.Join(t.TempDir(), "one.pdf")
	script := `from pypdf import PdfWriter
w = PdfWriter()
w.add_blank_page(width=200, height=100)
w.write("` + pdf + `")
`
	if out, err := exec.CommandContext(ctx, py.VenvPython, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("造 1 页 PDF 失败: %v\n%s", err, out)
	}
	res, err := RenderImages(context.Background(), rt, pdf, t.TempDir(), RenderOptions{DPI: 72, Limit: 3})
	if err != nil {
		t.Fatalf("1 页 PDF + Limit=3 不该报错（这正是「默认前 3 页」的真实形态）: %v", err)
	}
	if len(res.Pages) != 1 || res.TotalPages != 1 {
		t.Fatalf("期望 1 页，得到 pages=%d total=%d", len(res.Pages), res.TotalPages)
	}
}

// 越界页码必须报错（静默少给几页是最难察觉的错）。
func TestRenderImagesRejectsOutOfRangePage(t *testing.T) {
	rt := realRuntime(t)
	pdf := twoPagePDF(t, rt)
	_, err := RenderImages(context.Background(), rt, pdf, t.TempDir(), RenderOptions{Pages: []int{9}})
	if err == nil {
		t.Fatal("越界页码应报错")
	}
}
