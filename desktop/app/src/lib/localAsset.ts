// localAsset.ts — Markdown 里的**本地**资源解析：相对 src → 绝对路径 → data URL。
//
// 为什么需要这一层（README 实测缺陷）：README 用 `brand/hai-logo-dark.svg` 这类**相对** src
// 引用同目录 / 工作区里的图。渲染进程此前没有任何读本地图片的通道（本地文件只能由主进程
// 开进内嵌浏览器走 file://），于是预览里的这类 <img> 只能去请求一个不存在的 URL —— 也就是
// 「README 的 logo 永远显示不出来」。
//
// 三条设计约束：
//   ① 只接管**本地** src：http(s)/data/blob 原样透传（shields.io 那类远程徽标仍走网络，不归
//      这里管），协议相对的 //host/x 同理。
//   ② 相对路径的锚点是**引用它的那个 Markdown 文件所在目录**（GitHub 同口径）；对话区的
//      Markdown 没有文件可锚（正文是模型产出的），退回工作区根 —— 所以 baseDir 就这两种来源。
//   ③ 读盘走主进程 IPC（fs:read-image，见 electron/read-image.ts）：路径合法性判定只做一份，
//      渲染进程不碰文件系统，也不自己猜「文件在不在」（猜错的代价是把「没权限」说成「不存在」）。
import { useEffect, useMemo, useState } from 'react'
import { getTransport } from '../transport'

// 有 scheme 的（http: / https: / data: / blob: / mailto: …）与协议相对 URL（//cdn/x.png）
// 都不是本地路径。
// 注意：Windows 盘符路径（C:\img\x.png）在同一个正则下会被误判成 scheme —— 桌面端只发布
// macOS（见 desktop/app/package.json 的 electron-builder 配置），故不为它加特例。
const HAS_SCHEME = /^(?:[a-z][a-z0-9+.-]*:|\/\/)/i

/** 该 src 是否要按本地文件解析（含显式的 file:// 写法）。 */
export function isLocalAssetSrc(src: string | undefined): boolean {
  const s = String(src ?? '').trim()
  if (!s || s.startsWith('#')) return false
  if (/^file:/i.test(s)) return true // file:// 也按路径处理：主进程只认路径
  return !HAS_SCHEME.test(s)
}

// decodeSafe：src 里的空格/中文常被写成 %20/百分号编码，而文件系统要的是解码后的路径。
// 解不开（文件名本身含裸 %）就按原样用 —— 宁可少解码，不可把合法文件名改坏。
function decodeSafe(s: string): string {
  if (!s.includes('%')) return s
  try {
    return decodeURIComponent(s)
  } catch {
    return s
  }
}

// fold：折叠 . / .. 与重复斜杠，得到规范的绝对 POSIX 路径（渲染进程无 node:path，纯字符串处理）。
function fold(p: string): string {
  const out: string[] = []
  for (const seg of p.split('/')) {
    if (!seg || seg === '.') continue
    if (seg === '..') {
      out.pop()
      continue
    }
    out.push(seg)
  }
  return '/' + out.join('/')
}

/**
 * 本地 src → 绝对路径（拿不到锚点或不是本地 src → null）。
 * baseDir 必须自身是绝对路径：相对路径不该再被相对锚定（那会得到「相对于某处的某处」）。
 */
export function resolveLocalAssetPath(src: string, baseDir?: string | null): string | null {
  if (!isLocalAssetSrc(src)) return null
  let p = String(src).trim()
  if (/^file:/i.test(p)) p = p.replace(/^file:\/\/(?:localhost)?/i, '') // file:///a/b → /a/b
  p = decodeSafe(p).split(/[?#]/)[0].replaceAll('\\', '/')
  if (p.startsWith('/')) return fold(p)
  const base = String(baseDir ?? '').replaceAll('\\', '/').replace(/\/+$/, '')
  if (!base.startsWith('/')) return null
  return fold(`${base}/${p}`)
}

/** 目录部分（POSIX 字符串处理，渲染进程没有 node:path）：'/a/b/c.md' → '/a/b'，'c.md' → ''。 */
export function dirOf(p: string | undefined): string {
  const n = String(p ?? '').replaceAll('\\', '/').replace(/\/+$/, '')
  const i = n.lastIndexOf('/')
  return i < 0 ? '' : i === 0 ? '/' : n.slice(0, i)
}

/**
 * 被预览的 Markdown 文件 → 相对资源的锚点目录。
 * 文件路径可能是工作区相对（文件树给的就是这种）也可能是绝对（工作区外 open-file）：
 * 相对的锚到工作区，绝对的直接用自己那层目录。返回 undefined 表示「没有可用锚点」。
 */
export function baseDirFor(filePath: string | undefined, workspace?: string): string | undefined {
  const d = dirOf(filePath)
  if (!d) return workspace // 'README.md'：与文件同级的图就在工作区根
  if (d.startsWith('/')) return d
  const ws = String(workspace ?? '').replace(/\/+$/, '')
  return ws ? `${ws}/${d}` : undefined
}

/** 展示用的相对路径：工作区内显示成工作区相对（与文件栏口径一致），否则原样显示。 */
export function displayPath(abs: string | null | undefined, workspace?: string): string {
  const a = String(abs ?? '')
  const ws = String(workspace ?? '').replace(/\/+$/, '')
  if (ws && (a === ws || a.startsWith(ws + '/'))) return a.slice(ws.length + 1) || a
  return a
}

export type LocalAssetState = { url?: string; error?: string }

// 缓存只存**成功**：失败往往是「文件还没被写出来」（模型刚写完 README，图随后落盘），
// 把失败也缓存住会让重渲永远停在破图上。key 用绝对路径（与 workspace 无关）。
const cache = new Map<string, LocalAssetState>()
const inflight = new Map<string, Promise<LocalAssetState>>()

function fetchAsset(abs: string, workspace?: string): Promise<LocalAssetState> {
  const hit = cache.get(abs)
  if (hit) return Promise.resolve(hit)
  const running = inflight.get(abs)
  if (running) return running
  const req = (async (): Promise<LocalAssetState> => {
    try {
      const r = await getTransport().fsReadImage({ path: abs, workspace: String(workspace ?? '') })
      if (r?.ok && r.dataUrl) return { url: r.dataUrl }
      return { error: r?.error || '图片读取失败' }
    } catch (e) {
      return { error: (e as Error)?.message ?? String(e) }
    } finally {
      inflight.delete(abs)
    }
  })()
  inflight.set(abs, req)
  void req.then((r) => {
    if (r.url) cache.set(abs, r)
  })
  return req
}

/**
 * 本地资源 → data URL。返回 `{ abs, url, error }`：abs 供「读不到」时把路径显示给用户
 *（否则用户只看到一张破图，不知道该去哪找它）。
 */
export function useLocalAsset(
  src: string | undefined,
  baseDir?: string | null,
  workspace?: string,
): { abs: string | null } & LocalAssetState {
  const abs = useMemo(
    () => (isLocalAssetSrc(src) ? resolveLocalAssetPath(String(src), baseDir) : null),
    [src, baseDir],
  )
  const [state, setState] = useState<LocalAssetState | undefined>(() => (abs ? cache.get(abs) : undefined))
  useEffect(() => {
    if (!abs) {
      setState(undefined)
      return
    }
    const hit = cache.get(abs)
    if (hit) {
      setState(hit)
      return
    }
    let alive = true
    setState(undefined) // 换图先清旧值：否则上一张图会在新图到达前继续显示（张冠李戴）
    void fetchAsset(abs, workspace).then((r) => {
      if (alive) setState(r)
    })
    return () => {
      alive = false
    }
  }, [abs, workspace])
  return { abs, ...state }
}
