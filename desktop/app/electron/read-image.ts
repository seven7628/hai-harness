// read-image.ts — Markdown 预览的本地图片通道：路径 → data URL（主进程读盘，不落盘）。
//
// 路径口径**对齐 bridge 的预览通路（resolveWorkspacePath / file_preview）**，
// 而不是 electron/main.ts 的 fs:read-ref：
//   - @ 引用（readFsRef）是「用户显式引用工作区里的文件」，所以要求落在工作区内；
//   - 图片是**预览的附属资源**：工作区外的 ~/notes/README.md 打开后，它旁边的
//     img/x.png 必须跟着显示，否则这条通路等于没有（bridge main.go:2248「绝对路径
//     不依赖 workspace（工作区外统一 open-file）」；resolveWorkspacePath 的绝对分支
//     同样一律放行，只挡 harness 配置目录）。
//   于是此处：绝对路径直接采用；相对路径锚定 workspace 且挡 .. 逃逸。
// 敏感目录判定复用 fs-guard.ts 那一份（~/.go-code **不**拒绝 —— 与 electron/main.ts
// 既有产品决策一致：那是应用自身配置目录，密钥靠 0600 文件权限保护）。
import * as fs from 'node:fs'
import * as path from 'node:path'
import { isSensitiveFsPath } from './fs-guard'

// 单图上限 8MiB：base64 膨胀 4/3 → 约 10.7MiB 的行内 data URL，已接近渲染进程舒服
// 携带的上限；再大的图该走别的通路（缩略图 / 内嵌浏览器），而不是把 IPC 和主进程
// 内存撑爆。
export const MAX_IMAGE_BYTES = 8 * 1024 * 1024

// 扩展名白名单 → mime。按扩展名判类型而不是嗅探文件头：svh 是文本、ico 是容器，
// 嗅探对这两类都不可靠，而渲染进程需要的是一个**确定能用**的 data URL 前缀。
const IMAGE_MIMES: Record<string, string> = {
  '.png': 'image/png',
  '.jpg': 'image/jpeg',
  '.jpeg': 'image/jpeg',
  '.gif': 'image/gif',
  '.webp': 'image/webp',
  '.avif': 'image/avif',
  '.bmp': 'image/bmp',
  '.ico': 'image/x-icon',
  '.svg': 'image/svg+xml',
}

// imageMimeFor 扩展名 → mime（大小写不敏感）；非图片 / 无扩展名 → null。
export function imageMimeFor(p: string): string | null {
  return IMAGE_MIMES[path.extname(String(p ?? '')).toLowerCase()] ?? null
}

// readImageAsDataUrl 读本地图片 → data URL。任何失败都回 { ok:false, error } 而不是抛：
// 渲染进程的调用点（Markdown 里的 <img src>）没法接异常，只能按 ok 分流成「图 + 提示」。
export function readImageAsDataUrl(
  req: { path: string; workspace?: string },
  opts?: { maxBytes?: number },
): { ok: true; dataUrl: string; mime: string; bytes: number } | { ok: false; error: string } {
  const raw = String(req?.path ?? '')
  if (!raw) return { ok: false, error: 'path 为空' }
  const ws = String(req?.workspace ?? '')
  let target: string
  if (path.isAbsolute(raw)) {
    target = path.resolve(raw) // 绝对路径不依赖 workspace（工作区外统一 open-file）
  } else {
    // 相对路径没有锚点就无从判断指向哪里 —— 与 bridge 预览通路同样的拒绝理由。
    if (!ws || !path.isAbsolute(ws)) return { ok: false, error: 'workspace 为空' }
    const base = path.resolve(ws)
    target = path.resolve(base, raw)
    const rel = path.relative(base, target)
    if (rel === '..' || rel.startsWith('..' + path.sep) || path.isAbsolute(rel)) {
      return { ok: false, error: `图片路径在工作区外: ${raw}` }
    }
  }
  // 敏感目录先于存在性判定（与 readFsRef 同序）：受保护路径连「在不在」都不该由这里回答。
  if (isSensitiveFsPath(target)) return { ok: false, error: '该路径受保护（含敏感凭据）' }
  let st
  try {
    st = fs.statSync(target)
  } catch {
    return { ok: false, error: `图片文件不存在: ${target}` }
  }
  if (st.isDirectory()) return { ok: false, error: `图片路径是目录，不是文件: ${target}` }
  if (!st.isFile()) return { ok: false, error: `不是普通文件: ${target}` }
  const mime = imageMimeFor(target)
  if (!mime) return { ok: false, error: `不是图片文件: ${target}` }
  const maxBytes = opts?.maxBytes ?? MAX_IMAGE_BYTES
  if (st.size > maxBytes) return { ok: false, error: `图片过大: ${st.size} 字节（上限 ${maxBytes} 字节）` }
  let buf: Buffer
  try {
    buf = fs.readFileSync(target)
  } catch (e) {
    return { ok: false, error: (e as NodeJS.ErrnoException).message }
  }
  return { ok: true, dataUrl: `data:${mime};base64,${buf.toString('base64')}`, mime, bytes: buf.length }
}
