package main

// refextract.go —— @引用 PDF 抽取器（bridge 侧实现，注入给 agents.RefExtractor）。
//
// 为什么在 bridge 而不是 agents：抽取要真解析格式（pypdf + 受管 Python 运行时 +
// 沙箱执行），都是宿主能力面。agents 包只定义钩子协议（agents/refextract.go），
// 不 import tools/runtime/sandbox。装配点见 buildLoop 的 agents.WithRefExtractor。
//
// 与 xlsx 预览（tools/builtin/xlsx_preview.go）同范式，刻意保持一致的几点：
//   - 固定脚本（编译期常量）+ 固定解释器，命令行里没有任何来自模型/用户的自由文本
//     （路径经 builtin.ShQuote 单引号转义），故不存在「把用户输入拼进 shell」的面；
//   - 经沙箱执行（Seatbelt），产物与脚本都落受管临时目录，**不落工作区**；
//   - 运行时未装配 → 返回明确 error，不 panic（调用方据此回退既有行为）。
//
// 与 xlsx 预览不同的两点（故意的）：
//   - **文本**抽取不落产物文件：结果走 stdout 回传。文本引用天然有上限（调用方
//     maxRefFileChars=64K 截断），不是「产物得给浏览器 file:// 读」的场景。
//     唯一的产物例外是「无文本层 → 渲染成图片」那一路（见 renderScannedPDF）：
//     图片必须比本次调用活得久（模型稍后自己用 read_file 取），故它落长期保留
//     目录并复用 xlsx 预览的「按年龄清扫残留」策略（见 pdfImageStaleAge）。
//   - 沙箱按**会话工作区**构造（沿用 xlsxPreviewSandbox 那条只读转换边界），
//     而不是按被引用文件所在目录：@引用可指向工作区外，但 Seatbelt 策略里
//     home 读 + systemReadPaths（含 /Volumes、/tmp、/private/var）已覆盖用户
//     放文件的常见位置，不必为单次调用放大沙箱根。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agents"
	goruntime "github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// pdfExtractTimeout 单次抽取时限（含 runtime bootstrap：首次引用 PDF 会顺带把受管
// 运行时装起来 —— 建 venv + pip 装约 49MB 依赖，慢网分钟级）。与 XlsxPreviewTimeout
// 同量级，理由也同：这是「明显不正常就报错」的阈值，不是性能目标。
// 该值须 <= agents 侧 extractRefBackstop（那一层是防挂死兜底，比本值宽）。
const pdfExtractTimeout = 8 * time.Minute

// pdfPageMarker 脚本输出的页数标记前缀（协议：Go 侧解析这一行拿总页数，其后为正文）。
// 用带前缀的独立行而非「第 N 页」正文行 —— 后者在用户文档里可能真实出现（脚本自己
// 也往正文里写 "--- 第 N 页 ---"），前缀行是机器可判定的。
const pdfPageMarker = "__GOCODE_PDF_PAGES__"

// pdfNoTextMarker 无文本层标记：脚本遍历完全部页面但一个字都没抽到（扫描件/纯图 PDF）。
// 与「脚本报错/沙箱拦截/运行时不可用」严格区分 —— 前者是业务事实（→ ok=false，零 err，
// 调用方显式告知用户），后者是环境问题（→ err，调用方回退既有二进制行为）。
const pdfNoTextMarker = "__GOCODE_PDF_NOTEXT__"

// pdfExtractScript 固定的抽取脚本（编译期常量，模型/用户不可改写）。
//
// 关键取舍（改脚本前先读）：
//  1. **逐页 try**：单页解析失败（畸形对象流、字体表损坏）不该让整份文档读不出 ——
//     该页记成占位文字继续，其余页正文照常返回。整份读不出来（打开/页数都失败）才
//     走错误路径（非零退出）。
//  2. **extract_text() 返回 None 是常态**：pypdf 对图片页返回 None（不是异常），
//     一律 `or ""` 兜底；空页不产生 "--- 第 N 页 ---" 段（避免正文里堆一串空标题）。
//  3. **页数用独立标记行传出**：理由见 pdfPageMarker；此处打印的是**声明的总页数**
//     （len(reader.pages)），与「有几页抽到了字」无关 —— 用户问「一共几页」要的是前者。
//  4. **加密 PDF**：先试空密码解密（"打开就能看"的加密件常见），失败/有真密码时
//     reader.pages 抛错 → 非零退出 → Go 侧转 err（环境/格式问题），不会误报「无文字」。
//  5. **不做网络访问**、不写任何文件：只读 argv[1]，结果只走 stdout。
const pdfExtractScript = `
import sys

MARK_PAGES = "__GOCODE_PDF_PAGES__"
MARK_NOTEXT = "__GOCODE_PDF_NOTEXT__"


def main():
    src = sys.argv[1]
    try:
        import pypdf
    except ImportError as exc:
        sys.stderr.write("pypdf 不可用: %s\n" % exc)
        sys.exit(3)
    try:
        reader = pypdf.PdfReader(src)
    except Exception as exc:  # 损坏/加密/根本不是 PDF
        sys.stderr.write("无法解析 PDF: %s\n" % exc)
        sys.exit(2)
    if getattr(reader, "is_encrypted", False):
        try:
            reader.decrypt("")  # 空用户密码：pypdf 直接可读
        except Exception:
            pass
    try:
        total = len(reader.pages)
    except Exception as exc:
        sys.stderr.write("无法读取页数（加密或损坏）: %s\n" % exc)
        sys.exit(2)

    pages = []
    failed = 0
    for i, page in enumerate(reader.pages):
        try:
            text = page.extract_text() or ""
        except Exception as exc:  # 单页失败不拖垮整份
            failed += 1
            pages.append("--- 第 %d 页（解析失败: %s）---" % (i + 1, exc))
            continue
        if text.strip():
            pages.append("--- 第 %d 页 ---\n%s" % (i + 1, text.strip()))

    print("%s%d" % (MARK_PAGES, total))
    if failed:
        sys.stderr.write("有 %d 页解析失败（其余照常返回）\n" % failed)
    if not pages:
        # 一个字都没抽到：抽取器可用，但该文件没有文本层。
        print(MARK_NOTEXT)
        return
    print("\n".join(pages))


main()
`

// ---------- 无文本层 PDF 的图片渲染（pypdfium2） ----------

// 渲染产物的口径常量（**与引擎对齐**，改前先读 tools/builtin/image.go）：
//
//   - pdfImageMaxDim = 1568：tools/builtin.maxImageDim（视觉模型长边 >1568px 会被引擎
//     降采样 + JPEG 重编码）。故意渲染成「引擎不会再动它」的尺寸 —— 渲染 scale 按**长边**
//     反算（见 pdfRenderScript），而不是固定 scale=2.0：Letter 792pt 在 2.0 下是 1584px，
//     刚好越过阈值、被 read_file 二次重采样，白烧字节且多一次质量损失。
//   - pdfImageQuality = 85：tools/builtin.reencodeQuality 同值（文字/UI 截图上肉眼无损）。
//     1568px/q85 的 JPEG 通常数百 KB，远低于引擎单图 3MiB 闸门（tools.defaultMaxImageBytes），
//     故 read_file 会**原样内嵌**——这正是我们要的终态。
const (
	pdfImageMaxDim  = 1568
	pdfImageQuality = 85
)

// pdfImagePagesMax 单次渲染的页数上限。
//
// 为什么是 5：扫描件的价值全在前几页（封面/首页表格/签名页），而每页一张 1568px JPEG
// 约数百 KB，模型逐张 read_file 看图的成本随页数线性增长。5 页是「够看出这是什么文档」
// 与「不刷爆上下文」的折中。
//
// 注意**不要**把它说成「一次读 5 张」：引擎单次工具结果最多携带 4 张图
// （tools.defaultMaxResultImages），故正文措辞是「逐张查看」（每次 read_file 一张）。
const pdfImagePagesMax = 5

// pdfImageDirPrefix 产物目录前缀（受管临时目录下的长期保留产物，见 renderScannedPDF）。
// 前缀同时是**按年龄清扫**的匹配键（sweepStalePDFImageDirs）与残留排查的线索。
const pdfImageDirPrefix = "pdf-scan-"

// pdfImageStaleAge 产物目录的残留回收年龄门槛。
//
// 与 tools/builtin.staleTempAge 同值同因：图片必须比本次抽取调用活得久（模型是**稍后**
// 才 read_file 的，可能隔着若干轮对话），但也不能永久堆积 —— 每次渲染时顺手清掉过期的。
// 1 小时远超「引用一次文件到模型看完」的合理时长，故只会命中死进程（app 退出/被 SIGTERM）
// 留下的残留；代价是极端情况下残留多存活一小时（受管 scratch 区，可接受）。
const pdfImageStaleAge = time.Hour

// pdfRenderTimeout 渲染时限。
//
// 为什么单独一个常量而不是复用 pdfExtractTimeout：两段是不同性质的活（结构解析 vs 光栅化，
// 后者要跑原生 pdfium）。且它俩在**同一次**抽取调用里串行，agents 侧 extractRefBackstop=12min
// 是这次调用（两段之和）的兜底 —— 8+2=10 < 12 保证内层永远先超时，报出来的才是
// 「哪一段慢」这种可读信息，而不是被外层掐断后的「上下文取消」。
const pdfRenderTimeout = 2 * time.Minute

// pdfRenderOKMarker 渲染脚本走完所有页（至少写出一张图）时输出的标记行。
// 与 xlsxOKMarker 同范式：非零退出/沙箱拦截/pdfium 崩溃都会让这一行不出现，
// 故 Go 侧统一按「输出里没有标记」判失败（**降级**，不是 err —— 见 renderScannedPDF）。
const pdfRenderOKMarker = "__GOCODE_PDF_RENDER_OK__"

// pdfRenderImageMarker 单张产物行前缀，后接 `页号\tscale\t绝对路径`（制表符分隔：路径可含
// 空格，空格分隔会解析歧义；scale 是脚本实际用的渲染倍率，供测试钉住「按长边反算」这条决策）。
const pdfRenderImageMarker = "__GOCODE_PDF_IMAGE__"

// pdfRenderScript 固定的渲染脚本（编译期常量，模型/用户不可改写）。
//
// 关键取舍（改脚本前先读）：
//  1. **scale 按长边反算**：理由见 pdfImageMaxDim。pypdfium2 的 render(scale) 以 PDF
//     point（1/72 英寸）为基准，页尺寸取 page.get_size() —— 固定 scale 在 Letter
//     这种长边 792pt 的纸上会刚好越过 1568px 阈值（1584px），被 read_file 二次重编码。
//  2. **clamp**：MAX_SCALE 防极小页面被放大到爆内存（PDF 页是矢量，放大本身不失真，
//     限的是字节与耗时）；MIN_SCALE 防 get_size() 返回 0/负（畸形 MediaBox）时 scale<=0
//     直接抛错。渲染后**再钳一次长边**：位图尺寸是整数上取整，scale 反算后可能多出 1px，
//     钳回来才保证「read_file 不会再动它」。
//  3. **转 RGB 再存 JPEG**：pdfium 对有透明度的页返回 RGBA，PIL 直接 save 成 JPEG 是
//     OSError（实测 `cannot write mode RGBA as JPEG`）—— 不是静默降级，是整页失败。
//  4. **逐页 try**：单页渲染失败（畸形对象流/pdfium 对该页报错）只记 stderr 继续，
//     其余页照样给模型看；**一张都没写出来**才 exit(4)（Go 侧降级回「无文本层」）。
//  5. 不写工作区、不做网络访问：只读 argv[1]，只写 argv[2]（Go 侧给的受管临时目录）。
const pdfRenderScript = `
import sys

MARK_PAGES = "__GOCODE_PDF_PAGES__"
MARK_OK = "__GOCODE_PDF_RENDER_OK__"
MARK_IMAGE = "__GOCODE_PDF_IMAGE__"
MAX_DIM = 1568   # 长边像素上限（与 tools/builtin.maxImageDim 同口径）
QUALITY = 85     # JPEG 质量（与 tools/builtin.reencodeQuality 同口径）
MAX_PAGES = 5
MAX_SCALE = 8.0  # 极小页面（如 20pt 图标）不放大到这个倍数以上
MIN_SCALE = 0.1  # 畸形页尺寸（0/负）时兜底，避免 scale<=0


def main():
    src, outdir = sys.argv[1], sys.argv[2]
    try:
        import pypdfium2 as pdfium
    except ImportError as exc:
        sys.stderr.write("pypdfium2 不可用: %s\n" % exc)
        sys.exit(3)
    try:
        doc = pdfium.PdfDocument(src)
    except Exception as exc:
        sys.stderr.write("无法打开 PDF: %s\n" % exc)
        sys.exit(2)
    try:
        total = len(doc)
    except Exception as exc:
        sys.stderr.write("无法读取页数: %s\n" % exc)
        sys.exit(2)

    # 页数标记与抽取脚本同一常量：Go 侧解析出的是**声明的总页数**（用户问「共几页」
    # 要的是它，而不是「渲染成功了几页」）。
    print("%s%d" % (MARK_PAGES, total))

    wrote = 0
    for i in range(min(total, MAX_PAGES)):
        try:
            page = doc[i]
            w, h = page.get_size()
            longest = max(w, h)
            if longest <= 0:
                raise ValueError("页面尺寸异常: %sx%s" % (w, h))
            scale = MAX_DIM / longest
            scale = max(MIN_SCALE, min(MAX_SCALE, scale))
            pil = page.render(scale=scale).to_pil()
            if pil.mode != "RGB":
                pil = pil.convert("RGB")  # JPEG 无 alpha 通道（RGBA 直接 save 会 OSError）
            lw, lh = pil.size
            if max(lw, lh) > MAX_DIM:
                r = MAX_DIM / float(max(lw, lh))
                pil = pil.resize((max(1, int(round(lw * r))), max(1, int(round(lh * r)))))
            path = "%s/page-%04d.jpg" % (outdir.rstrip("/"), i + 1)
            pil.save(path, "JPEG", quality=QUALITY)
            # 页号 + 实际用的 scale + 路径（scale 一并报出：Go 侧测试据此钉住「按长边反算」
            # 这个设计点 —— 位图最终尺寸两法相同（有下方 clamp 兜底），只有 scale 能区分）
            print("%s%d\t%.4f\t%s" % (MARK_IMAGE, i + 1, scale, path))
            wrote += 1
        except Exception as exc:  # 单页失败不拖垮整份（其余页照样给模型看）
            sys.stderr.write("第 %d 页渲染失败: %s\n" % (i + 1, exc))

    if not wrote:
        sys.stderr.write("没有可渲染的页面（共 %d 页）\n" % total)
        sys.exit(4)
    print(MARK_OK)


main()
`

// newPDFExtractor 构造 @引用 PDF 抽取器（agents.RefExtractor 实现）。
//
// py = 受管运行时（与 run_python 同一个单例；nil = 未装配 → 每次调用返回明确错误，
// 调用方回退既有二进制行为，不 panic）。sbxFor = 按**会话工作区**构造沙箱后端
// （nil = NoSandbox 直通）；形态照 builtin.SandboxFor —— Seatbelt 的读写根在构造时
// 注入，而本抽取器是进程级单例、不按工作区隔离。workspace = 本会话工作区。
func newPDFExtractor(py *goruntime.Runtime, sbxFor func(ws string) sandbox.Sandbox, workspace string) agents.RefExtractor {
	return func(ctx context.Context, abs string) (string, bool, error) {
		if py == nil {
			// 未装配：环境问题（不是「这个 PDF 没文字」）→ 回 err，调用方回退占位。
			return "", false, fmt.Errorf("受管 Python 运行时未装配，无法抽取 PDF 文本")
		}
		st, err := os.Stat(abs)
		if err != nil {
			return "", false, fmt.Errorf("无法读取 PDF: %w", err)
		}
		if st.IsDir() {
			return "", false, fmt.Errorf("路径是目录，不是 PDF: %s", abs)
		}
		pyh, err := py.Ensure(ctx)
		if err != nil {
			return "", false, fmt.Errorf("受管 Python 运行时不可用: %w", err)
		}
		// 临时目录选在受管运行时子树（沙箱一定可读，且不污染工作区）：与 run_python /
		// xlsx_preview 共用 writeTempFile 的选址策略。
		scriptPath, cleanup, err := builtin.WriteTempFile(pyh, "pdf-extract-", "extract_pdf.py", []byte(pdfExtractScript))
		if err != nil {
			return "", false, err
		}
		defer cleanup() // 无产物文件：脚本读进内存、结果走 stdout，跑完即清理

		cmdline := builtin.ShQuote(pyh.VenvPython) + " " + builtin.ShQuote(scriptPath) + " " + builtin.ShQuote(abs)
		var sbx sandbox.Sandbox = sandbox.NoSandbox{}
		if sbxFor != nil {
			if s := sbxFor(workspace); s != nil {
				sbx = s
			}
		}
		out, err := sbx.Run(ctx, sandbox.ExecSpec{Command: cmdline, Cwd: filepath.Dir(scriptPath), Timeout: pdfExtractTimeout})
		if err != nil {
			return "", false, fmt.Errorf("执行 PDF 抽取脚本失败: %w", err)
		}
		content, ok, perr := parsePDFExtractOutput(out)
		if perr != nil || ok {
			return content, ok, perr
		}
		// 无文本层（扫描件/整页图片）：今天到此为止只会给模型一句「抽不到」，它拿不到
		// 任何内容。这里补上最后一段能力 —— 渲染成图片，正文给**路径**（模型自己用
		// read_file 看图，那条通路已把图片内嵌进上下文），故 ok=true。
		//
		// 为什么返回路径而不是图片本身：① 不改 agents.RefExtractor 的三态签名（返回值只有
		// 一个 string），② 不改 agents/ 下任何文件与前端，③ 复用已验证的 read_file 图片
		// 通路（含引擎的尺寸/体积闸门）。代价是多一次工具往返，对「看一眼扫描件」完全可接受。
		if body, rok := renderScannedPDF(ctx, pyh, abs, sbx); rok {
			return body, true, nil
		}
		// 渲染失败（pypdfium2 缺失/渲染报错/无可用输出目录）→ **保持今天的行为**：
		// ("", false, nil)，调用方照旧显式告知「无文本层」。
		//
		// 为什么不升级成 err：err 会让调用方回退成「[二进制文件，不读内容]」——
		// 那是一句更差的信息（用户与模型都不知道这其实是「扫描件、只是读不出字」）。
		// 环境的坏消息不值得用「丢掉准确信息」来换。
		return "", false, nil
	}
}

// renderScannedPDF 把无文本层 PDF 的前 pdfImagePagesMax 页渲染成 JPEG，返回给模型看的正文。
//
// 返回 ok=false 表示「这次渲染没成」（任何原因）：调用方照旧走「无文本层」那条路。
// **本函数不返回 error**：它的全部失败模式都是「没渲染出来」这一件事，且都该降级而不是
// 升级成环境错误（理由见调用点注释）。
//
// 产物生命周期是本函数最关键的取舍（与 xlsx 预览同范式，见 tools/builtin/xlsx_preview.go）：
// 图片写在**独立于脚本临时目录**的长期保留目录里，**不能**用 builtin.WriteTempFile 的
// cleanup —— 那个 cleanup 在函数返回时删掉整个目录，而图片必须活到模型稍后的 read_file
// （可能隔着若干轮对话，甚至别的会话），故残留改由「下一次渲染时按年龄清扫」回收
// （sweepStalePDFImageDirs + pdfImageStaleAge）。
func renderScannedPDF(ctx context.Context, pyh goruntime.Python, abs string, sbx sandbox.Sandbox) (string, bool) {
	scriptPath, cleanup, err := builtin.WriteTempFile(pyh, "pdf-render-", "render_pdf.py", []byte(pdfRenderScript))
	if err != nil {
		return "", false
	}
	defer cleanup() // 脚本自己可以跑完即删：图片在**另一个**目录（见上）

	imgDir, err := mkdirPDFImageDir(pyh)
	if err != nil {
		return "", false
	}
	ok := false
	defer func() {
		// 失败路径立即清理本次目录（没图可看，留着是垃圾）；成功路径保留产物，
		// 改由下一次渲染时的 sweepStalePDFImageDirs 按年龄回收。
		if !ok {
			_ = os.RemoveAll(imgDir)
		}
	}()

	cmdline := builtin.ShQuote(pyh.VenvPython) + " " + builtin.ShQuote(scriptPath) + " " + builtin.ShQuote(abs) + " " + builtin.ShQuote(imgDir)
	out, err := sbx.Run(ctx, sandbox.ExecSpec{Command: cmdline, Cwd: filepath.Dir(scriptPath), Timeout: pdfRenderTimeout})
	if err != nil {
		return "", false
	}
	paths, total := parsePDFRenderOutput(out)
	if len(paths) == 0 {
		return "", false
	}
	// 落盘校验：脚本说写了 ≠ 真在磁盘上（部分写入/被删/路径异常都可能）。交出去的
	// 路径必须是**模型真能 read_file 成功**的，故逐个确认存在且是 JPEG —— 这条正文
	// 就是我们对模型的一份承诺，不能让它去撞一个坏路径白耗一轮。
	kept := make([]string, 0, len(paths))
	for _, p := range paths {
		if jpegMagicOK(p) {
			kept = append(kept, p)
		}
	}
	// 页数上限在 Go 侧再钳一次（脚本里的 MAX_PAGES 是第一道闸）：Go 常量是这条产品决策的
	// 事实源，两处万一漂移（有人只改了脚本），这里保证交给模型的正文里最多 5 条路径 ——
	// 宁可少给一张，也不让「前 5 页」这个承诺随脚本改动悄悄变成 8 页。
	if len(kept) > pdfImagePagesMax {
		kept = kept[:pdfImagePagesMax]
	}
	if len(kept) == 0 {
		return "", false
	}
	sweepStalePDFImageDirs(pyh, imgDir)
	ok = true
	return pdfRenderNotice(kept, total), true
}

// pdfRenderNotice 组装「已渲染为图片」的正文（协议：路径一行一条，末行告诉模型下一步）。
//
// 为什么逐行给路径而不是嵌进一句话：模型后续要拿这些路径调 read_file，独立成行最容易
// 被准确复制（与 mkdir/list 类结果的惯例一致）。
//
// 措辞上刻意写「逐张查看」：引擎单次工具结果最多携带 4 张图（tools.defaultMaxResultImages），
// 让模型一次读 5 张会静默丢第 5 张；逐张读天然在限内。
func pdfRenderNotice(paths []string, total int) string {
	var b strings.Builder
	if total > len(paths) {
		fmt.Fprintf(&b, "[该 PDF 无文本层（扫描件/整页图片）。已将前 %d 页（共 %d 页）渲染为图片：\n", len(paths), total)
	} else {
		fmt.Fprintf(&b, "[该 PDF 无文本层（扫描件/整页图片）。已将全部 %d 页渲染为图片：\n", len(paths))
	}
	for _, p := range paths {
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString("可用 read_file 逐张查看这些图片（每次一张，文件名里的序号即页码）。]")
	return b.String()
}

// mkdirPDFImageDir 在受管临时目录下建一个**长期保留**的产物目录（pdf-scan-*）。
//
// 选址理由与 builtin.tempDirBases 完全一致（那边未导出，此处留一份最小实现）：
// <运行时根>/tmp 是 Seatbelt 的 carve-out（defaultOpenSubpaths 放行 ~/.go-code/runtime，
// 读写两开），只要 venv 解释器能跑，这个目录就一定可写；系统 tmp 作为兜底（NoSandbox /
// full-access 等无沙箱场景下同样可用，且 read_file 在主进程里读它不受沙箱限制）。
//
// 不落工作区：渲染产物不是用户资产，不该出现在 git status / 文件树 / 产出卡片里
// —— 与 xlsx 预览产物同一条理由。
func mkdirPDFImageDir(pyh goruntime.Python) (string, error) {
	var bases []string
	if pyh.Root != "" {
		bases = append(bases, filepath.Join(pyh.Root, "tmp"))
	}
	bases = append(bases, os.TempDir())

	var lastErr error
	for _, base := range bases {
		if base == "" {
			continue
		}
		// 基目录必须存在（MkdirTemp 不建父目录）：<运行时根>/tmp 首次调用时通常还没有。
		if err := os.MkdirAll(base, 0o700); err != nil {
			lastErr = err
			continue
		}
		dir, err := os.MkdirTemp(base, pdfImageDirPrefix)
		if err != nil {
			lastErr = err
			continue
		}
		return dir, nil
	}
	return "", fmt.Errorf("创建渲染产物目录失败: %w", lastErr)
}

// sweepStalePDFImageDirs 清扫受管临时目录里**过期的** pdf-scan-* 目录（keep 除外）。
// 尽力而为：目录不存在/无权限/单项删除失败都不影响本次渲染（图已经产出）。
//
// 为什么按年龄而不是「这次清上次」：渲染是**并发可达**的（两个会话同时引用两份扫描件），
// 按「上次是谁」清会删掉另一个会话正要 read_file 的图。年龄门槛的具体取值理由见 pdfImageStaleAge。
func sweepStalePDFImageDirs(pyh goruntime.Python, keep string) {
	var bases []string
	if pyh.Root != "" {
		bases = append(bases, filepath.Join(pyh.Root, "tmp"))
	}
	bases = append(bases, os.TempDir())
	for _, base := range bases {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), pdfImageDirPrefix) {
				continue
			}
			p := filepath.Join(base, e.Name())
			if p == keep {
				continue
			}
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < pdfImageStaleAge {
				continue
			}
			_ = os.RemoveAll(p)
		}
	}
}

// jpegMagicOK 判定路径上确实是（非空的）JPEG 文件。
// 只读前 3 字节就够：渲染产物是我们自己刚写出来的完整文件，这里防的是「路径不存在/
// 内容被截断/指向了别的东西」，判据与 tools/builtin.detectImageFormat 的 JPEG 分支同源
// （read_file 正是按这个魔数认图的）。
func jpegMagicOK(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [3]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return false
	}
	return head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF
}

// parsePDFRenderOutput 解析渲染脚本输出：返回（产物路径按页序, 声明的总页数）。
//
// 判定严格且顺序固定（协议见 pdfRenderScript 的常量注释）：
//  1. 没有页数标记行 → 脚本没走完（沙箱拦截/解释器崩溃/超时）→ 空；
//  2. 没有 OK 标记行 → 「一张都没写出来」（脚本 exit 4）→ 空 → 调用方降级回「无文本层」；
//  3. 有 OK 标记 → 收集 image 标记行的路径（畸形行直接跳过：宁可少给一张，也不给一条
//     解析错的路径——那会让模型去撞一个不存在的文件）。
//
// 注意 [exit: ...] 尾巴：sandbox 约定非零退出不返回 error，而是把原因并进输出文本
// （同 parsePDFExtractOutput 的处理）。
func parsePDFRenderOutput(out string) ([]string, int) {
	paths, _, total := parsePDFRenderOutputDetailed(out)
	return paths, total
}

// parsePDFRenderOutputDetailed 同 parsePDFRenderOutput，另返回每页脚本**实际用的渲染
// 倍率**（页序对齐）。
//
// 为什么要单独留一条带 scale 的解析：位图最终尺寸由脚本里的 clamp 兜底，故「固定 scale」
// 与「按长边反算」产出的图**像素尺寸相同**（前者多绕一次无用重采样），只有 scale 本身能
// 区分这两种实现 —— 生产代码不需要它（路径就够），测试需要（钉住设计决策）。
func parsePDFRenderOutputDetailed(out string) ([]string, []float64, int) {
	total, seenPages, sawOK := -1, false, false
	var paths []string
	var scales []float64
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case strings.HasPrefix(l, pdfPageMarker):
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(l, pdfPageMarker))); err == nil {
				total, seenPages = n, true
			}
		case strings.HasPrefix(l, pdfRenderImageMarker):
			// 协议：页号 \t scale \t 路径（scale 解析失败不丢整行 —— 路径才是生产在意的）
			rest := strings.TrimPrefix(l, pdfRenderImageMarker)
			parts := strings.Split(rest, "\t")
			if len(parts) < 3 {
				continue
			}
			p := strings.TrimSpace(parts[2])
			if p == "" {
				continue
			}
			paths = append(paths, p)
			if sc, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
				scales = append(scales, sc)
			} else {
				scales = append(scales, 0)
			}
		case strings.HasPrefix(l, pdfRenderOKMarker):
			sawOK = true
		}
	}
	if !seenPages || !sawOK {
		return nil, nil, 0
	}
	return paths, scales, total
}

// parsePDFExtractOutput 解析脚本输出（协议见 pdfExtractScript 的常量注释）。
//
// 判定顺序即三态协议的映射：
//  1. 没有页数标记行 → err：脚本没走完（沙箱拦截/解释器崩溃/超时/加密解析失败），
//     是环境或格式问题，调用方回退既有二进制占位；
//  2. 页数标记 + 无文本层标记 → ("", false, nil)：抽取器可用但没文字（扫描件），
//     调用方须显式告知用户；
//  3. 页数标记 + 正文 → (正文, true, nil)，首行标注总页数。
//
// 注意 [exit: ...] 尾巴：sandbox 约定非零退出不返回 error，而是把原因并进输出文本
// （见 sandbox.Sandbox 文档）—— 故不能「看到子串就判失败」，只看协议标记行在不在。
func parsePDFExtractOutput(out string) (string, bool, error) {
	pages, body := -1, ""
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if !strings.HasPrefix(l, pdfPageMarker) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(l, pdfPageMarker)))
		if err != nil {
			return "", false, fmt.Errorf("PDF 抽取输出异常（页数不可解析）: %q", l)
		}
		pages = n
		body = strings.Join(lines[i+1:], "\n")
		break
	}
	if pages < 0 {
		return "", false, fmt.Errorf("PDF 文本抽取失败: %s", singleLineOutput(out))
	}
	body = strings.TrimSpace(body)
	if body == "" || strings.HasPrefix(body, pdfNoTextMarker) {
		return "", false, nil // 可用但无文本层（扫描件/纯图）
	}
	return fmt.Sprintf("[PDF 文本，共 %d 页]\n%s", pages, body), true, nil
}

// singleLineOutput 把多行输出压成单行（错误文本会进 FileContent 与日志，多行会破格式）。
// 与 tools/builtin.singleLine 同口径（那边未导出，此处留一份最小实现）。
func singleLineOutput(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\r", " ")), " ")
}
