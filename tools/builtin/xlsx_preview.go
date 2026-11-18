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
	// 输出封顶：子进程的 stdout 不该无界进内存。
	out, err := sbx.Run(ctx, sandbox.ExecSpec{Command: cmdline, Cwd: dir, Timeout: XlsxPreviewTimeout, OutputLimit: 64 << 10})
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
//
//  6. **表格增强全部在服务端做完**（R08b / 决策 D-K：不加前端依赖）：
//     sheet 导航（sticky 锚点）、冻结窗格（position: sticky）、列宽（colgroup）、合并单元格
//     （colspan/rowspan）、数字格式（number_format → 文本）。**一条 JS 都不许有** ——
//     内嵌 iframe 是 sandbox="allow-same-origin" 没有 allow-scripts，脚本不会执行；
//     真正的实现手段只有 CSS。所以「多行冻结」用的是整块 thead 吸顶，而不是逐行算偏移。
//
//  7. **read_only 模式的取舍（改脚本前务必先读）**：read_only 的 worksheet 拿不到
//     freeze_panes / merged_cells / column_dimensions（openpyxl 根本不建这些对象），
//     但**有** cell.number_format。故：数字格式走 openpyxl；冻结/合并/列宽另开一份 zip
//     扫 sheet XML 取（只读 <pane>/<col>/<mergeCell>/<sheetFormatPr> 四个标签）。
//     不改成 read_only=False 全量加载：那会把整本工作簿搬进内存（转换上限是 100 MiB 输入），
//     而且现有两遍流式扫描的「只渲染窗口内」性质会一起丢掉。
//
//  8. **sheet 名的坑**：重名（crafted 文件，Excel 自己也拦不住）、超长（>31 字符串）、
//     特殊字符（& < > " '）都会撞锚点或撑破导航条。故锚点用**序号**（sheet-1..N，天然唯一），
//     显示名走 attr_clean（只转义、不塞 <br>），重名的加 (第 k 个) 后缀，长名交给 CSS ellipsis
//     + title 属性保留全名。
const xlsxToHTMLScript = `# -*- coding: utf-8 -*-
"""xlsx -> 自带样式的 HTML（go-code 应用内预览专用；脚本由 Go 侧生成，不接受外部输入）。

用法: python convert_xlsx.py <xlsx 绝对路径> <输出 html 绝对路径>
成功时 stdout 末行打印 GOCODE_XLSX_OK；失败时非零退出 + traceback（Go 侧不据此解析细节，
只把输出原样作为错误详情）。
"""
import bisect
import datetime
import html
import math
import os
import re
import sys
import zipfile
import xml.etree.ElementTree as ET

import openpyxl

MAX_SHEETS = 20
MAX_ROWS = 500
# 扫描行数上限（**解析成本闸**）：iter_rows 必须带边界。
# 不带的话 openpyxl 按 <dimension> 里声明的尺寸扫，而那个值是**文件里可控的**
# （最大 16384 列 × 1048576 行）⇒ 1 KB 的文件能烧掉几百秒 CPU（实测单个工作簿 839 s）。
# 5000 行足够看一个表的「头几屏」，且解析成本有界。
MAX_SCAN_ROWS = 5000
MAX_COLS = 40
MAX_CELLS = 8000

# 冻结窗格上限：Excel 可以冻 20 行，但预览里粘住半个屏幕比不冻更糟。
# 钳制必须**如实告知**（render_sheet 的说明条会写「原文件冻结了 N 行 × M 列，预览只保留
# 前 3 行 × 3 列」）—— 静默少冻几行等于骗人：用户会以为文件本身没冻。
MAX_FROZEN_ROWS = 3
MAX_FROZEN_COLS = 3

# 列宽口径。Excel 的 width 是「字符数」不是像素：px ≈ width * 7 + 5（11pt Calibri 的经验
# 换算，与 Excel 自己的列宽→像素同一量级）。钳到 [56, 400]：下界「至少看得见一个字」，
# 上界「别让一列吃掉整个视口」—— 实测 xlsx 里出现过 width=3000 的列，不钳就是 21000px
# 的横向滚动地狱。
COL_MIN_PX = 56
COL_MAX_PX = 400
COL_DEFAULT_PX = 96

STYLE = """
:root {
  color-scheme: light dark;
  --bg: #ffffff; --bg-even: #fcfcfd; --head-bg: #f9fafb; --line: #e5e7eb;
  --fg: #1c1c1e; --muted: #6b7280; --warn: #b45309;
  --nav-h: 38px;
}
* { box-sizing: border-box; }
body {
  margin: 0; padding: 18px 20px 40px;
  font: 13px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
        "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
  color: var(--fg); background: var(--bg);
}
h1 { font-size: 15px; font-weight: 600; margin: 0 0 4px; }
h2 { font-size: 13px; font-weight: 600; margin: 0; }
.meta, .notes { color: var(--muted); font-size: 12px; margin: 0 0 6px; }
.notes .warn { color: var(--warn); }
.trunc {
  border: 1px solid var(--warn); border-radius: 6px; padding: 8px 10px; margin: 10px 0 4px;
  color: var(--warn); font-size: 12px; background: rgba(180, 83, 9, 0.06);
}
.trunc b { display: block; }
.trunc ul { margin: 2px 0 0; padding-left: 18px; }
/* sheet 导航：sticky 单行（height 固定 = --nav-h，锚点跳转的偏移量才算得准）。
   .sheet 的 scroll-margin-top 让目标标题落在导航条**下方**而不是被它盖住。 */
.sheet-nav {
  position: sticky; top: 0; z-index: 20;
  display: flex; align-items: center; gap: 6px;
  height: var(--nav-h); padding: 5px 0;
  overflow-x: auto; overflow-y: hidden;
  background: var(--bg); border-bottom: 1px solid var(--line);
}
.sheet-nav .nav-label { flex: 0 0 auto; color: var(--muted); font-size: 12px; }
.sheet-nav a {
  flex: 0 0 auto; max-width: 200px; overflow: hidden; text-overflow: ellipsis;
  white-space: nowrap; font-size: 12px; line-height: 16px; padding: 3px 8px;
  border: 1px solid var(--line); border-radius: 999px;
  color: inherit; text-decoration: none;
}
.sheet-nav a:hover { border-color: #9ca3af; background: var(--head-bg); }
.sheet { scroll-margin-top: calc(var(--nav-h) + 12px); }
.sheet-head { display: flex; align-items: baseline; gap: 8px; margin: 18px 0 6px; }
.range { color: #9ca3af; font-size: 12px; font-variant-numeric: tabular-nums; }
.table-wrap { overflow: auto; max-height: 78vh; border: 1px solid var(--line); border-radius: 6px; }
/* table-layout: fixed + width: max-content：colgroup 里的像素宽**就是**最终宽度，冻结列的
   left 偏移才与视觉一致（auto 布局下浏览器会把窄列压缩/宽列拉伸，偏移立刻错位 —— 实测过）。
   border-collapse 必须是 separate：collapse 下 Chrome 会把表头的背景/层级并进表格的共享
   绘制层，sticky 的冻结行压不住滚动中的表身（实测：第二行冻结行被表身文字透出来，改成
   separate 后立刻正常）。代价是相邻单元格不能靠折叠合并边框 —— 故只画 right/bottom，
   左右上下相邻边自然只出现一条 1px。 */
table.grid { border-collapse: separate; border-spacing: 0; table-layout: fixed;
             width: max-content; font-variant-numeric: tabular-nums; }
th, td {
  border-right: 1px solid var(--line); border-bottom: 1px solid var(--line);
  padding: 4px 8px; text-align: left; vertical-align: top;
  background: var(--bg); overflow-wrap: anywhere; white-space: pre-wrap;
}
th { background: var(--head-bg); font-weight: 600; }
th:last-child, td:last-child { border-right: 0; }
/* 表头行（第 1 行）**永远**吸顶：这是预览的既有行为，与冻结窗格无关 —— A1 / 未设置
   freeze_panes 的工作簿同样如此（既有 e2e 断言 position:sticky）。区别只在于「冻结
   标记」：只有 freeze_panes 真的冻了行列才出 .frozen-head/.frozen-row/.frozen-col。
   换句话说「A1 → 无 sticky」应读作「无冻结」，不是「表头也不吸顶」。 */
thead th { position: sticky; top: 0; z-index: 1; }
tbody tr:nth-child(even) td { background: var(--bg-even); }
td.n, th.n { text-align: right; }
td.formula { color: var(--muted); font-style: italic; }
.empty { color: #9ca3af; }
/* 冻结窗格（R08b）。多行冻结用**整块 thead** 吸顶：逐行 sticky 要给每行一个 top 偏移，
   而偏移 = 上方各行高度之和，行高随内容变化 → 纯 CSS 算不出来（没有 JS 可用）。
   子单元格：top 放掉（纵向交给整块 thead），但必须**自己保持定位并抬 z-index** ——
   实测只给 thead 设 z-index 压不住表身（table row group 的 z-index 在 Chrome 里不建立
   层叠上下文），现象是滚动时表身压在第二行冻结行上：表头看得见、冻结的第二行被吃掉。
   子单元格也不能改 position: static —— 那会连冻结列的横向 sticky 一起废掉。 */
thead.frozen-head { position: sticky; top: 0; z-index: 3; }
thead.frozen-head th, thead.frozen-head td { position: sticky; top: auto; z-index: 4; }
thead.frozen-head .frozen-col { z-index: 5; }
/* 冻结列：left 由上面的列宽累加得出。z-index 顺序 = 冻结列(2) < 整块冻结行(3) < 左上角交叉(4)。 */
.frozen-col { position: sticky; z-index: 2; border-right: 1px solid #d1d5db; }
thead .frozen-col { z-index: 4; }
@media (prefers-color-scheme: dark) {
  :root { --bg: #1c1c1e; --bg-even: #232325; --head-bg: #2c2c2e;
          --line: #3a3a3c; --fg: #e5e7eb; }
  .sheet-nav a:hover { border-color: #6b7280; }
  .frozen-col { border-right-color: #4b4b4d; }
}
"""


def text_of(value, fmt=None):
    """单元格值 -> (展示文本, 是否右对齐)。数字/日期右对齐，与 Excel 观感一致。

    为什么必须先过 format_number：Excel 里看到的 "50.00%" / "1,234.50" / "2026-01-02" 是
    **缓存值 + number_format 两半**拼出来的。只读缓存值会得到 0.5 / 1234.5 —— 数字没错，
    但和用户在 Excel 里看到的不是同一个东西（千分位、百分号、货币符号全丢了）。
    """
    if isinstance(value, bool):  # 注意 bool 是 int 的子类，必须先判
        return ("TRUE" if value else "FALSE"), True
    if isinstance(value, (int, float, datetime.datetime, datetime.date, datetime.time)):
        text = format_number(value, fmt)
        if text is not None:
            return text, True
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


MONTH_ABBR = ["Jan", "Feb", "Mar", "Apr", "May", "Jun",
              "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"]
MONTH_FULL = ["January", "February", "March", "April", "May", "June",
              "July", "August", "September", "October", "November", "December"]
DAY_ABBR = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"]
DAY_FULL = ["Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"]


def format_number(value, fmt):
    """按 number_format 把缓存值渲染成 Excel 里看到的文本；不认识就返回 None（走默认）。

    覆盖到的是「最常见的四类」：百分比 / 千分位 / 日期 / 货币（含会计括号负数、颜色与
    区域前缀 [Red] [$EUR-407]）。刻意**不追求**实现完整的 Excel 格式串（条件段、科学计数、
    分数、区域货币名等）—— 不认识的格式返回 None，让调用方退回原来的数字文本：
    显示成 1234.5 只是不够漂亮，显示成错误的东西就是骗人。
    """
    try:
        return _format_number(value, fmt)
    except Exception:  # 格式串是外部数据，任何解析异常都不该让整页预览挂掉
        return None


def _format_number(value, fmt):
    if not isinstance(fmt, str):
        return None
    fmt = fmt.strip()
    if not fmt or fmt.lower() in ("general", "@"):
        return None
    sections = _split_sections(fmt)
    is_date = _has_date_tokens(fmt)
    neg = False
    if isinstance(value, (datetime.datetime, datetime.date, datetime.time)):
        if not is_date:
            return None      # 日期值 + 数字格式：Excel 会显示成序列号，我们不猜
        section = sections[0]
    elif isinstance(value, (int, float)) and not isinstance(value, bool):
        if is_date:
            return None      # 数字值 + 日期格式：同上，序列号语义不自造
        if value < 0 and len(sections) >= 2:
            # 第二段自带负号/括号（"#,##0.00;[Red]-#,##0.00"），不能再叠一个 '-'
            section, neg = sections[1], False
        elif value == 0 and len(sections) >= 3:
            section = sections[2]
        else:
            section, neg = sections[0], value < 0
    else:
        return None
    body = _strip_fmt(section)
    if is_date:
        return _date_text(value, body)
    num = abs(value) if neg else value
    if "%" in body:
        num *= 100  # 百分比是「值 + 格式」两半：0.5 + 0.00% 在 Excel 里显示 50.00%
    text = _number_text(num, body)
    if text is None:
        return None
    return "-" + text if neg else text


def _split_sections(fmt):
    """按 ';' 切正/负/零/文本四段（引号里的分号不算分隔符）。"""
    parts, cur, quoted = [], [], False
    for ch in fmt:
        if ch == '"':
            quoted = not quoted
        elif ch == ";" and not quoted:
            parts.append("".join(cur))
            cur = []
            continue
        cur.append(ch)
    parts.append("".join(cur))
    return parts


def _strip_fmt(section):
    """把格式段摊平成「字面量就地内联」的样式串。

    [Red] / [>=100] / [$-409] 这类指令整段丢掉（颜色与 locale 是 Excel 的显示细节，HTML 里
    没法照搬）；但 [$EUR-407] / [$$-409] 里的货币符号要留下 —— 那正是 "$#,##0.00" 的来源。
    \\x 是转义字面量，_x 是「留一个字符宽的空格」，*x 是填充符（丢掉）。
    """
    out, i, n = [], 0, len(section)
    while i < n:
        ch = section[i]
        if ch == '"':
            j = section.find('"', i + 1)
            j = n if j < 0 else j
            out.append(section[i + 1:j])
            i = j + 1
        elif ch == "\\":
            if i + 1 < n:
                out.append(section[i + 1])
            i += 2
        elif ch == "_":
            out.append(" ")
            i += 2
        elif ch == "*":
            i += 2
        elif ch == "[":
            j = section.find("]", i + 1)
            j = n if j < 0 else j
            inner = section[i + 1:j]
            if inner.startswith("$"):
                sym = inner[1:].split("-")[0]
                if sym:
                    out.append(sym)
            i = j + 1
        else:
            out.append(ch)
            i += 1
    return "".join(out)


def _has_date_tokens(section):
    """原始格式段里有没有日期/时间骨架（跳过引号与方括号里的字面量）。

    只看一个 y/d/h/s 就够：数字格式里不会出现这四个字母（'m' 有歧义 —— mm 既可能是月
    也可能是分，只在有 h/s 的上下文里才当时间），故不参与判定。
    """
    quoted, i, n = False, 0, len(section)
    while i < n:
        ch = section[i]
        if ch == '"':
            quoted = not quoted
            i += 1
        elif ch == "[":
            j = section.find("]", i)
            i = n if j < 0 else j + 1
        elif ch == "\\":
            i += 2
        elif not quoted and ch in "yYdDhHsS":
            return True
        else:
            i += 1
    return False


def _number_text(value, style):
    """数字骨架 -> 文本：千分位按 ',' 决定、小数位数按 0 的个数决定，其余字符原样保留
    （货币符号在前、百分号在后，都是「其余字符」）。"""
    m = re.search(r"[0#?,]+(?:\.[0#]+)?", style)
    if not m or re.search(r"[eE][+-]", style):
        return None      # 没有数字骨架 / 科学计数：不猜
    core = m.group(0)
    frac = core.partition(".")[2]
    dec = len([ch for ch in frac if ch in "0#"])
    text = ("%%.%df" % dec) % value
    if "," in core:
        whole, _, rest = text.partition(".")
        text = "{:,}".format(int(whole)) + ("." + rest if rest else "")
    return style[:m.start()] + text + style[m.end():]


def _date_text(value, style):
    """日期/时间格式 -> 文本。逐段映射 y/m/d/h/s 记号，其余字符原样搬运（- / : 空格 中文）。"""
    if isinstance(value, datetime.time):
        d = datetime.datetime(1899, 12, 31, value.hour, value.minute, value.second)
    elif isinstance(value, datetime.datetime):
        d = value
    else:
        d = datetime.datetime(value.year, value.month, value.day)
    low = style.lower()
    h12 = "am/pm" in low or "a/p" in low
    time_only = isinstance(value, datetime.time)
    out, i, n, hour_at = [], 0, len(style), -1
    while i < n:
        ch = low[i]
        k = 1
        while i + k < n and low[i + k] == ch:
            k += 1
        if ch == "y":
            out.append("%04d" % d.year if k >= 3 else "%02d" % (d.year % 100))
            i += k
        elif ch == "m":
            if time_only or hour_at >= 0:  # h 之后的 m 是分钟，不是月份
                out.append("%02d" % d.minute)
            elif k >= 4:
                out.append(MONTH_FULL[d.month - 1])
            elif k == 3:
                out.append(MONTH_ABBR[d.month - 1])
            elif k == 2:
                out.append("%02d" % d.month)
            else:
                out.append(str(d.month))
            i += k
        elif ch == "d":
            if k >= 4:
                out.append(DAY_FULL[d.weekday()])
            elif k == 3:
                out.append(DAY_ABBR[d.weekday()])
            elif k == 2:
                out.append("%02d" % d.day)
            else:
                out.append(str(d.day))
            i += k
        elif ch == "h":
            hour_at = max(i, 0)
            hh = (d.hour % 12 or 12) if h12 else d.hour
            out.append("%02d" % hh if k >= 2 else str(hh))
            i += k
        elif ch == "s":
            out.append("%02d" % d.second if k >= 2 else str(d.second))
            i += k
        elif low.startswith("am/pm", i):
            out.append("AM" if d.hour < 12 else "PM")
            i += 5
        elif low.startswith("a/p", i):
            out.append("A" if d.hour < 12 else "P")
            i += 3
        else:
            out.append(style[i])
            i += 1
    return "".join(out)


def clean(value):
    """展示文本 -> 可安全嵌入 HTML 的片段。

    先删控制字符（NUL/垂直制表等：xlsx 里偶有残留，浏览器会渲染成「替换符」糊在格子里，
    换行/制表符除外），再 html.escape（& < > " ' 全转 —— xlsx 内容是外部数据，
    未转义的 <script> 会被当脚本执行，这是实测过的注入面），最后把真换行换成 <br>
    （预览是浏览器，能显示真换行；这点与 CSV→GFM 那条路不同，那边只能折叠成 ↵）。
    """
    s = "".join(ch for ch in str(value) if ch == "\n" or ch == "\t" or ord(ch) >= 0x20)
    return html.escape(s, quote=True).replace("\n", "<br>")


def attr_clean(value):
    """属性值 / 导航标签用的转义：同样全转义 + 删控制字符，但**不**插 <br>。

    为什么不能复用 clean()：clean() 会把换行变成 <br>，塞进 id=""/title=""/data-* 就是
    一段错字面量。sheet 名是外部数据，两个入口都要转义（少一个就是一条注入面）。
    """
    s = "".join(ch for ch in str(value) if ch == "\t" or ord(ch) >= 0x20)
    return html.escape(s, quote=True)


def empty_meta():
    """空元数据。**每次新建**：把它写成模块级常量再 dict(...) 只拷一层，merges/widths 会
    被所有 sheet 共享（一个 sheet 解出来的合并信息串到另一个 sheet 上）。"""
    return {"frozen_rows": 0, "frozen_cols": 0, "merges": [], "widths": {}, "default_width": None}


def _int_attr(el, name):
    try:
        return int(float(el.get(name)))
    except (TypeError, ValueError):
        return 0


def sheet_paths(zf):
    """按 workbook.xml 里的 sheet 顺序，返回各自 worksheet XML 在 zip 内的路径。

    为什么自己解 rels 而不是读 openpyxl 的私有字段（ReadOnlyWorksheet._worksheet_path）：
    私有字段跨版本会改，而 rels 是 OOXML 公开结构。解析失败返回 [] —— 调用方据此降级成
    「无冻结/无合并/等宽列」，**元数据缺失绝不能毁掉整页预览**。
    """
    try:
        rels = {}
        root = ET.fromstring(zf.read("xl/_rels/workbook.xml.rels"))
        for el in root:
            if el.tag.rsplit("}", 1)[-1] != "Relationship":
                continue
            target = el.get("Target") or ""
            if target.startswith("/"):
                target = target[1:]
            elif not target.startswith("xl/"):
                target = "xl/" + target
            rels[el.get("Id")] = target
        out = []
        for el in ET.fromstring(zf.read("xl/workbook.xml")).iter():
            if el.tag.rsplit("}", 1)[-1] != "sheet":
                continue
            rid = ""
            for key, val in el.attrib.items():
                if key.rsplit("}", 1)[-1] == "id":
                    rid = val
            out.append(rels.get(rid, ""))
        return out
    except Exception:
        return []


def parse_meta(zf, path):
    """扫一个 worksheet 的 XML，取冻结窗格 / 合并单元格 / 列宽。

    只认四个标签（pane / col / mergeCell / sheetFormatPr），其余节点 clear() 掉边走边扔，
    内存不随表长增长。这些信息在 read_only 模式下 openpyxl 根本不建对象（见脚本头取舍 7），
    所以必须自己读一遍 XML —— 代价是每个渲染到的 sheet 多扫一次（实测 20k 行 × 20 列
    约 0.35s，与 openpyxl 自己的流式扫描同量级）。
    """
    meta = empty_meta()
    if not path:
        return meta
    try:
        with zf.open(path) as fh:
            for _event, el in ET.iterparse(fh, events=("end",)):
                tag = el.tag.rsplit("}", 1)[-1]
                if tag == "pane" and (el.get("state") or "frozen") == "frozen":
                    meta["frozen_rows"] = _int_attr(el, "ySplit")
                    meta["frozen_cols"] = _int_attr(el, "xSplit")
                    if not meta["frozen_rows"] and not meta["frozen_cols"]:
                        m = re.match(r"([A-Z]+)(\d+)$", el.get("topLeftCell") or "")
                        if m:  # 只给 topLeftCell 不写 Split 的实现（少见但有）
                            meta["frozen_rows"] = int(m.group(2)) - 1
                            meta["frozen_cols"] = top_left_col(m.group(1)) - 1
                elif tag == "col":
                    width = el.get("width")
                    if width:
                        hi = min(_int_attr(el, "max"), MAX_COLS)
                        for col in range(max(_int_attr(el, "min"), 1), hi + 1):
                            meta["widths"][col] = float(width)
                elif tag == "mergeCell":
                    m = re.match(r"([A-Z]+)(\d+):([A-Z]+)(\d+)$", el.get("ref") or "")
                    if m:
                        meta["merges"].append((int(m.group(2)), top_left_col(m.group(1)),
                                               int(m.group(4)), top_left_col(m.group(3))))
                elif tag == "sheetFormatPr":
                    dw = el.get("defaultColWidth")
                    if dw:
                        meta["default_width"] = float(dw)
                elif tag == "worksheet":
                    break
                el.clear()
    except Exception:
        pass
    return meta


def top_left_col(letters):
    """列字母 -> 列号（A=1）。自己算而不引 openpyxl.utils：这个换算在格式解析里也要用，
    两处依赖同一个实现比「一处库、一处手写」更不容易分叉。"""
    n = 0
    for ch in letters:
        n = n * 26 + (ord(ch) - 64)
    return n


def col_px(width_chars):
    """Excel 列宽（字符数）-> 像素，并钳到 [COL_MIN_PX, COL_MAX_PX]。"""
    if not width_chars:
        return COL_DEFAULT_PX
    return max(COL_MIN_PX, min(COL_MAX_PX, int(round(width_chars * 7 + 5))))


def rows_of(ws, max_col=None, max_row=None):
    """流式产出 (行号, {列号: 单元格})；read_only 模式下整行皆空的记录不产出。

    为什么用 enumerate 取行列号，而不是 c.row / c.column：
    read_only 迭代里**行内的空格是 EmptyCell**，而 EmptyCell 没有 .row / .column
    属性（只有真实 ReadOnlyCell 才有）—— 一行只要有任何一个格子为空（真实表格极常见，
    比如 "日期" 那行只有一个值），直接读 c.column 就 AttributeError 崩掉整个转换
    （实测报错：EmptyCell object has no attribute 'row'）。
    enumerate 的位置与 c.column 实测完全一致（含行首为空格的情况，iter_rows 从
    min_column 起算），故在此按位置取号是安全的。保留读 c.value 的判空语义。
    """
    # max_col/max_row **必须传**：openpyxl 在 read_only 模式下按 worksheet 的
    # <dimension> 扫，而那个尺寸是文件里可控的（不传边界 ⇒ 声明多大就扫多大）。
    for row_no, tup in enumerate(ws.iter_rows(max_col=max_col, max_row=max_row), 1):
        if not tup:
            continue
        cells = {}
        for col_no, c in enumerate(tup, 1):
            if c.value is not None:
                cells[col_no] = c
        if cells:
            yield row_no, cells


def cell_parts(cells, col, span, left_px):
    """一个格子的（属性串, 展示文本）：class / colspan / rowspan / 冻结列的 left 全在这里拼。

    表头行与数据行共用同一个函数 —— 两处各写一遍的话，冻结列或合并单元格迟早只在一边生效
    （这类「表头固定了、左侧列没固定」的半截 bug 在纯 CSS 里非常难看出来）。
    """
    text, num, is_formula = cells.get(col, ("", False, False))
    cls = []
    if num:
        cls.append("n")
    if is_formula:
        cls.append("formula")
    if left_px is not None:
        cls.append("frozen-col")
    attrs = ' class="%s"' % " ".join(cls) if cls else ""
    if span:
        rowspan, colspan = span
        if rowspan > 1:
            attrs += ' rowspan="%d"' % rowspan
        if colspan > 1:
            attrs += ' colspan="%d"' % colspan
    if left_px is not None:
        attrs += ' style="left:%dpx"' % left_px
    return attrs, text


def merge_spans(merges, rownos, cols):
    """合并区域 -> ({(行序, 列): (rowspan, colspan)}, 被它覆盖、不该再出 <td> 的格)。

    行号**可能不连续**（整行皆空的记录在 rows_of 里被跳过），故用 bisect 在「实际渲染到的
    行号」上取连续区间，而不是直接 r2-r1（拿 Excel 行号当数组下标会错位）。
    """
    spans, covered = {}, set()
    for r1, c1, r2, c2 in merges:
        c1, c2 = max(c1, 1), min(c2, cols)
        lo = bisect.bisect_left(rownos, r1)
        hi = bisect.bisect_right(rownos, r2)
        if c1 > c2 or lo >= hi:
            continue
        spans[(lo, c1)] = (hi - lo, c2 - c1 + 1)
        for i in range(lo, hi):
            for c in range(c1, c2 + 1):
                if (i, c) != (lo, c1):
                    covered.add((i, c))
    return spans, covered


def render_sheet(idx, name, state, ws_form, ws_val, meta, out):
    """渲染单个 sheet（返回统计：顶部截断汇总与导航条都读它，不去反查 DOM）。"""
    hidden = state not in (None, "visible")

    # 列上界：用 dimension 的 max_column（Excel 一定写；异常时按实际行宽兜底），受列闸约束。
    declared_cols = ws_form.max_column if isinstance(ws_form.max_column, int) else 0
    cols_cap = min(max(declared_cols, 1), MAX_COLS)
    rows_cap = min(MAX_ROWS, MAX_CELLS // cols_cap)

    # 扫描边界（**解析成本闸**，见 MAX_SCAN_ROWS）：列按列闸钳、行按扫描闸钳。
    # 没有 dimension 时（declared_cols == 0）用列闸当上界。
    scan_cols = max(1, min(declared_cols if declared_cols > 0 else MAX_COLS, MAX_COLS))
    form_rows = rows_of(ws_form, scan_cols, MAX_SCAN_ROWS)
    val_rows = rows_of(ws_val, scan_cols, MAX_SCAN_ROWS)
    v_row_no, v_cells = next(val_rows, (None, None))

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
            # number_format 在 read_only 模式下**是有的**（实测），所以数字格式不必开第二遍
            # 全量工作簿。取公式书那一份：缓存值缺失时也照样按格式渲染。
            fmt = getattr(fcell, "number_format", None)
            # 每个分支都必须经 clean()（转义 + 控制字符 + 换行）—— 单元格内容是外部数据，
            # 少走一次 clean 就是一次注入（实测：直接拼接会让 <script> 真的执行）。
            if v is not None:
                text, num = text_of(v, fmt)
                cells[col] = (clean(text), num, False)
            elif isinstance(f, str) and f.startswith("="):
                # openpyxl 写出的公式没有缓存值 -> 显示公式文本（见文件头取舍 1）
                formula_missing += 1
                cells[col] = (clean(f), False, True)
            elif f is not None:
                text, num = text_of(f, fmt)
                cells[col] = (clean(text), num, False)
            else:
                cells[col] = ("", False, False)
        shown.append((row_no, cells))

    total_rows = row_count
    scan_capped = row_count >= MAX_SCAN_ROWS
    hidden_rows = total_rows - len(shown)
    cols = max(min(max_col_seen, cols_cap), 1) if shown else cols_cap
    wide = declared_cols > MAX_COLS
    stats = {"idx": idx, "name": name, "rows": total_rows, "rows_shown": len(shown),
             "cols": max(declared_cols, max_col_seen),
             "cols_shown": min(max(declared_cols, max_col_seen), MAX_COLS)}

    label = attr_clean(name)
    if hidden:
        label += ' <span class="range">(隐藏)</span>'
    out.append('<section class="sheet" id="sheet-%d" data-sheet="%s">' % (idx + 1, attr_clean(name)))
    out.append('<div class="sheet-head"><h2>%d. %s</h2>' % (idx + 1, label))
    rows_label = ("≥%d" % total_rows) if scan_capped else ("%d" % total_rows)
    out.append('<span class="range">%s 行 × %d 列</span></div>' % (rows_label, max(declared_cols, max_col_seen)))

    # 冻结上限的钳制（见 MAX_FROZEN_*）。注意 head_rows 下面用的是 max(frozen_rows, 1)：
    # 没有冻结窗格时表头行照样进 thead（吸顶），只是整块 thead 不挂 frozen-head。
    frozen_rows = min(meta["frozen_rows"], MAX_FROZEN_ROWS, len(shown))
    frozen_cols = min(meta["frozen_cols"], MAX_FROZEN_COLS, cols)

    notes = []
    if idx == 0:
        notes.append("首行按表头显示（xlsx 没有表头标记，无法可靠推断）")
    if scan_capped:
        notes.append('<span class="warn">只扫描了前 %d 行（文件声明的行数更多）</span>' % MAX_SCAN_ROWS)
    if hidden_rows > 0:
        notes.append('<span class="warn">还有 %d 行未显示（单表上限 %d 行）</span>' % (hidden_rows, rows_cap))
    if wide:
        notes.append('<span class="warn">只显示前 %d 列（共 %d 列）</span>' % (MAX_COLS, declared_cols))
    if formula_missing > 0:
        notes.append("%d 个公式单元格没有缓存值，已显示为公式文本（用 Excel/WPS 打开并另存即可生成缓存值）" % formula_missing)
    if frozen_rows or frozen_cols:
        notes.append("冻结窗格：%d 行 × %d 列（滚动时固定）" % (frozen_rows, frozen_cols))
    if meta["frozen_rows"] > frozen_rows or meta["frozen_cols"] > frozen_cols:
        notes.append('<span class="warn">原文件冻结了 %d 行 × %d 列，预览只保留前 %d 行 × %d 列</span>'
                     % (meta["frozen_rows"], meta["frozen_cols"], frozen_rows, frozen_cols))
    if not shown:
        out.append('<p class="empty">（空表）</p></section>')
        return stats

    # 列宽：Excel 的 width（字符数）→ 像素，钳到 [56, 400]。**每一列都要有宽度** ——
    # table-layout: fixed 下 colgroup 就是最终宽度，冻结列的 left 偏移才算得准。
    widths = [col_px(meta["widths"].get(c) or meta["default_width"]) for c in range(1, cols + 1)]
    lefts, acc = [0] * (cols + 1), 0
    for c in range(1, cols + 1):
        lefts[c] = acc
        acc += widths[c - 1]
    spans, covered = merge_spans(meta["merges"], [row_no for row_no, _ in shown], cols)
    if spans:
        notes.insert(0, "%d 处合并单元格已按 colspan/rowspan 合并" % len(spans))
    if notes:
        out.append('<p class="notes">%s</p>' % " · ".join(notes))

    # <thead>/<tbody> 必须显式写出来（实测）：只写 <tr><th> 时浏览器会把整个表塞进
    # 隐式 tbody，thead th 那套「表头吸顶 + 底色」样式全部不生效（视觉上表头与数据行
    # 长得一样，长表滚下去就不知道哪列是什么）。
    # 冻结 N 行时：这 N 行都进 thead 并挂 frozen-head（整块 sticky），第一行仍是 <th>。
    head_rows = shown[:max(frozen_rows, 1)]
    out.append('<div class="table-wrap"><table class="grid">')
    out.append("<colgroup>%s</colgroup>" % "".join('<col style="width:%dpx">' % w for w in widths))
    out.append('<thead class="frozen-head">' if frozen_rows else "<thead>")
    for i, (_row_no, cells) in enumerate(head_rows):
        tag = "th" if i == 0 else "td"
        out.append('<tr class="frozen-row">' if frozen_rows else "<tr>")
        for col in range(1, cols + 1):
            if (i, col) in covered:
                continue
            attrs, text = cell_parts(cells, col, spans.get((i, col)),
                                     lefts[col] if col <= frozen_cols else None)
            out.append("<%s%s>%s</%s>" % (tag, attrs, text, tag))
        out.append("</tr>")
    out.append("</thead><tbody>")
    for i in range(len(head_rows), len(shown)):
        cells = shown[i][1]
        out.append("<tr>")
        for col in range(1, cols + 1):
            if (i, col) in covered:
                continue
            attrs, text = cell_parts(cells, col, spans.get((i, col)),
                                     lefts[col] if col <= frozen_cols else None)
            out.append("<td%s>%s</td>" % (attrs, text))
        out.append("</tr>")
    out.append("</tbody></table></div></section>")
    return stats


def sheet_label(st):
    """导航/标题里的显示名：序号 + 名字（+ 重名后缀）。

    序号已经让锚点唯一（sheet-1..N），重名后缀是为了「人眼能分辨」：crafted 文件里可以有
    两个都叫「汇总」的 sheet，光靠序号 12/13 看不出它们本来同名。超长名交给 CSS ellipsis，
    全名留在 title 属性里。
    """
    label = "%d. %s" % (st["idx"] + 1, st["name"])
    if st["dup_total"] > 1:
        label += "（第 %d/%d 个同名）" % (st["dup_ord"], st["dup_total"])
    return label


def trunc_block(stats, sheet_total, sheet_shown):
    """顶部「内容已截断」汇总条：大表被截断时**必须在最上面就说清楚**。

    为什么不能只留每个 sheet 自己的说明条：20 个 sheet 的页面里，第 3 个 sheet 的
    「还有 1501 行未显示」在首屏根本看不见，用户会以为预览把数据吃了。这条横幅是「如实
    告知」的入口，逐 sheet 的说明条是明细（两者都要，少一个都是不诚实）。
    """
    rows_cut = [s for s in stats if s["rows_shown"] < s["rows"]]
    cols_cut = [s for s in stats if s["cols_shown"] < s["cols"]]
    if not rows_cut and not cols_cut and sheet_total <= sheet_shown:
        return ""
    items = []
    for s in rows_cut + cols_cut:
        if len(items) >= 5:
            break
        parts = []
        if s["rows_shown"] < s["rows"]:
            parts.append("已显示 %d/%d 行" % (s["rows_shown"], s["rows"]))
        if s["cols_shown"] < s["cols"]:
            parts.append("只显示前 %d/%d 列" % (s["cols_shown"], s["cols"]))
        items.append("<li>第 %d 个工作表「%s」：%s</li>"
                     % (s["idx"] + 1, attr_clean(short_name(s["name"])), " · ".join(parts)))
    if sheet_total > sheet_shown:
        items.append("<li>只显示前 %d/%d 个工作表</li>" % (sheet_shown, sheet_total))
    more = len(rows_cut) + len(cols_cut) - 5
    extra = '<p class="meta">另有 %d 个工作表也被截断（见各自的说明条）</p>' % more if more > 0 else ""
    return ('<div class="trunc"><b>内容已截断（预览有上限，不是文件损坏）</b><ul>%s</ul>%s</div>'
            % ("".join(items), extra))


def short_name(name):
    return name if len(name) <= 24 else name[:24] + "…"


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
    seen = {}
    for name in names:
        seen[name] = seen.get(name, 0) + 1

    # sheet 元数据（冻结/合并/列宽）要另开一份 zip 扫 sheet XML（原因见脚本头取舍 7）。
    zf = zipfile.ZipFile(src)
    paths = sheet_paths(zf)

    chunks, stats = [], []
    for i, name in enumerate(names):
        if len(stats) >= MAX_SHEETS:
            break
        meta = parse_meta(zf, paths[i]) if i < len(paths) else empty_meta()
        st = render_sheet(len(stats), name, getattr(wb_form[name], "sheet_state", "visible"),
                          wb_form[name], wb_val[name], meta, chunks)
        st["dup_total"] = seen.get(name, 1)
        st["dup_ord"] = 1 + sum(1 for prev in stats if prev["name"] == name)
        stats.append(st)

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
    out.append(trunc_block(stats, len(names), len(stats)))
    if len(stats) > 1:
        links = []
        for st in stats:
            links.append('<a href="#sheet-%d" title="%s">%s</a>'
                         % (st["idx"] + 1, attr_clean(st["name"]),
                            attr_clean(sheet_label(st))))
        out.append('<nav class="sheet-nav" aria-label="工作表导航"><span class="nav-label">工作表</span>%s</nav>'
                   % "".join(links))
    out.extend(chunks)
    if len(names) > MAX_SHEETS:
        out.append('<p class="notes"><span class="warn">还有 %d 个工作表未显示（上限 %d）</span></p>'
                   % (len(names) - MAX_SHEETS, MAX_SHEETS))
    out.append("</body></html>")
    with open(dst, "w", encoding="utf-8") as fh:
        fh.write("\n".join(out))
    print("GOCODE_XLSX_OK", dst)


main()
`
