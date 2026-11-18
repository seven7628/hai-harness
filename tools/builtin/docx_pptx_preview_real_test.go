package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	goruntime "github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
)

// DocxPreviewer / PptxPreviewer 的**真链路慢测试**（-short 跳过）：真造 docx/pptx
// （python-docx / python-pptx 写）+ 真受管运行时 + 真 Seatbelt 跑转换，覆盖
// 「预览真的能在生产链路下产出可渲染 HTML」这一前提。
//
// 与显式 skip 的边界：受管运行时**已 bootstrap**（mammoth/python-docx/python-pptx 都已装），
// 故这些用例在开发机与 CI 上都是**真跑**；只有运行时确实缺失时才 skip，且 skip 理由会打印
// 出来（便于区分「跳过」与「悄悄没跑」）。

// officeTempSnapshot 列出受管临时目录下、以 prefix 开头的目录名集合。
//
// 用于「失败后不残留」的断言：比的是调用前后的**差值**，而不是全目录扫描 ——
// 成功路径有意保留产物（内嵌浏览器要读），所以共享 tmpRoot 里本就会有别的用例、
// 别的进程（乃至 e2e 脚本）留下的成功产物；全扫会把它们误报成残留，还会误删
// 别人正在用的产物。
func officeTempSnapshot(tmpRoot, prefix string) map[string]struct{} {
	set := map[string]struct{}{}
	entries, err := os.ReadDir(tmpRoot)
	if err != nil {
		return set
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			set[e.Name()] = struct{}{}
		}
	}
	return set
}

// realOfficeRuntime 真实受管运行时（已 bootstrap）+ 真 Seatbelt（可用时）。
// 返回 (运行时, 沙箱工厂, 工作区, 受管临时目录根)。三格式共用同一个运行时实例。
// tmpRoot 单独返回是为了断言「失败后不留残留」—— 它不在 Runtime 的导出面上。
func realOfficeRuntime(t *testing.T, probe string) (*goruntime.Runtime, SandboxFor, string, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("慢测试：需真实受管运行时")
	}
	rt := goruntime.New(goruntime.Config{})
	pyh, err := rt.Ensure(context.Background())
	if err != nil {
		t.Skipf("无已 bootstrap 的真实受管运行时（先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime）: %v", err)
	}
	ws := t.TempDir()
	// 探测依赖是否真在运行时里：缺了就 skip 并**说清缺什么**（不静默当通过）。
	if probe != "" {
		out, perr := (sandbox.NoSandbox{}).Run(context.Background(), sandbox.ExecSpec{
			Command: shQuote(pyh.VenvPython) + " -c " + shQuote(probe), Cwd: ws, Timeout: time.Minute,
		})
		if perr != nil || strings.Contains(out, "[exit:") {
			t.Skipf("受管运行时缺少本用例所需依赖（%s）: %v / %s", probe, perr, singleLine(out))
		}
	}
	var sbx sandbox.Sandbox
	if sandbox.Available() {
		if sb, serr := sandbox.NewSeatbelt(ws); serr == nil {
			sbx = sb
		} else {
			t.Logf("seatbelt 策略探测失败（改直通）: %v", serr)
		}
	}
	return rt, func(string) sandbox.Sandbox { return sbx }, ws, filepath.Join(pyh.Root, "tmp")
}

// makeOfficeFile 用受管解释器在 ws 下落一个文件（脚本经受管运行时跑，保证
// python-docx / python-pptx 与生产同源）。
func makeOfficeFile(t *testing.T, rt *goruntime.Runtime, ws, prefix, py string) {
	t.Helper()
	ctx := context.Background()
	pyh, err := rt.Ensure(ctx)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	p, cleanup, err := writeTempFile(pyh, prefix, "mk.py", []byte(py))
	if err != nil {
		t.Fatalf("写造文件脚本: %v", err)
	}
	defer cleanup()
	out, err := (sandbox.NoSandbox{}).Run(ctx, sandbox.ExecSpec{
		Command: shQuote(pyh.VenvPython) + " " + shQuote(p), Cwd: ws, Timeout: 2 * time.Minute,
	})
	if err != nil || strings.Contains(out, "[exit:") {
		t.Fatalf("造文件失败: %v / %s", err, out)
	}
}

// 1x1 的合法 PNG（74 字节）：用来造「文档里真的有内嵌图片」这一情形。
// 为什么不用现成图：测试要在临时目录里从零造文件，不依赖任何仓库内资产。
const tinyPNGPy = `
import struct, zlib
def _chunk(t, d):
    return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)
_ihdr = struct.pack(">IIBBBBB", 2, 2, 8, 6, 0, 0, 0)
_raw = b"".join(b"\x00" + bytes([255, 0, 0, 255] * 2) for _ in range(2))
open("tiny.png", "wb").write(b"\x89PNG\r\n\x1a\n" + _chunk(b"IHDR", _ihdr)
                             + _chunk(b"IDAT", zlib.compress(_raw)) + _chunk(b"IEND", b""))
`

const mkRichDocx = tinyPNGPy + `
import docx
from docx.shared import Inches
d = docx.Document()
d.add_heading("季度报告与摘要", level=1)
d.add_heading("第一部分", level=2)
p = d.add_paragraph()
p.add_run("这是")
r = p.add_run("粗体片段")
r.bold = True
d.add_paragraph("项目一", style="List Bullet")
d.add_paragraph("项目二", style="List Bullet")
d.add_paragraph("编号项", style="List Number")
t = d.add_table(rows=2, cols=2)
t.cell(0, 0).text = "单元格A1"
t.cell(0, 1).text = "单元格B1"
t.cell(1, 0).text = "含 <b>标签</b> 与 & 符号"
t.cell(1, 1).text = "值2"
d.add_picture("tiny.png", width=Inches(0.5))
d.save("rich.docx")
print("MK_OK")
`

func TestDocxPreviewRealConvert(t *testing.T) {
	rt, sbxFor, ws, _ := realOfficeRuntime(t, "import mammoth, docx; print('ok')")
	makeOfficeFile(t, rt, ws, "docx-mk-", mkRichDocx)

	p := NewDocxPreviewer(rt, sbxFor)
	res, err := p.Convert(context.Background(), filepath.Join(ws, "rich.docx"), ws)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	defer os.RemoveAll(res.Dir)
	raw, err := os.ReadFile(res.HTMLPath)
	if err != nil {
		t.Fatalf("读产物: %v", err)
	}
	html := string(raw)

	// 结构断言：标题层级 / 粗体 / 列表 / 表格单元格 / 内嵌图片 —— 这几项正是
	// 「为什么不用纯文本预览」的理由（文本预览全丢），必须真的在产物里。
	for _, want := range []string{
		`<meta charset="utf-8">`, "<style>",
		"季度报告与摘要", "第一部分",
		"<h1", "<h2",
		"<strong>粗体片段</strong>",
		"<ul>", "<li>项目一</li>", "<ol>",
		"<table>", "<td>", "单元格A1", "单元格B1",
		"&lt;b&gt;标签&lt;/b&gt;", // 标签是**文本**，不是被解析的元素
		"data:image/png;base64,",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML 缺少 %q", want)
		}
	}
	if strings.Contains(html, "<script>") {
		t.Error("脚本标签未转义（注入）")
	}
	// 版式损失的说明必须在页面上（不能让用户以为预览坏了）。
	if !strings.Contains(html, "不还原分页") {
		t.Error("页面未说明版式损失（分页/页眉页脚/文本框）")
	}
	t.Logf("✅ docx HTML %d 字节", len(html))
}

// Word 的 Title 不是 Heading 样式：mammoth 默认认不出（实测产出裸 <p>）。这条锁住
// style_map 生效 —— 否则标题会退化成正文，是最容易被静默改坏的一处。
func TestDocxPreviewMapsTitleStyle(t *testing.T) {
	rt, sbxFor, ws, _ := realOfficeRuntime(t, "import mammoth, docx; print('ok')")
	makeOfficeFile(t, rt, ws, "docx-mk-", `
import docx
d = docx.Document()
d.add_paragraph("我的大标题", style="Title")
d.add_paragraph("我的副标题", style="Subtitle")
d.save("t.docx")
print("MK_OK")
`)
	p := NewDocxPreviewer(rt, sbxFor)
	res, err := p.Convert(context.Background(), filepath.Join(ws, "t.docx"), ws)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	defer os.RemoveAll(res.Dir)
	raw, _ := os.ReadFile(res.HTMLPath)
	html := string(raw)
	if !strings.Contains(html, `<h1 class="doc-title">我的大标题</h1>`) {
		t.Errorf("Title 样式未映射为标题（会退化成裸 <p>）: %s", snippetAround(html, "我的大标题"))
	}
	if !strings.Contains(html, `<p class="doc-subtitle">我的副标题</p>`) {
		t.Errorf("Subtitle 样式未映射: %s", snippetAround(html, "我的副标题"))
	}
}

// 损坏/错格式的输入必须给出**单行中文原因**（不是 40 行 traceback —— toast 里塞堆栈等于没提示）。
func TestDocxPreviewBadInputMessages(t *testing.T) {
	rt, sbxFor, ws, tmpRoot := realOfficeRuntime(t, "import mammoth, docx; print('ok')")
	for _, tc := range []struct {
		name, file, content, want string
	}{
		{"垃圾文件", "broken.docx", "这不是 docx", "不是可读的 docx"},
		{"空文件", "empty.docx", "", "不是可读的 docx"},
		{"合法 zip 但没有正文部件", "ziponly.docx", "ZIPNOBODY", "结构不完整"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 失败路径不得残留临时目录 —— 断言**本次调用**的净增量，而不是扫描共享
			// tmpRoot 里的全部 docx-preview-*。理由：成功路径**有意保留**产物
			//（内嵌浏览器要读它，见 Convert 的清理策略「残留由下一次转换按年龄回收」），
			// 所以共享目录里本来就会有别的用例/别的进程留下的成功产物；扫描全目录会把
			// 它们误报成本次残留，还会**误删别的测试正在用的产物**（e2e 的断言依赖它）。
			before := officeTempSnapshot(tmpRoot, "docx-preview-")
			var py string
			if tc.content == "ZIPNOBODY" {
				py = `
import zipfile
with zipfile.ZipFile("ziponly.docx", "w") as z:
    z.writestr("a.txt", "hi")
print("MK_OK")
`
			} else {
				py = fmt.Sprintf(`
open(%q, "wb").write(%q.encode("utf-8"))
print("MK_OK")
`, tc.file, tc.content)
			}
			makeOfficeFile(t, rt, ws, "docx-mk-", py)
			p := NewDocxPreviewer(rt, sbxFor)
			_, err := p.Convert(context.Background(), filepath.Join(ws, tc.file), ws)
			if err == nil {
				t.Fatal("期望失败，却成功了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %q，期望含 %q", err.Error(), tc.want)
			}
			if strings.ContainsAny(err.Error(), "\n\r") {
				t.Errorf("错误信息必须单行（toast 里多行堆栈等于没提示）: %q", err.Error())
			}
			// 失败路径不得残留临时目录：只报**新增**的（before 里没有的）目录。
			after := officeTempSnapshot(tmpRoot, "docx-preview-")
			for name := range after {
				if _, existed := before[name]; !existed {
					t.Errorf("失败后残留预览临时目录: %s", name)
					_ = os.RemoveAll(filepath.Join(tmpRoot, name))
				}
			}
		})
	}
}

// 把 .xlsx 改名成 .docx：mammoth 抛 ValueError，我们的脚本要把它变成「这不是 Word 文档」，
// 而不是把 traceback 甩到 user 脸上。
func TestDocxPreviewWrongOOXMLFormat(t *testing.T) {
	rt, sbxFor, ws, _ := realOfficeRuntime(t, "import mammoth, docx, openpyxl; print('ok')")
	makeOfficeFile(t, rt, ws, "docx-mk-", `
import openpyxl
wb = openpyxl.Workbook()
wb.active["A1"] = "x"
wb.save("actually.xlsx")
import shutil
shutil.copy("actually.xlsx", "actually.docx")
print("MK_OK")
`)
	p := NewDocxPreviewer(rt, sbxFor)
	_, err := p.Convert(context.Background(), filepath.Join(ws, "actually.docx"), ws)
	if err == nil {
		t.Fatal("把 xlsx 当 docx 转不应成功")
	}
	if !strings.Contains(err.Error(), "这不是 Word 文档") {
		t.Errorf("错误 = %q，期望「这不是 Word 文档」", err.Error())
	}
}

// ——— pptx ———

const mkDeckPptx = tinyPNGPy + `
from pptx import Presentation
from pptx.util import Inches, Pt
from pptx.dml.color import RGBColor
from pptx.enum.shapes import MSO_SHAPE
prs = Presentation()
prs.slide_width = Inches(10)
prs.slide_height = Inches(7.5)
s1 = prs.slides.add_slide(prs.slide_layouts[0])
s1.shapes.title.text = "年度汇报 <x> & 计划"
s1.placeholders[1].text = "副标题 · 2026"
s2 = prs.slides.add_slide(prs.slide_layouts[1])
s2.shapes.title.text = "第二页：数据"
body = s2.placeholders[1]
body.text_frame.text = "第一点"
body.text_frame.add_paragraph().text = "第二点"
tb = s2.shapes.add_textbox(Inches(1), Inches(5), Inches(4), Inches(1))
tb.text_frame.text = "文本框"
p2 = tb.text_frame.add_paragraph()
r = p2.add_run()
r.text = "加粗红色"
r.font.bold = True
r.font.size = Pt(28)
r.font.color.rgb = RGBColor(0xC0, 0x00, 0x00)
s2.shapes.add_picture("tiny.png", Inches(6), Inches(3), Inches(2), Inches(2))
tbl = s2.shapes.add_table(2, 3, Inches(1), Inches(6), Inches(6), Inches(1.2))
tbl.table.cell(0, 0).text = "表头A"
tbl.table.cell(1, 0).text = "含 <b> 的值"
s3 = prs.slides.add_slide(prs.slide_layouts[6])
s3.background.fill.solid()
s3.background.fill.fore_color.rgb = RGBColor(0x11, 0x22, 0x33)
sh = s3.shapes.add_shape(MSO_SHAPE.ROUNDED_RECTANGLE, Inches(1), Inches(1), Inches(3), Inches(1))
sh.text_frame.text = "形状内文字"
s3.notes_slide.notes_text_frame.text = "演讲者备注"
prs.save("deck.pptx")
print("MK_OK")
`

func TestPptxPreviewRealConvert(t *testing.T) {
	rt, sbxFor, ws, _ := realOfficeRuntime(t, "from pptx import Presentation; print('ok')")
	makeOfficeFile(t, rt, ws, "pptx-mk-", mkDeckPptx)

	p := NewPptxPreviewer(rt, sbxFor)
	res, err := p.Convert(context.Background(), filepath.Join(ws, "deck.pptx"), ws)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	defer os.RemoveAll(res.Dir)
	raw, err := os.ReadFile(res.HTMLPath)
	if err != nil {
		t.Fatalf("读产物: %v", err)
	}
	html := string(raw)

	for _, want := range []string{
		`<meta charset="utf-8">`, "<style>",
		// 页数：每页一个 section 级的 slide 容器
		"第 1 页", "第 2 页", "第 3 页",
		"年度汇报 &lt;x&gt; &amp; 计划", // 页标题文字 + 转义
		"第二页：数据",
		"文本框", "加粗红色", "形状内文字", "演讲者备注",
		// 几何：10x7.5 英寸 → 960x720 px
		"width:960.0px;height:720.0px",
		// 真实文字色与粗体
		"color:#C00000", "font-weight:600",
		// 表格与图片
		"<table>", "表头A", "&lt;b&gt; 的值", "data:image/png;base64,",
		// 页面底色（第 3 页显式设了 112233）
		"background:#112233",
		// 诚实标注
		"近似版式重建（不是渲染）", "无字体保真", "SmartArt",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML 缺少 %q", want)
		}
	}
	if strings.Contains(html, "<script>") {
		t.Error("脚本标签未转义（注入）")
	}
	if n := strings.Count(html, `class="slide-wrap"`); n != 3 {
		t.Errorf("slide 容器数 = %d，期望 3", n)
	}
	t.Logf("✅ pptx HTML %d 字节", len(html))
}

// 样式继承链的真回归：默认模板的字号/居中/项目符号都在**母版 txStyles** 里，
// 只读形状自身属性会得到一页无层次的裸文本。这条锁住那几项真的被解析出来。
func TestPptxPreviewInheritsMasterTextStyle(t *testing.T) {
	rt, sbxFor, ws, _ := realOfficeRuntime(t, "from pptx import Presentation; print('ok')")
	makeOfficeFile(t, rt, ws, "pptx-mk-", `
from pptx import Presentation
from pptx.util import Inches
prs = Presentation()
prs.slide_width = Inches(10)
prs.slide_height = Inches(7.5)
s = prs.slides.add_slide(prs.slide_layouts[1])
s.shapes.title.text = "继承样式的标题"
both = s.placeholders[1]
both.text_frame.text = "正文第一点"
both.text_frame.add_paragraph().text = "正文第二点"
prs.save("inh.pptx")
print("MK_OK")
`)
	p := NewPptxPreviewer(rt, sbxFor)
	res, err := p.Convert(context.Background(), filepath.Join(ws, "inh.pptx"), ws)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	defer os.RemoveAll(res.Dir)
	raw, _ := os.ReadFile(res.HTMLPath)
	html := string(raw)

	// 标题：44pt（母版 titleStyle）+ 居中（母版 lvl1pPr/@algn=ctr）。
	titleSnip := snippetAround(html, "继承样式的标题")
	if !strings.Contains(titleSnip, "font-size:44.0pt") {
		t.Errorf("标题未继承母版字号（期望 44pt）: %s", titleSnip)
	}
	if !strings.Contains(titleSnip, "text-align:center") {
		t.Errorf("标题未继承母版居中: %s", titleSnip)
	}
	// 正文：32pt + 项目符号（母版 bodyStyle 的 buChar）。
	bodySnip := snippetAround(html, "正文第一点")
	if !strings.Contains(bodySnip, "font-size:32.0pt") {
		t.Errorf("正文未继承母版字号（期望 32pt）: %s", bodySnip)
	}
	if !strings.Contains(bodySnip, `class="bu"`) || !strings.Contains(bodySnip, "•") {
		t.Errorf("正文未显示母版定义的项目符号: %s", bodySnip)
	}
}

// 上限：70 页 / 单页 216 个形状 → 收敛且**如实标注**（不静默丢弃）。
func TestPptxPreviewCapsSlidesAndShapes(t *testing.T) {
	rt, sbxFor, ws, _ := realOfficeRuntime(t, "from pptx import Presentation; print('ok')")
	makeOfficeFile(t, rt, ws, "pptx-mk-", `
from pptx import Presentation
from pptx.util import Inches
prs = Presentation()
prs.slide_width = Inches(10)
prs.slide_height = Inches(7.5)
for i in range(70):
    s = prs.slides.add_slide(prs.slide_layouts[6])
    tb = s.shapes.add_textbox(Inches(0.5), Inches(0.5), Inches(5), Inches(1))
    tb.text_frame.text = "第 %d 页标题" % (i + 1)
    if i == 0:
        for j in range(215):
            t = s.shapes.add_textbox(Inches(0.2), Inches(1), Inches(1), Inches(0.3))
            t.text_frame.text = "s%d" % j
prs.save("big.pptx")
print("MK_OK")
`)
	p := NewPptxPreviewer(rt, sbxFor)
	res, err := p.Convert(context.Background(), filepath.Join(ws, "big.pptx"), ws)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	defer os.RemoveAll(res.Dir)
	raw, _ := os.ReadFile(res.HTMLPath)
	html := string(raw)

	if n := strings.Count(html, `class="slide-wrap"`); n != 60 {
		t.Errorf("渲染页数 = %d，期望 60（上限）", n)
	}
	if !strings.Contains(html, "还有 10 页未显示（上限 60 页）") {
		t.Error("页面未标注未显示的页数")
	}
	if !strings.Contains(html, "第 1 页 · 216 个形状（只显示前 200 个）") {
		t.Errorf("单页形状上限未生效或未标注: %s", snippetAround(html, "第 1 页"))
	}
}

// 全篇形状预算（2026-11-18，第 4 轮爬山）：每页 200 是**每层**预算，组合形状的子形状
// 又各套一层 ⇒ 嵌套 3 层 = 200³。现在改成一个 **deck 级**共享计数器。
//
// 断言：渲染出的形状数 ≤ 预算、且**如实标注**（不静默丢弃）。
func TestPptxPreviewCapsTotalShapes(t *testing.T) {
	rt, sbxFor, ws, _ := realOfficeRuntime(t, "from pptx import Presentation; print('ok')")
	// 35 页 × 200 个形状 = 7000 > 全篇预算 6000（无文字 ⇒ 走「不支持类型」的快路径，
	// 让这个用例只测**预算**、不掺渲染开销）。
	makeOfficeFile(t, rt, ws, "pptx-mk-", `
from pptx import Presentation
from pptx.util import Inches
prs = Presentation()
for i in range(35):
    s = prs.slides.add_slide(prs.slide_layouts[6])
    for j in range(200):
        s.shapes.add_textbox(Inches(0.01 * (j % 20)), Inches(0.01 * (j // 20)), Inches(0.4), Inches(0.2))
prs.save("many.pptx")
print("MK_OK")
`)
	p := NewPptxPreviewer(rt, sbxFor)
	res, err := p.Convert(context.Background(), filepath.Join(ws, "many.pptx"), ws)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	defer os.RemoveAll(res.Dir)
	raw, _ := os.ReadFile(res.HTMLPath)
	html := string(raw)

	n := strings.Count(html, `class="sh`)
	if n > 6000 {
		t.Errorf("渲染形状数 = %d，超过全篇预算 6000", n)
	}
	if n == 0 {
		t.Fatal("一个形状都没渲染（闸门把正常内容也拦了？）")
	}
	if !strings.Contains(html, "已达全篇形状上限（6000 个），部分形状未显示") {
		t.Errorf("形状预算生效但没有如实标注: %s", snippetAround(html, "形状"))
	}
}

// 错格式/垃圾输入的单行中文原因 + 失败不留残留。
func TestPptxPreviewBadInputMessages(t *testing.T) {
	rt, sbxFor, ws, tmpRoot := realOfficeRuntime(t, "from pptx import Presentation; import docx; print('ok')")
	makeOfficeFile(t, rt, ws, "pptx-mk-", `
open("broken.pptx", "wb").write("这不是 pptx".encode("utf-8"))
import docx
d = docx.Document()
d.add_paragraph("x")
d.save("aword.docx")
print("MK_OK")
`)
	for _, tc := range []struct{ file, want string }{
		{"broken.pptx", "不是可读的 pptx"},
		{"aword.docx", "这不是 PowerPoint 演示文稿"},
		{"missing.pptx", "不是可读的 pptx"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			// 只看本次调用的净增量：成功路径有意保留产物，共享 tmpRoot 里本就有别的产物
			//（理由同 TestDocxPreviewBadInputMessages 的同名断言）。
			before := officeTempSnapshot(tmpRoot, "pptx-preview-")
			p := NewPptxPreviewer(rt, sbxFor)
			_, err := p.Convert(context.Background(), filepath.Join(ws, tc.file), ws)
			if err == nil {
				t.Fatal("期望失败，却成功了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %q，期望含 %q", err.Error(), tc.want)
			}
			if strings.ContainsAny(err.Error(), "\n\r") {
				t.Errorf("错误信息必须单行: %q", err.Error())
			}
			after := officeTempSnapshot(tmpRoot, "pptx-preview-")
			for name := range after {
				if _, existed := before[name]; !existed {
					t.Errorf("失败后残留预览临时目录: %s", name)
					_ = os.RemoveAll(filepath.Join(tmpRoot, name))
				}
			}
		})
	}
}

// snippetAround 取 haystack 里 needle 附近的一段（断言失败时能看见实际产出了什么，
// 而不是只报「缺少某串」—— 样式类问题几乎都在「周围长什么样」里）。
func snippetAround(haystack, needle string) string {
	i := strings.Index(haystack, needle)
	if i < 0 {
		return "(产物里没有 " + needle + ")"
	}
	start := i - 220
	if start < 0 {
		start = 0
	}
	end := i + len(needle) + 80
	if end > len(haystack) {
		end = len(haystack)
	}
	return "..." + haystack[start:end] + "..."
}
