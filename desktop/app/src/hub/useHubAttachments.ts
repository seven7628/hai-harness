// Hub composer 的附件状态（图片 + 文件引用）。
//
// 与主会话 Composer 的分工：**采集与压缩**复用 lib/imageAttach（同一套 3MiB 闸门、
// 同一套 canvas 降采样），这里只补 Hub 特有的三件事：
//   1. 图片 + 文件引用同一个条带（张数上限合计 10）；
//   2. 文件只记路径（发送时拼 `@path`），绝不读文件内容；
//   3. 拿不到路径时**明确提示**，不静默丢弃（浏览器 File 没有磁盘路径）。
//
// 不做 modelSupportsImage 门控：Hub 里的模型由用户各自配置，能力不齐；真发不出去
// 时后端会回错误，交给 error/notice 显示，比在采集端猜模型更诚实。
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { getTransport } from '../transport'
import { MAX_HUB_ATTACHMENTS, MAX_HUB_IMAGE_BYTES, attachmentFingerprint, imagesBytes } from './attachments'
import { acceptImages, dataUrlBytes, fmtBytes } from '../lib/imageAttach'
import { useHubStore } from './useHubStore'
import type { HubAttachment } from './types'

export interface HubAttachments {
  images: HubAttachment[]
  files: HubAttachment[]
  /** 合并后的附件表（发送 payload / 乐观消息用；顺序 = 图片在前、文件在后）。 */
  all: HubAttachment[]
  total: number
  notice: string
  busy: boolean
  clearNotice(): void
  addImageFiles(files: File[]): Promise<void>
  addFilePaths(paths: string[]): Promise<void>
  /** 纸夹：原生多选文件 → 文件引用 chip。 */
  pickFiles(): Promise<void>
  /** 粘贴：只收 image/*；剪贴板里同时有文本时不 preventDefault（让浏览器照常粘字）。 */
  onPaste(e: React.ClipboardEvent<HTMLTextAreaElement>): void
  /** 拖拽：图片走压缩 → data URL，其它文件走路径引用。 */
  onDrop(e: React.DragEvent<HTMLElement>): void
  remove(kind: 'image' | 'file', index: number): void
  reset(): void
}

function fileName(path: string): string {
  const trimmed = path.replace(/[\\/]+$/, '')
  const i = Math.max(trimmed.lastIndexOf('/'), trimmed.lastIndexOf('\\'))
  return i >= 0 ? trimmed.slice(i + 1) : trimmed
}

export function useHubAttachments(): HubAttachments {
  const [images, setImagesState] = useState<HubAttachment[]>([])
  const [files, setFilesState] = useState<HubAttachment[]>([])
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  // 并发采集（连按两次粘贴 / 拖拽叠着选图）：压缩是异步的，第二个 addImageFiles
  // 如果读的是同一个闭包里的旧长度，就会各自越过张数/体积闸门。
  // 两道保险：ref 镜像（读最新值）+ 串行队列（压缩一次只跑一批）。
  const imagesRef = useRef<HubAttachment[]>([])
  const filesRef = useRef<HubAttachment[]>([])
  const queue = useRef<Promise<void>>(Promise.resolve())
  const setImages = useCallback((next: HubAttachment[] | ((prev: HubAttachment[]) => HubAttachment[])): void => {
    const value = typeof next === 'function' ? next(imagesRef.current) : next
    imagesRef.current = value
    setImagesState(value)
  }, [])
  const setFiles = useCallback((next: HubAttachment[] | ((prev: HubAttachment[]) => HubAttachment[])): void => {
    const value = typeof next === 'function' ? next(filesRef.current) : next
    filesRef.current = value
    setFilesState(value)
  }, [])

  const clearNotice = useCallback(() => setNotice(''), [])

  const addImageFiles = useCallback((list: File[]): Promise<void> => {
    const picked = (list ?? []).filter((f) => f && f.type.startsWith('image/'))
    if (picked.length === 0) return Promise.resolve()
    const run = async (): Promise<void> => {
      setBusy(true)
      try {
        const current = imagesRef.current.length
        const maxCount = Math.max(0, MAX_HUB_ATTACHMENTS - filesRef.current.length)
        const room = Math.max(0, maxCount - current)
        // acceptImages 自己会用 maxCount - currentCount 算 room；这里传**绝对上限**
        // （总附件数 - 文件数），不能再传 room —— 否则 current 被减两次，已有图片
        // 时会少放 N 张（5 张只进 4 张的上限 bug）。
        const res = await acceptImages(picked, imagesBytes(imagesRef.current), maxCount, current)
        if (res.accepted.length > 0) {
          const added: HubAttachment[] = res.accepted.map((n) => ({
            kind: 'image',
            mime_type: n.mimeType,
            bytes: dataUrlBytes(n.dataUrl),
            data_url: n.dataUrl,
          }))
          setImages((prev) => [...prev, ...added])
        }
        // 提示优先级：张数到顶 > 单张/总量超限 > 读取失败；成功一批就清掉旧提示。
        if (room === 0) setNotice(`最多 ${MAX_HUB_ATTACHMENTS} 个附件`)
        else if (res.tooLarge > 0) setNotice(`图片过大：单条消息的图片总量上限 ${fmtBytes(MAX_HUB_IMAGE_BYTES)}，已跳过 ${res.tooLarge} 张`)
        else if (res.rejected > 0) setNotice(`有 ${res.rejected} 张图片读取失败，已跳过`)
        else setNotice('')
      } finally {
        setBusy(false)
      }
    }
    const next = queue.current.then(run, run)
    queue.current = next.catch(() => {})
    return next
  }, [setImages])

  const addFilePaths = useCallback((paths: string[]): Promise<void> => {
    const incoming = (paths ?? []).map((p) => (typeof p === 'string' ? p.trim() : '')).filter(Boolean)
    if (incoming.length === 0) return Promise.resolve()
    const seen = new Set(filesRef.current.map((f) => f.path ?? ''))
    const next: HubAttachment[] = []
    let dup = 0
    let over = 0
    for (const path of incoming) {
      if (seen.has(path)) { dup++; continue }
      seen.add(path)
      if (imagesRef.current.length + filesRef.current.length + next.length >= MAX_HUB_ATTACHMENTS) { over++; continue }
      next.push({ kind: 'file', name: fileName(path), path })
    }
    if (next.length === 0) {
      if (over > 0) setNotice(`最多 ${MAX_HUB_ATTACHMENTS} 个附件`)
      else if (dup > 0) setNotice('这些文件已经在引用里了')
      return Promise.resolve()
    }
    // size 可选：stat 拿不到就留空（条带显示 —，不假造 0B）；失败不影响引用本身。
    const transport = getTransport()
    return Promise.all(next.map(async (a) => {
      try {
        const r = await transport.fsStat(a.path ?? '')
        return r.ok && typeof r.size === 'number' ? r.size : undefined
      } catch {
        return undefined
      }
    })).then((sizes) => {
      const withSize = next.map((a, i) => (sizes[i] === undefined ? a : { ...a, bytes: sizes[i] }))
      setFiles((prev) => {
        const have = new Set(prev.map((f) => f.path ?? ''))
        return [...prev, ...withSize.filter((a) => !have.has(a.path ?? ''))]
      })
      setNotice(over > 0 ? `最多 ${MAX_HUB_ATTACHMENTS} 个附件` : '')
    })
  }, [setFiles])

  const pickFiles = useCallback(async (): Promise<void> => {
    let picked: string[]
    try {
      picked = await getTransport().pickFsFiles()
    } catch (e) {
      setNotice(`选择文件失败：${e instanceof Error ? e.message : String(e)}`)
      return
    }
    if (picked.length === 0) return // 用户取消
    await addFilePaths(picked)
  }, [addFilePaths])

  const onPaste = useCallback((e: React.ClipboardEvent<HTMLTextAreaElement>): void => {
    const filesIn = Array.from(e.clipboardData?.items ?? [])
      .filter((item) => item.kind === 'file' && item.type.startsWith('image/'))
      .map((item) => item.getAsFile())
      .filter((f): f is File => Boolean(f))
    if (filesIn.length === 0) return // 纯文本保持浏览器原生粘贴
    const text = e.clipboardData?.getData('text/plain') ?? ''
    // 有文本时不拦默认行为：用户期待粘上的文字不能因为「还夹了张图」而丢；
    // 纯图片没有可插入的原生内容，拦掉以免出现无意义的空粘贴。
    if (!text) e.preventDefault()
    void addImageFiles(filesIn)
  }, [addImageFiles])

  const onDrop = useCallback((e: React.DragEvent<HTMLElement>): void => {
    const dropped = Array.from(e.dataTransfer?.files ?? [])
    if (dropped.length === 0) return
    e.preventDefault()
    const imgs = dropped.filter((f) => f.type.startsWith('image/'))
    const others = dropped.filter((f) => !f.type.startsWith('image/'))
    if (imgs.length > 0) void addImageFiles(imgs)
    if (others.length === 0) return
    const transport = getTransport()
    const paths: string[] = []
    let noPath = 0
    for (const f of others) {
      let p: string
      try {
        p = transport.getPathForFile(f)
      } catch {
        p = ''
      }
      if (p) paths.push(p)
      else noPath++
    }
    if (noPath > 0) {
      setNotice(`有 ${noPath} 个文件拿不到磁盘路径（浏览器里的文件没有路径），请用纸夹「引用文件」选择`)
    }
    if (paths.length > 0) void addFilePaths(paths)
  }, [addFilePaths, addImageFiles])

  const remove = useCallback((kind: 'image' | 'file', index: number): void => {
    if (kind === 'image') setImages((prev) => prev.filter((_, i) => i !== index))
    else setFiles((prev) => prev.filter((_, i) => i !== index))
    setNotice('')
  }, [setFiles, setImages])

  const reset = useCallback((): void => {
    setImages([])
    setFiles([])
    setNotice('')
  }, [setFiles, setImages])

  const all = useMemo(() => [...images, ...files], [images, files])

  return {
    images, files, all, total: all.length, notice, busy,
    clearNotice, addImageFiles, addFilePaths, pickFiles, onPaste, onDrop, remove, reset,
  }
}

/**
 * 发送结束后收拾附件条带（成功清空 / 失败保留）。
 *
 * composer 自己的 submit 走 `if (ok) attach.reset()` 就够了；但「失败气泡上的行内重试」
 * 不经过 composer，成功后条带会一直留着刚才那张图 —— 用户再点一次发送就会重复发图。
 * 这里按「发送期间条带里的附件指纹」对账：
 *   · 发送结束仍能在 failedByCid 里找到同指纹的失败记录 → 失败，保留（供重试）；
 *   · 找不到 → 这条已经成功（直发或重试），且条带没被用户改过 → 清空。
 * 用户中途改过附件（指纹变了）就不动，避免把新选的东西抹掉。
 */
export function useClearSentAttachments(
  attach: Pick<HubAttachments, 'all' | 'reset'>,
  kind: 'dm' | 'group',
  scopeID: string | null,
): void {
  const sending = useHubStore((s) => (kind === 'dm' ? s.sending : s.groupSending))
  const failedCount = useHubStore((s) => Object.keys(s.failedByCid).length)
  const latest = useRef(attach)
  latest.current = attach
  const pending = useRef('')
  useEffect(() => {
    if (sending) {
      pending.current = attachmentFingerprint(latest.current.all)
      return
    }
    const fp = pending.current
    if (!fp) return
    pending.current = ''
    const state = useHubStore.getState()
    const stillFailed = Object.values(state.failedByCid).some((r) => {
      const sameScope = kind === 'dm' ? r.kind === 'dm' && r.agentId === scopeID : r.kind === 'group' && r.groupId === scopeID
      return sameScope && (r.attachmentsFingerprint ?? '') === fp
    })
    if (stillFailed) return
    if (attachmentFingerprint(latest.current.all) === fp) latest.current.reset()
  }, [sending, failedCount, kind, scopeID])
}
