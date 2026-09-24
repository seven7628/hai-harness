package builtin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/sandbox"
)

// PptxPreviewer 把 .pptx/.pptm 幻灯片转成**近似版式的 HTML**（应用内预览专用）。
//
// 为什么是「近似重建」而不是「渲染」（产品决策，务必先读）：
//   - python-pptx 只读写 OOXML，**不能渲染**；本机也没有 LibreOffice（实测 /Applications
//     与 PATH 都没有 soffice），故「pptx → PDF → 内嵌 viewer」这条最保真的路不可行；
//   - 前端渲染要引新依赖（本项目「不加新 npm 依赖」是硬约束）；
//   - 剩下的真实信息是：页面尺寸、每个形状的 EMU 几何、文本与字号/粗斜体/颜色、图片字节。
//     拿这些做绝对定位的 HTML 重建，能给出**可用的版式感**（一张稿子长什么样、有哪些页、
//     每页的文字与大致排布），但不是像素级还原。
//
// 因此本转换器把「不还原什么」写在页面最上方（无字体保真/无动画/SmartArt 与图表不渲染），
// 不支持的形状在原位给**可见占位**而不是静默丢弃 —— 诚实比假装像更重要：用户看到
// 「［图表未渲染］」会去用 PowerPoint 打开，看到一片空白只会以为预览坏了。
//
// 归属与线程安全同 XlsxPreviewer/DocxPreviewer：不是 tools.Tool（不接受模型输入）、
// 不放 runtime 包、Convert 可并发调用（每次独立临时目录，清理只按年龄）。
type PptxPreviewer struct {
	py     PythonRuntime // 受管运行时（nil = 未装配 → 明确报错，不 panic）
	sbxFor SandboxFor    // 沙箱工厂（nil = 恒用 NoSandbox 直通）
}

// NewPptxPreviewer 创建演示文稿预览转换器。
// py = 受管运行时（与 run_python 同一个实例）；sbxFor = 按工作区构造沙箱（nil = NoSandbox）。
func NewPptxPreviewer(py PythonRuntime, sbxFor SandboxFor) *PptxPreviewer {
	return &PptxPreviewer{py: py, sbxFor: sbxFor}
}

// PptxPreviewTimeout 单次转换时限（含 python-pptx 解析 + 重建）。与另两个转换器同口径：
// 60s 是「明显不正常就报错」的阈值，不是性能目标；首次调用另需运行时 bootstrap（由 Ensure 管）。
const PptxPreviewTimeout = 60 * time.Second

// PptxConvertResult 转换产物。
type PptxConvertResult struct {
	HTMLPath string // 生成的 HTML 绝对路径（临时目录内；调用方按需保留/清理）
	Dir      string // 临时目录（同上）
}

// Convert 把 pptxPath（绝对路径）转换为 HTML，返回产物路径。
//
// workspace / 产物位置 / 清理策略与 XlsxPreviewer.Convert 逐字同义（见其长注释）：
// 沙箱根 = 工作区（脚本与产物都在受管临时目录，cwd 也在那里 —— 那是策略一定放行的位置），
// 失败路径立即清理本次目录，成功路径保留产物（浏览器还要读），残留由下一次转换按年龄回收。
func (p *PptxPreviewer) Convert(ctx context.Context, pptxPath, workspace string) (PptxConvertResult, error) {
	if p.py == nil {
		return PptxConvertResult{}, fmt.Errorf("no managed Python runtime is configured")
	}
	py, err := p.py.Ensure(ctx)
	if err != nil {
		return PptxConvertResult{}, fmt.Errorf("managed Python runtime unavailable: %w", err)
	}
	scriptPath, cleanup, err := writeTempFile(py, "pptx-preview-", "convert_pptx.py", []byte(pptxToHTMLScript))
	if err != nil {
		return PptxConvertResult{}, err
	}
	dir := filepath.Dir(scriptPath)
	done := false
	defer func() {
		if !done {
			cleanup()
		}
	}()
	htmlPath := filepath.Join(dir, "preview.html")

	// 固定解释器 + 三个路径参数（单引号转义：路径可含空格/引号/中文）。命令行里没有任何来自
	// 模型/前端的自由文本，故不存在「把用户输入拼进 shell」的面（同另两条预览通路）。
	cmdline := shQuote(py.VenvPython) + " " + shQuote(scriptPath) + " " + shQuote(pptxPath) + " " + shQuote(htmlPath)
	var sbx sandbox.Sandbox = sandbox.NoSandbox{}
	if p.sbxFor != nil {
		if s := p.sbxFor(workspace); s != nil {
			sbx = s
		}
	}
	out, err := sbx.Run(ctx, sandbox.ExecSpec{Command: cmdline, Cwd: dir, Timeout: PptxPreviewTimeout})
	if err != nil {
		return PptxConvertResult{}, fmt.Errorf("执行转换脚本失败: %w", err)
	}
	if !strings.Contains(out, pptxOKMarker) {
		return PptxConvertResult{}, fmt.Errorf("转换失败: %s", singleLine(out))
	}
	if _, err := os.Stat(htmlPath); err != nil {
		return PptxConvertResult{}, fmt.Errorf("转换脚本未产出 HTML: %w", err)
	}
	// 清扫三种办公预览的过期残留，keep 当前产物（见 docx_preview.go 的 sweepStaleOfficeTempDirs）。
	sweepStaleOfficeTempDirs(py, dir)
	done = true
	return PptxConvertResult{HTMLPath: htmlPath, Dir: dir}, nil
}

// pptxOKMarker 转换脚本成功时输出的标记行（Go 侧以此判定成败：非零退出/沙箱拦截/
// python-pptx 抛错都会让这一行不出现，故统一按「输出里没有标记」判失败）。
const pptxOKMarker = "GOCODE_PPTX_OK"

// pptxToHTMLScript 固定的转换脚本（**不是**模型可改写的输入：编译期常量）。
//
// 关键取舍（改脚本前先读）：
//
//  1. **文本样式是继承链，必须走完**：默认模板里标题的 44pt/居中、正文的项目符号与 32pt
//     全在**母版 txStyles** 里；只读形状自身属性会得到一页无层次的裸文本（实测对比明显）。
//     优先级：run > 段落 pPr > 形状 lstStyle > 布局占位符 > 母版占位符 > txStyles > 兜底。
//
//  2. **几何全部来自 EMU，按 96dpi 换 px**（EMU ÷ 914400 × 96）：页面尺寸与每个形状的
//     left/top/width/height 都是文件里的真实数值，故「页内相对位置」是可信的。
//
//  3. **组合形状要做坐标换算**：实测子形状的 left/top 是组合**子坐标系**下的值（可能是负数
//     或超出页面的值），直接用会让组内元素飞到页面外；按 chOff/chExt → off/ext 做仿射。
//
//  4. **项目符号与缩进的语义**：文本从 marL 开始，符号画在 marL+indent（indent 为负）。
//     实测踩过对调版本 —— 符号跑进文本里、文字反而贴左边。
//
//  5. **SUBTTITLE 不等于 TITLE**：占位符类型字符串 'SUBTITLE' 里含 'TITLE'，用子串匹配会把
//     副标题判成标题（拿到 44pt 标题字号，正文的 1.4 倍），必须比对去掉枚举后缀的整词。
//
//  6. **主题配色走 clrMap**：深色主题靠改写 clrMap（bg1→dk1）实现，只读主题槽位会把底色与
//     文字色搞反；另外 dk1/lt1 在默认主题里是 sysClr（只有 lastClr），漏掉会静默丢色。
//
//  7. **诚实标注 + 可见占位**：顶部说明做不到的四件事；不支持的形状（线条/SmartArt/图表/
//     嵌入对象）在原位给「［…未渲染］」。上限（60 页 / 每页 200 形状 / 图片 24MB）超出时
//     页面如实标注，不静默丢弃。
//
//  8. **转义**：文本/备注/文件名一律经 clean()；文件内容是外部数据，<script> 直接拼接就是注入。
//
//  9. **charset 与自带样式**：<meta charset="utf-8"> 必在内（缺了中文 mojibake）；
//     内嵌浏览器不继承应用样式，故模板内含完整样式（幻灯片底色由文件决定，保持原色不跟随
//     暗色主题 —— 那是稿子的一部分，不是我们的 UI）。
const pptxToHTMLScript = `# -*- coding: utf-8 -*-
"""pptx -> 近似版式 HTML（开发用副本；最终以 tools/builtin/pptx_preview.go 里的常量为准）。

用法: python convert_pptx.py <pptx 绝对路径> <输出 html 绝对路径>
成功时 stdout 末行打印 GOCODE_PPTX_OK；失败时非零退出 + 单行中文原因（预期内的用户错误）
或 traceback（我们的 bug，需要完整堆栈定位）。
"""
import base64
import html
import os
import re
import sys

from lxml import etree
from pptx import Presentation
from pptx.enum.dml import MSO_COLOR_TYPE, MSO_FILL
from pptx.enum.shapes import MSO_SHAPE, MSO_SHAPE_TYPE
from pptx.exc import PackageNotFoundError

A = "{http://schemas.openxmlformats.org/drawingml/2006/main}"
P = "{http://schemas.openxmlformats.org/presentationml/2006/main}"
RT_THEME = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/theme"

MAX_SLIDES = 60
MAX_SHAPES_PER_SLIDE = 200
MAX_IMAGE_BYTES = 24 * 1024 * 1024
MAX_NOTES_CHARS = 4000

EMU_PER_INCH = 914400.0
PX_PER_INCH = 96.0

PRST_CSS = {"ellipse": "border-radius:50%", "roundRect": "border-radius:8px"}

AUTO_NUM_LABELS = {"arabicPeriod": "1.", "romanUcPeriod": "I.", "romanLcPeriod": "i.",
                   "alphaUcPeriod": "A.", "alphaLcPeriod": "a.", "arabicParenR": "1)",
                   "circleNumDbPlain": "①"}

STYLE = """
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body {
  margin: 0; padding: 18px 20px 48px;
  font: 13px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
        "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
  color: #1c1c1e; background: #f2f3f5;
}
h1 { font-size: 15px; font-weight: 600; margin: 0 0 4px; }
.bar { border: 1px solid #e5e7eb; border-radius: 8px; padding: 10px 12px; background: #ffffff; margin: 0 0 16px; }
.meta, .notes, .topnotes { color: #6b7280; font-size: 12px; margin: 4px 0 0; }
.notes .warn { color: #b45309; }
.deck { display: flex; flex-direction: column; align-items: stretch; gap: 18px; }
.slide-wrap { display: flex; flex-direction: column; gap: 4px; }
.slide-label { color: #6b7280; font-size: 12px; }
.slide-scroll { max-width: 100%; overflow: auto; border: 1px solid #d1d5db; border-radius: 4px; background: #ffffff; }
.slide { position: relative; overflow: visible; }
.slide .sh { position: absolute; overflow: visible; }
.slide .sh img { width: 100%%; height: 100%%; }
.slide .sh p { margin: 0 0 2px; }
.slide .sh p:last-child { margin-bottom: 0; }
.slide .sh span { white-space: pre-wrap; }
.slide .sh .bu { position: absolute; }
.slide table { border-collapse: collapse; width: 100%%; height: 100%%; table-layout: fixed; }
.slide td, .slide th { border: 1px solid #9ca3af; padding: 3px 6px; vertical-align: top; overflow-wrap: anywhere; }
.slide th { background: #eef1f5; font-weight: 600; text-align: left; }
.slide .plain { border: 1px dashed #b45309; color: #b45309; font-size: 12px; padding: 4px 6px; overflow: hidden; }
.slide .notes-box { position: absolute; left: 4px; bottom: 0; right: 4px; font-size: 11px; color: #6b7280; }
.slide .notes-box summary { cursor: pointer; }
.slide .notes-box pre { white-space: pre-wrap; margin: 4px 0 0; }
@media (prefers-color-scheme: dark) {
  body { color: #e5e7eb; background: #1c1c1e; }
  .bar { background: #232325; border-color: #3a3a3c; }
  .meta, .notes, .topnotes, .slide-label, .slide .notes-box { color: #9ca3af; }
  .slide-scroll { border-color: #3a3a3c; }
}
"""


def clean(text):
    """展示文本 -> 可安全嵌入 HTML 的片段（删控制字符 + html.escape）。

    换行不转 <br>：pptx 里段落边界就是换行（每段一个 <p>），run 内软换行交给 CSS 的
    white-space: pre-wrap 显示（与 xlsx 那条路不同 —— 那边的单元格换行只能靠 <br>）。
    """
    s = "".join(ch for ch in str(text) if ch == "\n" or ch == "\t" or ord(ch) >= 0x20)
    return html.escape(s, quote=True)


def px(emu):
    return (emu or 0) / EMU_PER_INCH * PX_PER_INCH


def emu_px(emu):
    return "%.1fpx" % px(emu)


# ---------------- 主题配色 ----------------

THEME_KEY_ALIASES = {
    "text1": "tx1", "text2": "tx2", "background1": "bg1", "background2": "bg2",
    "dark1": "dk1", "dark2": "dk2", "light1": "lt1", "light2": "lt2",
    "hyperlink": "hlink", "followedhyperlink": "folhlink",
}


def theme_palette(master):
    """master 的「别名 -> #RRGGBB」映射（clrMap 把 tx1/bg1/... 指向主题里的实际槽位）。

    为什么走 clrMap 而不是直接读主题的 dk1/lt1：PowerPoint 允许配色映射被改写（深色主题
    把 bg1 映到 dk1 是常见做法），只读主题槽位会把这类稿子的底色与文字色搞反。
    """
    mapped = {}
    try:
        root = etree.fromstring(master.part.part_related_by(RT_THEME).blob)
    except Exception:
        return mapped
    scheme = root.find(A + "themeElements/" + A + "clrScheme")
    if scheme is None:
        return mapped
    slots = {}
    for c in scheme:
        v = c[0] if len(c) else None
        if v is None:
            continue
        # sysClr 只有 lastClr（系统色）：实测默认主题的 dk1/lt1 就是 sysClr，漏掉它会得到
        # 空值 —— 表现为底色/文字色静默丢失（比报错更难发现）。
        if v.tag.endswith("sysClr"):
            val = v.get("lastClr") or v.get("val")
        else:
            val = v.get("val") or v.get("lastClr")
        if val:
            slots[c.tag.split("}")[1]] = "#" + val.upper()
    cm = master._element.find(P + "clrMap")
    for key, target in (dict(cm.attrib) if cm is not None else {}).items():
        if target in slots:
            mapped[key] = slots[target]
    for k, v in slots.items():
        mapped.setdefault(k, v)
    for alias, real in THEME_KEY_ALIASES.items():
        if real in mapped:
            mapped.setdefault(alias, mapped[real])
    return mapped


def color_of(color, palette):
    """python-pptx 的颜色对象 -> #RRGGBB（解析不出返回 None，让调用方走默认）。"""
    try:
        ctype = color.type
    except Exception:
        return None
    if ctype is None:
        return None
    if ctype == MSO_COLOR_TYPE.RGB:
        try:
            return "#" + str(color.rgb).upper()
        except Exception:
            return None
    if ctype == MSO_COLOR_TYPE.SCHEME:
        try:
            name = str(color.theme_color).split(" ")[0].split(".")[-1].lower()
        except Exception:
            return None
        return palette.get(name)
    return None


def theme_fill_ref(shape, palette):
    """形状的 p:style/a:fillRef（主题填充样式）-> 近似色。

    为什么要近似而不是留空：用 python-pptx 的 add_shape 造出来的形状**没有显式填充**，
    颜色来自 fillRef（实测：默认矩形 = accent1 的 idx=3 渐变）。留空就是白色方块，
    整页看着像没渲染；用 fillRef 的 schemeClr 上色后至少颜色是对的（渐变/明暗差异不计）。
    """
    try:
        ref = shape._element.find(".//" + P + "style/" + A + "fillRef")
    except Exception:
        return None, False
    if ref is None:
        return None, False
    if (ref.get("idx") or "0") == "0":
        return None, False          # idx=0 = 无填充（明确语义，别自作主张上色）
    if not len(ref):
        return None, True
    v = ref[0]
    if v.tag == A + "srgbClr" and v.get("val"):
        return "#" + v.get("val").upper(), True
    if v.tag == A + "schemeClr" and v.get("val"):
        return palette.get(v.get("val")), True
    return None, True


def solid_fill_color(fill, palette):
    """显式 solidFill 的颜色；未显式设置返回 (None, False)。"""
    try:
        if fill.type != MSO_FILL.SOLID:
            return None
    except Exception:
        return None
    try:
        return color_of(fill.fore_color, palette)
    except Exception:
        return None


# ---------------- 文本样式继承链 ----------------
#
# OOXML 的文本属性是**多级继承**（ECMA-376：DrawingML 文本样式）。实测把这条链走完与
# 走不完的差别非常大：默认模板里标题的 44pt/居中、正文的项目符号与 32pt 全都在
# **母版 txStyles** 里，只读形状自己的属性会得到一页毫无层次的裸文本（字号全默认、无项目符号）。
# 故这里按优先级列出全部来源，每个属性独立取「第一个有值的」：
#   run > 段落 pPr/defRPr > 形状 lstStyle > 布局占位符 lstStyle > 母版占位符 lstStyle
#   > 母版 txStyles（titleStyle/bodyStyle/otherStyle）> 兜底默认
def _rpr_size_pt(rpr):
    sz = rpr.get("sz") if rpr is not None else None
    if not sz:
        return None
    try:
        return int(sz) / 100.0
    except ValueError:
        return None


def _rpr_bold(rpr):
    if rpr is None:
        return None
    b = rpr.get("b")
    return None if b is None else b in ("1", "true")


def _rpr_italic(rpr):
    if rpr is None:
        return None
    i = rpr.get("i")
    return None if i is None else i in ("1", "true")


def _auto_num_label(node):
    return AUTO_NUM_LABELS.get(node.get("type") or "arabicPeriod", "1.")


def _bullet_of(lvl):
    if lvl.find(A + "buNone") is not None:
        return ""
    if lvl.find(A + "buChar") is not None:
        return lvl.find(A + "buChar").get("char") or ""
    if lvl.find(A + "buAutoNum") is not None:
        return _auto_num_label(lvl.find(A + "buAutoNum"))
    return None


def _int_or_none(v):
    if v is None:
        return None
    try:
        return int(v)
    except ValueError:
        return None


def levels_of(node):
    """任意「含 lvlNpPr 子元素」的节点（a:lstStyle / p:titleStyle / p:bodyStyle / p:otherStyle）。

    返回按级号索引的列表；同时把**未显式给 algn** 的级补上 OOXML 的默认值（段落级默认左对齐，
    但 txStyles 的 titleStyle 首级默认居中）—— 少这一步会让标题跑到左边（实测差异明显）。
    """
    out = []
    if node is None:
        return out
    for lvl in node:
        if not lvl.tag.startswith(A + "lvl") and not lvl.tag.startswith(P + "lvl"):
            continue
        d = lvl.find(A + "defRPr")
        out.append({
            "sz": _rpr_size_pt(d), "b": _rpr_bold(d), "i": _rpr_italic(d),
            "algn": lvl.get("algn"), "marL": _int_or_none(lvl.get("marL")),
            "indent": _int_or_none(lvl.get("indent")), "bullet": _bullet_of(lvl),
        })
    return out


def para_sources(shape, ph_idx, layout, master, slide_title, is_body_ph):
    """按优先级返回「来源列表」（每项 = 按级号索引的列表）。"""
    srcs = []
    srcs.append(levels_of(shape._element.find(".//" + P + "txBody/" + A + "lstStyle")))
    if ph_idx is not None:
        lay_ph = find_ph(layout.placeholders, ph_idx)
        mas_ph = find_ph(master.placeholders, ph_idx)
        if lay_ph is not None:
            srcs.append(levels_of(lay_ph._element.find(".//" + P + "txBody/" + A + "lstStyle")))
        if mas_ph is not None:
            srcs.append(levels_of(mas_ph._element.find(".//" + P + "txBody/" + A + "lstStyle")))
    txs = master._element.find(P + "txStyles")
    if txs is not None:
        key = None
        if ph_idx is not None:
            key = "titleStyle" if slide_title else "bodyStyle"
        elif getattr(shape, "has_text_frame", False) and shape.shape_type != MSO_SHAPE_TYPE.TEXT_BOX:
            key = "otherStyle"
        if key is not None:
            srcs.append(levels_of(txs.find(P + key)))
        if ph_idx is None and not is_body_ph:
            # 文本框（TEXT_BOX）没有占位符语义：PowerPoint 用 otherStyle（实测默认 18pt）。
            srcs.append(levels_of(txs.find(P + "otherStyle")))
    return [s for s in srcs if s]


def pick(level, srcs, key):
    for src in srcs:
        if level < len(src) and src[level].get(key) is not None:
            return src[level][key]
    return None


# ---------------- 形状几何 ----------------

def placeholder_index(shape):
    try:
        if shape.is_placeholder:
            return shape.placeholder_format.idx
    except Exception:
        return None
    return None


def find_ph(src, idx):
    for sh in src:
        if placeholder_index(sh) == idx:
            return sh
    return None


def is_title_ph_type(type_str):
    """只有 TITLE/CENTER_TITLE 算标题。

    用子串匹配会把 SUBTITLE 也判成标题（'SUBTITLE' 里含 'TITLE'）：实测副标题因此拿到
    44pt 标题字号（正文的 1.4 倍），整页版式看着是坏的。故必须比对去掉枚举值后缀的整词。
    """
    inner = type_str.split("(")[0].strip()
    return inner in ("TITLE", "CENTER_TITLE", "VERTICAL_TITLE", "VERTICAL_CENTER_TITLE")


def is_title_like(shape, ph_idx, layout, master):
    if ph_idx is not None:
        for src in (layout.placeholders, master.placeholders):
            ph = find_ph(src, ph_idx)
            if ph is not None:
                try:
                    if is_title_ph_type(str(ph.placeholder_format.type)):
                        return True
                except Exception:
                    pass
        # 占位符但在布局/母版里找不到对应项：idx=0 是标题的约定俗成（各版式一致）。
        return ph_idx == 0
    # 非占位符：只有名字是 "Title ..." 的才算（**不能**用 "in"，见 is_title_ph_type 的理由）。
    name = (getattr(shape, "name", "") or "")
    return name == "Title" or name.startswith("Title ")


# ---------------- 渲染 ----------------

ALIGN_MAP = {"ctr": "center", "r": "right", "just": "justify", "l": "left", "dist": "justify"}
ALIGN_DEFAULTS = {"titleStyle": "center"}


def render_paragraphs(text_frame, srcs, palette, default_sz, default_align):
    """文本帧 -> HTML 段落（run 级粗/斜/下划线/删除线/颜色/字号 + 段落对齐/缩进/项目符号）。"""
    out = []
    for para in text_frame.paragraphs:
        lvl = para.level or 0
        spans = []
        for run in para.runs:
            text = clean(run.text)
            if not text:
                continue
            ppr = para._p.find(A + "pPr")
            d_rpr = ppr.find(A + "defRPr") if ppr is not None else None
            css = []
            size = None
            if run.font.size is not None:
                size = run.font.size.pt
            if size is None:
                size = _rpr_size_pt(d_rpr)
            if size is None:
                size = pick(lvl, srcs, "sz")
            if size is None:
                size = default_sz
            if size:
                css.append("font-size:%.1fpt" % size)
            bold = run.font.bold
            if bold is None:
                bold = _rpr_bold(d_rpr)
            if bold is None:
                bold = pick(lvl, srcs, "b")
            if bold:
                css.append("font-weight:600")
            italic = run.font.italic
            if italic is None:
                italic = _rpr_italic(d_rpr)
            if italic is None:
                italic = pick(lvl, srcs, "i")
            if italic:
                css.append("font-style:italic")
            deco = []
            if run.font.underline:
                deco.append("underline")
            if _run_strike(run):
                deco.append("line-through")
            if deco:
                css.append("text-decoration:%s" % " ".join(deco))
            col = color_of(run.font.color, palette)
            if col:
                css.append("color:%s" % col)
            style = (' style="%s"' % ";".join(css)) if css else ""
            href = run_href(run)
            if href:
                spans.append('<a href="%s"%s rel="noreferrer noopener">%s</a>' % (clean(href), style, text))
            else:
                spans.append("<span%s>%s</span>" % (style, text))
        if not spans:
            spans.append("<span></span>")

        ppr = para._p.find(A + "pPr")
        p_css = []
        algn = None
        if para.alignment is not None:
            algn = ALIGN_MAP.get(str(para.alignment).split(" ")[0].lower())
        if algn is None:
            raw = ppr.get("algn") if ppr is not None else None
            algn = ALIGN_MAP.get(raw) if raw else None
        if algn is None:
            raw = pick(lvl, srcs, "algn")
            algn = ALIGN_MAP.get(str(raw).lower()) if raw else None
        if algn is None:
            algn = default_align
        if algn and algn != "left":
            p_css.append("text-align:%s" % algn)

        # marL/indent 是 EMU：缩进 = marL + indent（indent 常为负），项目符号挂在缩进起点 ——
        # 这是 OOXML 里项目符号的真实排布方式（marL 是文本起点，符号画在 marL+indent）。
        marl = _int_or_none(ppr.get("marL")) if ppr is not None else None
        indent = _int_or_none(ppr.get("indent")) if ppr is not None else None
        if marl is None:
            marl = pick(lvl, srcs, "marL")
        if indent is None:
            indent = pick(lvl, srcs, "indent")
        # OOXML 的缩进语义：**文本**从 marL 开始，**项目符号**画在 marL + indent（indent
        # 为负，故符号在文字左边）。实测踩过：把两者对调后符号跑进文本里、文字反而贴左边。
        if marl:
            p_css.append("padding-left:%.1fpx" % px(marl))
        bullet = None
        if ppr is not None:
            bullet = _bullet_of(ppr) if (ppr.find(A + "buNone") is not None
                                         or ppr.find(A + "buChar") is not None
                                         or ppr.find(A + "buAutoNum") is not None) else None
        if bullet is None:
            bullet = pick(lvl, srcs, "bullet")
        prefix = ""
        if bullet:
            at = ' style="left:%.1fpx"' % px((marl or 0) + (indent or 0))
            prefix = '<span class="bu"%s>%s</span>' % (at, clean(bullet))
        out.append('<p%s>%s%s</p>' % ((' style="%s"' % ";".join(p_css)) if p_css else "", prefix, "".join(spans)))
    return "".join(out)


def _run_strike(run):
    """run 是否带删除线（a:rPr/@strike；python-pptx 没暴露这个属性）。"""
    rpr = run._r.find(A + "rPr")
    return rpr is not None and rpr.get("strike") in ("sngStrike", "dblStrike")


def run_href(run):
    try:
        addr = run.hyperlink.address
    except Exception:
        return None
    return addr or None


def render_table(shape, palette):
    tbl = shape.table
    n_rows, n_cols = len(tbl.rows), len(tbl.columns)
    first_row_header = False
    try:
        pr = tbl._tbl.find(A + "tblPr")
        first_row_header = pr is not None and pr.get("firstRow") in ("1", "true")
    except Exception:
        pass
    out = ["<table>"]
    for ri in range(n_rows):
        out.append("<tr>")
        for ci in range(n_cols):
            cell = tbl.cell(ri, ci)
            # 合并单元格：被并入的格不输出，合并起点给 colspan/rowspan —— 否则一行的 <td>
            # 数与表格列数不一致，浏览器布局直接错位。
            if cell.is_spanned:
                continue
            tag = "th" if (first_row_header and ri == 0) else "td"
            span = ' colspan="%d" rowspan="%d"' % (cell.span_width, cell.span_height) if cell.is_merge_origin else ""
            body = render_paragraphs(cell.text_frame, [], palette, 12.0, "left")
            out.append("<%s%s>%s</%s>" % (tag, span, body, tag))
        out.append("</tr>")
    out.append("</table>")
    return "".join(out)


SHAPE_KIND_LABELS = {
    "LINE": "线条", "AUTO_SHAPE": "自选图形", "FREEFORM": "任意多边形",
    "MEDIA": "音视频（不播放）", "DIAGRAM": "SmartArt 图示", "IGX_GRAPHIC": "SmartArt 图示",
    "CANVAS": "绘图画布", "LINKED_OLE_OBJECT": "嵌入对象（OLE）",
    "EMBEDDED_OLE_OBJECT": "嵌入对象（OLE）", "OLE_CONTROL_OBJECT": "OLE 控件",
    "WEB_VIDEO": "网页视频", "COMMENT": "批注", "INK": "墨迹", "INK_COMMENT": "墨迹批注",
    "TEXT_EFFECT": "文字效果", "SCRIPT_ANCHOR": "脚本锚点", "CHART": "图表",
}


def shape_kind_label(shape):
    try:
        stype = shape.shape_type
    except Exception:
        return "该形状无法识别"
    if stype is None:
        # python-pptx 对未识别的 graphicFrame（实测：SmartArt 的 dgm:relIds）返回 None。
        return "SmartArt/图示（该类形状不支持）"
    name = str(stype).split(" ")[0].split(".")[-1]
    return SHAPE_KIND_LABELS.get(name, "该形状不支持（%s）" % name)


def shape_box(l, t, w, h, extra=""):
    return "left:%s;top:%s;width:%s;height:%s;%s" % (emu_px(l), emu_px(t), emu_px(w), emu_px(h), extra)


def render_chart_label(shape):
    try:
        ch = shape.chart
        names = ", ".join(str(s.name) for s in ch.series if s.name)[:80]
        head = str(ch.chart_type).split(" ")[0].split(".")[-1]
        return "图表（%s，%d 个系列%s）未渲染" % (head, len(ch.series), "：" + names if names else "")
    except Exception:
        return "图表未渲染"


class Box(object):
    """页面坐标系下的矩形（组合形状的换算结果）。"""

    __slots__ = ("l", "t", "w", "h")

    def __init__(self, l, t, w, h):
        self.l, self.t, self.w, self.h = float(l or 0), float(t or 0), float(w or 0), float(h or 0)

    def child(self, shape, group_xfrm):
        """把「组合子坐标系」里的形状换算到页面坐标系。

        实测：子形状的 left/top 是**组合自己的子坐标系**下的值（可能是负数或超大），
        直接用会让组内元素飞到页面外。换算按 OOXML 的 chOff/chExt → off/ext 仿射：
            page = box + (child - chOff) * (box / chExt)
        """
        if group_xfrm is None:
            return Box(shape.left, shape.top, shape.width, shape.height)
        ch_ext = group_xfrm.chExt
        if ch_ext is None or not ch_ext.cx or not ch_ext.cy:
            return Box(shape.left, shape.top, shape.width, shape.height)
        sx = self.w / float(ch_ext.cx)
        sy = self.h / float(ch_ext.cy)
        bx = self.l - float(group_xfrm.chOff.x or 0) * sx
        by = self.t - float(group_xfrm.chOff.y or 0) * sy
        return Box(bx + float(shape.left or 0) * sx, by + float(shape.top or 0) * sy,
                   float(shape.width or 0) * sx, float(shape.height or 0) * sy)


def render_shape(shape, slide_info, box, stats, budget, depth=0):
    """单个形状 -> HTML。

    不支持的类型给**可见**占位而不是静默丢弃：用户对照原稿时能立刻看出「这里少了一个
    智能图形」，而不是以为是自己记错了。
    """
    rot = ""
    try:
        if shape.rotation:
            rot = "transform:rotate(%.1fdeg);" % shape.rotation
    except Exception:
        pass

    if shape.shape_type is not None and shape.has_text_frame and shape.text_frame.text.strip():
        html_ = render_text_shape(shape, slide_info, box, rot, stats)
        if html_ is not None:
            return html_

    if shape.shape_type == MSO_SHAPE_TYPE.PICTURE:
        try:
            blob = shape.image.blob
        except Exception:
            stats["other"] += 1
            return '<div class="sh plain" style="%s">［图片无法读取］</div>' % shape_box(box.l, box.t, box.w, box.h, rot)
        if budget["bytes"] + len(blob) > MAX_IMAGE_BYTES:
            stats["img_skipped"] += 1
            return ('<div class="sh plain" style="%s">［图片已省略：超出预览预算］</div>'
                    % shape_box(box.l, box.t, box.w, box.h, rot))
        budget["bytes"] += len(blob)
        data = base64.b64encode(blob).decode("ascii")
        return ('<div class="sh" style="%s"><img src="data:%s;base64,%s" alt=""/></div>'
                % (shape_box(box.l, box.t, box.w, box.h, rot), clean(shape.image.content_type), data))

    if getattr(shape, "has_table", False):
        return '<div class="sh" style="%s">%s</div>' % (
            shape_box(box.l, box.t, box.w, box.h, rot), render_table(shape, slide_info["palette"]))

    if getattr(shape, "has_chart", False):
        stats["charts"] += 1
        return ('<div class="sh plain" style="%s">［%s］</div>'
                % (shape_box(box.l, box.t, box.w, box.h, rot), clean(render_chart_label(shape))))

    if shape.shape_type == MSO_SHAPE_TYPE.GROUP:
        stats["groups"] += 1
        if depth >= 3:      # 嵌套组合设深度上限（病态文件里可以无限套）
            return '<div class="sh plain" style="%s">［组合嵌套过深，未展开］</div>' % shape_box(box.l, box.t, box.w, box.h, rot)
        xf = None
        try:
            xf = shape._element.grpSpPr.xfrm
        except Exception:
            pass
        inner = []
        for child in list(shape.shapes)[:MAX_SHAPES_PER_SLIDE]:
            inner.append(render_shape(child, slide_info, box.child(child, xf), stats, budget, depth + 1))
        return '<div class="sh" style="%s">%s</div>' % (shape_box(box.l, box.t, box.w, box.h, rot), "".join(inner))

    stats["other"] += 1
    return ('<div class="sh plain" style="%s">［%s］</div>'
            % (shape_box(box.l, box.t, box.w, box.h, rot), clean(shape_kind_label(shape))))


def render_text_shape(shape, slide_info, box, rot, stats):
    """有文字的形状（占位符/文本框/自选图形）→ HTML。无文字返回 None 交给调用方降级。"""
    ph_idx = placeholder_index(shape)
    layout, master = slide_info["layout"], slide_info["master"]
    title = is_title_like(shape, ph_idx, layout, master)
    srcs = para_sources(shape, ph_idx, layout, master, title, ph_idx is not None)
    default_sz = pick(0, [slide_info["tx_styles"].get("titleStyle" if title else "bodyStyle") or []], "sz") \
        or pick(0, [slide_info["tx_styles"].get("bodyStyle") or []], "sz") or 18.0
    default_align = ALIGN_DEFAULTS.get("titleStyle" if title else "") or "left"

    css = []
    fill = solid_fill_color(shape.fill, slide_info["palette"]) if hasattr(shape, "fill") else None
    if fill:
        css.append("background:%s" % fill)
    else:
        ref_col, had_ref = theme_fill_ref(shape, slide_info["palette"])
        if ref_col:
            css.append("background:%s" % ref_col)
            if shape.shape_type == MSO_SHAPE_TYPE.AUTO_SHAPE:
                stats["theme_fill"] += 1
        elif had_ref and shape.shape_type == MSO_SHAPE_TYPE.AUTO_SHAPE:
            stats["theme_fill_unknown"] += 1
    try:
        prst = shape._element.find(".//" + A + "prstGeom")
        if prst is not None and prst.get("prst") in PRST_CSS:
            css.append(PRST_CSS[prst.get("prst")])
    except Exception:
        pass
    try:
        va = shape.text_frame.vertical_anchor
        if va is not None:
            va_s = str(va).upper()
            css.append("display:flex;flex-direction:column;justify-content:%s"
                       % ("center" if "MIDDLE" in va_s else "flex-end" if "BOTTOM" in va_s else "flex-start"))
    except Exception:
        pass
    body = render_paragraphs(shape.text_frame, srcs, slide_info["palette"], default_sz, default_align)
    return '<div class="sh" style="%s">%s</div>' % (shape_box(box.l, box.t, box.w, box.h, (rot + ";".join(css)).rstrip(";")), body)


def read_tx_styles(master):
    """master 的 txStyles -> {titleStyle/bodyStyle/otherStyle: 各级属性}。"""
    out = {}
    txs = master._element.find(P + "txStyles")
    if txs is None:
        return out
    for st in txs:
        out[st.tag.split("}")[1]] = levels_of(st)
    return out


def slide_bg(slide, palette):
    """页面底色：slide -> layout -> master 的 p:bg（bgPr 实色 或 bgRef 主题色）。"""
    for step in (slide, slide.slide_layout, slide.slide_layout.slide_master):
        bg = step._element.find(P + "cSld/" + P + "bg")
        if bg is None:
            continue
        pr = bg.find(P + "bgPr")
        if pr is not None:
            solid = pr.find(A + "solidFill")
            if solid is not None and len(solid):
                v = solid[0]
                if v.tag.endswith("sysClr"):
                    val = v.get("lastClr") or v.get("val")
                else:
                    val = v.get("val") or v.get("lastClr")
                if v.tag == A + "srgbClr" and val:
                    return "#" + val.upper()
                if v.tag == A + "schemeClr" and val:
                    return palette.get(val)
            # noFill / 渐变 / 图片底 / 其他：按白底处理，不让底色变成随机继承值。
            return None
        ref = bg.find(P + "bgRef")
        if ref is not None and len(ref):
            v = ref[0]
            val = v.get("lastClr") or v.get("val")
            if v.tag == A + "srgbClr" and val:
                return "#" + val.upper()
            if val:
                return palette.get(val)
    return palette.get("bg1")


def render_slide(slide, index, prs, stats, budget):
    total_shapes = len(slide.shapes)
    master = slide.slide_layout.slide_master
    palette = theme_palette(master)
    info = {
        "palette": palette, "layout": slide.slide_layout, "master": master,
        "tx_styles": read_tx_styles(master),
    }
    out = ['<div class="slide-wrap">']
    out.append('<div class="slide-label">第 %d 页 · %d 个形状%s</div>' % (
        index + 1, total_shapes,
        "（只显示前 %d 个）" % MAX_SHAPES_PER_SLIDE if total_shapes > MAX_SHAPES_PER_SLIDE else ""))
    out.append('<div class="slide-scroll"><div class="slide" style="width:%s;height:%s;background:%s">' % (
        emu_px(prs.slide_width), emu_px(prs.slide_height), clean(slide_bg(slide, palette) or "#FFFFFF")))
    for shape in list(slide.shapes)[:MAX_SHAPES_PER_SLIDE]:
        out.append(render_shape(shape, info, Box(shape.left, shape.top, shape.width, shape.height), stats, budget))
    if slide.has_notes_slide:
        notes = (slide.notes_slide.notes_text_frame.text or "").strip()
        if notes:
            out.append('<div class="notes-box"><details><summary>演讲者备注</summary><pre>%s</pre></details></div>'
                       % clean(notes[:MAX_NOTES_CHARS]))
    out.append("</div></div></div>")
    return "".join(out)


def main():
    src, dst = sys.argv[1], sys.argv[2]
    try:
        prs = Presentation(src)
    except PackageNotFoundError as exc:
        sys.stderr.write("这不是可读的 pptx（文件损坏，或不是 OOXML 包）：%s\n" % exc)
        sys.exit(2)
    except (ValueError, KeyError) as exc:
        sys.stderr.write("这不是 PowerPoint 演示文稿（.docx/.xlsx 等其他 OOXML 容器请用对应的预览入口）：%s\n" % exc)
        sys.exit(2)

    total = len(prs.slides)
    stats = {"charts": 0, "other": 0, "groups": 0, "img_skipped": 0,
             "theme_fill": 0, "theme_fill_unknown": 0}
    budget = {"bytes": 0}
    out = ["<!doctype html>"]
    out.append('<html lang="zh"><head><meta charset="utf-8">')
    out.append('<meta name="viewport" content="width=device-width, initial-scale=1">')
    out.append("<title>%s</title>" % clean(os.path.basename(src)))
    out.append("<style>%s</style></head><body>" % STYLE)
    out.append("<h1>%s</h1>" % clean(os.path.basename(src)))
    out.append('<div class="bar">')
    out.append('<p class="meta">%d 页 · %d × %d px（按 96dpi 从 EMU 换算）</p>'
               % (total, round(px(prs.slide_width)), round(px(prs.slide_height))))
    # 诚实标注（产品决策）：这是**近似**版式重建，不是渲染。把做不到的四件事写在最上面，
    # 让用户一眼知道该不该信这一页 —— 假装像比明确说「不像」更糟。
    out.append('<p class="notes"><span class="warn">近似版式重建（不是渲染）</span>：'
               '位置/尺寸/字号/颜色取自文件里的真实数值并绝对定位；'
               '<b>无字体保真</b>（本机没有的字体由浏览器替换）、<b>无动画与切换效果</b>、'
               '<b>SmartArt/图表/嵌入对象不渲染</b>（在原位给可见占位）、'
               '主题渐变/明暗与自动缩放（autofit）按近似处理。要看原样请用「用默认程序打开」。</p>')
    out.append("</div>")
    out.append('<div class="deck">')
    for i, slide in enumerate(prs.slides):
        if i >= MAX_SLIDES:
            out.append('<p class="notes"><span class="warn">还有 %d 页未显示（上限 %d 页）</span></p>'
                       % (total - MAX_SLIDES, MAX_SLIDES))
            break
        out.append(render_slide(slide, i, prs, stats, budget))
    out.append("</div>")

    tail = []
    if stats["other"]:
        tail.append("%d 个形状未渲染（线条/SmartArt/嵌入对象等）" % stats["other"])
    if stats["charts"]:
        tail.append("%d 个图表未渲染" % stats["charts"])
    if stats["groups"]:
        tail.append("%d 个组合形状已按坐标展开" % stats["groups"])
    if stats["img_skipped"]:
        tail.append("%d 张图片超出预算未显示" % stats["img_skipped"])
    if stats["theme_fill"]:
        tail.append("%d 个形状的填充继承自主题样式，取主题色近似（虚线框标记）" % stats["theme_fill"])
    if stats["theme_fill_unknown"]:
        tail.append("%d 个形状的填充无法解析（渐变/图片填充）" % stats["theme_fill_unknown"])
    if tail:
        out.append('<p class="topnotes">%s</p>' % clean(" · ".join(tail)))
    out.append("</body></html>")
    with open(dst, "w", encoding="utf-8") as fh:
        fh.write("\n".join(out))
    print("GOCODE_PPTX_OK", dst)


main()
`
