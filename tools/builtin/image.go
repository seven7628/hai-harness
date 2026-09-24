package builtin

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"path/filepath"
	"strings"

	"github.com/seven7628/hai-harness/core"

	// 标准库解码器注册（image.Decode / DecodeConfig 走注册表；png 由下方显式引用）。
	_ "image/gif"
	_ "image/png"
)

// 图片读取（read_file 的二进制分支）：
//   - 仅标准库可解码的格式可内嵌（png / jpeg / gif）；webp/heic/bmp 明确报错
//     并提示转换 —— 比丢一堆乱码或静默失败对模型有用。
//   - 超过体积闸门的图自动降采样（长边 ≤ maxImageDim、JPEG 重编码），
//     保证「视觉模型够看清」且不撞链路闸门（stdin 单行 / 事件日志 / 上下文）。
const (
	// maxImageDim 内嵌图片的长边像素上限。依据：主流视觉模型把长边 >1568px 的
	// 图像降采样到该量级后再编码成视觉 token —— 再大只是白烧字节数与上下文。
	maxImageDim = 1568
	// maxInlineImageBytes 单张内嵌图片 data URL 的字节上限（与 tools 引擎默认闸门
	// defaultMaxImageBytes 一致，3MiB：超了引擎会丢弃并把图挡在模型之外）。
	maxInlineImageBytes = 3 << 20
	// reencodeQuality JPEG 重编码质量（降采样路径；85 在文字/UI 截图上肉眼无损）。
	reencodeQuality = 85
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
	block core.Content // image 内容块（data URL）
	text  string       // 结果文本（路径/尺寸/编码说明 —— 模型据此知道看了什么）
}

// loadImageForModel 把文件读成可内嵌的图片内容块：
//   - 格式不可解码（webp/heic/bmp）→ error 说明（模型改用 bash 转换等替代路径）；
//   - 体积已达标 → 原样内嵌（不解码、不重编码，零质量损失）；
//   - 体积超限 → 降采样 + JPEG 重编码（长边 ≤ maxImageDim，必要时逐级缩小）。
//
// maxBytes = data URL 体积上限（0 = 用 maxInlineImageBytes）。
func loadImageForModel(path string, data []byte, maxBytes int) (imageContentResult, error) {
	if maxBytes <= 0 {
		maxBytes = maxInlineImageBytes
	}
	format, ok := detectImageFormat(data)
	if !ok {
		return imageContentResult{}, fmt.Errorf("%s is not a supported image format", path)
	}
	name := filepath.Base(path)

	// 原图已达标：直接内嵌（保持原始字节 —— PNG 截图无损，正是视觉模型最需要的）。
	if encodedLen(len(data)) <= maxBytes {
		dim := ""
		if w, h, err := decodeImageSize(data); err == nil {
			dim = fmt.Sprintf(", %dx%d", w, h)
		}
		return imageContentResult{
			block: core.Content{
				Type:     core.ContentTypeImage,
				Content:  "data:" + format.mime + ";base64," + base64.StdEncoding.EncodeToString(data),
				MimeType: format.mime,
			},
			text: fmt.Sprintf("[read_file: %s — %s%s, %s, embedded as-is for direct viewing]", name, format.mime, dim, humanSize(len(data))),
		}, nil
	}

	if !format.decodable {
		return imageContentResult{}, fmt.Errorf(
			"%s is %s (%s) and exceeds the inline limit (%s); this harness cannot re-encode %s. Convert it first (e.g. `sips -s format png`, `ffmpeg`, `cwebp -d`) or read the original source instead",
			name, format.mime, humanSize(len(data)), humanSize(maxBytes), format.ext)
	}

	// 超限且可解码 → 降采样重编码。
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return imageContentResult{}, fmt.Errorf("%s: decode %s failed: %w", name, format.mime, err)
	}
	bounds := src.Bounds()
	origW, origH := bounds.Dx(), bounds.Dy()

	encoded, outW, outH, err := shrinkToLimit(src, maxBytes)
	if err != nil {
		return imageContentResult{}, fmt.Errorf("%s: cannot shrink %dx%d %s image under %s: %w",
			name, origW, origH, format.mime, humanSize(maxBytes), err)
	}
	return imageContentResult{
		block: core.Content{
			Type:     core.ContentTypeImage,
			Content:  "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(encoded),
			MimeType: "image/jpeg",
		},
		text: fmt.Sprintf("[read_file: %s — %s %dx%d %s, downscaled to %dx%d JPEG %s to fit the inline limit]",
			name, format.mime, origW, origH, humanSize(len(data)), outW, outH, humanSize(len(encoded))),
	}, nil
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
