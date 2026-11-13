// mediaFile.ts — 音视频文件的两条纯函数判据（扩展名清单 + 时长/码率格式化）。
//
// 为什么单独成文件：它被**三个**地方用，且三处都在不同的构建里 ——
//   ① 渲染器注册表的 `match`（src/surfaces/preview/renderers/media.tsx）；
//   ② 面板文案（「需要转码」的容器清单要能一眼看出是不是 mkv）；
//   ③ 协议单测（electron/media-protocol.test.ts 顺手断言「协议白名单 ⇄ 这份清单」不漂移）。
// 纯数据 + 纯函数，不 import 任何 node/electron 模块，所以能从 `electron/` 的测试里 import。
//
// 与 bridge 的编解码矩阵（desktop/bridge/media_matrix.go）的关系：那边判「能不能直放」
// （容器 + 编码 + 转码策略），这边只判「这是不是媒体、是视频还是音频」——两种问题，
// 两个进程，扩展名清单必须是同一批（协议白名单也在这批里，见 electron/media-protocol.ts）。

export const MEDIA_VIDEO_EXTS = [
  'mp4', 'm4v', 'mov', 'webm', 'ogv', 'mkv', 'avi', 'ts', 'm2ts', 'mts', '3gp',
] as const

export const MEDIA_AUDIO_EXTS = [
  'mp3', 'm4a', 'aac', 'wav', 'ogg', 'oga', 'opus', 'flac', 'wma',
] as const

export type MediaKind = 'video' | 'audio'

/** 小写扩展名（不含点）。`a.MP4` → `mp4`；`Makefile` → ``。 */
export function extOfPath(p: string): string {
  const base = p.slice(Math.max(p.lastIndexOf('/'), p.lastIndexOf('\\')) + 1)
  const i = base.lastIndexOf('.')
  return i > 0 ? base.slice(i + 1).toLowerCase() : ''
}

/** 扩展名 → 视频/音频；非媒体 → undefined（渲染器 match 的判据）。 */
export function mediaKindOf(p: string): MediaKind | undefined {
  const ext = extOfPath(p)
  if ((MEDIA_VIDEO_EXTS as readonly string[]).includes(ext)) return 'video'
  if ((MEDIA_AUDIO_EXTS as readonly string[]).includes(ext)) return 'audio'
  return undefined
}

/** 容器是否属于「Electron 直放名单」（与 bridge 的矩阵同口径；只用于探测未回来时的占位提示）。 */
export const NEEDS_TRANSCODE_EXTS = ['mkv', 'avi', 'ts', 'm2ts', 'mts'] as const

export function needsTranscodeByExt(p: string): boolean {
  return (NEEDS_TRANSCODE_EXTS as readonly string[]).includes(extOfPath(p))
}

/** 毫秒 → `1:02:03` / `2:03`（未知 → `--:--`）。面板里只需要一个稳定的短形式。 */
export function formatDuration(ms: number | undefined): string {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return '--:--'
  const total = Math.round(ms / 1000)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60
  const mm = h > 0 ? String(m).padStart(2, '0') : String(m)
  const ss = String(s).padStart(2, '0')
  return h > 0 ? `${h}:${mm}:${ss}` : `${mm}:${ss}`
}

/** 比特率 → `1.4 Mbps` / `320 kbps`（未知 → `--`）。 */
export function formatBitrate(bps: number | undefined): string {
  if (typeof bps !== 'number' || !Number.isFinite(bps) || bps <= 0) return '--'
  if (bps >= 1_000_000) return `${(bps / 1_000_000).toFixed(1)} Mbps`
  return `${Math.round(bps / 1000)} kbps`
}
