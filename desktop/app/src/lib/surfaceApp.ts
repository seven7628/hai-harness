// URL 型插件的地址拼接（纯函数，零 store/React 依赖）。
//
// 为什么不用 `new URL(path, base)`：path 里出现 `//evil.com` / `https://evil` 时，
// `new URL` 会把 origin 换掉。安装期（bridge）与索引读侧（electron/surface-index.ts）
// 已经各拒了一次；这里是第三道，且必须与它们同一条口径 —— 只拼字符串，不解析。
//
// base 可以有 path（如 `http://host/app/`）；path 去掉头斜杠后接在 base 后面，
// 所以 `base=http://host/app/` + `path=/admin` = `http://host/app/admin`。
export function appUrlOf(base: string, path?: string): string {
  const b = String(base ?? '').trim().replace(/\/+$/, '')
  const p = String(path ?? '').trim().replace(/^\/+/, '')
  return p ? `${b}/${p}` : `${b}/`
}
