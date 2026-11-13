// hub:// 深链的主进程侧纯逻辑（P4-02）。
//
// 刻意不 import electron：argv 提取是这里唯一有分支的部分，放在单独文件里
// 就能被 scripts/test-hub-context.mjs 直接 Node 测试（main.ts 不能）。

export const HUB_PROTOCOL = 'hub'

/**
 * 从进程 argv 里找 hub:// 深链。
 * Windows 常把带 ?seq= 的参数整段加引号传回来，去掉首尾引号再判。
 * 没有就返回 null（普通启动/其它参数不受影响）。
 */
export function extractHubDeepLinkArg(argv: readonly string[] | null | undefined): string | null {
  for (const raw of argv ?? []) {
    const arg = String(raw ?? '').trim().replace(/^["']+|["']+$/g, '')
    if (/^hub:\/\//i.test(arg)) return arg
  }
  return null
}
