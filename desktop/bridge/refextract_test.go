package main

// refextract_test.go —— PDF 抽取器快测试（默认跑，含 CI）：不依赖运行时/网络。
//
// 覆盖产出解析（三态协议）、未装配运行时的错误路径、临时目录选址与命令行形状。
// 「真造 PDF + 真 pypdf + 真沙箱」的慢测试见 refextract_real_test.go。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	goruntime "github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
)

// 抽到正文：正文与页数标注一起返回（ok=true）。
func TestParsePDFExtractOutputText(t *testing.T) {
	out := "noise before\n" + pdfPageMarker + "3\n--- 第 1 页 ---\n甲\n--- 第 3 页 ---\n丙\n"
	content, ok, err := parsePDFExtractOutput(out)
	if err != nil || !ok {
		t.Fatalf("应抽到内容: ok=%v err=%v", ok, err)
	}
	for _, want := range []string{"[PDF 文本，共 3 页]", "--- 第 1 页 ---", "甲", "--- 第 3 页 ---", "丙"} {
		if !strings.Contains(content, want) {
			t.Errorf("缺少 %q, got %q", want, content)
		}
	}
}

// 无文本层（扫描件）：("", false, nil) —— **不是** error，调用方据此显式告知用户。
func TestParsePDFExtractOutputNoTextLayer(t *testing.T) {
	out := pdfPageMarker + "2\n" + pdfNoTextMarker + "\n"
	content, ok, err := parsePDFExtractOutput(out)
	if err != nil {
		t.Fatalf("无文本层不应是错误（那是业务事实，不是环境问题）: %v", err)
	}
	if ok || content != "" {
		t.Errorf("无文本层应回空: content=%q ok=%v", content, ok)
	}
}

// 脚本没走完（沙箱拦截/解释器崩溃/加密解析失败）→ err，调用方回退既有行为。
func TestParsePDFExtractOutputScriptFailure(t *testing.T) {
	out := "无法解析 PDF: startxref not found\n[exit: exit status 2]"
	content, ok, err := parsePDFExtractOutput(out)
	if err == nil {
		t.Fatal("无协议标记行应报错")
	}
	if ok || content != "" {
		t.Errorf("失败时应回空: content=%q ok=%v", content, ok)
	}
	if !strings.Contains(err.Error(), "startxref not found") {
		t.Errorf("错误应带脚本输出（便于排查）: %v", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("错误文本须单行（多行会破 FileContent/日志格式）: %q", err.Error())
	}
}

// 页数标记行畸形 → 明确报错，不猜。
func TestParsePDFExtractOutputBadPageCount(t *testing.T) {
	if _, _, err := parsePDFExtractOutput(pdfPageMarker + "abc\n正文"); err == nil {
		t.Fatal("页数不可解析应报错")
	}
}

// 空输出（超时被 SIGKILL：sandbox 会把 [exit: ...] 并进文本）→ err。
func TestParsePDFExtractOutputEmpty(t *testing.T) {
	if _, _, err := parsePDFExtractOutput("[exit: signal: killed]"); err == nil {
		t.Fatal("空输出应报错")
	}
}

// 运行时未装配：明确错误（调用方回退二进制占位），不 panic。
func TestPDFExtractorNoRuntime(t *testing.T) {
	ext := newPDFExtractor(nil, nil, t.TempDir())
	if _, _, err := ext(context.Background(), "/tmp/nope.pdf"); err == nil {
		t.Fatal("未装配运行时应报错")
	}
}

// 路径不存在/是目录：明确错误，不进沙箱。
func TestPDFExtractorBadPath(t *testing.T) {
	ws := t.TempDir()
	ext := newPDFExtractor(goruntime.New(goruntime.Config{}), func(string) sandbox.Sandbox { return nil }, ws)

	if _, _, err := ext(context.Background(), filepath.Join(ws, "missing.pdf")); err == nil {
		t.Error("不存在的文件应报错")
	}
	if _, _, err := ext(context.Background(), ws); err == nil {
		t.Error("目录应报错")
	}
}

// 抽取脚本常量自检：协议标记必须都在（Go 侧解析依赖它们）——防手改脚本时漏改一侧。
// 注意 newPDFExtractor 收的是具体类型 *runtime.Runtime（与 xlsx 预览收 PythonRuntime
// 接口不同），故**命令行形状**无法用假运行时在这里断言；那条链路由
// refextract_real_test.go 的慢测试在生产同链路下覆盖（真脚本真执行）。
func TestPDFExtractScriptContract(t *testing.T) {
	if !strings.Contains(pdfExtractScript, "import pypdf") {
		t.Error("脚本应 import pypdf")
	}
	if !strings.Contains(pdfExtractScript, pdfPageMarker) || !strings.Contains(pdfExtractScript, pdfNoTextMarker) {
		t.Error("脚本必须打印两个协议标记（Go 侧解析依赖它们）")
	}
	if strings.Contains(pdfExtractScript, "[exit:") {
		t.Error("脚本不应自己产出 [exit: 前缀（那是 sandbox 加的）")
	}
}

// 编译期断言：newPDFExtractor 的返回值可直接喂给 agents.WithRefExtractor
// （装配点 main.go 的调用形状，防签名漂移）。
var _ agents.RefExtractor = newPDFExtractor(nil, nil, "")

// ---------- 无文本层 → 渲染成图片（快测试部分） ----------

// 渲染成功：OK 标记 + 逐页 image 标记行 → 路径按页序收集，总页数一并返回。
func TestParsePDFRenderOutputImages(t *testing.T) {
	out := "noise\n" + pdfPageMarker + "9\n" +
		pdfRenderImageMarker + "1\t1.8600\t/tmp/a/page-0001.jpg\n" +
		pdfRenderImageMarker + "2\t1.8600\t/tmp/b page with space/page-0002.jpg\n" +
		pdfRenderOKMarker + "\n"
	paths, total := parsePDFRenderOutput(out)
	if total != 9 {
		t.Errorf("总页数应为 9（声明的页数，不是渲染成功的页数）, got %d", total)
	}
	want := []string{"/tmp/a/page-0001.jpg", "/tmp/b page with space/page-0002.jpg"}
	if len(paths) != len(want) {
		t.Fatalf("应收集 %d 条路径, got %d (%v)", len(want), len(paths), paths)
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("第 %d 条路径应为 %q（含空格的路径不能被截断）, got %q", i, want[i], paths[i])
		}
	}
}

// 「一张都没写出来」（脚本 exit 4：pdfium 打不开/所有页都渲染失败）→ 空结果，
// 调用方据此降级回「无文本层」而不是把空正文说成「渲染成功」。
func TestParsePDFRenderOutputNoImages(t *testing.T) {
	out := pdfPageMarker + "3\n没有可渲染的页面（共 3 页）\n[exit: exit status 4]"
	paths, _ := parsePDFRenderOutput(out)
	if len(paths) != 0 {
		t.Fatalf("无 OK 标记时应回空（即便有 image 残行）, got %v", paths)
	}
	// 有 image 行但缺 OK 标记：同样算未完成（OK 是「脚本走完了」的唯一凭据）
	out2 := pdfPageMarker + "3\n" + pdfRenderImageMarker + "1\t2.0\t/tmp/a.jpg\n[exit: signal: killed]"
	if paths, _ := parsePDFRenderOutput(out2); len(paths) != 0 {
		t.Fatalf("缺 OK 标记（被 SIGKILL 截断）时应回空, got %v", paths)
	}
}

// 沙箱拦截/解释器崩溃（没有页数标记行）→ 空结果。
func TestParsePDFRenderOutputNoPageMarker(t *testing.T) {
	if paths, total := parsePDFRenderOutput("sh: command not found\n[exit: exit status 127]"); len(paths) != 0 || total != 0 {
		t.Fatalf("无页数标记行应回空: paths=%v total=%d", paths, total)
	}
}

// 畸形 image 标记行（缺制表符/路径为空）→ 跳过该行而不是塞一条坏路径给模型。
func TestParsePDFRenderOutputSkipsMalformedLines(t *testing.T) {
	out := pdfPageMarker + "2\n" +
		pdfRenderImageMarker + "1\t2.0\t\n" + // 路径为空
		pdfRenderImageMarker + "2\t2.0/tmp/x.jpg\n" + // 协议字段残缺（缺分隔的制表符）
		pdfRenderImageMarker + "3\t2.0\t/tmp/ok.jpg\n" +
		pdfRenderOKMarker + "\n"
	paths, total := parsePDFRenderOutput(out)
	if total != 2 {
		t.Errorf("total 应为 2, got %d", total)
	}
	if len(paths) != 1 || paths[0] != "/tmp/ok.jpg" {
		t.Fatalf("只应留下合法那一条, got %v", paths)
	}
}

// 渲染脚本常量自检：三个协议标记都在（Go 侧解析依赖它们）+ 口径常量与 Go 常量一致
// （防只改一侧：脚本里的 1568/q85 是引擎「不再动这张图」的前提）。
func TestPDFRenderScriptContract(t *testing.T) {
	for _, want := range []string{
		"import pypdfium2",
		pdfPageMarker, pdfRenderOKMarker, pdfRenderImageMarker,
		strconv.Itoa(pdfImageMaxDim), "quality=QUALITY",
		"scale = MAX_DIM / longest", // 按长边反算（不是固定 scale）
	} {
		if !strings.Contains(pdfRenderScript, want) {
			t.Errorf("渲染脚本必须含 %q（Go 侧解析/口径依赖它）", want)
		}
	}
	// 质量常量经 QUALITY 变量传入脚本：两边都得对得上
	if !strings.Contains(pdfRenderScript, "QUALITY = "+strconv.Itoa(pdfImageQuality)) {
		t.Errorf("脚本的 JPEG 质量须 == pdfImageQuality(%d)（与引擎重编码质量同口径）", pdfImageQuality)
	}
	if !strings.Contains(pdfRenderScript, "MAX_PAGES = "+strconv.Itoa(pdfImagePagesMax)) {
		t.Errorf("脚本的页数上限须 == pdfImagePagesMax(%d)", pdfImagePagesMax)
	}
	if !strings.Contains(pdfRenderScript, "argv[2]") {
		t.Error("渲染脚本必须从 argv[2] 取输出目录（argv[1]=PDF 路径）")
	}
	if strings.Contains(pdfRenderScript, "[exit:") {
		t.Error("脚本不应自己产出 [exit: 前缀（那是 sandbox 加的）")
	}
	// 抽取脚本仍只有单参数（防止有人把渲染逻辑塞进抽取脚本、破坏其单参数契约）
	if strings.Contains(pdfExtractScript, "argv[2]") {
		t.Error("抽取脚本不该有第二个参数（渲染是独立脚本）")
	}
}

// 渲染正文的措辞：页数如实+给出路径+指引逐张 read_file（不能建议「一次读 5 张」——
// 引擎单次工具结果最多 4 张图，会让第 5 张被静默丢弃）。
func TestPDFRenderNoticeWording(t *testing.T) {
	paths := []string{"/t/1.jpg", "/t/2.jpg"}
	got := pdfRenderNotice(paths, 7)
	for _, want := range []string{"无文本层", "前 2 页", "共 7 页", "/t/1.jpg", "/t/2.jpg", "read_file", "逐张"} {
		if !strings.Contains(got, want) {
			t.Errorf("正文缺少 %q\n got %q", want, got)
		}
	}
	// 部分渲染时必须说清「渲染了几页 / 共几页」，不能把总页数说成已渲染的页数
	// （用户按「前 5 页」理解产物，说成 7 页会让他以为拿到了整份文档）
	if strings.Contains(got, "将全部") {
		t.Errorf("只渲染了 2/7 页，不该说「全部」: %q", got)
	}
	// 全部页都渲染出来时不写「前 N 页（共 N 页）」这种啰嗦说法
	all := pdfRenderNotice(paths, 2)
	if !strings.Contains(all, "全部 2 页") {
		t.Errorf("全量渲染应说「全部 N 页」, got %q", all)
	}
	// 路径必须独立成行（模型后续要原样复制去调 read_file）
	if !strings.Contains(got, "/t/1.jpg\n/t/2.jpg\n") {
		t.Errorf("路径应各自独立成行, got %q", got)
	}
}

// JPEG 魔数校验：只认非空且以 FF D8 FF 开头的文件（read_file 正是按这个魔数认图的）。
func TestJpegMagicOK(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jpg")
	if err := os.WriteFile(good, append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 32)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if !jpegMagicOK(good) {
		t.Error("合法 JPEG 头应通过")
	}
	if jpegMagicOK(filepath.Join(dir, "missing.jpg")) {
		t.Error("不存在的路径应判失败（不能把坏路径承诺给模型）")
	}
	truncated := filepath.Join(dir, "short.jpg")
	if err := os.WriteFile(truncated, []byte{0xFF, 0xD8}, 0o600); err != nil {
		t.Fatal(err)
	}
	if jpegMagicOK(truncated) {
		t.Error("只有 2 字节的文件应判失败")
	}
	notImg := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(notImg, []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}
	if jpegMagicOK(notImg) {
		t.Error("非 JPEG 内容应判失败")
	}
}

// 产物清扫：只删**过期**的 pdf-scan-* 目录，且不碰 keep（本次产物）与其他前缀。
// 这是「图片必须比本次调用活得久」与「残留不会无限增长」两条要求的交汇点。
func TestSweepStalePDFImageDirs(t *testing.T) {
	// 产物目录是 <Runtime.Root>/tmp 下的子目录（与 builtin.tempDirBases 同选址），
	// 故测试里也要按这个层级摆：Root/tmp/pdf-scan-*
	root := t.TempDir()
	base := filepath.Join(root, "tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	stale := mk(pdfImageDirPrefix+"stale", 2*pdfImageStaleAge)
	fresh := mk(pdfImageDirPrefix+"fresh", time.Minute) // 另一个会话刚交付、可能正在被 read_file
	keep := mk(pdfImageDirPrefix+"keep", 3*pdfImageStaleAge)
	other := mk("xlsx-preview-old", 5*pdfImageStaleAge) // 别的通路的残留不归这里管

	sweepStalePDFImageDirs(goruntime.Python{Root: root}, keep)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("过期产物应被清掉（否则 ~/.go-code/runtime/python/tmp 单调增长）")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("未过期产物不该被清（可能正被另一个会话 read_file）")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("本次产物不该被自己的清扫删掉")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("非本前缀的目录不该被误删（xlsx 预览有自己的清扫）")
	}
}
