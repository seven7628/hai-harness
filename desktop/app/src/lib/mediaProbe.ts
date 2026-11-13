// mediaProbe.ts — `media_probe` 响应的**类型 + 解析**（前端侧）。
//
// 为什么要有解析层：data 是从 Go 经 bridge JSON 过来的 `unknown`。直接 `as MediaProbe`
// 就等于把「字段缺了/类型变了」变成面板里的一次崩溃（白屏），而 00 §4.3 的硬要求是
// 「每条失败路径都有写明原因的占位」。这里把未知输入收敛成一个**全字段都有值**的对象，
// 缺什么就落到保守默认（mode='unknown' → 先试播）。
import type { MediaKind } from './mediaFile'

export type MediaPlayMode = 'direct' | 'transcode' | 'unknown'

export interface MediaProbeStream {
  type: string
  codec: string
  width: number
  height: number
  channels: number
  sampleRate: number
  bitRate: number
}

export interface MediaPlayback {
  mode: MediaPlayMode
  kind: MediaKind
  container: string
  videoCodec: string
  audioCodec: string
  reason: string
  /** 结论是否用上了 ffprobe（false = 只看容器）。 */
  probed: boolean
}

export interface MediaProbe {
  path: string
  size: number
  modifiedMs: number
  available: boolean
  /** ok（探测成功）| unavailable（没装 ffprobe）| failed（装了但坏了/超时） */
  state: 'ok' | 'unavailable' | 'failed'
  reason: string
  format: string
  durationMs: number
  bitRate: number
  streams: MediaProbeStream[]
  playback: MediaPlayback
  cached: boolean
}

const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0)
const str = (v: unknown): string => (typeof v === 'string' ? v : '')
const obj = (v: unknown): Record<string, unknown> => (v && typeof v === 'object' ? (v as Record<string, unknown>) : {})
const mode = (v: unknown): MediaPlayMode => (v === 'direct' || v === 'transcode' ? v : 'unknown')
const kindOf = (v: unknown): MediaKind => (v === 'audio' ? 'audio' : 'video')

/** 把 IPC 回来的 data 收敛成 MediaProbe；完全不成形时返回 undefined（调用方按「读不到」处理）。 */
export function parseMediaProbe(raw: unknown): MediaProbe | undefined {
  const d = obj(raw)
  if (!d.path && !d.playback) return undefined
  const pb = obj(d.playback)
  const streamsRaw = Array.isArray(d.streams) ? d.streams : []
  return {
    path: str(d.path),
    size: num(d.size),
    modifiedMs: num(d.modified_ms),
    available: d.available === true,
    state: d.state === 'ok' || d.state === 'unavailable' || d.state === 'failed' ? d.state : 'failed',
    reason: str(d.reason),
    format: str(d.format),
    durationMs: num(d.duration_ms),
    bitRate: num(d.bit_rate),
    streams: streamsRaw.map((s) => {
      const o = obj(s)
      return {
        type: str(o.type),
        codec: str(o.codec),
        width: num(o.width),
        height: num(o.height),
        channels: num(o.channels),
        sampleRate: num(o.sample_rate),
        bitRate: num(o.bit_rate),
      }
    }),
    playback: {
      mode: mode(pb.mode),
      kind: kindOf(pb.kind),
      container: str(pb.container),
      videoCodec: str(pb.video_codec),
      audioCodec: str(pb.audio_codec),
      reason: str(pb.reason),
      probed: pb.probed === true,
    },
    cached: d.cached === true,
  }
}

/** 面板上的「分辨率 / 编码」串：视频流优先，音频只给编码与声道。 */
export function probeSummary(p: MediaProbe | undefined): { resolution: string; codecs: string } {
  if (!p) return { resolution: '', codecs: '' }
  const v = p.streams.find((s) => s.type === 'video' && (s.width > 0 || s.codec))
  const a = p.streams.find((s) => s.type === 'audio' && s.codec)
  const resolution = v && v.width > 0 && v.height > 0 ? `${v.width}×${v.height}` : ''
  const codecs = [v?.codec, a?.codec].filter(Boolean).join(' / ')
  return { resolution, codecs }
}
