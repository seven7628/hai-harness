package agenthub

// attachment.go：Hub 消息附件（图片 / 文件引用）。
//
// 设计取舍：
//   - 图片一律以 data URL 内联（不接受 http(s) URL）：它既要进模型上下文，
//     又要在刷新后由前端直接渲染，外链既不保证可达、也会把「模型看到什么」
//     交给远端决定。总量与主会话传输层闸门 maxImageBlockBytes 同量级（3MiB）。
//   - 文件只是**引用**：name/path 元数据，后端不读内容、不做路径校验之外的
//     文件访问。前端把文件引用拼成 `@绝对路径` 放进正文，模型按正文自行读取
//     （与主会话的 @ 文件语义一致）。
//   - 附件随消息走 Redacted()：owner-only 消息的附件同样不能泄给其他智能体。

import (
	"fmt"
	"strings"
)

const (
	AttachmentKindImage = "image"
	AttachmentKindFile  = "file"

	// MaxMessageAttachments 单条消息的附件数上限。
	MaxMessageAttachments = 10

	// MaxAttachmentImageBytes 单条消息内全部 image data URL 的总字节上限（含
	// `data:...;base64,` 前缀）。与 desktop/bridge 的 maxImageBlockBytes 同口径：
	// 整条命令经 stdin 传输（4MiB 硬限），base64 膨胀 4/3，留 ~1MiB 给正文。
	MaxAttachmentImageBytes = 3 * 1024 * 1024

	// MaxAttachmentNameLen / MaxAttachmentPathLen 元数据长度上限：附件会随消息
	// 写进 JSONL 并回给前端，超长字段只会把索引撑大，没有业务价值。
	MaxAttachmentNameLen = 512
	MaxAttachmentPathLen = 4096
)

// attachmentImageMIME 是允许内联的图片类型（与前端压缩产物、provider 支持面一致）。
var attachmentImageMIME = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/gif":  true,
}

// Attachment 是 Hub DM/群消息的一个附件引用。
//
//	{kind:"image", name?, mime_type?, bytes?, data_url}
//	{kind:"file",  name?, path?, bytes?}
//
// 字段名是全栈协议（前端/Mock 已按 kind/name/mime_type/bytes/data_url/path 对齐）。
type Attachment struct {
	Kind    string `json:"kind"`                // image | file
	Name    string `json:"name,omitempty"`      // 展示用文件名
	MIME    string `json:"mime_type,omitempty"` // image 用
	Bytes   int64  `json:"bytes,omitempty"`     // 原始字节数（展示用，不参与校验）
	DataURL string `json:"data_url,omitempty"`  // image only：data:image/...;base64,...
	Path    string `json:"path,omitempty"`      // file ref（绝对路径优先）
}

// IsImage 判断是否图片附件（唯一会进模型上下文的类型）。
func (a Attachment) IsImage() bool { return a.Kind == AttachmentKindImage }

// ImageMIME 返回图片附件的 MIME：优先显式 mime_type，否则取 data URL 的媒体类型。
func (a Attachment) ImageMIME() string {
	if m := strings.ToLower(strings.TrimSpace(a.MIME)); m != "" {
		return m
	}
	if mime, _, ok := parseAttachmentDataURL(a.DataURL); ok {
		return mime
	}
	return ""
}

// validateAttachmentMeta 校验两类附件共有的元数据。
func (a Attachment) validateAttachmentMeta() error {
	if len(a.Name) > MaxAttachmentNameLen {
		return fmt.Errorf("%w: attachment.name 过长（%d > %d 字节）", ErrInvalid, len(a.Name), MaxAttachmentNameLen)
	}
	if len(a.Path) > MaxAttachmentPathLen {
		return fmt.Errorf("%w: attachment.path 过长（%d > %d 字节）", ErrInvalid, len(a.Path), MaxAttachmentPathLen)
	}
	if a.Bytes < 0 {
		return fmt.Errorf("%w: attachment.bytes 不能为负", ErrInvalid)
	}
	return nil
}

// Validate 校验单个附件。所有错误都包裹 ErrInvalid。
func (a Attachment) Validate() error {
	if err := a.validateAttachmentMeta(); err != nil {
		return err
	}
	switch a.Kind {
	case AttachmentKindImage:
		return a.validateImage()
	case AttachmentKindFile:
		return a.validateFile()
	default:
		return fmt.Errorf("%w: attachment.kind 必须是 image 或 file", ErrInvalid)
	}
}

func (a Attachment) validateImage() error {
	if strings.TrimSpace(a.DataURL) == "" {
		return fmt.Errorf("%w: image 附件必须带 data_url（不接受 http(s) 外链）", ErrInvalid)
	}
	mime, payload, ok := parseAttachmentDataURL(a.DataURL)
	if !ok {
		return fmt.Errorf("%w: image 附件 data_url 只接受 data:image/(png|jpeg|webp|gif);base64,...（不接受 http(s) 外链）", ErrInvalid)
	}
	if m := strings.ToLower(strings.TrimSpace(a.MIME)); m != "" {
		if !attachmentImageMIME[m] {
			return fmt.Errorf("%w: image 附件 mime_type %q 不支持", ErrInvalid, a.MIME)
		}
		if m != mime {
			// 显式 MIME 与 data URL 自相矛盾：provider 会按其中一个解析，拒绝比猜好。
			return fmt.Errorf("%w: image 附件 mime_type %q 与 data_url 的 %q 不一致", ErrInvalid, a.MIME, mime)
		}
	}
	if !isAttachmentBase64Payload(payload) {
		return fmt.Errorf("%w: image 附件 data_url 的 base64 负载非法", ErrInvalid)
	}
	if a.Path != "" {
		// 图片只从 data URL 取内容，path 只会诱导调用方去读文件。
		return fmt.Errorf("%w: image 附件不能带 path", ErrInvalid)
	}
	return nil
}

func (a Attachment) validateFile() error {
	if strings.TrimSpace(a.Name) == "" && strings.TrimSpace(a.Path) == "" {
		return fmt.Errorf("%w: file 附件至少要有 name 或 path", ErrInvalid)
	}
	if strings.TrimSpace(a.DataURL) != "" {
		// 文件只做引用，内联内容既不符合协议也绕过图片体积闸门。
		return fmt.Errorf("%w: file 附件不能带 data_url（只做路径引用）", ErrInvalid)
	}
	return nil
}

// ValidateAttachments 校验一条消息的全部附件：数量、逐项、图片总量。
func ValidateAttachments(atts []Attachment) error {
	if len(atts) > MaxMessageAttachments {
		return fmt.Errorf("%w: 单条消息最多 %d 个附件（收到 %d 个）", ErrInvalid, MaxMessageAttachments, len(atts))
	}
	total := 0
	for i, a := range atts {
		if err := a.Validate(); err != nil {
			return fmt.Errorf("附件 %d: %w", i+1, err)
		}
		if a.IsImage() {
			total += len(a.DataURL)
		}
	}
	if total > MaxAttachmentImageBytes {
		return fmt.Errorf("%w: 图片附件总大小 %d 字节超过上限 %d 字节", ErrInvalid, total, MaxAttachmentImageBytes)
	}
	return nil
}

// parseAttachmentDataURL 解析 `data:image/<type>;base64,<payload>`，只认允许的类型。
func parseAttachmentDataURL(s string) (mime, payload string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return "", "", false
	}
	header := s[len("data:"):comma]
	if !strings.HasSuffix(header, ";base64") {
		return "", "", false
	}
	mime = strings.ToLower(strings.TrimSuffix(header, ";base64"))
	if !attachmentImageMIME[mime] {
		return "", "", false
	}
	payload = s[comma+1:]
	if payload == "" {
		return "", "", false
	}
	return mime, payload, true
}

// isAttachmentBase64Payload 只做字符表/长度检查（不解码，避免为校验多分配 2.25MiB）：
// 非法字符或非法长度会让 provider/前端解码失败，宁可在这里拒绝。
//
// 允许无填充 base64（长度 mod 4 为 2/3）：有些前端/库会省略尾部 `=`；但带填充时
// 总长度必须是 4 的倍数，且 `=` 只能出现在末尾。这样 `A`、`QUJD=`、`==` 都会被拒。
func isAttachmentBase64Payload(s string) bool {
	if s == "" {
		return false
	}
	data := 0
	pad := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			if pad > 0 {
				return false // '=' 之后又出现数据字符
			}
			data++
		case c == '=':
			pad++
			if pad > 2 {
				return false
			}
		default:
			return false
		}
	}
	if data == 0 {
		return false
	}
	// 无填充时长度 mod 4 只能是 0/2/3；mod 1 一定是坏串。
	if len(s)%4 == 1 {
		return false
	}
	// 一旦出现填充，整串就应当是标准 4 字节对齐。
	if pad > 0 && len(s)%4 != 0 {
		return false
	}
	return true
}
