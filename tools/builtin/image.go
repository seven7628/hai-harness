package builtin

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"path/filepath"
	"strings"

	"github.com/seven7628/hai-harness/core"

	// 标准库解码器注册（image.Decode / DecodeConfig 走注册表；png/gif 由下方引用与
	// 空导入共同保证：png 要显式引用（切片无重编码走 png.Encode），gif 只需注册）。
	_ "image/gif"
)

// 图片读取（read_file 的二进制分支）：
//   - 仅标准库可解码的格式可内嵌（png / jpeg / gif）；webp/heic/bmp 明确报错
//     并提示转换 —— 比丢一堆乱码或静默失败对模型有用。
//   - 超过**体积**闸门的图自动降采样（长边 ≤ maxImageDim、JPEG 重编码）。
//   - 超过**单边**闸门（maxImageSidePx）的图切片而不是缩小 —— 全页截图是窄高图，
//     等比缩到 1568 会长边达标而正文不可读（见 sliceTallImage）。
//
// 两道闸门互相独立：体积达标 ≠ 尺寸达标。2026-11-04 事故正是「体积达标所以原样
// 内嵌」的旧判据放行了一张 390x9697 的合法 PNG，上游直接 400（且报错文案谎称格式错）。
const (
	// maxImageDim 内嵌图片的长边像素上限。依据：主流视觉模型把长边 >1568px 的
	// 图像降采样到该量级后再编码成视觉 token —— 再大只是白烧字节数与上下文。
	maxImageDim = 1568
	// maxInlineImageBytes 单张内嵌图片 data URL 的字节上限（与 tools 引擎默认闸门
	// defaultMaxImageBytes 一致，3MiB：超了引擎会丢弃并把图挡在模型之外）。
	maxInlineImageBytes = 3 << 20
	// reencodeQuality JPEG 重编码质量（降采样路径；85 在文字/UI 截图上肉眼无损）。
	reencodeQuality = 85
	// maxImageSidePx 单边像素硬上限（判据与实测见 core.MaxImageSidePx：任一边 >8192
	// 上游直接 400，而报错文案谎称是格式错误）。取 8192 而不是 maxImageDim，是为了
	// 不砍掉本来就合格的常规长截图（1440x4151 / 2560x5644 上游都收，模型也读得出来）。
	maxImageSidePx = core.MaxImageSidePx
	// maxImageTiles 高窄超限图的切片数上限（与 tools.defaultMaxResultImages=4 同一
	// 口径：引擎在结果出口按 4 张卡上限，切更多也会被丢弃并告警）。
	maxImageTiles = 4
)

// imageFormat 图片格式说明（内嵌可用性 + 重编码能力）。
type imageFormat struct {
	mime      string
	ext       string
	decodable bool // 标准库可解码（可降采样重编码）
}

// detectImageFormat 按魔数识别图片格式（比扩展名可靠：扩展名可缺失或写错）。
// 返回 (格式, 是否图片)。非图片返回 false（调用方继续按文本处理）。
func detectImageFormat(data []byte) (imageFormat, bool) {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return imageFormat{mime: "image/png", ext: ".png", decodable: true}, true
	case len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF:
		return imageFormat{mime: "image/jpeg", ext: ".jpg", decodable: true}, true
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return imageFormat{mime: "image/gif", ext: ".gif", decodable: true}, true
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return imageFormat{mime: "image/webp", ext: ".webp"}, true
	case len(data) >= 2 && data[0] == 'B' && data[1] == 'M':
		return imageFormat{mime: "image/bmp", ext: ".bmp"}, true
	case len(data) >= 12 && bytes.Equal(data[4:12], []byte("ftypheic")):
		return imageFormat{mime: "image/heic", ext: ".heic"}, true
	}
	return imageFormat{}, false
}

// looksBinary 判断是否二进制内容（文本工具路径的兜底）：含 NUL 字节，或前 8KB 中
// 不可打印字节占比过高。避免 read_file 把二进制当文本倒出成乱码（浪费上下文且无用）。
// looksBinary 判定「非文本」内容（含 NUL 字节，或前 8KB 不可打印字符占比 > 30%）。
//
// 导出给宿主复用（LookBinary）：desktop bridge 的 file_preview 需要同一套判定，
// 否则二进制文件（xlsx/docx/图片）会被当字符串读出来塞进前端 shiki —— 显示乱码
// 且白耗渲染。两处若各写一份，判据迟早漂移。
func looksBinary(data []byte) bool {
	return LookBinary(data)
}

// LookBinary 见 looksBinary 的说明（对外唯一实现）。
func LookBinary(data []byte) bool {
	head := data
	if len(head) > 8*1024 {
		head = head[:8*1024]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return true
	}
	if len(head) == 0 {
		return false
	}
	nonPrintable := 0
	for _, b := range head {
		// 允许制表/换行/回车等常规空白与 UTF-8 高位字节（多字节字符）。
		if b < 0x09 || (b > 0x0D && b < 0x20) || b == 0x7F {
			nonPrintable++
		}
	}
	return nonPrintable*100/len(head) > 30
}

// imageContentResult 一次图片内嵌的结果。
type imageContentResult struct {
	blocks []core.Content // image 内容块（data URL）；高窄超限图可能有多块（切片）
	text   string         // 结果文本（路径/尺寸/编码说明 —— 模型据此知道看了什么）
}

// loadImageForModel 把文件读成可内嵌的图片内容块：
//   - 格式不可解码（webp/heic/bmp）→ error 说明（模型改用 bash 转换等替代路径）；
//   - 体积达标 + 单边 ≤ maxImageSidePx → 原样内嵌（不解码、不重编码，零质量损失）；
//   - 体积达标但单边超限（窄高全页截图）→ **按原生分辨率切片**，见 sliceTallImage；
//   - 体积超限 → 降采样 + JPEG 重编码（长边 ≤ maxImageDim，必要时逐级缩小）。
//
// maxBytes = 单张 data URL 的体积上限（0 = 用 maxInlineImageBytes）。
func loadImageForModel(path string, data []byte, maxBytes int) (imageContentResult, error) {
	if maxBytes <= 0 {
		maxBytes = maxInlineImageBytes
	}
	format, ok := detectImageFormat(data)
	if !ok {
		return imageContentResult{}, fmt.Errorf("%s is not a supported image format", path)
	}
	name := filepath.Base(path)
	w, h, sizeErr := decodeImageSize(data)
	sizeOK := encodedLen(len(data)) <= maxBytes
	// 单边闸门的三态：
	//   - 读得出尺寸 → 按 8192 判；
	//   - 读不出且**可解码**（png/jpeg/gif 头坏了）→ 当尺寸不达标处理，走下方解码报错，
	//     不把坏图原样发出去；
	//   - 读不出且**不可解码**（webp/bmp/heic，标准库没有解码器）→ 无从判断，保持旧行为
	//     「体积达标就原样内嵌」（webp 本来就在上游支持列表里，不能因为量不到就拒）。
	sideOK := true
	switch {
	case sizeErr == nil:
		sideOK = maxInt(w, h) <= maxImageSidePx
	case format.decodable:
		sideOK = false
	}

	// 原图双闸门都达标：直接内嵌（保持原始字节 —— PNG 截图无损，正是视觉模型最需要的）。
	// 注意两道闸门必须同时判：只看体积会放行 390x9697 这种 837KB 的超限图（上游 400）。
	if sizeOK && sideOK {
		dim := ""
		if sizeErr == nil {
			dim = fmt.Sprintf(", %dx%d", w, h)
		}
		return imageContentResult{
			blocks: []core.Content{{
				Type:     core.ContentTypeImage,
				Content:  "data:" + format.mime + ";base64," + base64.StdEncoding.EncodeToString(data),
				MimeType: format.mime,
			}},
			text: fmt.Sprintf("[read_file: %s — %s%s, %s, embedded as-is for direct viewing]", name, format.mime, dim, humanSize(len(data))),
		}, nil
	}

	if !format.decodable {
		return imageContentResult{}, fmt.Errorf(
			"%s is %s (%s) and exceeds the inline limit (%s); this harness cannot re-encode %s. Convert it first (e.g. `sips -s format png`, `ffmpeg`, `cwebp -d`) or read the original source instead",
			name, format.mime, humanSize(len(data)), humanSize(maxBytes), format.ext)
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return imageContentResult{}, fmt.Errorf("%s: decode %s failed: %w", name, format.mime, err)
	}

	// 体积达标、只是单边超限 → 切片（保住原生分辨率），不缩成一条看不懂的细线。
	if sizeOK {
		return sliceTallImage(name, format, src, data, maxBytes)
	}

	// 体积超限 → 降采样重编码（原行为）。
	bounds := src.Bounds()
	origW, origH := bounds.Dx(), bounds.Dy()

	encoded, outW, outH, err := shrinkToLimit(src, maxBytes)
	if err != nil {
		return imageContentResult{}, fmt.Errorf("%s: cannot shrink %dx%d %s image under %s: %w",
			name, origW, origH, format.mime, humanSize(maxBytes), err)
	}
	return imageContentResult{
		blocks: []core.Content{{
			Type:     core.ContentTypeImage,
			Content:  "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(encoded),
			MimeType: "image/jpeg",
		}},
		text: fmt.Sprintf("[read_file: %s — %s %dx%d %s, downscaled to %dx%d JPEG %s to fit the inline limit]",
			name, format.mime, origW, origH, humanSize(len(data)), outW, outH, humanSize(len(encoded))),
	}, nil
}

// maxInt 两个 int 取大（Go 1.21 的 max 只对同类型内置，这里保持显式）。
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// sliceTallImage 单边超限的图按原生分辨率切成 ≤ maxImageTiles 片，每片单边 ≤ maxImageSidePx。
//
// 为什么切而不缩：全页截图是窄高图（390x9697 这类）。等比缩到长边 1568 会得到
// 63x1568 —— 边长「达标」了，但正文字号被压成 1px 宽的竖条，模型什么都读不出来；
// 而原生分辨率的一刀切（390x4849）在视觉模型里是清晰的。所以先切，切不下才缩
// （片数上限 4 与引擎的结果图片上限同口径）。
func sliceTallImage(name string, format imageFormat, src image.Image, data []byte, maxBytes int) (imageContentResult, error) {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	rects := tileRects(w, h, maxImageSidePx, maxImageTiles)

	blocks := make([]core.Content, 0, len(rects))
	dims := make([]string, 0, len(rects))
	native := true // 是否保持了原生分辨率（片数封顶时会降采样，文案不能说谎）
	for n, r := range rects {
		tile := cropImage(src, r)
		raw, mime, tw, th, err := encodeTile(tile, maxBytes)
		if err != nil {
			return imageContentResult{}, fmt.Errorf("%s: cannot slice %dx%d %s image (tile %d/%d): %w",
				name, w, h, format.mime, n+1, len(rects), err)
		}
		blocks = append(blocks, core.Content{
			Type:     core.ContentTypeImage,
			Content:  "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw),
			MimeType: mime,
		})
		if tw != r.Dx() || th != r.Dy() {
			native = false
		}
		dims = append(dims, fmt.Sprintf("%dx%d", tw, th))
	}
	resolution := "original resolution"
	if !native {
		resolution = "downscaled to fit the tile limit"
	}
	return imageContentResult{
		blocks: blocks,
		text: fmt.Sprintf("[read_file: %s — %s %dx%d %s, single side exceeds the %dpx gateway limit → split into %d tiles (%s, same page top to bottom, %s) for direct viewing]",
			name, format.mime, w, h, humanSize(len(data)), maxImageSidePx, len(rects), strings.Join(dims, ", "), resolution),
	}, nil
}

// tileRects 沿长边把 w×h 切成 ≤ maxTiles 块，使每块长边尽量 ≤ maxSide。
// 片数先按 maxSide 求（能切就切到合法），超过 maxTiles 才退化为「片数封顶 +
// 每片可能仍超限」——后者由 encodeTile 再降采样兜底。
func tileRects(w, h, maxSide, maxTiles int) []image.Rectangle {
	long, horizontal := h, false
	if w > h {
		long, horizontal = w, true
	}
	if long <= maxSide || maxTiles <= 1 {
		return []image.Rectangle{image.Rect(0, 0, w, h)}
	}
	n := (long + maxSide - 1) / maxSide
	if n > maxTiles {
		n = maxTiles
	}
	rects := make([]image.Rectangle, 0, n)
	for i := 0; i < n; i++ {
		if horizontal {
			rects = append(rects, image.Rect(w*i/n, 0, w*(i+1)/n, h))
			continue
		}
		rects = append(rects, image.Rect(0, h*i/n, w, h*(i+1)/n))
	}
	return rects
}

// cropImage 按矩形裁剪（整图等价时零拷贝返回原图）。
func cropImage(src image.Image, r image.Rectangle) image.Image {
	b := src.Bounds()
	if r == b {
		return src
	}
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(dst, dst.Bounds(), src, r.Min, draw.Src)
	return dst
}

// encodeTile 把一片图编码成能过闸门的 data URL 主体：
// 片内单边仍超限（片数封顶时）→ 先缩到 maxSide；PNG 优先（无损，截图文字最怕 JPEG），
// PNG 体积过不了闸门才退 JPEG q85，再不行交给 shrinkToLimit。
func encodeTile(img image.Image, maxBytes int) (data []byte, mime string, w, h int, err error) {
	bb := img.Bounds()
	if maxInt(bb.Dx(), bb.Dy()) > maxImageSidePx {
		img = scaleToFit(img, maxImageSidePx)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err == nil && encodedLen(buf.Len()) <= maxBytes {
		b := img.Bounds()
		return buf.Bytes(), "image/png", b.Dx(), b.Dy(), nil
	}
	buf.Reset()
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: reencodeQuality}); err == nil && encodedLen(buf.Len()) <= maxBytes {
		b := img.Bounds()
		return buf.Bytes(), "image/jpeg", b.Dx(), b.Dy(), nil
	}
	raw, ow, oh, err := shrinkToLimit(img, maxBytes)
	if err != nil {
		return nil, "", 0, 0, err
	}
	return raw, "image/jpeg", ow, oh, nil
}

// shrinkToLimit 逐级降采样直到 data URL 体积达标：
// 首轮把长边钳到 maxImageDim（视觉模型有效分辨率），之后每次减半，最多 6 轮
// （1568 → 784 → 392 …；配合 JPEG q85 足以把任意合理截图压到 3MiB 以内）。
// 每轮先试质量，再试尺寸 —— 避免为了达标把本该清晰的图压成糊图。
func shrinkToLimit(src image.Image, maxBytes int) ([]byte, int, int, error) {
	longest := src.Bounds().Dx()
	if src.Bounds().Dy() > longest {
		longest = src.Bounds().Dy()
	}
	dim := maxImageDim
	if longest < dim {
		dim = longest
	}
	var lastLen int
	for attempt := 0; attempt < 6; attempt++ {
		scaled := scaleToFit(src, dim)
		// 先按固定质量编一次；仍超限则降到 q60（文字截图降质量比降尺寸更保可读性）。
		for _, q := range []int{reencodeQuality, 60, 45} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: q}); err != nil {
				return nil, 0, 0, err
			}
			lastLen = buf.Len()
			if encodedLen(buf.Len()) <= maxBytes {
				return buf.Bytes(), scaled.Bounds().Dx(), scaled.Bounds().Dy(), nil
			}
		}
		dim /= 2
		if dim < 64 {
			break
		}
	}
	_ = lastLen
	return nil, 0, 0, fmt.Errorf("still %s after downscaling", humanSize(lastLen))
}

// encodedLen base64 编码后的长度（data URL 主体的字节数）。
func encodedLen(raw int) int {
	return base64.StdEncoding.EncodedLen(raw)
}

// decodeImageSize 只读图片头部取尺寸（不解码像素，大图也零成本）。
func decodeImageSize(data []byte) (int, int, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// scaleToFit 等比缩放到长边 = dim（原图更小则原样返回）。
// 使用面积平均（box filter）降采样：对文字/UI 截图能保住笔画连续性，明显优于最近邻。
func scaleToFit(src image.Image, dim int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	longest := w
	if h > longest {
		longest = h
	}
	if longest <= dim || longest == 0 {
		return src
	}
	dstW := w * dim / longest
	dstH := h * dim / longest
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	return boxResize(src, dstW, dstH)
}

// boxResize 面积平均降采样：每个目标像素 = 覆盖到的源像素平均值
// （降采样可安全跳过插值；此处只用于缩小，故不需要上采样路径）。
func boxResize(src image.Image, dstW, dstH int) *image.RGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	if sw == 0 || sh == 0 {
		return dst
	}
	for dy := 0; dy < dstH; dy++ {
		y0 := sb.Min.Y + dy*sh/dstH
		y1 := sb.Min.Y + (dy+1)*sh/dstH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		if y1 > sb.Max.Y {
			y1 = sb.Max.Y
		}
		for dx := 0; dx < dstW; dx++ {
			x0 := sb.Min.X + dx*sw/dstW
			x1 := sb.Min.X + (dx+1)*sw/dstW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			if x1 > sb.Max.X {
				x1 = sb.Max.X
			}
			var r, g, b, a uint64
			var n uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					cr, cg, cb, ca := src.At(x, y).RGBA()
					r += uint64(cr) // 16-bit 预乘 alpha；下同
					g += uint64(cg)
					b += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				continue
			}
			// RGBA() 返回 16 位预乘 alpha，与 color.RGBA 的语义一致 → 平均值右移 8 位。
			dst.SetRGBA(dx, dy, color.RGBA{
				R: uint8(r / n >> 8), G: uint8(g / n >> 8),
				B: uint8(b / n >> 8), A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}

// humanSize 人类可读字节数。
func humanSize(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// imageExtHint 扩展名提示（错误信息里告诉模型「这个扩展名通常是图片」，
// 帮助它在魔数无法识别（如损坏/空文件）时判断意图）。
func imageExtHint(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".heic", ".tif", ".tiff", ".avif":
		return true
	}
	return false
}
