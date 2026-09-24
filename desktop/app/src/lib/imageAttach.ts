// imageAttach.ts —— 对话页图片附件的采集与规范化（P0：统一体积口径）。
//
// 为什么需要压缩：图片以 base64 data URL 经 stdin 单行传给 bridge，受
// maxCommandLineBytes（4MiB）硬限；base64 膨胀 4/3 → 3MB 的原图就会撞线，
// 而撞线表现为整批 ask_batch 被拒（错误信息对用户毫无指导性）。
// 因此在**采集入口**就统一压到与三层闸门同口径的预算内：
//   - bridge 单条消息图片总量 ≤ 3MiB（maxImageBlockBytes）
//   - tools 引擎单张结果图 ≤ 3MiB（defaultMaxImageBytes）
//   - 视觉模型有效分辨率：长边 ~1568px 以上收益骤减
//
// 策略（与 tools/builtin/image.go 的 loadImageForModel 同思路，保持前后端一致）：
//   1. 原图已达标 → 原样使用（零质量损失，PNG 截图无损保留）；
//   2. 超限 → canvas 等比缩放（长边 ≤ MAX_IMAGE_DIM）+ JPEG 重编码，必要时逐级降；
//   3. 降不动（异常大图）→ 明确失败，调用方提示用户。

/** 单张附件 data URL 的体积上限（与 bridge/tools 同口径：3MiB）。 */
export const MAX_IMAGE_DATA_URL_BYTES = 3 * 1024 * 1024
/** 单条消息的图片总量上限（bridge 对单条消息的闸门；一条消息通常 1~2 张）。 */
export const MAX_MESSAGE_IMAGE_BYTES = 3 * 1024 * 1024
/** 内嵌图片长边像素上限（视觉模型有效分辨率）。 */
export const MAX_IMAGE_DIM = 1568
/** 附件数量上限。 */
export const MAX_IMAGE_ATTACHMENTS = 4

/** data URL 的体积（字节数；String.length 对 base64 即字节数，ASCII 安全）。 */
export function dataUrlBytes(dataUrl: string): number {
  return dataUrl.length
}

/** 人类可读体积（提示文案用）。 */
export function fmtBytes(n: number): string {
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)}MB`
  if (n >= 1024) return `${Math.round(n / 1024)}KB`
  return `${n}B`
}

export interface NormalizedImage {
  dataUrl: string
  mimeType: string
  /** 是否经过压缩（调用方据此提示用户）。 */
  compressed: boolean
  /** 原始体积（提示文案用）。 */
  originalBytes: number
}

export class ImageTooLargeError extends Error {
  constructor(public readonly bytes: number, public readonly limit: number) {
    super(`image too large: ${fmtBytes(bytes)} > ${fmtBytes(limit)}`)
    this.name = 'ImageTooLargeError'
  }
}

/** 读取 File 为 data URL（附件采集第一步）。 */
export function readFileAsDataURL(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(new Error(`read failed: ${file.name}`))
    reader.onload = () => {
      const r = String(reader.result ?? '')
      r ? resolve(r) : reject(new Error(`empty read: ${file.name}`))
    }
    reader.readAsDataURL(file)
  })
}

/**
 * 把一张图片规格化为「可安全经 IPC 传输」的附件：
 * 达标原样返回；超限则降采样 + JPEG 重编码到 limit 以内。
 *
 * 只处理浏览器可解码的格式（png/jpeg/gif/webp/bmp）；不可解码（如 heic）在
 * 达标时仍原样放行（服务端可能拒绝，但用户意图明确），超限则抛错而非静默丢。
 */
export async function normalizeImage(dataUrl: string, mimeType: string, limit = MAX_IMAGE_DATA_URL_BYTES): Promise<NormalizedImage> {
  const originalBytes = dataUrlBytes(dataUrl)
  if (originalBytes <= limit) {
    return { dataUrl, mimeType, compressed: false, originalBytes }
  }
  const shrunk = await shrinkImage(dataUrl, limit)
  if (!shrunk) {
    throw new ImageTooLargeError(originalBytes, limit)
  }
  return { dataUrl: shrunk.dataUrl, mimeType: shrunk.mimeType, compressed: true, originalBytes }
}

/** shrinkImage 逐级降采样直到达标；无法解码 / 压不动 → null。 */
async function shrinkImage(dataUrl: string, limit: number): Promise<{ dataUrl: string; mimeType: string } | null> {
  let bitmap: ImageBitmap
  try {
    bitmap = await createImageBitmap(await blobOf(dataUrl))
  } catch {
    return null // 浏览器无法解码（heic 等）→ 调用方决定报错
  }
  try {
    const longEdge = Math.max(bitmap.width, bitmap.height)
    let dim = Math.min(MAX_IMAGE_DIM, longEdge)
    for (let attempt = 0; attempt < 6; attempt++) {
      // 先试质量（文字/UI 截图降质量比降尺寸更保可读性），再试尺寸。
      for (const quality of [0.85, 0.6, 0.45]) {
        const out = encodeJpeg(bitmap, dim, quality)
        if (out && dataUrlBytes(out) <= limit) return { dataUrl: out, mimeType: 'image/jpeg' }
      }
      dim = Math.floor(dim / 2)
      if (dim < 64) break
    }
    return null
  } finally {
    bitmap.close()
  }
}

/** blobOf data URL → Blob（createImageBitmap 的输入）。 */
async function blobOf(dataUrl: string): Promise<Blob> {
  const res = await fetch(dataUrl)
  return res.blob()
}

/** encodeJpeg 把位图按长边 dim、给定质量编成 JPEG data URL。 */
function encodeJpeg(bitmap: ImageBitmap, dim: number, quality: number): string | null {
  const longEdge = Math.max(bitmap.width, bitmap.height)
  const scale = longEdge > dim ? dim / longEdge : 1
  const w = Math.max(1, Math.round(bitmap.width * scale))
  const h = Math.max(1, Math.round(bitmap.height * scale))
  const canvas = document.createElement('canvas')
  canvas.width = w
  canvas.height = h
  const ctx = canvas.getContext('2d')
  if (!ctx) return null
  // JPEG 无 alpha：先铺白底，避免透明区域变黑（截图/图标常见）。
  ctx.fillStyle = '#fff'
  ctx.fillRect(0, 0, w, h)
  ctx.drawImage(bitmap, 0, 0, w, h)
  try {
    return canvas.toDataURL('image/jpeg', quality)
  } catch {
    return null
  }
}

/**
 * 校验一批待添加的附件：逐张压缩并保证**总量**不超单条消息预算。
 * 返回可添加项 + 因超限被拒的项（调用方提示用户）。
 * 已存在的附件体积参与总量计算（追加后整条消息仍须达标）。
 */
export async function acceptImages(
  files: File[],
  existingBytes: number,
  maxCount: number,
  currentCount: number,
): Promise<{ accepted: NormalizedImage[]; rejected: number; tooLarge: number }> {
  const room = Math.max(0, maxCount - currentCount)
  const targets = files.slice(0, room)
  const accepted: NormalizedImage[] = []
  let rejected = files.length - targets.length
  let tooLarge = 0
  let used = existingBytes
  for (const file of targets) {
    let raw: string
    try {
      raw = await readFileAsDataURL(file)
    } catch {
      rejected++
      continue
    }
    let norm: NormalizedImage
    try {
      norm = await normalizeImage(raw, file.type || 'image/png', MAX_IMAGE_DATA_URL_BYTES)
    } catch {
      rejected++
      tooLarge++
      continue
    }
    // 单条消息总量预算：逐张递减剩余额度（同消息多图合计不越界）。
    const remaining = MAX_MESSAGE_IMAGE_BYTES - used
    if (remaining <= 0 || dataUrlBytes(norm.dataUrl) > remaining) {
      // 还能再压一轮到剩余额度内吗？额度太小就不值得（压成糊图无意义）。
      if (remaining < 64 * 1024) {
        rejected++
        tooLarge++
        continue
      }
      try {
        norm = await normalizeImage(raw, file.type || 'image/png', remaining)
      } catch {
        rejected++
        tooLarge++
        continue
      }
    }
    used += dataUrlBytes(norm.dataUrl)
    accepted.push(norm)
  }
  return { accepted, rejected, tooLarge }
}
