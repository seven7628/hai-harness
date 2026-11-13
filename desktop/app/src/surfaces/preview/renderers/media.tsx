// media 渲染器（R08c）：原生 <video controls> / <audio controls> + 元信息 + 编解码判断
// + 「用系统播放器打开」。
//
// 四条必须守住的东西：
//
// ① **不把文件读进内存**。src 用 `hai-media://`（主进程按 Range 回 206），不是 data URL，
//    也不是 file://（打包后渲染进程是 http://，跨源一律拒）。见 electron/media-protocol.ts。
//
// ② **切走再切回不重新下载**。<video> 由 lib/mediaStage.ts 的元素池持有：切标签时从 DOM
//    摘下但**不销毁**，缓冲区与 currentTime 都还在（销毁元素 = 重新发 Range 请求 + 重新
//    解容器，1 小时视频上就是几秒黑屏）。
//
// ③ **能不能放，由 bridge 的矩阵说了算**，组件不自己再判一次（否则两边口径迟早分叉）。
//    这里只做三件事：显示结论、显示理由、给系统播放器入口。mkv/avi/ts 不渲染播放器 ——
//    渲染了也只是黑框 + 一个 error 事件，用户不知道是文件坏了还是格式不支持。
//
// ④ **读不到元信息不是不能播**。ffprobe 缺失/超时都只让「时长/编码」这几项显示为未知，
//    播放器照常出现（阈值见 media_probe.go 的降级口径）。整块变成 unavailable 只在
//    「协议都不给 URL」时发生（工作区外的文件、扩展名不在白名单）—— 那种情况确实放不了。
import { useCallback, useEffect, useRef, useState } from 'react'
import { useAppStore } from '../../../store/useAppStore'
import { t as tStatic, useT } from '../../../i18n'
import { displayPath } from '../../../lib/localAsset'
import { formatBitrate, formatDuration, mediaKindOf, needsTranscodeByExt } from '../../../lib/mediaFile'
import { parseMediaProbe, probeSummary, type MediaProbe } from '../../../lib/mediaProbe'
import { acquireMediaElement } from '../../../lib/mediaStage'
import type { PreviewProps } from '../types'

interface Loaded {
  url?: string
  abs?: string
  probe?: MediaProbe
  /** 协议不给 URL / preload 缺方法 —— 整块 unavailable 的两种原因。 */
  error?: string
  /** 元信息读不到（probe 命令失败）。**不挡播放**，只让面板多一行说明。 */
  probeError?: string
}

export default function MediaPreview({ file, tabId, onUnavailable }: PreviewProps) {
  const t = useT()
  const workspace = useAppStore((s) => s.workspace)
  const [loaded, setLoaded] = useState<Loaded>({})
  const [elementError, setElementError] = useState(false)
  // ffprobe 缺失时从元素自己身上补一个时长（不额外请求：metadata 本来就拉了）
  const [elementDurationMs, setElementDurationMs] = useState(0)
  const hostRef = useRef<HTMLDivElement>(null)

  const path = file.path
  // 注意依赖里**没有** `t`：useT() 每次渲染返回新函数，放进来就是「effect 每帧重跑 →
  // 重新拉 probe」的死循环。effect 里要用文案就取模块级的 tStatic（读 store 当前语言）。
  useEffect(() => {
    let alive = true
    const d = typeof window === 'undefined' ? undefined : window.desktop
    if (!d?.mediaUrl || !d?.mediaProbe) {
      const reason = tStatic('rp.preview.mediaNoChannel')
      setLoaded({ error: reason })
      onUnavailable?.(reason)
      return () => { alive = false }
    }
    setLoaded({})
    setElementError(false)
    setElementDurationMs(0)
    // 两件事并行：URL（协议通道 + 路径校验）与元信息（ffprobe）。探测失败不挡播放，
    // 所以用 allSettled 而不是 all —— 各自落各自的字段。
    const ws = workspace ?? ''
    void Promise.allSettled([
      d.mediaUrl({ path, workspace: ws }),
      d.mediaProbe({ path, workspace: ws }),
    ]).then(([uRes, pRes]) => {
      if (!alive) return
      const u = uRes.status === 'fulfilled' ? uRes.value : undefined
      const p = pRes.status === 'fulfilled' ? pRes.value : undefined
      if (!u?.ok || !u.url) {
        const reason = u?.error ?? tStatic('rp.preview.mediaNoChannel')
        setLoaded({ error: reason })
        onUnavailable?.(reason)
        return
      }
      const probe = p?.ok === true ? parseMediaProbe(p.data) : undefined
      setLoaded({
        url: u.url,
        abs: u.abs,
        probe,
        probeError: !probe && p?.ok === false ? (p.error ?? tStatic('rp.preview.mediaNoProbe')) : undefined,
      })
    })
    return () => { alive = false }
  }, [path, workspace, onUnavailable])

  const kind = mediaKindOf(path) ?? loaded.probe?.playback.kind ?? 'video'
  // 没有探测结果时按扩展名给一个保守判断（mkv/avi/ts 明确提示转码；其余先试播）
  const mode = loaded.probe?.playback.mode ?? (needsTranscodeByExt(path) ? 'transcode' : 'unknown')
  const { resolution, codecs } = probeSummary(loaded.probe)
  const durationMs = loaded.probe?.durationMs || elementDurationMs
  const degraded = (!!loaded.probe && loaded.probe.state !== 'ok') || !!loaded.probeError
  const degradedReason = loaded.probe?.reason || loaded.probeError || ''
  const shown = displayPath(loaded.abs ?? path, workspace)

  // 元素池：同一个 URL 只建一次。切标签 → cleanup 里 detach（暂停 + 摘下来，不销毁）。
  const url = loaded.url
  const modifiedMs = loaded.probe?.modifiedMs
  useEffect(() => {
    const host = hostRef.current
    if (!url || !host) return
    // srcPath = 标签里的原始路径：store 关标签时按同一个字符串回收元素（见 mediaStage 头注释）
    const handle = acquireMediaElement(url, kind, url, { modifiedMs, srcPath: path, tabId })
    handle.attach(host)
    const el = handle.el
    const onError = () => setElementError(true)
    const onMeta = () => {
      setElementError(false)
      if (Number.isFinite(el.duration) && el.duration > 0) setElementDurationMs(Math.round(el.duration * 1000))
    }
    el.addEventListener('error', onError)
    el.addEventListener('loadedmetadata', onMeta)
    if (el.readyState >= 1 && Number.isFinite(el.duration) && el.duration > 0) {
      setElementDurationMs(Math.round(el.duration * 1000))
    }
    return () => {
      el.removeEventListener('error', onError)
      el.removeEventListener('loadedmetadata', onMeta)
      handle.detach()
    }
  }, [url, kind, modifiedMs, path, tabId])

  const openExternal = useCallback(() => {
    const abs = loaded.abs
    if (!abs) return
    void window.desktop?.openPath?.(abs)
  }, [loaded.abs])

  if (loaded.error) {
    return (
      <div className="pv-media" data-renderer="media" data-state="unavailable" role="status">
        <div className="pv-media-empty">
          <div className="pv-media-empty-title">⚠ {t('rp.preview.mediaUnavailable')}</div>
          <div className="pv-media-empty-reason">{loaded.error}</div>
          <div className="pv-media-path mono">{shown}</div>
        </div>
      </div>
    )
  }

  const badge = mode === 'direct'
    ? t('rp.preview.mediaDirect')
    : mode === 'transcode' ? t('rp.preview.mediaTranscode') : t('rp.preview.mediaTryPlay')

  return (
    <div
      className="pv-media"
      data-renderer="media"
      data-playback-mode={mode}
      data-media-kind={kind}
      data-probe-state={loaded.probe?.state ?? 'pending'}
    >
      <div className="pv-media-bar">
        <span className="pv-media-kind">{kind === 'audio' ? t('rp.preview.mediaAudio') : t('rp.preview.mediaVideo')}</span>
        <span className={`pv-media-badge ${mode}`} data-mode={mode}>{badge}</span>
        {loaded.probe?.playback.container && <span className="pv-media-container mono">.{loaded.probe.playback.container}</span>}
        <button className="pv-media-open" onClick={openExternal} disabled={!loaded.abs} title={loaded.abs || ''}>
          {t('rp.preview.mediaOpenExternal')}
        </button>
      </div>

      {degraded && (
        <div className="pv-media-warn" data-state="degraded" role="status">
          {t('rp.preview.mediaNoProbe')}
          {degradedReason ? ` · ${degradedReason}` : ''}
        </div>
      )}

      {mode === 'transcode' ? (
        <div className="pv-media-transcode" data-state="transcode">
          <div className="pv-media-transcode-title">{t('rp.preview.mediaTranscodeTitle')}</div>
          <div className="pv-media-transcode-body">{loaded.probe?.playback.reason || t('rp.preview.mediaTranscodeBody')}</div>
          <div className="pv-media-transcode-hint">{t('rp.preview.mediaTranscodeHint')}</div>
        </div>
      ) : (
        <div className="pv-media-stage" ref={hostRef} data-kind={kind} />
      )}

      {elementError && mode !== 'transcode' && (
        <div className="pv-media-warn" data-state="element-error" role="status">
          {t('rp.preview.mediaElementError')}
        </div>
      )}

      <dl className="pv-media-meta mono">
        <div><dt>{t('rp.preview.mediaDuration')}</dt><dd>{formatDuration(durationMs)}</dd></div>
        {kind === 'video' && <div><dt>{t('rp.preview.mediaResolution')}</dt><dd>{resolution || '--'}</dd></div>}
        <div><dt>{t('rp.preview.mediaCodec')}</dt><dd>{codecs || '--'}</dd></div>
        <div><dt>{t('rp.preview.mediaBitrate')}</dt><dd>{formatBitrate(loaded.probe?.bitRate)}</dd></div>
      </dl>
      <div className="pv-media-path mono" title={loaded.abs ?? path}>{shown}</div>
    </div>
  )
}
