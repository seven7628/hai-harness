package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
)

// SandboxFor 按工作区根构造执行沙箱（nil 返回值 = NoSandbox 直通兜底）。
//
// 为什么是工厂而不是一个 sandbox.Sandbox 实例：Seatbelt 的读写根在**构造时**注入
// （workspace 参数），而 previewer 是进程级单例（不按工作区隔离，同受管运行时），
// 每个工作区的转化要用各自的根 —— 故把「怎么造沙箱」交给调用方（bridge 侧
// xlsxPreviewSandbox），builtin 包不关心策略细节，测试注入假实现也只需一个函数。
type SandboxFor func(workspace string) sandbox.Sandbox

// XlsxPreviewer 把 .xlsx/.xlsm 工作簿转成**自带样式的 HTML**（应用内预览专用）。
//
// 为什么走「服务端转 HTML + 内嵌浏览器 file://」这条路（而不是前端解析 / 文本预览）：
//   - 前端解析 xlsx 需要新依赖（SheetJS 等），本项目「不加新 npm 依赖」；
//   - 纯文本预览（file_preview）对二进制 xlsx 只会显示乱码；
//   - 内嵌浏览器已实测能渲染 file://（PDF 走的同一条通路），HTML 里表格/样式/中文都由
//     浏览器原生处理，成熟且零新前端代码。
//
// 为什么落临时 HTML 文件、而不是把 HTML 内容回给前端（内嵌浏览器 data URL 已有先例
// openHtmlContentInBrowser）：
//   - 大表 HTML 可达数百 KB～数 MB，而 bridge 的 stdin**单条命令上限 4 MiB**
//     （main.go maxCommandLineBytes，超限直接拒绝整条命令），把产物塞进命令响应等于
//     给预览加一个硬天花板，且失败模式是「命令被拒」而非「内容被截断」，很难排查；
//   - file:// 的通路已被 PDF 实测验证过，且不把 HTML 内容搬进 store 状态（不占渲染内存）。
//
// 归属说明（为什么不放别的层）：
//   - 不是 tools.Tool：本转换**不接受模型输入**（脚本固定、参数只有一个路径），注册进
//     工具集只会让模型多一个可调用面，且 FileTools.Tools() 清单被多处复用（Explore 白名单、
//     schema 巡检）；装配方式对齐 run_python（bridge 显式接线）；
//   - 不是 runtime 包的职责：runtime 只管受管运行时的 bootstrap（venv/依赖/Ensure），
//     把「xlsx 怎么转 HTML」塞进去会让一个纯环境包背上业务语义；
//   - 与 run_python 共用同一套执行前提（受管解释器 + 沙箱 + 临时目录选择），这些辅助
//     （PythonRuntime / writeTempFile / shQuote）都在本包，放这里零重复。
//
// 线程安全：Convert 可并发调用（每次独立临时目录；清理只按「年龄」判定，
// 不跟踪「谁是我上一次的产物」—— 见 Convert 的清理策略）。
type XlsxPreviewer struct {
	py     PythonRuntime // 受管运行时（nil = 未装配 → 明确报错，不 panic）
	sbxFor SandboxFor    // 沙箱工厂（nil = 恒用 NoSandbox 直通）
}

// NewXlsxPreviewer 创建工作簿预览转换器。
// py = 受管运行时（与 run_python 同一个实例）；sbxFor = 按工作区构造沙箱（nil = NoSandbox）。
func NewXlsxPreviewer(py PythonRuntime, sbxFor SandboxFor) *XlsxPreviewer {
	return &XlsxPreviewer{py: py, sbxFor: sbxFor}
}

// XlsxPreviewTimeout 单次转换时限（含 openpyxl 解析 + 渲染；大工作簿数秒级，
// 60s 是「明显不正常就报错」的阈值，不是性能目标）。首次调用另需运行时 bootstrap
// （建 venv + 装依赖，慢网到分钟级）—— 那部分由 Ensure 自己的时限管。
const XlsxPreviewTimeout = 60 * time.Second

// XlsxConvertResult 转换产物。
type XlsxConvertResult struct {
	HTMLPath string // 生成的 HTML 绝对路径（临时目录内；调用方按需保留/清理）
	Dir      string // 临时目录（同上）
}

// Convert 把 xlsxPath（绝对路径）转换为 HTML，返回产物路径。
//
// workspace = 沙箱的读写根（决定策略里哪些路径可读写；与 run_python 同锚：工作区读写两开、
// 受管运行时子树读写两开）。脚本/产物都在受管临时目录里，cwd 也设在那里 —— 那是策略一定
// 放行的位置，故解释器写 __pycache__ 与产物都不会被拦；xlsx 用绝对路径**只读**。
//
// 产物位置：受管临时目录（见 tempDirBases 的理由），**不落在工作区** —— 预览产物不是
// 用户资产，不该出现在 git status / 文件树 / 产出卡片里。
//
// 清理策略（**只按年龄，不记「上一次是谁」**）：
//   - 失败路径立即清理本次目录（产物没成，留着是垃圾）；
//   - 成功路径保留产物（浏览器还要读、可能还要 reload），改由下一次转换时的
//     sweepStaleTempDirs 按年龄回收过期目录。
//
// 为什么不做「这次清掉上次那个」（最初的设计，已改掉）：预览是**并发可达**的 —— 用户在
// A 卡片点了预览（产物 a 已交给浏览器），紧接着点 B 卡片 → b 的转换完成时按「上次是 a」
// 把 a 删掉，而浏览器可能还在加载/刷新 a → 白屏。而「按年龄清扫 + keep 当前」既不会
// 误删活着的产物，也不会让残留无限增长（见 staleTempAge）。
func (p *XlsxPreviewer) Convert(ctx context.Context, xlsxPath, workspace string) (XlsxConvertResult, error) {
	if p.py == nil {
		return XlsxConvertResult{}, fmt.Errorf("no managed Python runtime is configured")
	}
	py, err := p.py.Ensure(ctx)
	if err != nil {
		return XlsxConvertResult{}, fmt.Errorf("managed Python runtime unavailable: %w", err)
	}
	scriptPath, cleanup, err := writeTempFile(py, "xlsx-preview-", "convert_xlsx.py", []byte(xlsxToHTMLScript))
	if err != nil {
		return XlsxConvertResult{}, err
	}
	dir := filepath.Dir(scriptPath)
	done := false
	defer func() {
		// 只有失败路径才清理（成功路径要留着给浏览器读，见函数注释的清理策略）。
		if !done {
			cleanup()
		}
	}()
	htmlPath := filepath.Join(dir, "preview.html")

	// 固定解释器 + 三个路径参数（单引号转义：路径可含空格/引号/中文）。命令行里没有
	// 任何来自模型/前端的自由文本，故不存在「把用户输入拼进 shell」的面。
	cmdline := shQuote(py.VenvPython) + " " + shQuote(scriptPath) + " " + shQuote(xlsxPath) + " " + shQuote(htmlPath)
	var sbx sandbox.Sandbox = sandbox.NoSandbox{}
	if p.sbxFor != nil {
		if s := p.sbxFor(workspace); s != nil {
			sbx = s
		}
	}
	out, err := sbx.Run(ctx, sandbox.ExecSpec{Command: cmdline, Cwd: dir, Timeout: XlsxPreviewTimeout})
	if err != nil {
		return XlsxConvertResult{}, fmt.Errorf("执行转换脚本失败: %w", err)
	}
	if !strings.Contains(out, xlsxOKMarker) {
		return XlsxConvertResult{}, fmt.Errorf("转换失败: %s", singleLine(out))
	}
	if _, err := os.Stat(htmlPath); err != nil {
		return XlsxConvertResult{}, fmt.Errorf("转换脚本未产出 HTML: %w", err)
	}
	// 清扫过期残留（含「上一个进程被杀时留下的」与「本次之前很久的」），keep 当前产物。
	sweepStaleTempDirs(py, dir)
	done = true
	return XlsxConvertResult{HTMLPath: htmlPath, Dir: dir}, nil
}

// staleTempAge 残留回收的年龄门槛。
//
// 为什么需要回收（而不是「下次转换清上次」）：bridge 被 SIGTERM 杀掉时（app 退出的常态）
// 当前产物目录不会被回收，故**每次应用运行都会留下一个目录**（含一份 HTML，大表可达
// 数 MB）。不扫的话 ~/.go-code/runtime/python/tmp 会随使用时长单调增长。
//
// 为什么带年龄门槛而不是全清：同一 HOME 下可能有并发存活的另一个 bridge（多实例/开发
// 调试），或本进程刚交付给浏览器、正在加载的产物 —— 把活的删掉会让那个窗口白屏。
// 1 小时远超一次转换的合理时长（实测小表 <1s、2000 行约 0.6s），故只会命中死进程的残留。
// 代价是极端情况下残留多存活一小时 —— 受管 scratch 区，可接受。
const staleTempAge = time.Hour

// sweepStaleTempDirs 清扫受管临时目录里**过期的** xlsx-preview-* 目录（keep 除外）。
// 尽力而为：目录不存在/无权限/单项删除失败都不影响本次转换（预览已经成功）。
func sweepStaleTempDirs(py runtime.Python, keep string) {
	for _, base := range tempDirBases(py) {
		if base == "" {
			continue
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "xlsx-preview-") {
				continue
			}
			p := filepath.Join(base, e.Name())
			if p == keep {
				continue
			}
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < staleTempAge {
				continue
			}
			_ = os.RemoveAll(p)
		}
	}
}

// xlsxOKMarker 转换脚本成功时输出的标记行（Go 侧以此判定成败：非零退出/沙箱拦截/
// openpyxl 抛错都会让这一行不出现，故统一按「输出里没有标记」判失败）。
const xlsxOKMarker = "GOCODE_XLSX_OK"

// xlsxToHTMLScript 固定的转换脚本（**不是**模型可改写的输入：编译期常量）。
//
// 关键取舍（改脚本前先读）：
//
//  1. **data_only 取舍**：默认 `data_only=True` 读「Excel 算好的缓存值」，这才是用户
//     在 Excel 里看到的数字。代价：openpyxl 自己写出的公式**没有缓存值**（它只写 <f>，
//     不写 <v>）→ 读回是 None。此时**不能显示成空白**（用户会以为单元格是空的，然后
//     去怀疑预览坏了），故再开一份 data_only=False 的「公式书」：缓存值为 None 且该坐标
//     是公式 → 显示公式文本（=SUM(A1:A2)）并标记 class="formula"，页面上另有计数说明。
//     两份 workbook 都是 read_only 流式读，且只遍历到渲染窗口为止。
//
//  2. **三重闸**（同 desktop/app/src/lib/csv.ts 的口径：列 ≤ 40 / 行 ≤ 500 / 总格数 ≤ 8000，
//     外加 sheet 数 ≤ 20）：这些数字是「防卡死」的硬闸。预览产物是一整页 DOM，单元格数
//     直接决定布局与绘制成本；宽表按总格数预算进一步收敛行数（宽表少显示几行好过整页卡住）。
//     收敛结果在页面顶部与每个 sheet 的说明条里**如实告知**（"还有 N 行未显示"），不静默丢弃。
//
//  3. **转义**：单元格内容一律 html.escape(text, quote=True) 后再拼进文档 —— xlsx 内容是
//     外部数据，`<script>` 直接拼进 HTML 就是注入（实测：真被当脚本执行）。换行在转义后
//     再换成 <br>（预览是浏览器，能显示真换行，这点比 CSV→markdown 那条路宽松）。
//
//  4. **charset**：<meta charset="utf-8"> 必须在内（实测教训：textutil 转 HTML 时缺
//     charset 声明会让中文全部 mojibake）。产物固定以 UTF-8 写出。
//
//  5. **样式自带**：内嵌浏览器不继承应用样式（同 sandbox iframe 的约定），不写 <style>
//     就是裸表格。故模板内含完整样式（含暗色 media query，跟随系统/应用外观）。
const xlsxToHTMLScript = `# -*- coding: utf-8 -*-
"""xlsx -> 自带样式的 HTML（go-code 应用内预览专用；脚本由 Go 侧生成，不接受外部输入）。

用法: python convert_xlsx.py <xlsx 绝对路径> <输出 html 绝对路径>
成功时 stdout 末行打印 GOCODE_XLSX_OK；失败时非零退出 + traceback（Go 侧不据此解析细节，
只把输出原样作为错误详情）。
"""
import datetime
import html
import math
import os
import sys
import zipfile

import openpyxl

MAX_SHEETS = 20
MAX_ROWS = 500
MAX_COLS = 40
MAX_CELLS = 8000

STYLE = """
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body {
  margin: 0; padding: 18px 20px 40px;
  font: 13px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
        "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
  color: #1c1c1e; background: #ffffff;
}
h1 { font-size: 15px; font-weight: 600; margin: 0 0 4px; }
h2 { font-size: 13px; font-weight: 600; margin: 22px 0 8px; }
.meta { color: #6b7280; font-size: 12px; margin: 0 0 6px; }
.notes { color: #6b7280; font-size: 12px; margin: 0 0 6px; }
.notes .warn { color: #b45309; }
.sheet-head { display: flex; align-items: baseline; gap: 8px; margin: 22px 0 6px; }
.sheet-head h2 { margin: 0; }
.range { color: #9ca3af; font-size: 12px; font-variant-numeric: tabular-nums; }
.table-wrap { overflow: auto; max-height: 78vh; border: 1px solid #e5e7eb; border-radius: 6px; }
table { border-collapse: collapse; font-variant-numeric: tabular-nums; }
th, td {
  border-right: 1px solid #e5e7eb; border-bottom: 1px solid #e5e7eb;
  padding: 4px 8px; text-align: left; vertical-align: top;
  max-width: 380px; overflow-wrap: anywhere; white-space: pre-wrap;
}
th:last-child, td:last-child { border-right: 0; }
thead th { position: sticky; top: 0; background: #f9fafb; font-weight: 600; z-index: 1; }
tbody tr:nth-child(even) td { background: #fcfcfd; }
td.n, th.n { text-align: right; }
td.formula { color: #6b7280; font-style: italic; }
.empty { color: #9ca3af; }
@media (prefers-color-scheme: dark) {
  body { color: #e5e7eb; background: #1c1c1e; }
  .table-wrap { border-color: #3a3a3c; }
  th, td { border-color: #3a3a3c; }
  thead th { background: #2c2c2e; }
  tbody tr:nth-child(even) td { background: #232325; }
  .meta, .notes, .range { color: #9ca3af; }
}
"""


def text_of(value):
    """单元格值 -> (展示文本, 是否右对齐)。数字/日期右对齐，与 Excel 观感一致。"""
    if isinstance(value, bool):  # 注意 bool 是 int 的子类，必须先判
        return ("TRUE" if value else "FALSE"), True
    if isinstance(value, (int, float)):
        if isinstance(value, float) and (math.isnan(value) or math.isinf(value)):
            return str(value), True
        if isinstance(value, float) and value == int(value) and abs(value) < 1e16:
            return str(int(value)), True  # 12.0 -> 12（Excel 的显示语义，别显示成 12.0）
        return str(value), True
    if isinstance(value, datetime.datetime):
        if value.hour or value.minute or value.second:
            return value.strftime("%Y-%m-%d %H:%M:%S"), True
        return value.strftime("%Y-%m-%d"), True
    if isinstance(value, datetime.date):
        return value.strftime("%Y-%m-%d"), True
    if isinstance(value, datetime.time):
        return value.strftime("%H:%M:%S"), True
    if isinstance(value, datetime.timedelta):
        total = int(round(value.total_seconds()))
        sign = "-" if total < 0 else ""
        total = abs(total)
        hours, rem = divmod(total, 3600)
        return "%s%d:%02d:%02d" % (sign, hours, rem // 60, rem % 60), True
    if isinstance(value, bytes):
        return value.decode("utf-8", "replace"), False
    return str(value), False


def clean(value):
    """展示文本 -> 可安全嵌入 HTML 的片段。

    先删控制字符（NUL/垂直制表等：xlsx 里偶有残留，浏览器会渲染成「替换符」糊在格子里，
    换行/制表符除外），再 html.escape（& < > " ' 全转 —— xlsx 内容是外部数据，
    未转义的 <script> 会被当脚本执行，这是实测过的注入面），最后把真换行换成 <br>
    （预览是浏览器，能显示真换行；这点与 CSV→GFM 那条路不同，那边只能折叠成 ↵）。
    """
    s = "".join(ch for ch in str(value) if ch == "\n" or ch == "\t" or ord(ch) >= 0x20)
    return html.escape(s, quote=True).replace("\n", "<br>")


def rows_of(ws):
    """流式产出 (行号, {列号: 单元格})；read_only 模式下整行皆空的记录不产出。

    为什么用 enumerate 取行列号，而不是 c.row / c.column：
    read_only 迭代里**行内的空格是 EmptyCell**，而 EmptyCell 没有 .row / .column
    属性（只有真实 ReadOnlyCell 才有）—— 一行只要有任何一个格子为空（真实表格极常见，
    比如 "日期" 那行只有一个值），直接读 c.column 就 AttributeError 崩掉整个转换
    （实测报错：EmptyCell object has no attribute 'row'）。
    enumerate 的位置与 c.column 实测完全一致（含行首为空格的情况，iter_rows 从
    min_column 起算），故在此按位置取号是安全的。保留读 c.value 的判空语义。
    """
    for row_no, tup in enumerate(ws.iter_rows(), 1):
        if not tup:
            continue
        cells = {}
        for col_no, c in enumerate(tup, 1):
            if c.value is not None:
                cells[col_no] = c
        if cells:
            yield row_no, cells


def render_sheet(idx, name, state, ws_form, ws_val, out):
    """渲染单个 sheet（返回该 sheet 的统计，供说明条使用）。"""
    hidden = state not in (None, "visible")
    form_rows = rows_of(ws_form)
    val_rows = rows_of(ws_val)
    v_row_no, v_cells = next(val_rows, (None, None))

    # 列上界：用 dimension 的 max_column（Excel 一定写；异常时按实际行宽兜底），受列闸约束。
    declared_cols = ws_form.max_column if isinstance(ws_form.max_column, int) else 0
    cols_cap = min(max(declared_cols, 1), MAX_COLS)
    rows_cap = min(MAX_ROWS, MAX_CELLS // cols_cap)

    shown = []          # [(行号, {列号: (文本, 右对齐, 是否公式未计算)})]
    max_col_seen = 0
    row_count = 0       # 全部数据行数（含未显示的）
    formula_missing = 0
    for row_no, f_cells in form_rows:
        row_count += 1
        # 值书与公式书按行号对齐推进（值书会跳过「整行都是未计算公式」的行，不能并行取）。
        while v_row_no is not None and v_row_no < row_no:
            v_row_no, v_cells = next(val_rows, (None, None))
        v_here = v_cells if v_row_no == row_no else {}
        if v_row_no == row_no:
            v_row_no, v_cells = next(val_rows, (None, None))
        if len(shown) >= rows_cap:
            continue  # 只计数，不再取单元格（hidden 统计要准）
        cells = {}
        for col, fcell in f_cells.items():
            if col > cols_cap:
                continue
            if col > max_col_seen:
                max_col_seen = col
            vcell = v_here.get(col)
            v = vcell.value if vcell is not None else None
            f = fcell.value
            # 每个分支都必须经 clean()（转义 + 控制字符 + 换行）—— 单元格内容是外部数据，
            # 少走一次 clean 就是一次注入（实测：直接拼接会让 <script> 真的执行）。
            if v is not None:
                text, num = text_of(v)
                cells[col] = (clean(text), num, False)
            elif isinstance(f, str) and f.startswith("="):
                # openpyxl 写出的公式没有缓存值 -> 显示公式文本（见文件头取舍 1）
                formula_missing += 1
                cells[col] = (clean(f), False, True)
            elif f is not None:
                text, num = text_of(f)
                cells[col] = (clean(text), num, False)
            else:
                cells[col] = ("", False, False)
        shown.append((row_no, cells))

    total_rows = row_count
    hidden_rows = total_rows - len(shown)
    cols = max(min(max_col_seen, cols_cap), 1) if shown else cols_cap
    wide = declared_cols > MAX_COLS

    label = "%s%s" % (clean(name), ' <span class="range">(隐藏)</span>' if hidden else "")
    out.append('<div class="sheet-head"><h2>%d. %s</h2>' % (idx + 1, label))
    out.append('<span class="range">%d 行 × %d 列</span></div>' % (total_rows, max(declared_cols, max_col_seen)))

    notes = []
    if idx == 0:
        notes.append("首行按表头显示（xlsx 没有表头标记，无法可靠推断）")
    if hidden_rows > 0:
        notes.append('<span class="warn">还有 %d 行未显示（单表上限 %d 行）</span>' % (hidden_rows, rows_cap))
    if wide:
        notes.append('<span class="warn">只显示前 %d 列（共 %d 列）</span>' % (MAX_COLS, declared_cols))
    if formula_missing > 0:
        notes.append("%d 个公式单元格没有缓存值，已显示为公式文本（用 Excel/WPS 打开并另存即可生成缓存值）" % formula_missing)
    if notes:
        out.append('<p class="notes">%s</p>' % " · ".join(notes))

    if not shown:
        out.append('<p class="empty">（空表）</p>')
        return

    # <thead>/<tbody> 必须显式写出来（实测）：只写 <tr><th> 时浏览器会把整个表塞进
    # 隐式 tbody，thead th 那套「表头吸顶 + 底色」样式全部不生效（视觉上表头与数据行
    # 长得一样，长表滚下去就不知道哪列是什么）。
    out.append('<div class="table-wrap"><table>')
    out.append("<thead><tr>")
    for col in range(1, cols + 1):
        text, num, _ = shown[0][1].get(col, ("", False, False))
        attr = ' class="n"' if num else ""
        out.append("<th%s>%s</th>" % (attr, text))
    out.append("</tr></thead><tbody>")
    for row_no, cells in shown[1:]:
        out.append("<tr>")
        for col in range(1, cols + 1):
            text, num, is_formula = cells.get(col, ("", False, False))
            cls = []
            if num:
                cls.append("n")
            if is_formula:
                cls.append("formula")
            attr = ' class="%s"' % " ".join(cls) if cls else ""
            out.append("<td%s>%s</td>" % (attr, text))
        out.append("</tr>")
    out.append("</tbody></table></div>")


def main():
    src, dst = sys.argv[1], sys.argv[2]
    # 两份 workbook：值书（缓存值，用户看到的数字）+ 公式书（判断「空值其实是未计算公式」）。
    # data_only 取舍见脚本头注释。
    try:
        wb_val = openpyxl.load_workbook(src, data_only=True, read_only=True)
    except openpyxl.utils.exceptions.InvalidFileException as exc:
        # 可预期的用户错误（最常见：.xls 老格式、文件其实不是工作簿、zip 损坏）→
        # **单行**中文提示，不把 traceback 甩到用户脸上（toast 里塞 40 行堆栈等于没提示）。
        # 其余异常照旧上抛（那是我们的 bug，需要完整堆栈来定位）。
        sys.stderr.write("这不是 openpyxl 能读的工作簿（.xls 是老格式，请另存为 .xlsx）：%s\n" % exc)
        sys.exit(2)
    except (zipfile.BadZipFile, OSError, KeyError) as exc:
        sys.stderr.write("工作簿文件损坏或不是有效的 xlsx：%s\n" % exc)
        sys.exit(2)
    wb_form = openpyxl.load_workbook(src, data_only=False, read_only=True)
    names = wb_form.sheetnames
    out = []
    out.append("<!doctype html>")
    out.append('<html lang="zh"><head><meta charset="utf-8">')
    out.append('<meta name="viewport" content="width=device-width, initial-scale=1">')
    out.append("<title>%s</title>" % clean(os.path.basename(src)))
    out.append("<style>%s</style></head><body>" % STYLE)
    out.append("<h1>%s</h1>" % clean(os.path.basename(src)))
    out.append('<p class="meta">%d 个工作表%s · 单表最多显示 %d 行 × %d 列（共 %d 格）</p>'
               % (len(names), "（只显示前 %d 个）" % MAX_SHEETS if len(names) > MAX_SHEETS else "",
                  MAX_ROWS, MAX_COLS, MAX_CELLS))
    rendered = 0
    for i, name in enumerate(names):
        if rendered >= MAX_SHEETS:
            out.append('<p class="notes"><span class="warn">还有 %d 个工作表未显示（上限 %d）</span></p>'
                       % (len(names) - MAX_SHEETS, MAX_SHEETS))
            break
        render_sheet(rendered, name, getattr(wb_form[name], "sheet_state", "visible"),
                     wb_form[name], wb_val[name], out)
        rendered += 1
    out.append("</body></html>")
    with open(dst, "w", encoding="utf-8") as fh:
        fh.write("\n".join(out))
    print("GOCODE_XLSX_OK", dst)


main()
`
