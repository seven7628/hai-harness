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

// DocxPreviewer 把 .docx/.docm 文档转成**自带样式的 HTML**（应用内预览专用）。
//
// 为什么走「服务端转 HTML + 内嵌浏览器 file://」这条路（与 xlsx 预览同一条通路，见
// xlsx_preview.go 的长注释）：前端解析 docx 要引新依赖（本项目「不加新 npm 依赖」）；
// 纯文本预览对二进制 docx 只会显示乱码（bridge 的 read_file 早就对二进制回 binary=true）；
// 而内嵌浏览器渲染 file:// 已被 PDF／xlsx 实测验证，HTML 里的标题/表格/图片/中文都由
// 浏览器原生处理，零新前端代码。
//
// 为什么用 mammoth（脚本里也有同一份理由，写在脚本头）：mammoth 的定位就是
// 「docx → 语义 HTML」，实测保留标题层级/粗斜体/项目符号/表格/内嵌图片（data: URI），
// 而 python-docx 只给文档对象模型（样式→HTML 的映射要自己写，写出来是更差的 mammoth），
// textutil -convert html 丢标题层级与表格行列关系，LibreOffice 本机不存在。
//
// 归属与线程安全同 XlsxPreviewer：不是 tools.Tool（不接受模型输入）、不放 runtime 包
// （纯环境包不该背业务语义）、Convert 可并发调用（每次独立临时目录，清理只按年龄）。
type DocxPreviewer struct {
	py     PythonRuntime // 受管运行时（nil = 未装配 → 明确报错，不 panic）
	sbxFor SandboxFor    // 沙箱工厂（nil = 恒用 NoSandbox 直通）
}

// NewDocxPreviewer 创建文档预览转换器。
// py = 受管运行时（与 run_python 同一个实例）；sbxFor = 按工作区构造沙箱（nil = NoSandbox）。
func NewDocxPreviewer(py PythonRuntime, sbxFor SandboxFor) *DocxPreviewer {
	return &DocxPreviewer{py: py, sbxFor: sbxFor}
}

// DocxPreviewTimeout 单次转换时限（含 mammoth 解析 + 渲染）。与 XlsxPreviewTimeout 同口径：
// 60s 是「明显不正常就报错」的阈值，不是性能目标；首次调用另需运行时 bootstrap（由 Ensure 管）。
const DocxPreviewTimeout = 60 * time.Second

// DocxConvertResult 转换产物。
type DocxConvertResult struct {
	HTMLPath string // 生成的 HTML 绝对路径（临时目录内；调用方按需保留/清理）
	Dir      string // 临时目录（同上）
}

// Convert 把 docxPath（绝对路径）转换为 HTML，返回产物路径。
//
// workspace / 产物位置 / 清理策略与 XlsxPreviewer.Convert 逐字同义（见其长注释）：
// 沙箱根 = 工作区（脚本与产物都在受管临时目录，cwd 也在那里 —— 那是策略一定放行的位置），
// 失败路径立即清理本次目录，成功路径保留产物（浏览器还要读），残留由下一次转换按年龄回收。
func (p *DocxPreviewer) Convert(ctx context.Context, docxPath, workspace string) (DocxConvertResult, error) {
	if p.py == nil {
		return DocxConvertResult{}, fmt.Errorf("no managed Python runtime is configured")
	}
	py, err := p.py.Ensure(ctx)
	if err != nil {
		return DocxConvertResult{}, fmt.Errorf("managed Python runtime unavailable: %w", err)
	}
	scriptPath, cleanup, err := writeTempFile(py, "docx-preview-", "convert_docx.py", []byte(docxToHTMLScript))
	if err != nil {
		return DocxConvertResult{}, err
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
	// 模型/前端的自由文本，故不存在「把用户输入拼进 shell」的面（同 xlsx 预览）。
	cmdline := shQuote(py.VenvPython) + " " + shQuote(scriptPath) + " " + shQuote(docxPath) + " " + shQuote(htmlPath)
	var sbx sandbox.Sandbox = sandbox.NoSandbox{}
	if p.sbxFor != nil {
		if s := p.sbxFor(workspace); s != nil {
			sbx = s
		}
	}
	out, err := sbx.Run(ctx, sandbox.ExecSpec{Command: cmdline, Cwd: dir, Timeout: DocxPreviewTimeout})
	if err != nil {
		return DocxConvertResult{}, fmt.Errorf("执行转换脚本失败: %w", err)
	}
	if !strings.Contains(out, docxOKMarker) {
		return DocxConvertResult{}, fmt.Errorf("转换失败: %s", singleLine(out))
	}
	if _, err := os.Stat(htmlPath); err != nil {
		return DocxConvertResult{}, fmt.Errorf("转换脚本未产出 HTML: %w", err)
	}
	// 清扫过期残留，keep 当前产物（见 sweepStaleOfficeTempDirs 的理由）。
	sweepStaleOfficeTempDirs(py, dir)
	done = true
	return DocxConvertResult{HTMLPath: htmlPath, Dir: dir}, nil
}

// sweepStaleOfficeTempDirs 清扫受管临时目录里**过期的**办公预览残留目录（keep 除外）。
//
// 为什么一条清扫覆盖三种格式（而 xlsx_preview.go 里的旧版只扫 xlsx-preview-*）：
// 这三种产物是同一类东西（同样落在受管 tmp、同样按年龄可回收、同样在 bridge 被杀时留下），
// 三个转换器各扫自己那一份等于「谁最后被用到，谁的残留才被回收」—— 用户只预览 Word 时
// 过期的 xlsx 产物永远没人收。合并成一个清扫点后，任何一次预览都会顺手收掉全部过期残留。
// 旧函数（sweepStaleTempDirs）保持原样不动：它的行为被既有快测试锁定，且 xlsx 那条路
// 无需因为新增格式而改变自己的回收范围。
//
// 年龄门槛优先于「谁在用」：1 小时（staleTempAge）远超一次转换的合理时长，只会命中
// 死进程留下的残留；并发存活的另一个 bridge 的产物（或本进程刚交付给浏览器的）都在门槛内。
func sweepStaleOfficeTempDirs(py runtime.Python, keep string) {
	prefixes := []string{"xlsx-preview-", "docx-preview-", "pptx-preview-"}
	for _, base := range tempDirBases(py) {
		if base == "" {
			continue
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			matched := false
			for _, pfx := range prefixes {
				if strings.HasPrefix(e.Name(), pfx) {
					matched = true
					break
				}
			}
			if !matched {
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

// docxOKMarker 转换脚本成功时输出的标记行（Go 侧以此判定成败：非零退出/沙箱拦截/
// mammoth 抛错都会让这一行不出现，故统一按「输出里没有标记」判失败）。
const docxOKMarker = "GOCODE_DOCX_OK"

// docxToHTMLScript 固定的转换脚本（**不是**模型可改写的输入：编译期常量）。
//
// 关键取舍（改脚本前先读）：
//
//  1. **只做「文档 → HTML」，不做版式还原**：docx 的排版信息（页面尺寸/分页/浮动图文框）
//     是「面向打印」的，HTML 里没有对应的可靠模型；mammoth 的取舍是给出**语义**结构
//     （标题/段落/列表/表格/图片），这也是 Word 文档在应用内预览时真正要看的东西。
//     代价：分页位置、页眉页脚、脚注编号、文本框位置都不还原 —— 页面上如实说明。
//
//  2. **样式映射（style_map）要显式给**：Word 的 Title / Subtitle 不是 Heading 样式，
//     mammoth 默认认不出（实测：产出裸 <p> 且附带一条 "Unrecognised paragraph style"
//     warning，纯外观损失）；映射后 Title → h1.doc-title。中文 Word 的样式名是本地化的，
//     故中英名并列写（未命中的规则无害：实测不报错、不产生 warning）。
//
//  3. **图片走 data: URI，但要有预算**：mammoth 的 data_uri 转换器把整张图 base64 进
//     HTML（实测可用，不依赖外部文件路径 —— 平台沙箱下最省事的一条）。代价是产物体积
//     随图片线性膨胀（base64 约 +37%），故设两张闸：张数上限 + 总字节上限；超出的图**不
//     静默消失**，替换成可见的「［图片已省略］」并在页面顶部计数。
//
//  4. **正文长度闸**：产物是浏览器里的一整页 DOM，超长文档（实测极端：数十万段落）会让
//     布局与绘制卡住。超出 MAX_FRAGMENT_CHARS 时在**标签边界**截断（见 truncate_fragment
//     的理由）并在末尾如实标注。
//
//  5. **转义**：文档内容是外部数据。mammoth 自己会转义元素文本与属性，本脚本只额外处理
//     「我们自己拼进去的部分」（文件名/警告文案，经 clean()）与截断点。HTML 里出现的
//     <script> 只可能是被转义后的文本（e2e 有断言：文档里的 <script> 不得变成元素）。
//
//  6. **charset**：<meta charset="utf-8"> 必须在内（实测教训：缺声明会让中文全部
//     mojibake）。产物固定以 UTF-8 写出。
//
//  7. **样式自带**：内嵌浏览器不继承应用样式，故模板内含完整样式（含暗色 media query）。
const docxToHTMLScript = `# -*- coding: utf-8 -*-
"""docx -> 自带样式的 HTML（go-code 应用内预览专用；脚本是 Go 侧的编译期常量，不接受外部输入）。

用法: python convert_docx.py <docx 绝对路径> <输出 html 绝对路径>
成功时 stdout 末行打印 GOCODE_DOCX_OK；失败时非零退出 + 单行中文原因（预期内的用户错误）
或 traceback（我们的 bug，需要完整堆栈定位）。
"""
import html
import os
import re
import sys
import zipfile

import mammoth

MAX_IMAGES = 300                       # 内嵌图片张数上限
MAX_IMAGE_BYTES = 40 * 1024 * 1024     # 内嵌图片总字节上限（base64 之后的解码体积估算）
MAX_FRAGMENT_CHARS = 6 * 1024 * 1024   # 正文片段字符上限（超出在标签边界截断）

STYLE = """
:root { color-scheme: light dark; }
* { box-sizing: border-box; }
body {
  margin: 0; padding: 20px 24px 48px;
  font: 14px/1.7 -apple-system, BlinkMacSystemFont, "Segoe UI", "PingFang SC",
        "Hiragino Sans GB", "Microsoft YaHei", sans-serif;
  color: #1c1c1e; background: #ffffff;
}
.doc-wrap { max-width: 820px; margin: 0 auto; }
h1.doc-title { font-size: 24px; line-height: 1.35; margin: 0 0 10px; }
p.doc-subtitle { font-size: 15px; color: #6b7280; margin: 0 0 14px; }
h1, h2, h3, h4, h5, h6 { line-height: 1.35; margin: 1.4em 0 0.5em; }
h1 { font-size: 22px; } h2 { font-size: 19px; } h3 { font-size: 17px; }
h4 { font-size: 15px; } h5 { font-size: 14px; } h6 { font-size: 13px; }
p { margin: 0.65em 0; }
ul, ol { margin: 0.65em 0; padding-left: 1.6em; }
li { margin: 0.2em 0; }
blockquote { margin: 0.7em 0; padding: 2px 0 2px 12px; border-left: 3px solid #d1d5db; color: #4b5563; }
table { border-collapse: collapse; margin: 0.9em 0; max-width: 100%; }
td, th { border: 1px solid #d1d5db; padding: 5px 9px; vertical-align: top; }
td > p:first-child, th > p:first-child { margin-top: 0; }
td > p:last-child, th > p:last-child { margin-bottom: 0; }
img { max-width: 100%; height: auto; }
a { color: #2563eb; }
.bar { border: 1px solid #e5e7eb; border-radius: 8px; padding: 10px 12px; margin: 0 0 16px; background: #f9fafb; }
.bar .name { font-weight: 600; }
.meta, .notes { color: #6b7280; font-size: 12px; margin: 4px 0 0; }
.notes .warn { color: #b45309; }
details { margin-top: 6px; }
summary { cursor: pointer; color: #6b7280; font-size: 12px; }
details pre { white-space: pre-wrap; font-size: 12px; color: #6b7280; margin: 6px 0 0; }
.img-skip, .cut { color: #b45309; font-size: 12px; }
@media (prefers-color-scheme: dark) {
  body { color: #e5e7eb; background: #1c1c1e; }
  .bar { border-color: #3a3a3c; background: #232325; }
  h1.doc-title, h2, h3, h4, h5, h6 { color: #f3f4f6; }
  p.doc-subtitle, blockquote { color: #9ca3af; }
  blockquote { border-left-color: #4b5563; }
  td, th { border-color: #3a3a3c; }
  .meta, .notes, summary, details pre { color: #9ca3af; }
  a { color: #60a5fa; }
}
"""

# 中英并列的样式映射（见文件头取舍 2）。为什么连「引用」这类都写：中文 Word 的样式名是
# 本地化的，而 mammoth 按**样式名**匹配，不写中文名的话中文文档里的这些段落会退化成裸 <p>。
STYLE_MAP = "\n".join([
    "p[style-name='Title'] => h1.doc-title:fresh",
    "p[style-name='标题'] => h1.doc-title:fresh",
    "p[style-name='Subtitle'] => p.doc-subtitle:fresh",
    "p[style-name='副标题'] => p.doc-subtitle:fresh",
    "p[style-name='Quote'] => blockquote:fresh",
    "p[style-name='引用'] => blockquote:fresh",
    "p[style-name='Intense Quote'] => blockquote:fresh",
    "p[style-name='明显引用'] => blockquote:fresh",
])

# <img src="data:image/...;base64,..."> 的整标签匹配。安全性：base64 字母表不含 '>' 与 '"'，
# 故 [^>]* 不会越界到别的标签；替换是**整标签**级的（不碰标签外的文本，也就不会破坏转义）。
IMG_TAG = re.compile(r"<img\b[^>]*>")
DATA_SRC = re.compile(r'src="data:image/[^;"]+;base64,([^"]*)"')


def clean(text):
    """展示文本 -> 可安全嵌入 HTML 的片段（删控制字符 + html.escape）。

    与 xlsx 预览的 clean() 同口径：内容可能来自任意文档，未转义的 <script> 会被当脚本执行。
    与 xlsx 的差别：这里不做换行 -> <br>（文档正文的空行由 mammoth 的 <p> 承担，段落内换行
    在 docx 里是 <w:br>，mammoth 已经处理）。
    """
    s = "".join(ch for ch in str(text) if ord(ch) >= 0x20 or ch == "\t")
    return html.escape(s, quote=True)


def cap_images(fragment):
    """按预算裁剪内嵌图片，返回 (片段, 统计)。

    为什么在**产出的 HTML 字符串**上做、而不是在 mammoth 的 convert_image 回调里做：
    回调的返回类型是文档化程度最低的那一层（mammoth.images.img_element 返回的属性表只能
    产出 <img>，要产出「可见的省略占位」就得自己拼 HTML 节点树），而这里的替换是整标签级、
    正则安全性可论证（见 IMG_TAG 注释）。代价是多扫一遍字符串（数 MB 级，实测毫秒量级）。
    """
    stats = {"count": 0, "bytes": 0, "skipped": 0}

    def repl(match):
        tag = match.group(0)
        stats["count"] += 1
        m = DATA_SRC.search(tag)
        if m is not None:
            if stats["count"] > MAX_IMAGES or stats["bytes"] + len(m.group(1)) * 3 // 4 > MAX_IMAGE_BYTES:
                stats["skipped"] += 1
                return '<span class="img-skip">［图片已省略：超出预览预算］</span>'
            stats["bytes"] += len(m.group(1)) * 3 // 4
        return tag

    return IMG_TAG.sub(repl, fragment), stats


def truncate_fragment(fragment):
    """超长正文在**标签边界**截断，返回 (片段, 是否截断)。

    为什么截在最后一个 '>' 而不是按字符数硬切：mammoth 产出的 HTML 里，属性值中的 '>' 与 '-'
    都被实体化（&gt;），故裸 '>' 只可能是标签结束符 —— 截在它之后得到的仍是结构合法的前缀
    （不会留下半截标签让浏览器把后面所有文本吞进属性里）。浏览器对未闭合的容器（如
    <ul><li>）会自行补全，故无需在这里补闭合标签。
    """
    if len(fragment) <= MAX_FRAGMENT_CHARS:
        return fragment, False
    cut = fragment.rfind(">", 0, MAX_FRAGMENT_CHARS)
    if cut < 0:
        return "", True
    return fragment[:cut + 1], True


def build(src, fragment, stats, cut, messages):
    """片段 -> 完整 HTML 文档。"""
    name = os.path.basename(src)
    parts = []
    parts.append("<!doctype html>")
    parts.append('<html lang="zh"><head><meta charset="utf-8">')
    parts.append('<meta name="viewport" content="width=device-width, initial-scale=1">')
    parts.append("<title>%s</title>" % clean(name))
    parts.append("<style>%s</style></head><body>" % STYLE)
    parts.append('<div class="doc-wrap">')

    meta = ["正文 %d KB" % (len(fragment.encode("utf-8")) // 1024)]
    if stats["count"]:
        meta.append("内嵌图片 %d 张" % stats["count"])
    notes = []
    if stats["skipped"]:
        notes.append('<span class="warn">%d 张图片未显示（预览预算：最多 %d 张 / %d MB）</span>'
                     % (stats["skipped"], MAX_IMAGES, MAX_IMAGE_BYTES // (1024 * 1024)))
    if cut:
        notes.append('<span class="warn">文档过长，只显示前 %d 字符（约 %d MB）</span>'
                     % (MAX_FRAGMENT_CHARS, MAX_FRAGMENT_CHARS // (1024 * 1024)))
    # 版式信息的缺失必须写在页面上（而不是让用户以为是「预览坏了」）：HTML 里没有分页模型。
    meta.append("预览为语义重排：不还原分页/页眉页脚/脚注编号/文本框位置")

    parts.append('<div class="bar"><div class="name">%s</div>' % clean(name))
    parts.append('<p class="meta">%s</p>' % clean(" · ".join(meta)))
    if notes:
        parts.append('<p class="notes">%s</p>' % " · ".join(notes))
    if messages:
        # mammoth 的 warning（未识别的样式等）不是错误，但也不该静默：折叠展示前几条。
        shown = messages[:5]
        parts.append('<details><summary>%d 条转换提示（未识别的样式等，通常不影响内容）</summary><pre>%s</pre></details>'
                     % (len(messages), clean("\n".join(shown))))
    parts.append("</div>")

    parts.append(fragment)
    if cut:
        parts.append('<p class="cut">［以下内容已截断］</p>')
    parts.append("</div></body></html>")
    return "\n".join(parts)


def main():
    src, dst = sys.argv[1], sys.argv[2]
    try:
        with open(src, "rb") as fh:
            result = mammoth.convert_to_html(fh, style_map=STYLE_MAP,
                                             convert_image=mammoth.images.data_uri)
    except zipfile.BadZipFile as exc:
        # 可预期的用户错误（最常见：文件损坏、把别的东西改名成 .docx、空文件）→ 单行中文提示，
        # 不把 traceback 甩到用户脸上（toast 里塞 40 行堆栈等于没提示）。
        sys.stderr.write("这不是可读的 docx（文件损坏或不是 zip 容器）：%s\n" % exc)
        sys.exit(2)
    except ValueError as exc:
        # mammoth 对「合法 zip 但不是 Word 文档」（例如把 .xlsx 改名成 .docx）抛 ValueError。
        sys.stderr.write("这不是 Word 文档（.xlsx/其他 OOXML 容器请用对应的预览入口）：%s\n" % exc)
        sys.exit(2)
    except OSError as exc:
        # 「是 zip，但没有 word/document.xml」。
        sys.stderr.write("docx 结构不完整（缺少正文部件）：%s\n" % exc)
        sys.exit(2)

    fragment, stats = cap_images(result.value)
    fragment, cut = truncate_fragment(fragment)
    messages = [m.message for m in result.messages if getattr(m, "message", "")]
    with open(dst, "w", encoding="utf-8") as fh:
        fh.write(build(src, fragment, stats, cut, messages))
    print("GOCODE_DOCX_OK", dst)


main()
`
