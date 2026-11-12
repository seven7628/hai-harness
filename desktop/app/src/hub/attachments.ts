// Hub 附件（图片 / 文件引用）的纯逻辑：抓取、校验、指纹、正文拼接。
//
// 为什么要独立成纯函数模块：附件是「多入口（粘贴 / 拖拽 / 选图 / 纸夹）+ 多出口
// （乐观气泡 / 失败重试 / mock 回显 / 历史）」的共享状态，任何一处口径不同都会让
// 「重试用同一 cid」或「file 引用重复进正文」这类事悄悄发生。这里不碰 DOM、不碰
// store，node 直跑就能钉死（scripts/test-hub-attach.mjs）。
import { MAX_IMAGE_ATTACHMENTS, MAX_MESSAGE_IMAGE_BYTES, dataUrlBytes, fmtBytes } from '../lib/imageAttach'
import type { HubAttachment } from './types'

/** 单条消息的附件总数上限（图片 + 文件引用合计）。 */
export const MAX_HUB_ATTACHMENTS = MAX_IMAGE_ATTACHMENTS
/** 单条消息的图片总量上限（与主会话 / bridge 同口径：3MiB）。 */
export const MAX_HUB_IMAGE_BYTES = MAX_MESSAGE_IMAGE_BYTES

export const HUB_ATTACH_COUNT_ERROR = `最多 ${MAX_HUB_ATTACHMENTS} 个附件`

function str(v: unknown): string {
  return typeof v === 'string' ? v.trim() : ''
}

function num(v: unknown): number | undefined {
  const n = typeof v === 'number' ? v : Number.NaN
  return Number.isFinite(n) && n >= 0 ? n : undefined
}

/**
 * 把一条（可能来自旧版本 / 后端的）附件规范成 UI 能直接用的形状。
 * 不是对象的条目直接丢掉；kind 缺失时按「有 data_url → image，有 path → file」推断。
 */
export function normalizeAttachment(raw: unknown): HubAttachment | null {
  if (!raw || typeof raw !== 'object') return null
  const o = raw as Record<string, unknown>
  const dataUrl = str(o.data_url)
  const path = str(o.path)
  const kind = o.kind === 'image' || o.kind === 'file' ? o.kind : dataUrl ? 'image' : path ? 'file' : ''
  if (!kind) return null
  const out: HubAttachment = { kind }
  const name = str(o.name)
  if (name) out.name = name
  const mime = str(o.mime_type)
  if (mime) out.mime_type = mime
  const bytes = num(o.bytes)
  if (bytes !== undefined) out.bytes = bytes
  if (kind === 'image' && dataUrl) out.data_url = dataUrl
  if (kind === 'file' && path) out.path = path
  // 既没有内容也没有名字（或路径）的条目没有意义：不留在条带里制造空 chip。
  if (!out.data_url && !out.path && !out.name) return null
  return out
}

/** 旧消息 attachments 为 undefined / 非数组时返回空表（不崩、不造数据）。 */
export function normalizeAttachments(list: unknown): HubAttachment[] {
  if (!Array.isArray(list)) return []
  const out: HubAttachment[] = []
  for (const item of list) {
    const a = normalizeAttachment(item)
    if (a) out.push(a)
  }
  return out
}

export function imagesOf(list?: HubAttachment[] | null): HubAttachment[] {
  return normalizeAttachments(list).filter((a) => a.kind === 'image')
}

export function filesOf(list?: HubAttachment[] | null): HubAttachment[] {
  return normalizeAttachments(list).filter((a) => a.kind === 'file')
}

/** 一段字符串的稳定短哈希（指纹里代替整段 data URL，避免把几 MB 复制进失败记录）。 */
function fnv1a(text: string): string {
  let h = 0x811c9dc5
  for (let i = 0; i < text.length; i++) {
    h ^= text.charCodeAt(i)
    h = Math.imul(h, 0x01000193)
  }
  return (h >>> 0).toString(16).padStart(8, '0')
}

/** data URL 的指纹片段：短图原样，长图用「长度 + 哈希」（不会因同一张图的不同编码缩写撞车）。 */
function dataUrlKey(dataUrl: string): string {
  if (!dataUrl) return ''
  return `${dataUrl.length}:${fnv1a(dataUrl)}`
}

/**
 * 附件集合的稳定指纹（失败重试 / 同文重发的幂等键）。
 *
 * 规则：逐条取 kind/name/size/data_url/path，**排序后**拼接 —— 同一批附件换顺序
 * 不该换 cid，但换一张图（或加一个文件引用）必须换 cid，否则「换图重发」会被
 * bridge 的 client_msg_id 幂等挡成旧消息。undefined → 空串（旧调用的指纹）。
 */
export function attachmentFingerprint(list?: HubAttachment[] | null): string {
  const parts = normalizeAttachments(list).map((a) => [
    a.kind,
    a.name ?? '',
    a.bytes === undefined ? '' : String(a.bytes),
    a.mime_type ?? '',
    dataUrlKey(a.data_url ?? ''),
    a.path ?? '',
  ].join('\u0001'))
  return parts.sort().join('\u0002')
}

/** 一条 `@path` 是否已在正文里（按 token 边界判，避免 `/a/b.go` 命中 `/a/b.go.bak`）。 */
function hasRef(text: string, token: string): boolean {
  let i = text.indexOf(token)
  while (i >= 0) {
    const after = text[i + token.length]
    if (after === undefined || /\s/.test(after)) return true
    i = text.indexOf(token, i + 1)
  }
  return false
}

/**
 * 把文件引用拼进正文：每个 file 一行 `@<path>`，跳过重复路径与正文里已有的引用。
 *
 * 正文原样保留（含换行）；空正文时只返回引用行，不留前导空行。图片**不**进正文
 * （走 attachments），后端不读文件，只把 `@path` 当引用记号。
 */
export function withFileRefs(text: string, files?: HubAttachment[] | null): string {
  const base = typeof text === 'string' ? text : ''
  const seen = new Set<string>()
  const refs: string[] = []
  for (const f of filesOf(files)) {
    const path = (f.path ?? '').trim()
    if (!path || seen.has(path)) continue
    seen.add(path)
    const token = `@${path}`
    if (hasRef(base, token)) continue
    refs.push(token)
  }
  if (refs.length === 0) return base
  if (!base.trim()) return refs.join('\n')
  return base + (base.endsWith('\n') ? '' : '\n') + refs.join('\n')
}

export interface AttachmentCheck {
  ok: boolean
  /** 规范化后的附件（顺序保持；非法条目被丢掉）。 */
  attachments: HubAttachment[]
  error?: string
}

/**
 * 发送前的最后一道闸：张数 / 图片体积 / 必需字段。
 *
 * 采集入口（hook）已经压过一次，这里兜「重试的是旧记录」「后端/历史给的数据
 * 不完整」这类旁路；返回 error 时调用方直接提示，不发请求。
 */
export function validateAttachments(list?: HubAttachment[] | null): AttachmentCheck {
  const attachments = normalizeAttachments(list)
  if (attachments.length === 0) return { ok: true, attachments }
  if (attachments.length > MAX_HUB_ATTACHMENTS) {
    return { ok: false, attachments, error: HUB_ATTACH_COUNT_ERROR }
  }
  let imageBytes = 0
  for (const a of attachments) {
    if (a.kind === 'image') {
      if (!a.data_url) return { ok: false, attachments, error: '图片缺少数据，请重新选择' }
      imageBytes += dataUrlBytes(a.data_url)
    } else if (!a.path) {
      return { ok: false, attachments, error: '文件引用缺少路径，请重新选择' }
    }
  }
  if (imageBytes > MAX_HUB_IMAGE_BYTES) {
    return { ok: false, attachments, error: `图片总量 ${fmtBytes(imageBytes)} 超过 ${fmtBytes(MAX_HUB_IMAGE_BYTES)}` }
  }
  return { ok: true, attachments }
}

/** 图片总量（字节）：hook 的剩余额度与提示文案共用。 */
export function imagesBytes(list?: HubAttachment[] | null): number {
  return imagesOf(list).reduce((n, a) => n + (a.data_url ? dataUrlBytes(a.data_url) : 0), 0)
}

/**
 * 复制一张图到剪贴板（ClipboardItem）。
 *
 * 浏览器只稳定支持 image/png：JPEG / WebP 先经 canvas 重编码成 PNG 再写。返回
 * 形状带 error，调用方负责 toast（绝不 alert，也不假装成功）。
 */
export async function copyImageDataUrl(dataUrl: string): Promise<{ ok: boolean; error?: string }> {
  if (!dataUrl) return { ok: false, error: '没有图片数据' }
  if (typeof navigator === 'undefined' || !navigator.clipboard?.write || typeof ClipboardItem === 'undefined') {
    return { ok: false, error: '此环境不支持复制图片' }
  }
  let blob: Blob
  try {
    const res = await fetch(dataUrl)
    blob = await res.blob()
  } catch {
    return { ok: false, error: '读取图片失败' }
  }
  if (blob.type !== 'image/png') {
    let png: Blob | null
    try {
      png = await toPngBlob(blob)
    } catch {
      return { ok: false, error: '图片转 PNG 失败' }
    }
    if (!png) return { ok: false, error: '图片转 PNG 失败' }
    blob = png
  }
  try {
    await navigator.clipboard.write([new ClipboardItem({ 'image/png': blob })])
    return { ok: true }
  } catch (e) {
    return { ok: false, error: e instanceof Error ? e.message : '写入剪贴板失败' }
  }
}

function toPngBlob(blob: Blob): Promise<Blob | null> {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(blob)
    const img = new Image()
    img.onerror = () => {
      URL.revokeObjectURL(url)
      reject(new Error('decode failed'))
    }
    img.onload = () => {
      try {
        const canvas = document.createElement('canvas')
        canvas.width = img.naturalWidth || img.width
        canvas.height = img.naturalHeight || img.height
        const ctx = canvas.getContext('2d')
        if (!ctx) throw new Error('no 2d ctx')
        ctx.drawImage(img, 0, 0)
        canvas.toBlob((out) => {
          URL.revokeObjectURL(url)
          resolve(out)
        }, 'image/png')
      } catch (e) {
        URL.revokeObjectURL(url)
        reject(e)
      }
    }
    img.src = url
  })
}
