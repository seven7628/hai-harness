// plugin-app-reply.ts —— 插件页面 host.command 的回执归一（纯函数，可在 Node 里单测）。
//
// 为什么单独一个文件：`plugin-app.ts` import 了 electron，Node 单测加载不了；而这条归一
// 曾经漏掉过顶层展开字段（页面拿到 `{ok, data:{session_id}}`，按文档读 `r.session_id`
// 得到 undefined）。把这段逻辑留在可测的纯函数里，回归不需要启动整个 Electron。
//
// 形状来源：渲染层 createHost 对 agent.* 回执做了 `{ok:true, data, ...data}` 的归一
// （§5.1 的 `{ok, session_id}` 与 §6.6 的 wire 形状同时成立）。主进程不能只挑
// ok/reason/data —— 那会把两种契约里更常用的那一种丢掉。

/** 主进程 → 插件页面看到的回执；顶层展开字段原样保留。 */
export interface PluginAppHostReply {
  ok: boolean
  reason?: string
  data?: unknown
  [key: string]: unknown
}

export function normalizeHostReply(raw: unknown): PluginAppHostReply {
  const reply = raw && typeof raw === 'object' ? (raw as Record<string, unknown>) : {}
  return {
    ...reply,
    ok: reply.ok === true,
    ...(typeof reply.reason === 'string' ? { reason: reply.reason } : {}),
    ...(reply.data !== undefined ? { data: reply.data } : {}),
  }
}
