package main

// refextract_real_test.go —— PDF 抽取器的端到端测试（-short 跳过）：真造 PDF +
// 真受管 Python 运行时（真 pypdf）+ 真 Seatbelt 沙箱，跑生产同一条链路。
//
// 覆盖两件快测试（refextract_test.go）证明不了的事：
//  1. 抽取脚本本身能用（协议标记行/逐页正文在实际 pypdf 下真的产出）；
//  2. **扫描件**（图片页、无文本层）真的落到「无文本层」这一路 —— 而不是静默空串，
//     也不是被误判成「脚本失败 → 回退二进制占位」。
//
// 与 tools/builtin/xlsx_preview_real_test.go 同风格（同样 -short 跳过、同样在无就绪
// 运行时时跳而非失败）。PDF 用无依赖的手写生成器造（requirements 里没有 reportlab/
// fpdf，不能为一个测试引入新依赖）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg" // 标准库 JPEG 解码器（decodeJPEGSize 用 image.DecodeConfig 验产物）
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	goruntime "github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// refLoopProvider 发射一轮 LLM（stop），并捕获发给 provider 的请求消息 ——
// 供断言「模型真正收到的内容」（而不是只断言中间函数的返回值）。
type refLoopProvider struct {
	mu   sync.Mutex
	sent [][]core.Message
}

func (p *refLoopProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.sent = append(p.sent, append([]core.Message(nil), req.Messages...))
	p.mu.Unlock()
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
}

// tailUserText 截取文本尾部（user 消息在 system prompt 之后，日志只留这一截）。
func tailUserText(s string) string {
	if len(s) <= 400 {
		return s
	}
	return "…" + s[len(s)-400:]
}

// lastRequestText 最后一轮请求里全部 user 消息的文本（@引用展开后的内容）。
func (p *refLoopProvider) lastRequestText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sent) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range p.sent[len(p.sent)-1] {
		for _, c := range m.Content {
			if c.Type == core.ContentTypeText {
				b.WriteString(c.Content)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// realPDFExtractor 真实受管运行时（已 bootstrap）+ 真 Seatbelt（可用时）。
// 无就绪运行时/无 Seatbelt 时跳过（CI 不依赖外网与真实安装）。
func realPDFExtractor(t *testing.T, ws string) (func(context.Context, string) (string, bool, error), *goruntime.Runtime) {
	t.Helper()
	if testing.Short() {
		t.Skip("慢测试：需真实受管运行时")
	}
	rt := goruntime.New(goruntime.Config{})
	if st, err := rt.Status(context.Background()); err != nil || !st.Ready {
		// 指纹不匹配 = 运行时**存在但与源码不一致**（旧二进制写过共享运行时 / 源码刚改了
		// 依赖清单）：这是可修复的破损状态，**必须响亮失败**。若在这里 skip，整套真链路测试
		// 会全部静默跳过而套件仍显示全绿 —— 实际一条都没跑（2026-09 实测踩过两次）。
		if st.Stale {
			t.Fatalf("受管运行时与当前依赖清单不匹配（%s）：真链路测试会全部跳过、套件仍显示全绿。"+
				"修复：go test ./runtime/ -run TestRealEnsureInstallsRuntime", st.Reason)
		}
		t.Skipf("无已 bootstrap 的真实受管运行时（先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime）: ready=%v err=%v", st.Ready, err)
	}
	if !sandbox.Available() {
		t.Skip("无 sandbox-exec（非 macOS），跳过真实沙箱链路")
	}
	sb, err := sandbox.NewSeatbelt(ws)
	if err != nil {
		t.Skipf("seatbelt 策略探测失败（本环境不支持真实沙箱）: %v", err)
	}
	return newPDFExtractor(rt, func(string) sandbox.Sandbox { return sb }, ws), rt
}

// 手写最小 PDF（无第三方依赖）：一个文本页，内容 "Quarterly Report" /
// "Revenue up 12 percent"。必须带正确的 xref/startxref —— pypdf 严格要求，
// 缺了会 PdfReadError("startxref not found")。
const mkTextPDFScript = `
objs = []
objs.append(b"<< /Type /Catalog /Pages 2 0 R >>")
objs.append(b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
objs.append(b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "
            b"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>")
content = (b"BT /F1 24 Tf 72 700 Td (Quarterly Report) Tj ET\n"
           b"BT /F1 12 Tf 72 660 Td (Revenue up 12 percent) Tj ET\n")
objs.append(b"<< /Length %d >>\nstream\n" % len(content) + content + b"endstream")
objs.append(b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

out = bytearray(b"%PDF-1.4\n")
offsets = []
for i, body in enumerate(objs, start=1):
    offsets.append(len(out))
    out += b"%d 0 obj\n" % i + body + b"\nendobj\n"
xref = len(out)
out += b"xref\n0 %d\n" % (len(objs) + 1)
out += b"0000000000 65535 f \n"
for off in offsets:
    out += b"%010d 00000 n \n" % off
out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs) + 1, xref)
open("text.pdf", "wb").write(bytes(out))
`

// 图片型 PDF（模拟扫描件）：整页一张位图、没有任何文字对象 → pypdf 抽不到文本。
const mkScanPDFScript = `
from PIL import Image
im = Image.new("RGB", (600, 400), "white")
px = im.load()
for y in range(80, 240):          # 画几道"文字"般的黑条，但不含任何文本对象
    for x in range(60, 540):
        if (y // 12) % 2 == 0:
            px[x, y] = (0, 0, 0)
im.save("scan.pdf", "PDF", resolution=72)
`

// 多页扫描件（pages 参数由调用方经 argv 传入）：顺带用 A4 纸面尺寸
// （595x842pt，长边 842 → 1568/842 = 1.86 倍 scale），覆盖「按长边反算 scale」这条路径。
// 每页画不同位置的色块：产物图不是同一张，能顺带证明逐页渲染真的逐页不同。
const mkMultiScanPDFScript = `
import sys
from PIL import Image
pages = []
n = int(sys.argv[1])
for i in range(n):
    im = Image.new("RGB", (595, 842), "white")
    px = im.load()
    top = 60 + (i * 90) % 640     # 每页色块位置不同
    for y in range(top, min(top + 80, 842)):
        for x in range(60, 535):
            if (y // 10 + i) % 2 == 0:
                px[x, y] = (0, 0, 0)
    pages.append(im)
pages[0].save("multi.pdf", "PDF", resolution=72, save_all=True, append_images=pages[1:])
`

// letterPageLongEdgePt Letter 纸面长边（pt）：612x792 中的 792。
// 选它当测试纸面因为它是「固定 scale=2.0 会刚好越线」的典型（1584 > 1568）。
const letterPageLongEdgePt = 792.0

// 大纸面扫描件：**专门用来抓固定 scale 的坑**。Letter 纸 612x792pt 在 scale=2.0 下
// 渲染成 1224x1584 —— 长边 1584 刚好越过引擎的 1568 阈值，read_file 会再降采样重编码一次
// （白烧字节 + 多一次质量损失）。按长边反算（1568/792 ≈ 1.98）才落在阈值内。
// 用小纸面（600x400）测不出这个差别：那边 scale=2.0 只有 1200px，本来就没越线。
const mkLargePageScanPDFScript = `
from PIL import Image
im = Image.new("RGB", (612, 792), "white")   # Letter 纸面（pt）
px = im.load()
for y in range(100, 700):
    for x in range(70, 540):
        if (y // 14) % 2 == 0:
            px[x, y] = (0, 0, 0)
im.save("letter.pdf", "PDF", resolution=72)
`

// runPyArgs 同 runPy，但给造件脚本透传 argv（多页造件的页数）。
func runPyArgs(t *testing.T, rt *goruntime.Runtime, ws, name, src string, args ...string) string {
	t.Helper()
	ctx := context.Background()
	pyh, err := rt.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	p, cleanup, err := builtin.WriteTempFile(pyh, "pdf-mk-", "mk.py", []byte(src))
	if err != nil {
		t.Fatalf("写造件脚本: %v", err)
	}
	defer cleanup()
	cmd := builtin.ShQuote(pyh.VenvPython) + " " + builtin.ShQuote(p)
	for _, a := range args {
		cmd += " " + builtin.ShQuote(a)
	}
	if out, err := (sandbox.NoSandbox{}).Run(ctx, sandbox.ExecSpec{
		Command: cmd, Cwd: ws, Timeout: 2 * time.Minute,
	}); err != nil || strings.Contains(out, "[exit:") {
		t.Fatalf("造 %s 失败: %v / %s", name, err, out)
	}
	return filepath.Join(ws, name)
}

// pdfImagePathsFrom 从抽取正文里挑出产物路径行（正文格式见 pdfRenderNotice：
// 首行说明 + 每行一条路径 + 末行指引）—— 模拟模型「照着正文去调 read_file」。
func pdfImagePathsFrom(t *testing.T, content string) []string {
	t.Helper()
	var paths []string
	for _, l := range strings.Split(content, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "/") && strings.HasSuffix(l, ".jpg") {
			paths = append(paths, l)
		}
	}
	if len(paths) == 0 {
		t.Fatalf("正文里没有可用的图片路径: %q", content)
	}
	return paths
}

// cleanupRenderedImages 在测试结束时删掉产物目录。
//
// 为什么测试要自己收（生产靠 sweepStalePDFImageDirs 按年龄回收）：
//   - 产物的正确行为是**比调用活得久**（见 renderScannedPDF），故不能在抽取函数里删；
//     但测试反复跑会在共享的 ~/.go-code/runtime/python/tmp 里堆几十个目录，
//     而年龄清扫要等 1 小时才生效（实测：一轮 TDD 跑下来堆了 95 个）。
//   - 与 xlsx 预览真链路测试的 `defer os.RemoveAll(res.Dir)` 同口径。
//
// 放在 t.Cleanup 而不是立刻删：断言（含「产物在调用返回后仍在」那条）跑完了才轮到它。
func cleanupRenderedImages(t *testing.T, paths []string) {
	t.Helper()
	for _, p := range paths {
		dir := filepath.Dir(p)
		if strings.Contains(filepath.Base(dir), pdfImageDirPrefix) {
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
		}
	}
}

// decodeJPEGSize 用标准库真解码图片并返回尺寸：既证明文件是**合法 JPEG**
// （不是「扩展名对了、内容截断」），又拿到像素尺寸供「长边 ≤ 1568」断言。
func decodeJPEGSize(t *testing.T, path string) (int, int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开产物 %s: %v", path, err)
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("产物 %s 不是可解码的图片（扩展名骗人）: %v", path, err)
	}
	if format != "jpeg" {
		t.Fatalf("产物 %s 应为 JPEG（引擎按此格式内嵌）, got %s", path, format)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return cfg.Width, cfg.Height, int(st.Size())
}

// mkShadowVenv 造一份受管运行时的**影子副本**，剔除 skip 前缀的包 —— 模拟「用户机器上
// 这个库没装上」（真实存在：pypdfium2 5.10+ 的 wheel 平台标签会挡住 macOS 11/12 用户，
// 见 runtime/python.go 的版本钉注释）。
//
// 为什么这样做而不是改名/删真运行时的目录：真运行时是进程外共享单例（用户会话可能正在
// 用），动它是破坏性操作；且只删一个目录 ≠ 缺库（别的 site-packages 仍可能解析到）。
// 影子副本里逐个符号链接（跳过目标包）—— 解释器真跑、sys.prefix 指向影子根，
// 与「pip 没装成」的现场同构，且完全落在 t.TempDir() 里，跑完即弃。
//
// 两个**必须**一并做的细节（少一个测试就白测）：
//   - pyvenv.cfg 要复制：解释器靠它认 sys.prefix（缺了会退回基座环境 → 库其实都在）；
//   - .ready.json 要造：否则 Ensure 视为「未 bootstrap」→ 真去 pip install 把缺的包补上
//     （实测：这就是本测试第一版失败的原因 —— 影子环境被 pip 补全后渲染照样成功）。
//     指纹不是硬编码：按 runtime.fingerprint 的定义（requirements.lock 内容的 sha256
//     前 16 字节）现算，故 runtime/python.go 改依赖清单时这里自动跟随。
func mkShadowVenv(t *testing.T, pyh goruntime.Python, dst, skipPrefix string) error {
	t.Helper()
	sp, err := filepath.Glob(filepath.Join(pyh.VenvDir, "lib", "python*", "site-packages"))
	if err != nil || len(sp) != 1 {
		return fmt.Errorf("定位 site-packages 失败: %v (%v)", err, sp)
	}
	shadowVenv := filepath.Join(dst, "venv")
	shadowSP := filepath.Join(shadowVenv, "lib", filepath.Base(filepath.Dir(sp[0])), "site-packages")
	if err := os.MkdirAll(shadowSP, 0o700); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(pyh.VenvDir, "pyvenv.cfg"), filepath.Join(shadowVenv, "pyvenv.cfg")); err != nil {
		return err
	}
	// bin/python 指向 venv 惯例的目标（venv 的 bin/python 本就是指向基座解释器的符号链接，
	// 照抄这个链接即可 —— 硬链不可行：跨设备会失败）。
	binDir := filepath.Join(shadowVenv, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return err
	}
	if err := os.Symlink(pyh.VenvPython, filepath.Join(binDir, "python")); err != nil {
		return err
	}
	entries, err := os.ReadDir(sp[0])
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), skipPrefix) {
			continue // 这就是「没装这个库」的现场
		}
		if err := os.Symlink(filepath.Join(sp[0], e.Name()), filepath.Join(shadowSP, e.Name())); err != nil {
			return err
		}
	}
	// 就绪标记：指纹按 runtime.fingerprint 的定义现算（见函数注释）。
	lock, err := os.ReadFile(filepath.Join(pyh.Root, "requirements.lock"))
	if err != nil {
		return fmt.Errorf("读 requirements.lock: %w", err)
	}
	sum := sha256.Sum256(lock)
	marker := fmt.Sprintf("{\n  \"version\": 1,\n  \"requirements\": %q,\n  \"base\": %q,\n  \"venv_python\": %q,\n  \"created_at\": %q\n}\n",
		hex.EncodeToString(sum[:16]), pyh.Base, filepath.Join(binDir, "python"), time.Now().UTC().Format(time.RFC3339))
	return os.WriteFile(filepath.Join(dst, ".ready.json"), []byte(marker), 0o600)
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// runPy 用受管解释器跑一段造文件脚本（NoSandbox 直通：造文件不是被测对象）。
func runPy(t *testing.T, rt *goruntime.Runtime, ws, name, src string) string {
	t.Helper()
	ctx := context.Background()
	pyh, err := rt.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	p, cleanup, err := builtin.WriteTempFile(pyh, "pdf-mk-", "mk.py", []byte(src))
	if err != nil {
		t.Fatalf("写造件脚本: %v", err)
	}
	defer cleanup()
	if out, err := (sandbox.NoSandbox{}).Run(ctx, sandbox.ExecSpec{
		Command: builtin.ShQuote(pyh.VenvPython) + " " + builtin.ShQuote(p), Cwd: ws, Timeout: 2 * time.Minute,
	}); err != nil || strings.Contains(out, "[exit:") {
		t.Fatalf("造 %s 失败: %v / %s", name, err, out)
	}
	return filepath.Join(ws, name)
}

// 有文本层的 PDF → 真抽出正文，且页数标注/页分隔行都在。
func TestPDFExtractorRealTextLayer(t *testing.T) {
	ws := t.TempDir()
	ext, rt := realPDFExtractor(t, ws)
	pdf := runPy(t, rt, ws, "text.pdf", mkTextPDFScript)

	content, ok, err := ext(context.Background(), pdf)
	if err != nil {
		t.Fatalf("抽取报错: %v", err)
	}
	if !ok {
		t.Fatal("有文本层的 PDF 应抽到内容（ok=true）")
	}
	for _, want := range []string{"[PDF 文本，共 1 页]", "--- 第 1 页 ---", "Quarterly Report", "Revenue up 12 percent"} {
		if !strings.Contains(content, want) {
			t.Errorf("抽取内容缺少 %q\n got %q", want, content)
		}
	}
	if strings.Contains(content, "%PDF-") {
		t.Error("不应含 PDF 源码（修复前会注入源码）")
	}
	t.Logf("✅ 抽取成功：%q", content)
}

// 图片型 PDF（扫描件）→ 渲染成图片路径（ok=true），**不是** err。
// 这是"显式告知而不是静默空"这条产品决策在真实链路下的证明；本测试的断言随
// 「渲染成图片」能力上线而升级：正文里现在带的是**真能 read_file 看到的图**。
func TestPDFExtractorRealScannedNoTextLayer(t *testing.T) {
	ws := t.TempDir()
	ext, rt := realPDFExtractor(t, ws)
	pdf := runPy(t, rt, ws, "scan.pdf", mkScanPDFScript)

	content, ok, err := ext(context.Background(), pdf)
	if err != nil {
		t.Fatalf("图片型 PDF 不应报环境错误（应落到「无文本层」并渲染为图片）: %v", err)
	}
	if !ok {
		t.Fatalf("扫描件应渲染成图片并返回路径（ok=true）, got content=%q", content)
	}
	if !strings.Contains(content, "无文本层") {
		t.Errorf("正文仍须说明「无文本层」（不能说成抽到了文字）, got %q", content)
	}
	// 扫描件的判定不能靠 pypdf 的 extract_text() 侥幸成功：正文里不该有 PDF 源码
	if strings.Contains(content, "%PDF-") {
		t.Errorf("不应含 PDF 源码\n got %q", content)
	}
	// 本测试只关心「扫描件被识别出来」；产物本身由渲染专项测试断言，这里顺手收掉
	// （否则反复跑会在共享的受管 tmp 里堆目录 —— 生产的年龄清扫要 1 小时才生效）。
	cleanupRenderedImages(t, pdfImagePathsFrom(t, content))
}

// 端到端（生产链路）：真 AgentLoop + WithRefExtractor（真抽取器）→ 走 streamOnce
// 的 @引用展开 → 断言**模型真正收到的请求消息**。这是用户报的那个场景本身：
// 「@引用一个 PDF，模型拿不到内容」。
func TestPDFExtractorEndToEndAgentLoop(t *testing.T) {
	ws := t.TempDir()
	ext, rt := realPDFExtractor(t, ws)
	textPDF := runPy(t, rt, ws, "text.pdf", mkTextPDFScript)
	scanPDF := runPy(t, rt, ws, "scan.pdf", mkScanPDFScript)

	prov := &refLoopProvider{}
	loop := agents.NewAgentLoop(
		agents.WithProvider(prov),
		agents.WithWorkingDir(ws),
		agents.WithRefExtractor(ext),
	)
	ac := loop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "总结 @" + textPDF}),
	}, func(context.Context, events.Event) {})
	if ac.Err != nil {
		t.Fatalf("Run: %v", ac.Err)
	}

	got := prov.lastRequestText()
	for _, want := range []string{"[PDF 文本，共 1 页]", "Quarterly Report", "Revenue up 12 percent"} {
		if !strings.Contains(got, want) {
			t.Errorf("模型收到的内容缺少 %q\n got %q", want, got)
		}
	}
	if strings.Contains(got, "%PDF-") {
		t.Errorf("模型不应收到 PDF 源码（修复前行为）\n got %q", got)
	}

	// 扫描件：第二条消息 → 模型收到的是显式告知
	prov2 := &refLoopProvider{}
	loop2 := agents.NewAgentLoop(
		agents.WithProvider(prov2),
		agents.WithWorkingDir(ws),
		agents.WithRefExtractor(ext),
	)
	ac2 := loop2.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "看看 @" + scanPDF}),
	}, func(context.Context, events.Event) {})
	if ac2.Err != nil {
		t.Fatalf("Run: %v", ac2.Err)
	}
	scan := prov2.lastRequestText()
	if !strings.Contains(scan, "无文本层") {
		t.Errorf("扫描件应显式告知无文本层\n got %q", scan)
	}
	// 扫描件现在**有内容可看**：模型收到的正文里必须带真图片路径（本能力的目标），
	// 且路径经 read_file 真能变成图片块（见 TestPDFExtractorRealImagesReadableByReadFile）。
	// 这里只断言「路径出现在模型请求里」—— 即用户报的「拿不到内容」已经变成「拿到了图」。
	imgPaths := pdfImagePathsFrom(t, scan)
	cleanupRenderedImages(t, imgPaths)
	for _, p := range imgPaths {
		if _, _, size := decodeJPEGSize(t, p); size <= 0 {
			t.Errorf("模型收到的路径指向空产物: %s", p)
		}
	}
	// 仍然不能是二进制占位（那说明走了 err 分支、丢掉了「无文本层」这条准确信息）
	if strings.Contains(scan, "二进制文件") {
		t.Errorf("扫描件不应回退成二进制占位（那就丢掉了「抽不到」的告知）\n got %q", scan)
	}
	// 日志只留 user 部分（system prompt 有几千字符，全量打出来会淹没信号）
	t.Logf("✅ 文本 PDF → 模型收到的 user 消息: %q", tailUserText(got))
	t.Logf("✅ 扫描 PDF → 模型收到的 user 消息: %q", tailUserText(scan))
}

// 真实场景：**工作区外**的 PDF（用户从 ~/Desktop / 下载目录上传是常态）。
// 沙箱根 = 会话工作区，而被引用文件在别处 —— 这条路径必须真跑一次，证明
// Seatbelt 策略下「读工作区外的引用文件」可行（不然抽取会在生产里 EPERM，
// 而单测全绿）。
func TestPDFExtractorRealOutsideWorkspace(t *testing.T) {
	ws := t.TempDir()      // 会话工作区
	outside := t.TempDir() // 模拟 ~/Desktop（另一个目录，不在 ws 内）
	ext, rt := realPDFExtractor(t, ws)
	pdf := runPy(t, rt, outside, "text.pdf", mkTextPDFScript)

	content, ok, err := ext(context.Background(), pdf)
	if err != nil {
		t.Fatalf("工作区外 PDF 抽取报错（沙箱边界需复查）: %v", err)
	}
	if !ok || !strings.Contains(content, "Quarterly Report") {
		t.Fatalf("工作区外 PDF 应抽到内容: ok=%v content=%q", ok, content)
	}
	t.Logf("✅ 工作区外 PDF 抽取成功（沙箱未拦）: %q", content)
}

// 沙箱拦截（EPERM）必须与「无文本层」区分开：被拦的文件读不出 → 脚本非零退出、
// 没有协议标记行 → **err**（调用方回退二进制占位），绝不静默报成「这个 PDF 没文字」。
//
// 造法：用 WithSensitivePaths 把一个临时目录声明为敏感（deny 读），PDF 放在里面。
func TestPDFExtractorRealSandboxDeniedIsError(t *testing.T) {
	ws := t.TempDir()
	denied := t.TempDir() // 将被声明为敏感（deny 读写）
	if testing.Short() {
		t.Skip("慢测试：需真实受管运行时")
	}
	rt := goruntime.New(goruntime.Config{})
	if st, err := rt.Status(context.Background()); err != nil || !st.Ready {
		if st.Stale {
			t.Fatalf("受管运行时与当前依赖清单不匹配（%s）：真链路测试会全部跳过而套件显示全绿。"+
				"修复：go test ./runtime/ -run TestRealEnsureInstallsRuntime", st.Reason)
		}
		t.Skipf("无已 bootstrap 的真实受管运行时: ready=%v err=%v", st.Ready, err)
	}
	if !sandbox.Available() {
		t.Skip("无 sandbox-exec（非 macOS）")
	}
	sb, err := sandbox.NewSeatbelt(ws, sandbox.WithSensitivePaths([]string{denied}))
	if err != nil {
		t.Skipf("seatbelt 策略探测失败: %v", err)
	}
	ext := newPDFExtractor(rt, func(string) sandbox.Sandbox { return sb }, ws)
	pdf := runPy(t, rt, denied, "text.pdf", mkTextPDFScript)

	content, ok, err := ext(context.Background(), pdf)
	if err == nil {
		t.Fatalf("沙箱拒绝读入时不应成功（ok=%v content=%q）—— 否则 EPERM 被当成「无文本层」", ok, content)
	}
	if ok || content != "" {
		t.Errorf("失败时应回空: ok=%v content=%q", ok, content)
	}
	t.Logf("✅ 沙箱拦截 → err（回退二进制占位，不误报无文本层）: %v", err)
}

// ---------- 无文本层 → 渲染成图片（真链路） ----------

// 核心交付：扫描件 → ok=true，正文里的路径**真在磁盘上、真是合法 JPEG、尺寸符合引擎口径**。
//
// 断言三件事，对应三条不同的失败方式：
//  1. 路径存在 —— 最直白的「产物真的落地了」（写错目录/被 cleanup 删掉都会在这里现形）；
//  2. 标准库真解码 + format=="jpeg" —— 防「扩展名对、内容截断/是别的格式」；
//  3. 长边 ≤ pdfImageMaxDim —— 引擎不再对它降采样重编码（这是「按长边反算 scale」的目的）。
//
// 用 **Letter 纸面**（612x792pt）：只在纸面够大时这条断言才有区分力 —— 固定 scale=2.0
// 会渲染出 1584px 长边、越过阈值，故本测试能真正抓住「按长边反算」这个设计点
// （小纸面 600x400 在 scale=2.0 下只有 1200px，改坏了也测不出来 —— 已用变异验证过）。
func TestPDFExtractorRealScannedRendersImages(t *testing.T) {
	ws := t.TempDir()
	ext, rt := realPDFExtractor(t, ws)
	pdf := runPy(t, rt, ws, "letter.pdf", mkLargePageScanPDFScript)

	content, ok, err := ext(context.Background(), pdf)
	if err != nil {
		t.Fatalf("渲染扫描件报错（应降级为「无文本层」而不是错误）: %v", err)
	}
	if !ok {
		t.Fatalf("扫描件应渲染成图片并返回路径:\ncontent=%q", content)
	}
	paths := pdfImagePathsFrom(t, content)
	cleanupRenderedImages(t, paths)
	if len(paths) != 1 {
		t.Fatalf("单页扫描件应产出 1 张图, got %d (%v)", len(paths), paths)
	}
	scales := renderedScales(t, pdf)
	w, h, size := decodeJPEGSize(t, paths[0])
	if w <= 0 || h <= 0 {
		t.Fatalf("产物尺寸异常: %dx%d", w, h)
	}
	if longest := max(w, h); longest > pdfImageMaxDim {
		t.Errorf("长边 %d 超过引擎阈值 %d：read_file 会二次降采样（scale 应按长边反算）", longest, pdfImageMaxDim)
	}
	if size <= 0 {
		t.Errorf("产物为空文件: %s", paths[0])
	}
	if !strings.Contains(paths[0], pdfImageDirPrefix) {
		t.Errorf("产物应落在 %s* 长期保留目录（便于按年龄清扫）, got %s", pdfImageDirPrefix, paths[0])
	}
	// scale 必须是**按长边反算**的（不是固定 2.0）。断言写成「scale × 纸面长边 ≈ 1568」
	// 而不是「scale ≈ 1.98 常量」：前者直接表达设计意图，且区分度大得多 ——
	// Letter 长边 792pt 下，按长边反算得 1568（正中阈值），固定 scale=2.0 得 1584（越线 16px）。
	// 用常量比较时两者只差 0.02，容差稍宽就漏（第一版就是这么漏掉的，已用变异验证）。
	//
	// 为什么必须从脚本实际输出的 scale 上验证：位图最终尺寸由脚本里的 clamp 兜底，
	// 两种实现产出的**像素尺寸相同**（固定 scale 只是白绕一次重采样），光看产物认不出。
	if got := scales[0] * letterPageLongEdgePt; math.Abs(got-pdfImageMaxDim) > 1.5 {
		t.Errorf("渲染倍率应让长边落在 %d px（按长边反算）, 实际指向 %.1f px（scale=%.4f）—— "+
			"固定 scale 会先渲成 1584px 再被钳回，白绕一次重采样", pdfImageMaxDim, got, scales[0])
	}
	// 正文须说明这是「无文本层」的产物，不能冒充抽到的文字
	if !strings.Contains(content, "无文本层") {
		t.Errorf("正文应说明无文本层: %q", content)
	}
	t.Logf("✅ 扫描件渲染成功: %dx%d JPEG %d 字节 → %s", w, h, size, paths[0])
	t.Logf("✅ 返回给模型的正文: %q", content)
}

// renderedScales 取一次抽取里脚本实际用的渲染倍率（见 parsePDFRenderOutputDetailed）。
// 通过真实脚本执行路径拿到 —— 只解析脚本输出是不足以证明「脚本真按长边算」的。
func renderedScales(t *testing.T, pdf string) []float64 {
	t.Helper()
	ctx := context.Background()
	if testing.Short() {
		t.Skip("慢测试：需真实受管运行时")
	}
	rt := goruntime.New(goruntime.Config{})
	pyh, err := rt.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	script, cleanup, err := builtin.WriteTempFile(pyh, "pdf-render-probe-", "render_pdf.py", []byte(pdfRenderScript))
	if err != nil {
		t.Fatalf("写渲染脚本: %v", err)
	}
	defer cleanup()
	outDir, err := mkdirPDFImageDir(pyh)
	if err != nil {
		t.Fatalf("建产物目录: %v", err)
	}
	defer os.RemoveAll(outDir)
	out, err := (sandbox.NoSandbox{}).Run(ctx, sandbox.ExecSpec{
		Command: builtin.ShQuote(pyh.VenvPython) + " " + builtin.ShQuote(script) + " " +
			builtin.ShQuote(pdf) + " " + builtin.ShQuote(outDir),
		Cwd: filepath.Dir(script), Timeout: pdfRenderTimeout,
	})
	if err != nil {
		t.Fatalf("渲染脚本执行失败: %v / %s", err, out)
	}
	_, scales, _ := parsePDFRenderOutputDetailed(out)
	if len(scales) == 0 {
		t.Fatalf("未解析到渲染倍率（脚本输出协议变了?）: %s", out)
	}
	if !strings.Contains(out, pdfRenderOKMarker) {
		t.Fatalf("渲染脚本未走完（无 OK 标记）: %s", out)
	}
	return scales
}

// 前 5 页上限（共 7 页）：模型拿到 5 条路径、且正文如实说明「共 7 页」。
// 这是「不刷爆上下文」那条决策在真实链路下的证明。
func TestPDFExtractorRealScannedCapsAtFivePages(t *testing.T) {
	ws := t.TempDir()
	ext, rt := realPDFExtractor(t, ws)
	pdf := runPyArgs(t, rt, ws, "multi.pdf", mkMultiScanPDFScript, "7")

	content, ok, err := ext(context.Background(), pdf)
	if err != nil {
		t.Fatalf("渲染多页扫描件报错: %v", err)
	}
	if !ok {
		t.Fatalf("多页扫描件应渲染成图片:\ncontent=%q", content)
	}
	paths := pdfImagePathsFrom(t, content)
	cleanupRenderedImages(t, paths)
	if len(paths) != pdfImagePagesMax {
		t.Fatalf("7 页扫描件应只渲染前 %d 页, got %d (%v)", pdfImagePagesMax, len(paths), paths)
	}
	if !strings.Contains(content, "共 7 页") {
		t.Errorf("正文应如实标注总页数（7）, got %q", content)
	}
	for i, p := range paths {
		if _, _, size := decodeJPEGSize(t, p); size <= 0 {
			t.Errorf("第 %d 张产物为空: %s", i+1, p)
		}
	}
	// 逐页渲染真的逐页不同（防「5 条路径指向同一张图」这类伪实现）
	first, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	last, err := os.ReadFile(paths[len(paths)-1])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, last) {
		t.Error("第 1 页与第 5 页产物字节完全相同：逐页渲染没生效")
	}
	t.Logf("✅ 7 页扫描件 → 渲染 %d 页: %v", len(paths), paths)
}

// 产物必须比**本次抽取调用**活得久：调用返回后再自查一次，并显式证明
// 「函数返回没有把目录一起带走」（最初的实现用 builtin.WriteTempFile 的 cleanup，
// 那会在函数返回时删掉整个目录 —— 模型几轮之后 read_file 就是 file not found）。
func TestPDFExtractorRealImagesOutliveTheCall(t *testing.T) {
	ws := t.TempDir()
	ext, rt := realPDFExtractor(t, ws)
	pdf := runPy(t, rt, ws, "scan.pdf", mkScanPDFScript)

	content, ok, err := ext(context.Background(), pdf)
	if err != nil || !ok {
		t.Fatalf("渲染失败: ok=%v err=%v content=%q", ok, err, content)
	}
	paths := pdfImagePathsFrom(t, content)
	cleanupRenderedImages(t, paths)

	// 模拟「模型隔了几轮对话才去读」：调用栈早已返回，期间还有别的抽取发生。
	time.Sleep(50 * time.Millisecond)
	other := runPyArgs(t, rt, ws, "multi.pdf", mkMultiScanPDFScript, "6")
	second, ok2, err2 := ext(context.Background(), other)
	if err2 != nil || !ok2 {
		t.Fatalf("第二次抽取失败（不应影响第一次的产物）: ok=%v err=%v", ok2, err2)
	}
	cleanupRenderedImages(t, pdfImagePathsFrom(t, second))

	for _, p := range paths {
		if _, _, size := decodeJPEGSize(t, p); size <= 0 {
			t.Errorf("产物在后续抽取后消失/损坏（被 cleanup 或清扫误删）: %s", p)
		}
	}
	t.Logf("✅ 后续抽取后首次产物仍在: %s", paths[0])
}

// ---------- 端到端闭环：路径 → read_file → 模型真的看得见 ----------
//
// 这是本能力「真的把图交给模型」的唯一证明：只用 read_file（生产同一条通路
// tools/builtin.FileTools，含魔数识别 → 降采样 → image 内容块）去读渲染产物，
// 断言拿到的是 **image 内容块**（data URL），而不是文本/错误。
//
// 为什么必须真断言内容块：抽取器返回的是**路径**（故意的间接层），如果 read_file
// 读不出图，「已渲染为图片」这句话就是空头支票 —— 路径存在不代表模型看得见。
func TestPDFExtractorRealImagesReadableByReadFile(t *testing.T) {
	ws := t.TempDir()
	ext, rt := realPDFExtractor(t, ws)
	pdf := runPy(t, rt, ws, "scan.pdf", mkScanPDFScript)

	content, ok, err := ext(context.Background(), pdf)
	if err != nil || !ok {
		t.Fatalf("渲染失败: ok=%v err=%v content=%q", ok, err, content)
	}
	paths := pdfImagePathsFrom(t, content)
	cleanupRenderedImages(t, paths)
	target := paths[0]

	// 生产同一条 read_file 通路（工作区根 = ws；产物在工作区外 → 走绝对路径分支，
	// 这正是模型会用的形态，也顺带证明 ~/.go-code/runtime 子树对文件工具放行）
	ft := builtin.NewFileTools(ws)
	var readTool tools.Tool
	for _, tl := range ft.Tools() {
		if tl.Name() == "read_file" {
			readTool = tl
		}
	}
	if readTool == nil {
		t.Fatal("未找到 read_file 工具")
	}
	args, _ := json.Marshal(map[string]string{"path": target})
	ctx := tools.WithImageSink(context.Background(), &tools.ImageSink{})
	sinkObj := tools.ImageSinkFrom(ctx)
	if sinkObj == nil {
		t.Fatal("图片收集槽未注入")
	}
	out, err := readTool.Call(ctx, "read_file", string(args))
	if err != nil {
		t.Fatalf("read_file 读渲染产物失败（模型就看不到图了）: %v", err)
	}

	imgs := sinkObj.Images()
	if len(imgs) != 1 {
		t.Fatalf("read_file 应产出 1 个 image 内容块, got %d（文本输出 %q）", len(imgs), out)
	}
	blk := imgs[0]
	if blk.Type != core.ContentTypeImage {
		t.Errorf("内容块类型应为 image, got %q", blk.Type)
	}
	if !strings.HasPrefix(blk.Content, "data:image/jpeg;base64,") {
		t.Errorf("内容块应是 JPEG data URL, got %.60s…", blk.Content)
	}
	if blk.MimeType != "image/jpeg" {
		t.Errorf("MimeType 应为 image/jpeg, got %q", blk.MimeType)
	}
	// 引擎闸门：单图 3MiB（tools.defaultMaxImageBytes）。超了模型**看不到图**（被丢弃），
	// 故这里必须断言在我们的口径下不会撞闸门。
	if n := len(blk.Content); n > 3<<20 {
		t.Errorf("data URL %d 字节超过引擎 3MiB 闸门（模型将看不到图）", n)
	}
	t.Logf("✅ read_file 真读出图片内容块: %s", out)
	t.Logf("✅ data URL %d 字节（引擎闸门 3MiB 内，模型可见）", len(blk.Content))
}

// ---------- 降级：pypdfium2 不可用 → 保持今天的行为 ----------
//
// 用**同一份受管运行时的影子副本**模拟「装好了 pypdf 但没装 pypdfium2」：克隆目录里
// 逐个符号链接 site-packages（跳过 pypdfium2*），pyvenv.cfg 与解释器一起复制 ——
// 解释器真跑、sys.prefix 指向影子根，与「用户机器上库没装上」同构。
//
// 断言 ok=false **且 err=nil**：这是本次改动最容易做错的一处 —— 把它变成 err 会让
// 调用方回退成「[二进制文件，不读内容]」，比「无文本层」这句更差（丢掉了准确信息）。
//
// 为什么不用「把真运行时改个名」：那是台共享机器上的真运行时（用户会话可能正在用），
// 且 pypdfium2 的导入解析会穿透到别的 site-packages，改名根本不是「缺库」的忠实模拟。
func TestPDFExtractorRealRenderUnavailableFallsBack(t *testing.T) {
	ws := t.TempDir()
	if testing.Short() {
		t.Skip("慢测试：需真实受管运行时")
	}
	if !sandbox.Available() {
		t.Skip("无 sandbox-exec（非 macOS）")
	}
	real := goruntime.New(goruntime.Config{})
	st, err := real.Status(context.Background())
	if err != nil || !st.Ready {
		t.Skipf("无已 bootstrap 的真实受管运行时: ready=%v err=%v", st.Ready, err)
	}
	pyh, err := real.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	shadow := t.TempDir()
	if err := mkShadowVenv(t, pyh, shadow, "pypdfium2"); err != nil {
		t.Fatalf("建影子运行时失败: %v", err)
	}
	shadowPy := filepath.Join(shadow, "venv", "bin", "python")

	// 前置校验：影子运行时必须**已经就绪**（否则 Ensure 会去 pip install 把缺的包补上，
	// 「降级成功」就成了假象 —— 第一版测试正是这样失败的）。同时确认它真缺 pypdfium2
	// 而不缺别的：不校验的话，「降级」可能只是因为整个影子环境是坏的，测不到想测的路。
	shadowRT := goruntime.New(goruntime.Config{Root: shadow})
	sst, err := shadowRT.Status(context.Background())
	if err != nil || !sst.Ready {
		t.Fatalf("影子运行时须被视为已就绪（否则 Ensure 会 pip install 补全缺的包，测不出降级）: ready=%v reason=%q err=%v",
			sst.Ready, sst.Reason, err)
	}
	probe := "import pypdf, PIL\n" +
		"try:\n    import pypdfium2\n    print('HAS_PYPDFIUM')\nexcept ImportError:\n    print('NO_PYPDFIUM')\n"
	out, err := (sandbox.NoSandbox{}).Run(context.Background(), sandbox.ExecSpec{
		Command: builtin.ShQuote(shadowPy) + " -c " + builtin.ShQuote(probe), Cwd: ws, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("影子解释器不可用: %v / %s", err, out)
	}
	if !strings.Contains(out, "NO_PYPDFIUM") {
		t.Fatalf("影子环境应缺 pypdfium2 但能跑 pypdf/PIL, got %q", out)
	}

	// 影子运行时作为「抽取器看到的受管运行时」：抽取（pypdf 在）成功走到「无文本层」，
	// 渲染（pypdfium2 不在）失败 → 应降级成 ("", false, nil)。
	sb, err := sandbox.NewSeatbelt(ws)
	if err != nil {
		t.Skipf("seatbelt 策略探测失败: %v", err)
	}
	ext := newPDFExtractor(shadowRT, func(string) sandbox.Sandbox { return sb }, ws)
	pdf := runPy(t, real, ws, "scan.pdf", mkScanPDFScript)

	content, ok, err := ext(context.Background(), pdf)
	if err != nil {
		t.Fatalf("渲染器不可用**不该**变成 err（那会让调用方回退成「二进制文件，不读内容」，"+
			"比「无文本层」更差）: %v", err)
	}
	if ok || content != "" {
		t.Fatalf("渲染器不可用时应保持今天的行为 (\"\", false, nil), got ok=%v content=%q", ok, content)
	}
	// 收尾复检：整个降级流程里真运行时不该被动过（它是用户会话可能正在用的共享单例）。
	if rst, rerr := real.Status(context.Background()); rerr != nil || !rst.Ready {
		t.Fatalf("真运行时在测试后被破坏: ready=%v reason=%q err=%v", rst.Ready, rst.Reason, rerr)
	}
	t.Logf("✅ pypdfium2 缺失 → (\"\", false, nil)（调用方照旧说「无文本层」）")
}
