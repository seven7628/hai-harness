package main

// hub_attachments.go：Hub DM/群消息的附件协议（图片来源进模型上下文）。
//
// 前端把文件引用拼成 `@绝对路径` 放进正文，后端**不读文件**：这里只做协议解析、
// 校验与图片块装配。纯图片消息（正文为空）用占位正文 `[图片]` 落账 —— 群快照/
// perception 依赖正文非空判断「可用消息」，占位符让纯图消息不被当成空消息丢掉。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
)

const (
	// hubImageOnlyText 纯图片消息的占位正文（前端可识别并隐藏该气泡文字）。
	hubImageOnlyText = "[图片]"
	// hubFileOnlyText 纯文件引用消息的占位正文（群消息允许只有附件）。
	hubFileOnlyText = "[文件]"
)

// hubAttachmentsFromPayload 解析 payload.attachments（JSON 数组）。
// 缺字段/为 null → nil（旧消息行为完全不变）；类型不对 → ErrInvalid。
func hubAttachmentsFromPayload(payload map[string]any) ([]agenthub.Attachment, error) {
	raw, ok := payload["attachments"]
	if !ok || raw == nil {
		return nil, nil
	}
	if atts, ok := raw.([]agenthub.Attachment); ok {
		return atts, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: attachments 必须是附件对象数组", agenthub.ErrInvalid)
	}
	var atts []agenthub.Attachment
	if err := json.Unmarshal(b, &atts); err != nil {
		return nil, fmt.Errorf("%w: attachments 必须是附件对象数组", agenthub.ErrInvalid)
	}
	return atts, nil
}

// hubParseAttachments 解析并校验附件；任何失败都不落账。
func hubParseAttachments(payload map[string]any) ([]agenthub.Attachment, error) {
	atts, err := hubAttachmentsFromPayload(payload)
	if err != nil {
		return nil, err
	}
	if err := agenthub.ValidateAttachments(atts); err != nil {
		return nil, err
	}
	return atts, nil
}

// hubHasImageAttachment 判断是否有图片附件（纯图消息允许正文为空）。
func hubHasImageAttachment(atts []agenthub.Attachment) bool {
	for _, a := range atts {
		if a.IsImage() {
			return true
		}
	}
	return false
}

// 群唤醒补发图片的上限：游标之后可能积压多条带图消息（目标忙、无 @ 纯图等），
// 一次性把所有 data URL 塞进请求会撑爆 provider。优先保证当前触发消息的图片，
// 更早的图片按「从新到旧」补，超出的在上下文里显式说明，不静默丢。
const (
	// hubWakePendingImageLimit 与前端单条消息的附件张数上限对齐（MAX_IMAGE_ATTACHMENTS=10，
	// 见 desktop/app/src/lib/imageAttach.ts）—— 前端能发 10 张、补发只给 6 张是自相矛盾的口径。
	hubWakePendingImageLimit = 10
	// hubWakePendingImageBytes 是单轮补发的图片字节预算：前端单条消息 3MiB（采集压缩后的
	// 硬闸门，MAX_MESSAGE_IMAGE_BYTES）× 3 条 = 9MiB。上界由事件行容量决定：补发图要和
	// 触发消息自身的图（≤3MiB）落在同一条 user_inputs_consumed 事件行里，必须留在
	// maxEventLineBytes（16MiB）以内（见 snapshot.go）。
	hubWakePendingImageBytes = 9 * 1024 * 1024
)

// 积压补发的**扫描**预算：游标之后可能积压任意多条消息，而 ReadMessages 单页上限是
// 500（agenthub.maxMessageLimit）。旧实现只读最近 500 条，第 501 条再往前的图片永远
// 补不上（场景 5-3）。这里从新往旧分页翻，直到覆盖游标或撞上预算 —— 撞预算时
// 显式标记 truncated（模型必须知道「还有更早的消息没读」），不静默丢。
const (
	hubWakeBacklogPage = 500 // 单页条数：与 store 的 maxMessageLimit 对齐，一页翻满
	// hubWakeBacklogMessages 是单轮最多扫描的消息条数（长群不能把 wake 拖成 O(全群)）。
	hubWakeBacklogMessages = 5000
	// hubWakeBacklogBytes 是单轮最多扫描的图片字节数（每页都可能带 data URL）。
	// 预算按条评估，但单页是 store 一次性返回的 —— 极端情况下（一页 500 条全是巨图）
	// 内存峰值由单页大小决定，不由这个预算决定。
	hubWakeBacklogBytes = 64 << 20
)

// hubPendingImages 是一次积压图片补发的结果。
type hubPendingImages struct {
	atts []agenthub.Attachment // 时间正序（模型按「先发生先看到」阅读，与快照一致）
	seqs []int64               // 与 atts 对应消息的 seq，按首次出现去重、时间正序
	// omitted 是因张数/字节预算没进上下文的图片数。图片一旦被读出来就能数清楚，
	// 所以这个数字是精确的，必须显式告诉模型。
	omitted int
	// truncated 表示扫描预算用完就停了：更早的消息**根本没读**，其中的图片数量未知，
	// 只能显式说明「还有更早的消息没看」，不能假装没有。
	truncated bool
	scanned   int // 已扫描的消息条数（含被可见性/kind 过滤掉的），诊断与测试用
	bytes     int // 已扫描的图片 data URL 字节数（含没采用的），扫描预算口径
}

// hubPendingImage 是被选中的一张补发图片及其消息 seq。
type hubPendingImage struct {
	att agenthub.Attachment
	seq int64
}

// hubPendingImagePicker 从新到旧累计补发图片，只保留预算内最新的那几张。
//
// 做成累加器而不是「先全读再排序」：分页读取永远只能先看到较新的消息，累加器保证
// 越界图片只计数不占内存，整个积压窗口（可能有几千条消息）不会被搬进内存。
type hubPendingImagePicker struct {
	newest  []hubPendingImage // 从新到旧
	total   int
	omitted int
}

// addImages 收一条消息的图片：消息内也从后往前（同一条消息的多张图同样「新的优先」）。
func (p *hubPendingImagePicker) addImages(m agenthub.Message) {
	for j := len(m.Attachments) - 1; j >= 0; j-- {
		a := m.Attachments[j]
		if !a.IsImage() || a.DataURL == "" {
			continue
		}
		if len(p.newest) >= hubWakePendingImageLimit || p.total+len(a.DataURL) > hubWakePendingImageBytes {
			p.omitted++
			continue
		}
		p.newest = append(p.newest, hubPendingImage{att: a, seq: m.Seq})
		p.total += len(a.DataURL)
	}
}

// result 反转成时间顺序，并按消息 seq 去重（同一消息多图只记一次 seq）。
func (p *hubPendingImagePicker) result() hubPendingImages {
	out := hubPendingImages{omitted: p.omitted}
	if len(p.newest) == 0 {
		return out
	}
	out.atts = make([]agenthub.Attachment, 0, len(p.newest))
	out.seqs = make([]int64, 0, len(p.newest))
	seen := map[int64]bool{}
	for i := len(p.newest) - 1; i >= 0; i-- {
		out.atts = append(out.atts, p.newest[i].att)
		if !seen[p.newest[i].seq] {
			seen[p.newest[i].seq] = true
			out.seqs = append(out.seqs, p.newest[i].seq)
		}
	}
	return out
}

// hubBacklogMessageEligible 判断一条消息的图片是否属于补发窗口：
// 游标之后、触发消息之前，且是智能体上下文里看得见的人话（纯工具轮/回执/审批占位、
// owner-only 消息都不采集）。
func hubBacklogMessageEligible(m agenthub.Message, cursor, triggerSeq int64) bool {
	if m.Seq <= cursor {
		return false
	}
	if triggerSeq > 0 && m.Seq >= triggerSeq {
		return false
	}
	return m.AgentContextKind() && m.VisibleTo(false)
}

// hubPendingImagesFromStore 从**游标之后的所有群消息**里收集补发图片。
//
// 分页从新往旧翻（ReadMessages 的 beforeSeq 独占），每页处理完只在最旧 seq 上继续
// 往前翻，直到：覆盖游标（更早的都已读过）/ 日志翻到头 / 撞上扫描预算。
//
// head 是调用方已经读到的最近一页（旧→新）——唤醒路径本来就要读它建快照，复用它
// 省掉一次重复读；传 nil 时自己读最新一页。
func hubPendingImagesFromStore(ctx context.Context, st agenthub.Store, gid agenthub.GroupID,
	head []agenthub.Message, cursor, triggerSeq int64) (hubPendingImages, error) {
	p := hubPendingImagePicker{}
	out := hubPendingImages{}
	page := head
	if len(page) == 0 {
		first, err := st.ReadMessages(ctx, gid, 0, hubWakeBacklogPage)
		if err != nil {
			return out, fmt.Errorf("读取群日志失败: %w", err)
		}
		page = first
	}
	for len(page) > 0 {
		stop := false
		for i := len(page) - 1; i >= 0; i-- {
			m := page[i]
			if m.Seq <= cursor {
				stop = true // 更早的消息都读过了，与补发无关
				break
			}
			if out.scanned >= hubWakeBacklogMessages || out.bytes >= hubWakeBacklogBytes {
				// 预算用完：剩下（更旧）的消息一条都没读，其中的图片数量未知 —— 只能
				// 显式说「还有更早的没看」，让模型自己决定要不要请人补发。
				out.truncated = true
				stop = true
				break
			}
			out.scanned++
			for _, a := range m.Attachments {
				if a.IsImage() {
					out.bytes += len(a.DataURL) // 未采用的图也占内存/IO，按读到的字节算
				}
			}
			if !hubBacklogMessageEligible(m, cursor, triggerSeq) {
				continue
			}
			p.addImages(m)
		}
		if stop || len(page) < hubWakeBacklogPage {
			break // 撞条件 / 日志翻到头（不足一页 = 没有更旧的了）
		}
		next, err := st.ReadMessages(ctx, gid, page[0].Seq, hubWakeBacklogPage)
		if err != nil {
			return out, fmt.Errorf("读取群日志失败: %w", err)
		}
		page = next
	}
	res := p.result()
	res.truncated = out.truncated
	res.scanned = out.scanned
	res.bytes = out.bytes
	return res, nil
}

// hubPendingImageBlocks 把补发结果拼成模型内容块：说明 → 图片（时间正序）→
// 省略/截断说明。省略与截断都必须有文字：模型看不到的图不能假装看过。
func hubPendingImageBlocks(p hubPendingImages) []core.Content {
	var blocks []core.Content
	if len(p.atts) > 0 {
		seqText := make([]string, 0, len(p.seqs))
		for _, seq := range p.seqs {
			seqText = append(seqText, fmt.Sprintf("#%d", seq))
		}
		blocks = append(blocks, core.Content{
			Type:    core.ContentTypeText,
			Content: fmt.Sprintf("[补发此前未送达的图片：%s]", strings.Join(seqText, "、")),
		})
		blocks = append(blocks, hubAttachmentImageBlocks(p.atts)...)
	}
	if p.omitted > 0 {
		blocks = append(blocks, core.Content{
			Type:    core.ContentTypeText,
			Content: fmt.Sprintf("[另有 %d 张较早的图片因单轮补发上限未附上（没有进入本轮上下文）]", p.omitted),
		})
	}
	if p.truncated {
		blocks = append(blocks, core.Content{
			Type: core.ContentTypeText,
			Content: fmt.Sprintf("[另有更早的群消息因单轮补发扫描上限（最多回溯 %d 条）未读取，其中的图片未附上（没有进入本轮上下文）]",
				hubWakeBacklogMessages),
		})
	}
	return blocks
}

// hubAttachmentImageBlocks 把 image 附件转成模型内容块。
// 文件附件不进上下文：前端已把 `@路径` 放进正文，由模型按需读取。
func hubAttachmentImageBlocks(atts []agenthub.Attachment) []core.Content {
	var out []core.Content
	for _, a := range atts {
		if !a.IsImage() || a.DataURL == "" {
			continue
		}
		out = append(out, core.Content{
			Type:     core.ContentTypeImage,
			Content:  a.DataURL,
			MimeType: a.ImageMIME(),
		})
	}
	return out
}

// hubUserContentBlocks 组装一条人类消息的模型内容：非空正文一个 text 块 + 每个
// 图片一个 image 块。纯图消息没有 text 块（正文只是落账占位）。
func hubUserContentBlocks(text string, atts []agenthub.Attachment) []core.Content {
	var blocks []core.Content
	if text != "" {
		blocks = append(blocks, core.Content{Type: core.ContentTypeText, Content: text})
	}
	return append(blocks, hubAttachmentImageBlocks(atts)...)
}

// hubDisplayText 返回落账/展示用的正文：正文为空时按附件类型给占位。
// 纯图必须给占位，否则快照/perception 会把这条消息当成空消息（不可用）。
func hubDisplayText(text string, atts []agenthub.Attachment) string {
	if text != "" {
		return text
	}
	if hubHasImageAttachment(atts) {
		return hubImageOnlyText
	}
	if len(atts) > 0 {
		return hubFileOnlyText
	}
	return ""
}
