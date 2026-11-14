// 会话归属路径（owner path）—— 虚拟 Project（插件）在前端的唯一编码。
//
// 问题：渲染层只有**一个** `state.workspace`，但插件会话的归属是虚拟路径
// （`<data-dir>/plugin-sessions/<id>/home`），它既不是工作区、也不进 projects 的
// localStorage。只靠 `state.workspace` 发命令 → 插件会话的 ask/new_session 会落到
// 当时打开的真实项目上（跨项目误投，且服务端会按真实工作区解析出**另一个**会话）。
//
// 解法（docs/creator-mode-v0.1/09-agent-bridge.md §4.5）：给每个 sid 记一张
// 「会话 → 归属路径」的映射，`send()` 按
//   payload.workspace → sessionOwnerPath[sid] → state.workspace
// 取值。这张表是**模块级 Map**（不是 store 字段）：`send()` 要在同步路径上读它，
// 而它不参与渲染 —— 放进 store 只会多一轮重渲和一份可被误改的副本。
//
// 与 lib/owner.ts 的分工：owner.ts 回答「谁是归属」（workspace:/agent:/group:，
// 与 Go 的 owner 包逐字对应）；本模块回答「归属到哪个路径」—— 只对虚拟 Project 有
// 增量语义，真实项目仍由 state.workspace 决定。**不要**把两者合并成一个概念。

/** 插件会话 home 的路径特征：`<data-dir>/plugin-sessions/<plugin-id>/home`。
 *
 *  为什么不按 `~/.go-code/plugin-sessions/` 全串匹配：data-dir 可被 HAI_DATA_DIR 改写，
 *  只有中间这段是稳定不变的（Go 侧 `isPluginHomePath` 也是按这段判）。 */
const PLUGIN_SESSIONS_SEGMENT = '/plugin-sessions/'

/** isPluginProjectPath 路径是否落在插件会话区（虚拟 Project 的 home）。
 *
 *  权威判据始终是 `projects[].virtual` —— 这里只用于「手上只有路径」的入口
 *  （send 守卫、接到未知 path 的命令、持久化过滤）。两处判据都必须过：
 *  后端一旦改目录布局，`virtual` 标志仍然对，本函数退化为保守（false）。 */
export function isPluginProjectPath(path: unknown): boolean {
  const p = String(path ?? '').replace(/\\/g, '/')
  if (!p) return false
  return p.includes(PLUGIN_SESSIONS_SEGMENT) || p.startsWith('~/.go-code/plugin-sessions/')
}

/** sessionOwnerPath：会话 id → 归属路径（虚拟 Project 的 home）。模块级，非渲染状态。 */
const sessionOwnerPath = new Map<string, string>()

/** getSessionOwnerPath 读会话归属路径；没有登记 → undefined（调用方回落 state.workspace）。 */
export function getSessionOwnerPath(sessionId: unknown): string | undefined {
  const sid = String(sessionId ?? '')
  if (!sid) return undefined
  return sessionOwnerPath.get(sid)
}

/** setSessionOwnerPath 登记会话归属路径（空 sid / 空 path 忽略，不留半个键）。 */
export function setSessionOwnerPath(sessionId: unknown, path: unknown): void {
  const sid = String(sessionId ?? '')
  const p = String(path ?? '')
  if (!sid || !p) return
  sessionOwnerPath.set(sid, p)
}

/** forgetSessionOwnerPath 删会话时清掉它的归属（否则同 id 复用会继承旧路径）。 */
export function forgetSessionOwnerPath(sessionId: unknown): void {
  const sid = String(sessionId ?? '')
  if (sid) sessionOwnerPath.delete(sid)
}

/** forgetSessionOwnerPathsOf 删项目/工作区时清掉该路径下的全部归属。 */
export function forgetSessionOwnerPathsOf(path: unknown): void {
  const p = String(path ?? '')
  if (!p) return
  for (const [sid, owner] of [...sessionOwnerPath]) if (owner === p) sessionOwnerPath.delete(sid)
}

/** clearSessionOwnerPaths 清空（测试用；生产只在「重开 bridge」这类整树重建处调用）。 */
export function clearSessionOwnerPaths(): void {
  sessionOwnerPath.clear()
}

/** sessionOwnerPathEntries 只读快照（测试断言用；生产代码不读）。 */
export function sessionOwnerPathEntries(): [string, string][] {
  return [...sessionOwnerPath.entries()]
}

export interface ResolveSendWorkspaceInput {
  /** 调用方显式给的 payload.workspace（最强：命令的目标就是它，例如左栏「在该项目新建会话」）。 */
  payloadWorkspace?: unknown
  /** 命令目标会话 id（payload.session_id / payload.id）。 */
  sessionId?: unknown
  /** 当前活跃工作区（store 的 state.workspace）。 */
  activeWorkspace?: unknown
  /** 会话归属查询（生产 = getSessionOwnerPath；测试可注入）。 */
  ownerOf?: (sessionId: string) => string | undefined
}

/** resolveSendWorkspace send() 的归属取值：payload.workspace → sessionOwner[sid] → activeWorkspace。
 *
 *  为什么是这个优先级：
 *   · payload 显式指定 = 调用方已经决定了目标（「在该项目里新建会话」不能因为用户
 *     当前停在别处就丢掉）；
 *   · 会话归属次之：会话一旦有了归属（插件 home），它的后续命令（ask/interrupt/
 *     switch_model）必须回到那里，**不能**跟着 UI 当前停在哪个项目走 —— 这正是
 *     插件会话最容易被误投的地方；
 *   · 最后才是当前活跃工作区（无会话的裸命令：workspace_add/list/git_* 等）。
 *
 *  返回值已 trim；全部缺失 → ''（调用方据此决定「不带 workspace 字段」）。 */
export function resolveSendWorkspace(input: ResolveSendWorkspaceInput): string {
  const explicit = String(input.payloadWorkspace ?? '').trim()
  if (explicit) return explicit
  const sid = String(input.sessionId ?? '')
  if (sid && input.ownerOf) {
    const owned = String(input.ownerOf(sid) ?? '').trim()
    if (owned) return owned
  }
  return String(input.activeWorkspace ?? '').trim()
}
