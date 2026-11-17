// surfaces/pluginAsset.ts —— 插件图标字段的两种形态判定（纯函数，无 React / 无 loader）。
//
// `Project.icon` / `entries[].icon` 里可能是 emoji（老回执），也可能是目录内相对路径
// （新 manifest：`icon.svg`）。判定必须只有一份：左栏、入口菜单、右侧 rail 各写一次，
// 迟早出现「同一张图在一处是图片、另一处是四个字符」。
const ASSET_RE = /\.(svg|png|jpe?g|gif|webp)$/i

/**
 * isPluginAssetPath 这个 icon 值是不是「目录内相对路径」。
 *
 * 判据刻意只看图片扩展名 + 不含空白：manifest 安装期已校验路径（相对、不逃逸），
 * 这里再判「像不像文件」只是为了把 emoji 留在文本分支 —— 不能拿 `includes('.')`
 * 当判据（`v1.0` 这种文案会被误判成文件）。
 */
export function isPluginAssetPath(icon: string | undefined | null): boolean {
  const s = (icon ?? '').trim()
  return !!s && ASSET_RE.test(s) && !/\s/.test(s)
}

/**
 * canLoadPluginAsset 这个 URL 在当前环境里**值得**放进 `<img src>` 吗。
 *
 * `hai-surface://` 只在 Electron 主进程注册（main.tsx 给 body 加 `is-electron`）。
 * 浏览器里（vite dev / e2e 探针）放进去必然 ERR_UNKNOWN_URL_SCHEME：图不显示，
 * 控制台多一条红，而 onError 只会退回图标 —— 不如从一开始就不发这个请求。
 * 同源静态目录（探针把 base 换成 `/e2e/...`）不受影响。
 */
export function canLoadPluginAsset(url: string): boolean {
  if (!url) return false
  if (!url.startsWith('hai-surface://')) return true
  return typeof document !== 'undefined' && document.body?.classList?.contains('is-electron') === true
}
