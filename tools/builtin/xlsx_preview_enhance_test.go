package builtin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	goruntime "github.com/seven7628/hai-harness/runtime"
)

// R08b 表格增强的服务端产物回归（决策 D-K：不加前端依赖，全部在 HTML/CSS 里做）。
//
// 断言的对象是**生产转换器吐出的 HTML**，不是某个内部函数：入口是 XlsxPreviewer.Convert
// （bridge 的 xlsx_preview 命令就是调它），fixture 由**受管解释器**用真 openpyxl 造。
// 唯一没有覆盖的一层是「Python 运行时安装/自举」（runtime.Ensure 在未就绪时直接 skip）。
//
// 为什么不能用假的 HTML 模板做快照测试：这些增强全部依赖「表格里有 thead/colgroup/
// 冻结类」这类**结构**，而结构与 CSS 是一起生效的（实测过一次：结构对、CSS 错，
// 结果 = 表头看得见但第二行冻结行被表身吃掉）。单测只能锁结构与文本形态，真渲染交给
// desktop/app/e2e/verify_xlsx_preview.mjs（getBoundingClientRect 比对 sticky 边界）。

// realXlsxPreviewer 指向真实受管运行时的转换器；环境未就绪则 skip（CI 不带运行时）。
//
// 为什么不注入假 PythonRuntime：§4.7 的规矩是测试必须从生产入口进。这里唯一能被替身
// 掉的是「解释器在哪」，而它正好是 openpyxl 装配的载体 —— 换个假路径等于在测试里另建
// 一个等价物（F53 的形状），跑通了也不证明生产能跑。
func realXlsxPreviewer(t *testing.T) (*XlsxPreviewer, goruntime.Python) {
	t.Helper()
	if testing.Short() {
		t.Skip("慢测试：需真实受管 Python 运行时（openpyxl）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rt := goruntime.New(goruntime.Config{})
	if st, err := rt.Status(ctx); err != nil || !st.Ready {
		if st.Stale {
			// 与 refextract_real_test.go 同口径：运行时存在但与源码不一致是**可修复的
			// 破损状态**，此时全套真链路测试会静默跳过而套件仍显示全绿。
			t.Fatalf("受管运行时与依赖清单不匹配（%s）：先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime", st.Reason)
		}
		t.Skipf("受管 Python 运行时未就绪（%s）：先跑 go test ./runtime/ -run TestRealEnsureInstallsRuntime", st.Reason)
	}
	py, err := rt.Ensure(ctx)
	if err != nil {
		t.Fatalf("runtime.Ensure: %v", err)
	}
	return NewXlsxPreviewer(rt, nil), py
}

// xlsxFixture 在临时工作区里用受管解释器跑一段建表脚本，返回工作区目录。
func xlsxFixture(t *testing.T, py goruntime.Python, script string) string {
	t.Helper()
	ws := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, py.VenvPython, "-c", script)
	cmd.Dir = ws
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("造 fixture 失败: %v\n%s", err, out)
	}
	return ws
}

// convertXlsx 走生产入口转换并读回 HTML（产物目录随测试清理）。
func convertXlsx(t *testing.T, pv *XlsxPreviewer, ws, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := pv.Convert(ctx, filepath.Join(ws, name), ws)
	if err != nil {
		t.Fatalf("Convert(%s): %v", name, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(res.Dir) })
	raw, err := os.ReadFile(res.HTMLPath)
	if err != nil {
		t.Fatalf("读产物 %s: %v", res.HTMLPath, err)
	}
	return string(raw)
}

// TestXlsxPreviewSheetNav：20 个 sheet → 20 项导航、锚点唯一、目标存在，且重名/超长/
// 特殊字符的 sheet 名互相可区分。
func TestXlsxPreviewSheetNav(t *testing.T) {
	pv, py := realXlsxPreviewer(t)
	// 3 = 特殊字符（openpyxl 拒绝 / \ * ? : [ ]，故用无斜杠的 <img> 注入串）
	// 4 = 超长（> Excel 的 31 字符上限）
	// 5/6 = 重名（openpyxl 建表时自动改名，故先建占位名、再改 zip 里的 workbook.xml）
	ws := xlsxFixture(t, py, `
import openpyxl, zipfile, shutil, html
wb = openpyxl.Workbook()
for i in range(1, 21):
    s = wb.active if i == 1 else wb.create_sheet()
    if i == 3:
        s.title = '"><img src=x onerror=alert(1)>'
    elif i == 4:
        s.title = "超长" * 20
    elif i == 5:
        s.title = "同名表"
    elif i == 6:
        s.title = "同名表占位"
    else:
        s.title = "第%02d张" % i
    s.append(["表头A", "表头B"])
    s.append(["第%d张-值1" % i, "第%d张-值2" % i])
def patch(path, pairs):
    tmp = path + ".tmp"
    with zipfile.ZipFile(path) as zin, zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as zout:
        for item in zin.infolist():
            data = zin.read(item.filename)
            if item.filename == "xl/workbook.xml":
                for old, new in pairs:
                    data = data.replace(old.encode(), html.escape(new, quote=False).encode())
            zout.writestr(item, data)
    shutil.move(tmp, path)
wb.save("nav.xlsx")
patch("nav.xlsx", [("同名表占位", "同名表")])
`)
	raw := convertXlsx(t, pv, ws, "nav.xlsx")

	hrefs := regexp.MustCompile(`<a href="#(sheet-\d+)"`).FindAllStringSubmatch(raw, -1)
	if len(hrefs) != 20 {
		t.Fatalf("导航项 = %d，期望 20（20 个 sheet 必须有 20 个可跳转锚点）", len(hrefs))
	}
	ids := regexp.MustCompile(`<section class="sheet" id="(sheet-\d+)"`).FindAllStringSubmatch(raw, -1)
	seen := map[string]bool{}
	for _, m := range ids {
		if seen[m[1]] {
			t.Fatalf("锚点 %s 重复：跳转会落到第一个同名目标上", m[1])
		}
		seen[m[1]] = true
	}
	for _, m := range hrefs {
		if !seen[m[1]] {
			t.Fatalf("导航 href=#%s 没有对应目标（点了会不动）", m[1])
		}
	}
	if got := hrefs[16][1]; got != "sheet-17" {
		t.Fatalf("第 17 项 href = %s，期望 sheet-17（顺序必须与 sheet 顺序一致）", got)
	}
	// 第 17 张的标题与数据都在：锚点跳到的是**那一张**，不是任意一张。
	if !strings.Contains(raw, "17. 第17张") || !strings.Contains(raw, "第17张-值1") {
		t.Fatal("第 17 个 sheet 的标题/单元格缺失")
	}
	// 特殊字符：标题与 title 属性都要转义（注入面见 TestXlsxPreviewEscapesHostileContent）。
	if !strings.Contains(raw, `title="&quot;&gt;&lt;img src=x onerror=alert(1)&gt;"`) {
		t.Fatal("特殊字符 sheet 名的 title 属性未按 HTML 实体转义")
	}
	// 超长名：导航标签里保留全名（浏览器用 CSS ellipsis 截断显示，title 兜底）。
	if !strings.Contains(raw, strings.Repeat("超长", 20)) {
		t.Fatal("超长 sheet 名的全名没进 HTML（被截断就再也看不到原名）")
	}
	// 重名：两个同名 sheet 的标签必须能分辨（否则导航里两个一模一样的「同名表」）。
	if !strings.Contains(raw, "第 1/2 个同名") || !strings.Contains(raw, "第 2/2 个同名") {
		t.Fatal("重名 sheet 的导航标签没有序号区分")
	}
}

// TestXlsxPreviewFreezePanes：C3 = 冻结 2 行 2 列（含交叉区）；A1/None = 完全没有冻结标记。
func TestXlsxPreviewFreezePanes(t *testing.T) {
	pv, py := realXlsxPreviewer(t)
	ws := xlsxFixture(t, py, `
import openpyxl
wb = openpyxl.Workbook()
for title, freeze in (("冻结C3", "C3"), ("显式A1", "A1"), ("未设置", None)):
    s = wb.active if title == "冻结C3" else wb.create_sheet()
    s.title = title
    s.append(["表头1", "表头2", "表头3", "表头4"])
    for r in range(2, 8):
        s.append(["行%d列%d" % (r, c) for c in range(1, 5)])
    s.freeze_panes = freeze
wb.save("freeze.xlsx")
`)
	raw := convertXlsx(t, pv, ws, "freeze.xlsx")
	section := func(name string) string {
		at := strings.Index(raw, `data-sheet="`+name+`"`)
		if at < 0 {
			t.Fatalf("找不到 sheet %s", name)
		}
		rest := raw[at:]
		if end := strings.Index(rest, "</section>"); end >= 0 {
			return rest[:end]
		}
		return rest
	}

	c3 := section("冻结C3")
	if !strings.Contains(c3, `<thead class="frozen-head">`) {
		t.Fatal("C3 冻结：thead 没有 frozen-head（整块吸顶靠它）")
	}
	if n := strings.Count(c3, `<tr class="frozen-row">`); n != 2 {
		t.Fatalf("C3 冻结：frozen-row 行数 = %d，期望 2（冻结 2 行）", n)
	}
	// 冻结列：表头行 2 个 + 表体 6 行 × 2 = 14 个（含左上角交叉区）
	if n := strings.Count(c3, `class="frozen-col"`); n < 2 {
		t.Fatalf("C3 冻结：frozen-col 计数 = %d，期望 ≥2", n)
	}
	if !strings.Contains(c3, `<th class="frozen-col" style="left:0px">`) ||
		!strings.Contains(c3, `frozen-col" style="left:`) {
		t.Fatal("C3 冻结：第 1 列没有 left:0，或第 2 列没有累加偏移")
	}
	if !strings.Contains(c3, "冻结窗格：2 行 × 2 列") {
		t.Fatal("C3 冻结：说明条没有如实写出冻结了几行几列")
	}

	for _, name := range []string{"显式A1", "未设置"} {
		got := section(name)
		for _, probe := range []string{"frozen-head", "frozen-row", "frozen-col"} {
			if strings.Contains(got, probe) {
				t.Fatalf("%s：不该出现 %s（A1/未设置 = 没有冻结窗格）", name, probe)
			}
		}
	}
}

// TestXlsxPreviewNumberFormats：四类必须覆盖的格式，断言的是**渲染出来的文本**，
// 不是类名 —— 类名对了但显示 0.5 而不是 50.00%，用户看到的仍然是错的。
func TestXlsxPreviewNumberFormats(t *testing.T) {
	pv, py := realXlsxPreviewer(t)
	ws := xlsxFixture(t, py, `
import openpyxl, datetime
wb = openpyxl.Workbook(); s = wb.active; s.title = "格式"
s.append(["百分比", "千分位", "日期", "货币", "负数货币"])
s.append([0.5, 1234.5, datetime.date(2026, 1, 2), 1234.5, -1234.5])
for col, fmt in (("A", "0.00%"), ("B", "#,##0.00"), ("C", "yyyy-mm-dd"),
                 ("D", "$#,##0.00"), ("E", "$#,##0.00")):
    s[col + "2"].number_format = fmt
wb.save("fmt.xlsx")
`)
	raw := convertXlsx(t, pv, ws, "fmt.xlsx")
	for _, want := range []string{
		">50.00%<",     // 0.5 + 0.00%
		">1,234.50<",   // #,##0.00
		">2026-01-02<", // yyyy-mm-dd
		">$1,234.50<",  // $#,##0.00
		">-$1,234.50<", // 负号在货币符号前
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("数字格式没渲染成 %s（原文里找不到）", want)
		}
	}
}

// TestXlsxPreviewMergedCellsAndWidths：合并 → colspan/rowspan 且被覆盖的格不再输出；
// 列宽按 Excel 的字符宽换算成像素并钳到 [56, 400]。
func TestXlsxPreviewMergedCellsAndWidths(t *testing.T) {
	pv, py := realXlsxPreviewer(t)
	ws := xlsxFixture(t, py, `
import openpyxl
wb = openpyxl.Workbook(); s = wb.active; s.title = "合并"
s.append(["合并头", "B1不该出现", "C1不该出现", "D1"])
s.append(["A2", "B2", "C2", "D2"])
s.append(["A3", "B3", "C3", "D3"])
s.append(["纵向合并", "B4", "C4", "D4"])
s.append(["A5不该出现", "B5", "C5", "D5"])
s.append(["A6不该出现", "B6", "C6", "D6"])
s.merge_cells("A1:C1")
s.merge_cells("A4:A6")
s.column_dimensions["A"].width = 300   # 2105px -> 钳到 400
s.column_dimensions["B"].width = 2     # 19px  -> 钳到 56
s.column_dimensions["C"].width = 12    # 89px  -> 原样
wb.save("merge.xlsx")
`)
	raw := convertXlsx(t, pv, ws, "merge.xlsx")
	if !strings.Contains(raw, `colspan="3"`) {
		t.Fatal("A1:C1 合并没有输出 colspan=3")
	}
	if !strings.Contains(raw, `rowspan="3"`) {
		t.Fatal("A4:A6 合并没有输出 rowspan=3")
	}
	for _, gone := range []string{"B1不该出现", "C1不该出现", "A5不该出现", "A6不该出现"} {
		if strings.Contains(raw, gone) {
			t.Fatalf("%s 是合并区里被覆盖的格子，不该再输出 <td>", gone)
		}
	}
	for _, want := range []string{`<col style="width:400px">`, `<col style="width:56px">`, `<col style="width:89px">`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("列宽没有按 clamp(字符宽*7+5, 56, 400) 生成：缺 %s", want)
		}
	}
}

// TestXlsxPreviewCapsMergeCount：合并区域的**条数**必须有上限（第 4 轮爬山）。
//
// 为什么单独测条数：<mergeCell> 是 worksheet XML 里的元素，**文件里可控** ⇒ 一份
// 构造过的文件可以声明几十万条（旧实现逐条收进 meta["merges"]，内存随之线性增长）。
// 而**跨度**（每条覆盖多少行/列）早被列闸钳住，不是这里的问题。
//
// 判据：条数封顶 + **如实标注**丢弃了多少（不静默丢弃）。
func TestXlsxPreviewCapsMergeCount(t *testing.T) {
	pv, py := realXlsxPreviewer(t)
	ws := xlsxFixture(t, py, `
import zipfile
import openpyxl
wb = openpyxl.Workbook(); s = wb.active; s.title = "多合并"
s.append(["a", "b"])
s.append(["c", "d"])
wb.save("m.xlsx")
# 直接改 sheet XML：造 3000 条合并区域（openpyxl 的 merge_cells 一次一个，慢）
items = {}
with zipfile.ZipFile("m.xlsx") as z:
    for i in z.infolist():
        items[i.filename] = z.read(i.filename)
sheet = "xl/worksheets/sheet1.xml"
xml = items[sheet].decode("utf-8")
block = "<mergeCells>" + "".join('<mergeCell ref="A%d:B%d"/>' % (i + 1, i + 1) for i in range(3000)) + "</mergeCells>"
xml = xml.replace("</worksheet>", block + "</worksheet>")
items[sheet] = xml.encode("utf-8")
with zipfile.ZipFile("m.xlsx", "w", zipfile.ZIP_DEFLATED) as z:
    for name, data in items.items():
        z.writestr(name, data)
print("MK_OK")
`)
	raw := convertXlsx(t, pv, ws, "m.xlsx")
	if !strings.Contains(raw, "合并区域超过 2000 处") {
		t.Fatalf("合并区域条数上限未生效或未标注")
	}
	if !strings.Contains(raw, "有 1000 处未处理") {
		t.Fatalf("未如实标注丢弃的合并区域数（应为 3000-2000=1000）")
	}
}

// TestXlsxPreviewFormulaCells：公式单元格的两条路径（第 4 轮爬山：单书/双书）。
//
// ① 无缓存值（openpyxl 写出的文件就是这样）⇒ 显示**公式文本**并如实标注；
// ② 有缓存值（Excel/WPS 存过的文件）⇒ 显示**值**，且不出现公式那条说明。
//
// ② 走的是「单书快路径」（整本工作簿没有未计算公式 ⇒ 不加载第二本书、只扫一遍表），
// ① 走双书路径 —— 两条都断言，是防止「优化把公式显示弄没了」的那张网。
func TestXlsxPreviewFormulaCells(t *testing.T) {
	pv, py := realXlsxPreviewer(t)
	ws := xlsxFixture(t, py, `
import openpyxl
wb = openpyxl.Workbook(); s = wb.active; s.title = "公式"
s.append(["名称", "金额"])
s.append(["甲", 10])
s.append(["乙", "=SUM(B2:B2)"])
wb.save("f.xlsx")
`)
	raw := convertXlsx(t, pv, ws, "f.xlsx")
	if !strings.Contains(raw, "=SUM(B2:B2)") {
		t.Fatalf("无缓存值的公式应显示为公式文本")
	}
	if !strings.Contains(raw, "个公式单元格没有缓存值") {
		t.Fatalf("应如实标注公式单元格没有缓存值")
	}

	// ② 给公式补上缓存值（模拟 Excel/WPS 另存过的文件）⇒ 应显示值、不显示公式。
	ws2 := xlsxFixture(t, py, `
import re, zipfile
import openpyxl
wb = openpyxl.Workbook(); s = wb.active; s.title = "公式"
s.append(["名称", "金额"])
s.append(["甲", 10])
s.append(["乙", "=SUM(B2:B2)"])
wb.save("f2.xlsx")
items = {}
with zipfile.ZipFile("f2.xlsx") as z:
    for i in z.infolist():
        items[i.filename] = z.read(i.filename)
sheet = "xl/worksheets/sheet1.xml"
xml = items[sheet].decode()
xml = re.sub(r"(<f>[^<]*</f>)<v></v>", r"\1<v>42</v>", xml)
items[sheet] = xml.encode()
with zipfile.ZipFile("f2.xlsx", "w", zipfile.ZIP_DEFLATED) as z:
    for name, data in items.items():
        z.writestr(name, data)
`)
	raw2 := convertXlsx(t, pv, ws2, "f2.xlsx")
	if strings.Contains(raw2, "=SUM(B2:B2)") {
		t.Fatalf("有缓存值的公式应显示值，不该显示公式文本")
	}
	if strings.Contains(raw2, "个公式单元格没有缓存值") {
		t.Fatalf("有缓存值时不该出现「公式没有缓存值」那条说明")
	}
	if !strings.Contains(raw2, "42") {
		t.Fatalf("有缓存值的公式单元格应显示缓存值 42")
	}
}

// TestXlsxPreviewTruncationToldAtTop：大表被截断时，**页面顶部**要如实写「已显示 N/M 行」，
// 逐 sheet 的「还有 N 行未显示」也不能丢（那是明细）。
func TestXlsxPreviewTruncationToldAtTop(t *testing.T) {
	pv, py := realXlsxPreviewer(t)
	ws := xlsxFixture(t, py, `
import openpyxl
wb = openpyxl.Workbook(); s = wb.active; s.title = "大表"
s.append(["行号", "名称"])
for i in range(2000):
    s.append([i, "项目%d" % i])
wb.save("big.xlsx")
`)
	raw := convertXlsx(t, pv, ws, "big.xlsx")
	at := strings.Index(raw, "已显示 500/2001 行")
	if at < 0 {
		t.Fatal("顶部没有「已显示 500/2001 行」的截断告知（2001 行只渲染 500 行）")
	}
	if first := strings.Index(raw, "<section"); first >= 0 && at > first {
		t.Fatal("截断告知不在页面顶部：它排在第一个 sheet 之后，首屏看不见等于没告知")
	}
	if !strings.Contains(raw, "还有 1501 行未显示") {
		t.Fatal("逐 sheet 的「还有 N 行未显示」明细丢了")
	}
}

// TestXlsxPreviewEscapesHostileContent：sheet 名与单元格内容都是外部数据，任何一处漏转义
// 都是注入。这里用 craft 过的 xlsx（openpyxl 自己拒绝写 <script> 名字，故改 zip 里的
// workbook.xml）连名字带单元格一起打。
func TestXlsxPreviewEscapesHostileContent(t *testing.T) {
	pv, py := realXlsxPreviewer(t)
	ws := xlsxFixture(t, py, `
import openpyxl, zipfile, shutil, html
wb = openpyxl.Workbook(); s = wb.active; s.title = "SHEETNAME"
s.append(["cell1", "cell2"])
s.append(["=1+1", "x"])
wb.save("evil.xlsx")
# openpyxl 写的是 inline string（没有 sharedStrings.xml），故按内容找所在的 zip 成员再替换。
esc = lambda v: html.escape(v, quote=False).encode()
tmp = "evil.xlsx.tmp"
with zipfile.ZipFile("evil.xlsx") as zin, zipfile.ZipFile(tmp, "w", zipfile.ZIP_DEFLATED) as zout:
    for item in zin.infolist():
        data = zin.read(item.filename)
        if b"SHEETNAME" in data or b"cell1" in data:
            data = data.replace(b"SHEETNAME", esc("<script>alert(1)</script>"))
            data = data.replace(b"cell1", esc("<script>alert(2)</script>"))
            data = data.replace(b"cell2", esc('"><img src=x onerror=alert(3)>'))
        zout.writestr(item, data)
shutil.move(tmp, "evil.xlsx")
`)
	raw := convertXlsx(t, pv, ws, "evil.xlsx")
	if strings.Contains(strings.ToLower(raw), "<script") {
		t.Fatal("产物里出现了 <script 元素：sheet 名或单元格内容没转义（预览页会执行它）")
	}
	if strings.Contains(strings.ToLower(raw), "<img") {
		t.Fatal("产物里出现了 <img 元素：单元格内容没转义")
	}
	if !strings.Contains(raw, "&lt;script&gt;alert(2)&lt;/script&gt;") {
		t.Fatal("单元格里的 <script> 没有按实体输出（应显示成文本）")
	}
	if !strings.Contains(raw, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("sheet 名里的 <script> 没有按实体输出")
	}
	// 属性里的引号也必须转义，否则 data-sheet="..." 会被提前闭合。
	if strings.Contains(raw, `data-sheet="><`) {
		t.Fatal("data-sheet 属性被单元格/名字里的引号闭合了")
	}
}
