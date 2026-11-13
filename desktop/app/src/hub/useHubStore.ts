import { create } from 'zustand'
import { getTransport } from '../transport'
import { hubCommand } from './api'
import type {
  HubAgent, HubAlert, HubArtifactPreviewData, HubAttachment, HubConversation, HubDelivery, HubDMInfo, HubGroup, HubGroupPolicy,
  HubHistoryPage,
  HubMessage, HubGroupActivity, HubGroupProgress, HubNeed, HubPage, HubPromptLayer, HubPromptPreviewData,
  HubRound, HubRunEntry, HubSearchCursor, HubSearchHit, HubSearchPage, HubSearchScope, HubTask,
  HubTaskArtifact, HubTaskUpdate,
} from './types'
import { attachmentFingerprint, validateAttachments } from './attachments'
import {
  aroundHasMore, hasSeq, mapSearchHit, mapSearchPage, mergeAroundPage, mergeSearchHits,
  normalizeSearchQuery, normalizeSearchScope, searchAroundPlan, searchPagePayload,
} from './search'
import { addHubAlert, alertFromEvent } from './alerts'
import { applyHubDelta, dmScopeFromSession, dropDraftsForAgent, finalizeHubDraft, groupScopeFromSession, reconcileDraftsWithMessages, type HubMessageDelta, type HubStreamDraft } from './groupStream'
import {
  isHubTargetActive, projectGroupMessage, projectGroupRead, projectTaskUpdate, shouldApplyDMInfo,
  shouldApplyGroupProgress,
} from './groupProjection'
import { copyText } from '../lib/clipboard'
import {
  forgetTodoSnapshots, hubRunKey, isProjectSession, isTerminalTaskStatus,
  mergeTodoSnapshot, putRunName, putTodoSnapshot,
  type HubRunNameEvent, type HubRunNames, type HubTodoEvent, type HubTodoSnapshot,
} from '../lib/hubTodoScope'
import {
  formatHubDeepLink, hubDeepLinkKey, HUB_DEEP_LINK_INVALID_ERROR, parseHubDeepLink,
  type HubDeepLinkKind,
} from './deeplink'
import type { HubJumpRequest } from './seqJump'
import { normalizePromptScope, promptScopeMatches } from './promptScope'

// Agent Hub 的前端状态（§8.2 Hub Rail + §8.4 DM 视图）。
//
// 独立于 useAppStore：Hub 的 session_id（hub.<agent>.dm）与工作区会话是两套语义；
// 混进主 store 会让导航/右栏/恢复逻辑打架（主 store 也已在 wire() 里过滤 hub_* 事件，
// 避免 views/busySids 里留下永不清理的 hub 正文）。
//
// 这里的每条不变量都对应 P1-08 复审实测到的一个缺陷：
//   - select 用 token 防「先点 A 后点 B，A 的迟到响应覆盖 B」（B4）
//   - 只认根 run 的 chunk/agent_start/agent_end（B5：子 agent 会串进流式、提前熄运行点）
//   - 发送失败保留原文和失败气泡（B2：之前气泡与输入框一起清空）
//   - live 在根 run 结束时清空、重进会话时重置（B3：半截气泡残留）

/** 任务产物预览弹窗（App 级）：key 用于丢弃迟到的旧响应。 */
export interface HubArtifactPreviewState {
  key: string
  taskId: string
  path: string
  name: string
  loading: boolean
  error: string | null
  data: HubArtifactPreviewData | null
}

/**
 * Context 面板的提示词预览状态：loaded=true 时**同 agent + 同作用域**可复用（只读缓存）。
 *
 * `groupId` 是缓存键的一半，不是展示字段：L1 协议与 L2 名册按作用域分叉，只按 agent
 * 复用会让群作用域永远显示 DM 变体（切群也不重新请求）。空串 = DM。
 */
export interface HubPromptState {
  agentId: string
  /** 请求时的群 id；空串 = DM 作用域（后端的 group_id 缺省语义）。 */
  groupId: string
  layers: HubPromptLayer[]
  note: string
  totalBytes: number
  loading: boolean
  loaded: boolean
  error: string | null
}

/**
 * 一次发送失败的记录（DM / 群共用）。
 *
 * `clientMsgId` 是桥的幂等键（desktop/bridge/hub_commands.go 的 hubSend：命中同 cid
 * 直接回原 seq，不再 Ask 一次）。失败后重试必须沿用首发那一个 —— 换新 cid 会让
 * 「响应丢了但消息其实已入队」的那条重复进模型上下文（HUB-IDEM-01）。
 */
export interface HubFailedSend {
  kind: 'dm' | 'group'
  text: string
  clientMsgId: string
  /** kind=dm 的目标智能体。 */
  agentId?: string
  /** kind=group 的目标群。 */
  groupId?: string
  /** 群的 @ 目标（重发要原样带上，否则唤醒面变小）。 */
  mentions?: string[]
  mentionAll?: boolean
  /** 首发时的附件：重试必须原样带上（含图片 data URL），否则「重试」会丢图。 */
  attachments?: HubAttachment[]
  /**
   * 附件指纹（attachmentFingerprint）：重试/同文重发的幂等键要比较它。
   * 只比正文的话，用户「删掉旧图换一张新图再发」会命中旧 cid 被 bridge 挡成旧消息。
   */
  attachmentsFingerprint?: string
  /**
   * 首发时的**原始草稿**（`withFileRefs` 拼 `@path` 之前）—— 只用于「成功后要不要清 composer」。
   *
   * `text` 已经是拼过文件引用的正文（`草稿 + '\n@/a.md'`），拿它跟输入框里的草稿比
   * 永远不相等：行内重试成功后草稿不清，用户再按回车就把同一条又发一遍（多发）。
   * 老调用没有这个字段时退回比较 `text`（今天的行为）。
   */
  sourceDraft?: string
}

interface HubState {
  open: boolean
  agents: HubAgent[]
  dm: Record<string, HubDMInfo>
  activeId: string | null
  /** 每次 select/selectGroup（含深链）自增：左栏据此把 tab 同步到目标 scope。 */
  selectionNonce: number
  profile: HubAgent | null
  sid: string | null
  messages: HubMessage[]
  hasMore: boolean
  loading: boolean
  loadingOlder: boolean
  sending: boolean
  error: string | null
  /** 一次性提示（回滚成功等），与 error 分开显示。 */
  notice: string | null
  /** 当前 DM 的流式正文（只累计根 run 的 content_chunk）。 */
  live: string
  /** 当前根 run 的 run_id（子 agent chunk 的 run_id 不同，据此过滤）。 */
  rootRunId: string | null
  /** agent_id → 是否在跑（左栏状态环）。 */
  running: Record<string, boolean>
  /** 每个 agent 未发送的草稿（切 surface 不丢）。 */
  drafts: Record<string, string>
  /** 每个群 composer 的草稿（与 drafts 同语义；重试成功后按正文比对清空）。 */
  groupDraftText: Record<string, string>
  /**
   * 失败发送记录表：client_msg_id → 记录。
   *
   * 不能只留「最新一条」单槽：成功一条就把槽清空的话，旧失败气泡还在时间线上，
   * 但它的 cid 已经丢了 —— 重发会换新 cid、账本多一条（复审 B3）。
   */
  failedByCid: Record<string, HubFailedSend>
  /** 最新一条失败（= failedByCid 里最后写入的那条）：横幅只取最新的这条。 */
  failed: HubFailedSend | null
  /** 任务板/Needs you/会话信息/上下文（只读）抽屉。 */
  inspector: 'tasks' | 'needs' | 'session' | 'context' | null
  tasks: HubTask[]
  tasksLoading: boolean
  /**
   * Project 会话的待办快照（场景2：DM/群内联卡）。键 = (session, run)，
   * 只由 `tool_run_end` 的 `todos` 字段喂进来；Hub 自己的会话不记。
   * 只读展示，**绝不**写进 hub_message / refs（不进群快照、不进智能体上下文）。
   */
  todoSnapshots: Record<string, HubTodoSnapshot>
  /** Project 会话的 run → 展示名（agent_start 维护；root = 主 run）。有界。 */
  runNames: HubRunNames
  needs: HubNeed[]
  needsLoading: boolean
  /** Hub 全局告警（P2.5-05）：恢复失败/预算/推送失败等，按 envelope 去重。 */
  alerts: HubAlert[]
  /** DM 右栏视图：Chat = 对话，Run = 工具/压缩运行日志。 */
  view: 'chat' | 'run'
  /** 当前 DM 的 Run 日志（只收该 session 的根/子工具事件）。 */
  runLog: HubRunEntry[]
  /** 系统动作行（压缩回执/清空等），与用户气泡同列显示。 */
  actions: HubMessage[]
  /** 群组（P3a-08）：列表/当前群/时间线/最近一轮路由。 */
  groups: HubGroup[]
  groupsLoading: boolean
  activeGroupId: string | null
  groupMessages: HubMessage[]
  groupRounds: HubRound[]
  groupActivity: HubGroupActivity[]
  groupSending: boolean
  /** 群内流式草稿（P3a-09）：按 message_id 并行，final 后删除。 */
  groupDrafts: HubStreamDraft[]
  /** final 过的 message_id 最近 200 个；迟到 delta 据此丢弃。 */
  finalizedMessageIds: string[]
  /** 左栏群组投影（hub_group_list.progress + hub_message 增量）。 */
  groupInfo: Record<string, HubGroupProgress>
  /** 每群自动对话视图（hub_conversation 事件 + hub_group_get 水合）。 */
  groupConversations: Record<string, HubConversation>
  /** 每群人类已读游标（hub_group_get.cursor / hub_read；多端共享）。 */
  groupReadSeq: Record<string, number>
  /** 派任务进行中 / 上一次派发的说明（queued 或已终止原因）。 */
  groupAssigning: boolean
  groupAssignNote: string | null
  /** hub_conversation_stop 进行中。 */
  groupConvStopping: boolean
  /** 待执行的跳转/高亮（深链、Context 面板的触发 seq）；消费方按 targetId 认领。 */
  jump: HubJumpRequest | null
  /**
   * 「本次 Hub 是被深链打开的」：为 true 时 HubSurface 挂载不再自动重新水合
   * （resumeOpenTarget 直接返回）。不能做成一次性 consume：React StrictMode 会
   * 双跑 mount effect，第二次就会把深链自己的 selectGroup 换代 abort（复审 B1）。
   * 生命周期 = 本次打开：closeHub / openHub 都清掉，深链自己也在打开时重置。
   */
  deepLinkOwnedOpen: boolean
  /** 当前选中智能体的系统提示词预览（只读；hub_agent_prompt_preview）。 */
  prompt: HubPromptState | null
  /**
   * Agent 编辑器：null = 关闭；'new' = 新建；HubAgent = 编辑该智能体。
   * 放在 store 而不是 HubSurface：左栏「＋」、中栏「编辑」、右栏详情都要打开同一个弹层。
   */
  editor: HubAgent | 'new' | null
  /** 任务卡产物预览弹窗；null = 关闭。 */
  artifactPreview: HubArtifactPreviewState | null

  // —— 全历史搜索（hub_search，P4-04b-1）——
  /** 已提交的查询（输入框由 HubSidebar 自己持有，避免 debounce 回环）。 */
  searchQuery: string
  searchScope: HubSearchScope
  searchHits: HubSearchHit[]
  searchHasMore: boolean
  /** 下一页游标（(created_at,id)）；null = 没有更旧的结果。 */
  searchNext: HubSearchCursor | null
  searchLoading: boolean
  searchError: string | null

  openHub(): void
  closeHub(): void
  /** 从左侧栏点智能体：确保 Hub 打开，再选中该 DM（打开即已读的语义不变）。 */
  openAgent(agentID: string): Promise<void>
  /** 从左侧栏点群组：确保 Hub 打开，再选中该群。 */
  openGroup(groupID: string): Promise<void>
  /** 打开 Agent 编辑器（null = 新建）。 */
  openEditor(agent: HubAgent | null): void
  closeEditor(): void
  /** 打开任务产物预览；同一 task/path 重入会作废旧请求（key 竞态保护）。 */
  openArtifactPreview(taskId: string, artifact: HubTaskArtifact): Promise<void>
  closeArtifactPreview(): void
  refresh(): Promise<void>
  select(agentID: string): Promise<void>
  clearSelection(): void
  loadOlder(): Promise<void>
  /**
   * 发一条 DM 消息。
   *
   * `sourceDraft` = 拼文件引用**之前**的 composer 草稿：成功后据此清草稿
   * （见 clearsDraft）。重试时由失败记录带回来，调用方不用自己算。
   */
  send(text: string, reuseClientMsgId?: string, attachments?: HubAttachment[], sourceDraft?: string): Promise<boolean>
  /**
   * 重发一条失败的消息（DM 或群），沿用首发 client_msg_id。
   *
   * `clientMsgId` 不传 = 最新的那条失败（横幅用）；传了就按气泡自己的 cid 找记录 ——
   * 多条失败时点 A 的重试必须发 A，不能取「最新的那条」（复审 B1）。
   */
  retry(clientMsgId?: string): Promise<boolean>
  setDraft(agentID: string, text: string): void
  setGroupDraft(groupID: string, text: string): void
  clearError(): void
  clearNotice(): void
  /** 人类全历史搜索（250ms debounce 在 UI 层；这里只发一次 hub_search，旧响应按代际丢弃）。 */
  search(query: string, scope?: HubSearchScope): Promise<void>
  /** 用 next_before 游标翻更旧的一页（结果去重合并）。 */
  searchMore(): Promise<void>
  /** 清空查询/结果（Escape、清空输入、关闭面板）。 */
  clearSearch(): void
  /** 点击命中：打开会话 → 命中 seq 不在已加载历史时加载附近一页 → 跳转+高亮。 */
  openSearchHit(hit: HubSearchHit): Promise<void>
  setSearchScope(scope: HubSearchScope): void
  dismissAlert(id: string): void
  setInspector(panel: 'tasks' | 'needs' | 'session' | 'context' | null): void
  setView(view: 'chat' | 'run'): void
  /** 请求当前视图跳到 seq（Context 面板/触发 chip 用；由目标视图自己消费）。 */
  requestJump(seq: number): void
  /** 消费方只清掉自己接的那一次（nonce 防旧请求被后来的跳转误清）。 */
  clearJump(nonce: number): void
  /** HubSurface 挂载时调用：重新水合当前 target（深链拥有本次打开时跳过）。 */
  resumeOpenTarget(): void
  /** hub:// 深链：打开 Hub → 选中目标 → 历史就绪后定位 seq。重复调用共享同一次加载。 */
  openDeepLink(raw: string): Promise<boolean>
  /** 复制深链（Clipboard API + 主进程兜底，见 lib/clipboard）；结果写 notice/error。 */
  copyDeepLink(kind: HubDeepLinkKind, id: string, seq?: number): Promise<boolean>
  /**
   * 只读拉取系统提示词分层（同 agent **且同作用域**已加载则复用；force 强制刷新）。
   *
   * `groupID` 可选：带上是群变体 + 名册，不带是 DM 变体（与后端 hubPromptPreview 的
   * 缺省语义一致）。旧调用 `loadPromptPreview(agentId, force)` 语义不变。
   */
  loadPromptPreview(agentID: string, force?: boolean, groupID?: string): Promise<void>
  loadGroups(): Promise<void>
  /**
   * 新建群组（hub_group_create）：成功后重拉左栏列表并打开新群。
   *
   * 空名 / 空成员在前端就挡掉：后端也会校验，但弹层要立刻给出原因，不必等一次往返。
   * 失败统一写 error 并返回 null（弹层留在原地显示错误）；成功返回服务端分配的 group。
   */
  createGroup(req: {
    name: string
    member_ids: string[]
    owner_agent_id?: string
    policy?: HubGroupPolicy
  }): Promise<HubGroup | null>
  selectGroup(groupID: string): Promise<void>
  loadGroupActivity(groupID?: string): Promise<void>
  clearGroup(): void
  sendGroup(text: string, mentions?: string[], mentionAll?: boolean, reuseClientMsgId?: string, attachments?: HubAttachment[], sourceDraft?: string): Promise<boolean>
  sendGroupAssign(groupID: string, req: { agent_id: string; instruction: string; title?: string }): Promise<boolean>
  stopGroupConversation(groupID: string): Promise<boolean>
  /** P6-04 ④ 手动重置本群对话限制（hub_conversation_reset）。 */
  resetGroupConversation(groupID: string): Promise<boolean>
  loadTasks(): Promise<void>
  loadNeeds(): Promise<void>
  cancelTask(taskID: string): Promise<void>
  retryTask(taskID: string): Promise<void>
  resolveNeed(needID: string, resolution: string, answers?: Record<string, string>): Promise<void>
  clearContext(): Promise<void>
  /** 收一条 Project 会话的 tool_run_end 待办快照（Hub 自身会话 / 无 todos 字段不记）。 */
  applyTodoSnapshot(event: HubTodoEvent): void
  /** 记 Project 会话的 run 展示名（agent_start；根 run 也记，用于区分「主会话」）。 */
  rememberRunName(event: HubRunNameEvent): void
  /** 任务终态：立刻收掉属于它的内联待办卡（不必等 LRU）。 */
  forgetTodosForTask(task: HubTask): void
  handleEvent(line: string): void
}

let optimisticSeq = 0 // 负数临时 seq，避免与真实 seq（正数）混淆
let groupSendGen = 0 // 群发送代际：切群/新一轮发送后，旧响应不得改状态
let groupSelectGen = 0 // 选群代际：A→B→A 时旧响应不得覆盖新状态
// 已请求过的群已读游标：同一 seq 不重复发 hub_mark_read（活跃群每条新消息一次，够用且不刷屏）。
const groupReadRequested = new Map<string, number>()
// 每个群本地投影最近一次更新时间：loadGroups 响应用它挡「事件先到、响应后到」的旧快照。
const groupInfoUpdatedAt = new Map<string, number>()
// 每个智能体 DM 投影最近一次本地更新时间：refresh 响应用它挡刷新期间到达的消息（P3b-08）。
const dmInfoUpdatedAt = new Map<string, number>()
// 跳转请求代际：消费方按 nonce 只清自己接的那一次（切视图时的旧请求不能误清新请求）。
let jumpNonce = 0
// hub:// 深链在飞表：OS 重传/连点同一链接时共享同一次选择与加载（P4-02）。
const deepLinkInflight = new Map<string, Promise<boolean>>()
// 跳转请求的存活时间：视图消费后不立刻从 store 清除（StrictMode 会双跑 effect，
// 第一实例清掉后第二实例就看不到跳转、高亮丢失）。2s 后自动过期，避免一条
// 永不消费的 jump 永久挡住贴底跟随（复审 B1/B3）。
export const HUB_JUMP_TTL_MS = 2000
let jumpTimer: ReturnType<typeof setTimeout> | null = null
// 搜索代际：输入 debounce 里后发的查询必须能丢弃先发的迟到响应（否则结果会闪回旧词）。
let searchGen = 0
// 产物预览代际：旧请求的迟到响应不得覆盖新打开的文件（也不得在关闭后复活弹窗）。
let artifactPreviewGen = 0
// 待办快照的到达序（单调）：同键后到覆盖先到、LRU 淘汰都按它排序。
// 用代际计数而不是 Date.now()：同一毫秒内多条 todo_* 事件必须仍可比较先后。
let todoArrival = 0

function clearJumpTimer(): void {
  if (jumpTimer) {
    clearTimeout(jumpTimer)
    jumpTimer = null
  }
}

/** 给 jump 装上过期定时器；只清仍属于这个 nonce 的那一条（期间可能已被新跳转替换）。 */
function armJumpTimer(nonce: number, clear: () => void): void {
  clearJumpTimer()
  jumpTimer = setTimeout(() => {
    jumpTimer = null
    clear()
  }, HUB_JUMP_TTL_MS)
}

/** 本地投影变了：记一笔时间，供 loadGroups 的竞态守卫判断谁更新。 */
function touchGroupInfo(gid: string): void {
  groupInfoUpdatedAt.set(gid, Date.now())
}

/** DM 投影本地变了（新消息/打开清零）：refresh 的竞态守卫按这个时点判新旧。 */
function touchDMInfo(agentID: string): void {
  if (!agentID) return
  dmInfoUpdatedAt.set(agentID, Date.now())
}

/** 失败记录表（Object 保留插入顺序：最后一条就是「最新失败」）。 */
type FailedMap = Record<string, HubFailedSend>

function latestFailed(map: FailedMap): HubFailedSend | null {
  const cids = Object.keys(map)
  return cids.length > 0 ? map[cids[cids.length - 1]] : null
}

/**
 * 清掉一条 cid 的失败记录，并把 `failed`（横幅用）重新指向剩下的最新一条。
 *
 * 成功一条只清这一条：别的失败气泡还在时间线上，它们的 cid 必须留着（复审 B3）。
 */
function dropFailed(map: FailedMap, cid: string): { failedByCid: FailedMap; failed: HubFailedSend | null } {
  if (!cid || !map[cid]) return { failedByCid: map, failed: latestFailed(map) }
  const next: FailedMap = { ...map }
  delete next[cid]
  return { failedByCid: next, failed: latestFailed(next) }
}

function rememberFailed(map: FailedMap, rec: HubFailedSend): Pick<HubState, 'failedByCid' | 'failed'> {
  // 同 cid 再次失败：覆盖正文/目标，但保留它在时间线上的位置（Object 覆盖不换位）。
  return { failedByCid: { ...map, [rec.clientMsgId]: rec }, failed: rec }
}

/**
 * 按「正文 + 目标」找回这一行气泡的首发 cid。
 *
 * 用户没点「重试」、直接把失败的正文再发一次时也要沿用，否则账本会多一条
 * （HUB-IDEM-01 / 复审 B3）。同文多条失败取最早的那条（时间线上第一个失败气泡）。
 */
function reuseCidFor(map: FailedMap, kind: 'dm' | 'group', scopeID: string, text: string, attachmentsFingerprint = ''): string {
  for (const cid of Object.keys(map)) {
    const rec = map[cid]
    if (rec.kind !== kind || rec.text !== text) continue
    // 正文相同但附件不同 = 另一条消息（换图重发不能被幂等挡成旧消息）。
    if ((rec.attachmentsFingerprint ?? '') !== attachmentsFingerprint) continue
    if (kind === 'dm' ? rec.agentId === scopeID : rec.groupId === scopeID) return cid
  }
  return ''
}

/** 草稿是否仍是这次发出去的正文：用户后来改过的输入不能被重试抹掉（复审 B2）。 */
function draftEquals(draft: string | undefined, body: string): boolean {
  return typeof draft === 'string' && draft.trim() === body
}

/**
 * 这次发送成功后，composer 里那条草稿还能不能清掉。
 *
 * 优先比 `sourceDraft`（拼 @path 之前的原始草稿）：文件引用消息的 wire 正文是
 * `草稿 + '\n@/path'`，比正文永远不等，草稿就永远清不掉（行内重试后按回车会多发一条）。
 * 老记录没有 sourceDraft 时退回比 `text` —— 与之前一致，不做更激进的事。
 */
function clearsDraft(draft: string | undefined, sourceDraft: string | undefined, text: string): boolean {
  return draftEquals(draft, sourceDraft ?? text)
}

/**
 * 发送失败后要不要把原文放回输入框。
 *
 * 草稿现在是**乐观清空**（按下发送/回车立刻清，见 send/sendGroup 的第一处 set）：
 * 失败时必须还回去，否则用户得重打一遍。但用户已经在这段等待里输入了新内容时
 * 绝不覆盖 —— 输入框里用户的字优先于「方便重试」。
 *
 * 返回 null = 不动草稿；返回字符串 = 写回这段原文。
 */
function restoreDraft(draft: string | undefined, sourceDraft: string | undefined, text: string): string | null {
  if (typeof draft === 'string' && draft.trim()) return null
  const original = (sourceDraft ?? text).trim()
  return original || null
}

function roundHasAction(r: HubRound | null | undefined): boolean {
  return !!r && (!!r.mention_all || (r.targets ?? []).length > 0 || (r.dropped ?? 0) > 0)
}

function upsertRound(list: HubRound[], round: HubRound | null | undefined): HubRound[] {
  if (!roundHasAction(round) || !round) return list
  const next = list.filter((r) => r.id !== round.id)
  next.push(round)
  next.sort((a, b) => (a.trigger_seq ?? 0) - (b.trigger_seq ?? 0))
  return next
}

/**
 * 群已读游标 best-effort（§7.5.7）：只前进、同 seq 只发一次；失败不回滚 UI
 * （未读点已经清掉也无妨，下次 open/select 会再对齐），但允许下一次 seq 重试。
 */
async function markGroupRead(groupID: string, seq: number): Promise<void> {
  const latest = Math.max(seq, groupReadRequested.get(groupID) ?? 0)
  if (latest <= (groupReadRequested.get(groupID) ?? 0)) return
  groupReadRequested.set(groupID, latest)
  const r = await hubCommand<{ seq?: number }>('hub_mark_read', { group_id: groupID, seq: latest })
  if (!r.ok && groupReadRequested.get(groupID) === latest) groupReadRequested.delete(groupID)
}

export const useHubStore = create<HubState>((set, get) => ({
  open: false,
  agents: [],
  dm: {},
  activeId: null,
  selectionNonce: 0,
  profile: null,
  sid: null,
  messages: [],
  hasMore: false,
  loading: false,
  loadingOlder: false,
  sending: false,
  error: null,
  notice: null,
  live: '',
  rootRunId: null,
  running: {},
  drafts: {},
  groupDraftText: {},
  failedByCid: {},
  failed: null,
  inspector: null,
  tasks: [],
  tasksLoading: false,
  todoSnapshots: {},
  runNames: {},
  needs: [],
  needsLoading: false,
  alerts: [],
  view: 'chat',
  runLog: [],
  actions: [],
  groups: [],
  groupsLoading: false,
  activeGroupId: null,
  groupMessages: [],
  groupRounds: [],
  groupActivity: [],
  groupSending: false,
  groupDrafts: [],
  finalizedMessageIds: [],
  groupInfo: {},
  groupConversations: {},
  groupReadSeq: {},
  groupAssigning: false,
  groupAssignNote: null,
  groupConvStopping: false,
  jump: null,
  deepLinkOwnedOpen: false,
  prompt: null,
  editor: null,
  artifactPreview: null,
  searchQuery: '',
  searchScope: 'all',
  searchHits: [],
  searchHasMore: false,
  searchNext: null,
  searchLoading: false,
  searchError: null,

  openHub() {
    set({ open: true, deepLinkOwnedOpen: false })
    void get().refresh()
    void get().loadTasks()
    void get().loadNeeds()
    void get().loadGroups()
  },

  closeHub() {
    // 关闭即作废：深链的所有权只覆盖「这一次打开」，正常重开仍要 mount 水合；
    // 未执行的跳转也一并丢弃（下次打开从当前时间线底部开始）。
    // 右栏回到默认概览、编辑器一并关闭：退出 Hub 后不该留一个悬空的弹层。
    // 待办快照/run 名表**刻意不清**：Hub 关闭期间仍在收 Project 事件（场景2），
    // 重开时内联卡应当已经是最新的；它们的归属由任务的 origin 决定，不会跨作用域串台。
    clearJumpTimer()
    set({ open: false, deepLinkOwnedOpen: false, jump: null, inspector: null, editor: null, view: 'chat' })
  },

  async openAgent(agentID) {
    if (!agentID) return
    if (!get().open) get().openHub()
    await get().select(agentID)
  },

  async openGroup(groupID) {
    if (!groupID) return
    if (!get().open) get().openHub()
    await get().selectGroup(groupID)
  },

  openEditor(agent) {
    set({ editor: agent ?? 'new' })
  },

  closeEditor() {
    set({ editor: null })
  },

  async openArtifactPreview(taskId, artifact) {
    const path = String(artifact.path ?? '').trim()
    const name = String(artifact.name ?? '').trim()
    const key = `${taskId}\u0000${path || name}`
    const data: HubArtifactPreviewData | null = null
    const gen = ++artifactPreviewGen
    set({
      artifactPreview: {
        key, taskId, path, name, loading: true, error: null, data,
      },
    })
    const payload: Record<string, unknown> = { task_id: taskId, path }
    if (!path && name) payload.name = name
    const r = await hubCommand<HubArtifactPreviewData>('hub_task_artifact_preview', payload)
    // 旧响应不能覆盖新请求（快速连点 A→B）；关闭后到达的响应也直接丢弃。
    if (gen !== artifactPreviewGen) return
    const cur = get().artifactPreview
    if (!cur || cur.key !== key) return
    if (!r.ok || !r.data) {
      set({ artifactPreview: { ...cur, loading: false, error: r.error || '预览失败', data: null } })
      return
    }
    set({ artifactPreview: { ...cur, loading: false, error: null, data: r.data } })
  },

  closeArtifactPreview() {
    artifactPreviewGen++
    set({ artifactPreview: null })
  },

  async refresh() {
    // 不碰 loading：每次发送完都刷新列表，共用 loading 会让消息区闪「加载中」。
    const startedAt = Date.now()
    const r = await hubCommand<{ agents?: HubAgent[]; dm?: Record<string, HubDMInfo> }>('hub_agent_list', {})
    if (!r.ok) {
      set({ error: r.error || '读取智能体列表失败' })
      return
    }
    const agents = r.data?.agents ?? []
    const dm = r.data?.dm ?? {}
    const completedAt = Date.now()
    // 事件可能比列表响应先到（§7.5.7）：DM 未读/预览用同一套 last_seq + 请求时点守卫
    // 逐个合并，旧快照不能把刷新期间刚落地的消息/未读覆盖回去（P3b-07 复审残余 3）。
    const live = new Set(agents.map((a) => a.id))
    set((s) => {
      const merged: Record<string, HubDMInfo> = {}
      for (const [id, info] of Object.entries(s.dm)) {
        // 服务端已经不存在的智能体不再留投影（与整表替换时的口径一致）。
        if (live.has(id)) merged[id] = info
      }
      for (const [id, info] of Object.entries(dm)) {
        if (shouldApplyDMInfo(merged[id], info, dmInfoUpdatedAt.get(id) ?? 0, startedAt)) {
          merged[id] = info
          dmInfoUpdatedAt.set(id, completedAt)
        }
      }
      // 排序：有未读优先 → 最近有消息 → 名字（左栏是「谁在等我」，不是通讯录）。
      // 用合并后的投影排序：本地更新的未读/时间要参与排序，而不是服务端旧值。
      const rank = (a: HubAgent): [number, number, string] => {
        const info = merged[a.id] ?? {}
        return [info.unread && info.unread > 0 ? 0 : 1, -(Date.parse(String(info.last_at ?? '')) || 0), a.name]
      }
      const sorted = [...agents].sort((a, b) => {
        const ra = rank(a)
        const rb = rank(b)
        return ra[0] - rb[0] || ra[1] - rb[1] || ra[2].localeCompare(rb[2])
      })
      return { agents: sorted, dm: merged }
    })
  },

  async select(agentID) {
    if (!agentID) return
    groupSendGen++
    groupSelectGen++
    set((s) => ({
      activeId: agentID,
      selectionNonce: s.selectionNonce + 1,
      activeGroupId: null,
      groupMessages: [],
      groupActivity: [],
      groupRounds: [],
      groupSending: false,
      groupDrafts: [],
      messages: [],
      hasMore: false,
      live: '',
      rootRunId: null,
      runLog: [],
      actions: [],
      error: null,
      // 切目标就丢掉失败记录：失败气泡已随消息列表清空，留着它重试会发到别人的会话。
      failed: null,
      failedByCid: {},
      loading: true,
      // 每次打开/切换 DM 都回到 Chat：Run 日志是「当前会话的运行态」，不该跨会话泄漏。
      view: 'chat',
      // 手动切换目标：上一次深链/触发 chip 留下的跳转不再适用于新会话。
      jump: null,
      // 离开期间的事件没人接：重开会话先假设空闲，运行中的话下面的 chunk 会把它点亮。
      running: { ...s.running, [agentID]: false },
    }))
    const r = await hubCommand<HubPage>('hub_open', {
      target: { kind: 'dm', agent_id: agentID },
      limit: 50,
    })
    // 竞态：A 的响应可能晚于 B。只要用户已经切走，这一份就丢掉。
    if (get().activeId !== agentID) return
    if (!r.ok) {
      set({ loading: false, error: r.error || '打开对话失败' })
      return
    }
    set((s) => {
      const history = r.data?.messages ?? []
      const patch: Partial<HubState> = {
        loading: false,
        profile: r.data?.agent ?? null,
        sid: r.data?.session_id ?? null,
        messages: history,
        hasMore: r.data?.has_more === true,
      }
      // 断线/重连对账（场景2b，与群的 selectGroup 同一套）：历史里已有的 message_id
      // 说明那轮已经落地，残留草稿必须删掉并记入 finalized，迟到 delta 不能再复活它。
      const rec = reconcileDraftsWithMessages(
        s.groupDrafts,
        s.finalizedMessageIds,
        history.map((m) => m.id).filter((id): id is string => typeof id === 'string' && id.length > 0),
      )
      if (rec.drafts !== s.groupDrafts) {
        patch.groupDrafts = rec.drafts
        patch.finalizedMessageIds = rec.finalized
      }
      return patch
    })
    // 打开过 = 已读，但前提是 Hub 仍然开着：关闭 Hub 后才落地的响应
    // 不能把「关闭期间新到的消息」标成已读（B4）。
    if (!get().open) return
    touchDMInfo(agentID) // 本地清零也算一次更新：刷新请求若在飞，不能被旧未读覆盖回来
    set((s) => {
      const dm = { ...s.dm }
      const info = { ...(dm[agentID] ?? {}) }
      info.unread = 0
      dm[agentID] = info
      return { dm }
    })
  },

  clearSelection() {
    set({ activeId: null, profile: null, sid: null, messages: [], hasMore: false, live: '', rootRunId: null, failed: null, failedByCid: {}, jump: null, view: 'chat' })
  },

  async loadOlder() {
    const { activeId, messages, hasMore, loadingOlder } = get()
    if (!activeId || !hasMore || loadingOlder || messages.length === 0) return
    const before = messages[0].seq
    set({ loadingOlder: true })
    const r = await hubCommand<HubHistoryPage>('hub_history', {
      agent_id: activeId,
      before_seq: before,
      limit: 50,
    })
    if (get().activeId !== activeId) return
    if (!r.ok) {
      set({ loadingOlder: false, error: r.error || '加载更早消息失败' })
      return
    }
    const older = r.data?.messages ?? []
    set((s) => {
      const pageSeqs = new Set(older.map((m) => m.seq))
      // 保留：本页之前的真实消息 + 所有乐观/失败气泡（seq<=0，不能被翻页滤掉）。
      const keep = s.messages.filter((m) => m.seq > 0 && !pageSeqs.has(m.seq))
      return {
        loadingOlder: false,
        hasMore: r.data?.has_more === true,
        messages: [...older, ...keep].sort((a, b) => a.seq - b.seq),
      }
    })
  },

  async send(text, reuseClientMsgId, attachments, sourceDraft) {
    const { activeId, sending } = get()
    const body = (text ?? '').trim()
    // 拼 @path 之前的草稿（只用来决定成功后清不清输入框）；老调用没有就退回正文。
    const draft0 = typeof sourceDraft === 'string' ? sourceDraft.trim() : undefined
    // 附件（图片）可以没有正文：图片走 attachments，正文允许为空（后端存「[图片]」）。
    const check = validateAttachments(attachments)
    const atts = check.ok ? check.attachments : []
    if (!activeId || (!body && atts.length === 0) || sending) return false
    if (!check.ok) {
      set({ error: check.error || '附件不合法' })
      return false
    }
    // 幂等键（§7.1.3）：首发时定下，失败记录保留它；重试 / 同一条正文重发都沿用它。
    // 桥按 client_msg_id 去重（hubSend 命中即回原 seq、deduplicated），换新 cid 会让
    // 「响应丢了但消息其实已入队」的那条重复进模型上下文（HUB-IDEM-01）。
    // 按失败表里「同正文 + 同目标 + 同附件指纹」的那条找，而不是只看最新一条：中间成功
    // 过别的消息之后，旧失败气泡的 cid 仍要能找回（复审 B3）；换图重发则是新消息。
    const fp = attachmentFingerprint(atts)
    const reuse = reuseClientMsgId || reuseCidFor(get().failedByCid, 'dm', activeId, body, fp)
    const n = ++optimisticSeq
    const clientMsgId = reuse || `ui-${Date.now()}-${n}`
    const optimistic: HubMessage = {
      seq: -n,
      sender: { kind: 'human' },
      text: body,
      ...(atts.length > 0 ? { attachments: atts } : {}),
      pending: true,
      clientMsgId,
    }
    set((s) => ({
      // 重发上一次失败的那条：先把失败气泡从时间线摘掉，再挂新的（别叠出第二条，HUB-FAIL-01）。
      messages: [...(reuse ? s.messages.filter((m) => !(m.failed && m.clientMsgId === reuse)) : s.messages), optimistic],
      sending: true,
      error: null,
      notice: null,
      // 只摘这一条 cid：同会话里还有别的失败气泡，它们的记录要留着（复审 B3）。
      ...dropFailed(s.failedByCid, reuse),
      // 乐观清空：消息已经进时间线了，输入框不该再等响应（真 bridge 慢时尤其明显）。
      // 失败分支按 restoreDraft 放回去 —— 只在用户没输入新内容时。
      ...(clearsDraft(s.drafts[activeId], draft0, body) ? { drafts: { ...s.drafts, [activeId]: '' } } : {}),
    }))
    const r = await hubCommand<{ seq?: number; deduplicated?: boolean }>('hub_send', {
      target: { kind: 'dm', agent_id: activeId },
      text: body,
      client_msg_id: clientMsgId,
      ...(atts.length > 0 ? { attachments: atts } : {}),
    })
    if (!r.ok) {
      // 失败：气泡标 failed（可重试），失败记录带原文 + 附件 + 首发 cid —— 不能两头都清。
      const rec: HubFailedSend = {
        kind: 'dm', text: body, clientMsgId, agentId: activeId,
        ...(draft0 === undefined ? {} : { sourceDraft: draft0 }),
        ...(atts.length > 0 ? { attachments: atts, attachmentsFingerprint: fp } : {}),
      }
      set((s) => {
        // 乐观清空后失败：把原文放回输入框（用户已输入新内容时不动，见 restoreDraft）。
        const restore = restoreDraft(s.drafts[activeId], draft0, body)
        return {
          sending: false,
          error: r.error || '发送失败',
          ...rememberFailed(s.failedByCid, rec),
          messages: s.messages.map((m) => (m.clientMsgId === clientMsgId ? { ...m, pending: false, failed: true } : m)),
          ...(restore === null ? {} : { drafts: { ...s.drafts, [activeId]: restore } }),
        }
      })
      return false
    }
    const seq = Number(r.data?.seq ?? 0)
    if (seq > 0) {
      // hub_message 事件通常已经先到（emit 在 resp 之前）；这里兜底把乐观消息就地落成真实 seq。
      set((s) => ({
        messages: s.messages.map((m) => (m.clientMsgId === clientMsgId ? { ...m, seq, pending: false, failed: false } : m)),
      }))
    }
    set((s) => ({
      sending: false,
      // 成功只清这一条 cid 的失败记录（复审 B3）。
      ...dropFailed(s.failedByCid, clientMsgId),
      // 草稿还是这条（原始草稿）才清：横幅/气泡重试成功后输入框要空，用户后来改过的输入不清（复审 B2）。
      // 有文件引用时 body 多出几行 @path，必须比 sourceDraft —— 否则草稿不清，用户再回车会多发一条。
      // 发送时已经乐观清过一次，这里幂等兜底（等待期间用户新输入的正文不匹配，不会被动）。
      ...(clearsDraft(s.drafts[activeId], draft0, body) ? { drafts: { ...s.drafts, [activeId]: '' } } : {}),
    }))
    void get().refresh()
    return true
  },

  async retry(clientMsgId) {
    const s = get()
    // 按气泡自己的 cid 取记录：多条失败时点 A 的重试必须发 A（复审 B1）。
    const failed = clientMsgId ? s.failedByCid[clientMsgId] : s.failed
    if (!failed) return false
    // 沿用首发 cid 与原文重发；失败气泡在 send/sendGroup 里按 cid 先摘掉（HUB-FAIL-01）。
    if (failed.kind === 'group') {
      const { activeGroupId, groupSending } = get()
      if (!activeGroupId || groupSending || activeGroupId !== failed.groupId) return false
      return get().sendGroup(failed.text, failed.mentions, failed.mentionAll, failed.clientMsgId, failed.attachments, failed.sourceDraft)
    }
    const { activeId, sending } = get()
    if (!activeId || sending || activeId !== failed.agentId) return false
    return get().send(failed.text, failed.clientMsgId, failed.attachments, failed.sourceDraft)
  },

  setDraft(agentID, text) {
    if (!agentID) return
    set((s) => ({ drafts: { ...s.drafts, [agentID]: text } }))
  },

  setGroupDraft(groupID, text) {
    if (!groupID) return
    set((s) => ({ groupDraftText: { ...s.groupDraftText, [groupID]: text } }))
  },

  clearError() {
    set({ error: null })
  },

  clearNotice() {
    set({ notice: null })
  },

  // —— 全历史搜索（hub_search）——
  //
  // 只做三件事：发命令、按代际接收、把命中映射成可渲染的领域对象。
  // 「点了命中之后」的加载/跳转在 openSearchHit 里，必须走和深链同一套 jump 守卫。
  async search(query, scope) {
    const q = normalizeSearchQuery(query)
    const sc = normalizeSearchScope(scope ?? get().searchScope)
    const gen = ++searchGen
    if (!q) {
      set({
        searchQuery: '', searchScope: sc, searchHits: [], searchHasMore: false,
        searchNext: null, searchLoading: false, searchError: null,
      })
      return
    }
    set({ searchQuery: q, searchScope: sc, searchLoading: true, searchError: null })
    const r = await hubCommand<HubSearchPage>('hub_search', searchPagePayload(q, sc))
    if (searchGen !== gen) return // 已有更新的查询：丢弃迟到响应
    if (!r.ok) {
      set({ searchLoading: false, searchError: r.error || '搜索失败', searchHits: [], searchHasMore: false, searchNext: null })
      return
    }
    const page = mapSearchPage(r.data)
    set({ searchLoading: false, searchHits: page.hits, searchHasMore: page.hasMore, searchNext: page.next })
  },

  async searchMore() {
    const { searchQuery, searchScope, searchNext, searchHasMore, searchLoading } = get()
    if (!searchQuery || !searchHasMore || !searchNext || searchLoading) return
    const gen = ++searchGen
    set({ searchLoading: true, searchError: null })
    const r = await hubCommand<HubSearchPage>('hub_search', searchPagePayload(searchQuery, searchScope, { cursor: searchNext }))
    if (searchGen !== gen) return
    if (!r.ok) {
      set({ searchLoading: false, searchError: r.error || '加载更早结果失败' })
      return
    }
    const page = mapSearchPage(r.data)
    set((s) => ({
      searchLoading: false,
      searchHits: mergeSearchHits(s.searchHits, page.hits),
      searchHasMore: page.hasMore,
      searchNext: page.next,
    }))
  },

  clearSearch() {
    searchGen++ // 作废在飞的响应
    set({
      searchQuery: '', searchHits: [], searchHasMore: false, searchNext: null,
      searchLoading: false, searchError: null,
    })
  },

  setSearchScope(scope) {
    set({ searchScope: normalizeSearchScope(scope) })
  },

  async openSearchHit(hit) {
    const target = mapSearchHit(hit)
    if (!target) return
    const key = target.scope === 'group' ? (target.group_id ?? '') : (target.agent_id ?? '')
    if (!key) return
    // 搜索框常驻左栏：Hub 关着时点命中同样要进入会话（不能只靠调用方先 openHub）。
    if (!get().open) get().openHub()
    searchGen++ // 选中即离开结果面板：不让在飞的翻页把结果写回来
    if (target.scope === 'group') await get().selectGroup(key)
    else await get().select(key)
    // selectGroup/select 都可能被用户切走（代际在它们内部）：只认当前目标。
    const state = get()
    const activeKey = target.scope === 'group' ? state.activeGroupId : state.activeId
    if (activeKey !== key) return
    const loaded = hasSeq(target.scope === 'group' ? state.groupMessages : state.messages, target.seq)
    if (!loaded) {
      // 命中可能在历史深处：拉「以命中 seq 结尾」的一页（before_seq = seq+1），
      // 与已加载时间线合并（不是替换），否则 requestJump 会因为「目标没加载」被拒绝。
      const plan = searchAroundPlan(target.scope, key, target.seq)
      if (plan) {
        const r = await hubCommand<HubHistoryPage>(plan.command, plan.payload)
        const current = get()
        const stillActive = target.scope === 'group' ? current.activeGroupId : current.activeId
        if (stillActive !== key) return
        if (r.ok) {
          const page = r.data?.messages ?? []
          set((s) => {
            if (target.scope === 'group') {
              const merged = mergeAroundPage(s.groupMessages, page)
              return { groupMessages: merged, loading: false, hasMore: s.groupMessages.length === 0 ? aroundHasMore(merged) : s.hasMore }
            }
            const merged = mergeAroundPage(s.messages, page)
            return { messages: merged, loading: false, hasMore: s.messages.length === 0 ? aroundHasMore(merged) : s.hasMore }
          })
        } else {
          set({ error: r.error || '加载命中附近的历史失败' })
        }
      }
    }
    // requestJump 只在 seq 已加载时登记；上面失败时它会给出「不在已加载历史里」的提示。
    get().requestJump(target.seq)
  },

  dismissAlert(id) {
    set((s) => ({ alerts: s.alerts.filter((a) => a.id !== id) }))
  },

  setInspector(panel) {
    set({ inspector: panel })
    if (panel === 'tasks') void get().loadTasks()
    if (panel === 'needs') void get().loadNeeds()
  },

  setView(view) {
    set({ view })
  },

  requestJump(seq) {
    const state = get()
    const target = state.activeGroupId || state.activeId
    const n = Math.trunc(Number(seq))
    if (!target || !Number.isSafeInteger(n) || n <= 0) return
    const messages = state.activeGroupId ? state.groupMessages : state.messages
    if (!messages.some((m) => Number(m.seq) === n)) {
      // 目标消息不在已加载历史里：**不能**留下一条永远消费不掉的 jump。
      // 视图的贴底 guard 见到「属于本目标的 jump」就让位，一条永不执行的 jump
      // 会让新消息不再跟随（复审 B3）。这里只提示一次，并清掉旧 jump。
      clearJumpTimer()
      set({ notice: `消息 #${n} 不在已加载的历史里`, jump: null })
      return
    }
    const nonce = ++jumpNonce
    set({ jump: { seq: n, targetId: target, nonce } })
    armJumpTimer(nonce, () => {
      if (get().jump?.nonce === nonce) set({ jump: null })
    })
  },

  clearJump(nonce) {
    const current = get().jump
    // 只清自己接的那一次：旧消费方（例如切群前的 GroupView effect）不能误清新请求。
    if (current && current.nonce === nonce) {
      clearJumpTimer()
      set({ jump: null })
    } else if (!current) {
      clearJumpTimer()
    }
  },

  async openDeepLink(raw) {
    const link = parseHubDeepLink(raw)
    if (!link) {
      set({ error: HUB_DEEP_LINK_INVALID_ERROR })
      return false
    }
    const key = hubDeepLinkKey(link)
    const running = deepLinkInflight.get(key)
    // 同一深链重复触发（OS 重传 / 连点 / 主进程两次转发）共享同一次加载：
    // 再跑一遍 select 会把已经滚好的位置重置回底部。
    if (running) return running
    const wasOpen = get().open
    const run = (async (): Promise<boolean> => {
      // 深链是新动作：上一次的提示/错误不该继续挂着（避免「已复制」盖住这次的结果）。
      // 从关闭态打开时登记所有权：本次打开期间 HubSurface 挂载不得再自动 select，
      // 否则 selectGroup/select 换代会把下面这次选择 abort 掉，跳转就丢了（B1）。
      set({ open: true, error: null, notice: null, ...(wasOpen ? {} : { deepLinkOwnedOpen: true }) })
      const hasTarget = () =>
        link.kind === 'group'
          ? get().groups.some((g) => g.id === link.id)
          : get().agents.some((a) => a.id === link.id)
      // 冷启动时列表还没拉过；已经拉过就不再为了深链再打一次全量列表。
      if (!hasTarget()) {
        if (link.kind === 'group') await get().loadGroups()
        else await get().refresh()
      }
      if (!hasTarget()) {
        set({ error: `深链目标不存在：${link.kind}/${link.id}` })
        return false
      }
      void get().loadTasks()
      void get().loadNeeds()
      if (link.kind === 'group') {
        await get().selectGroup(link.id)
        if (get().activeGroupId !== link.id) return false
      } else {
        await get().select(link.id)
        if (get().activeId !== link.id) return false
      }
      if (link.seq <= 0) return true
      // DM 有分页：最多回翻 4 页找目标 seq（复用 loadOlder，不另写一条加载路径）。
      if (link.kind === 'dm') {
        for (let i = 0; i < 4; i++) {
          if (get().activeId !== link.id) return false
          if (get().messages.some((m) => Number(m.seq) === link.seq)) break
          if (!get().hasMore) break
          await get().loadOlder()
        }
      }
      const loaded = (link.kind === 'group' ? get().groupMessages : get().messages).some(
        (m) => Number(m.seq) === link.seq,
      )
      if (loaded) {
        // 只登记请求，滚动/高亮由目标视图消费（seqJumpPlan 校验目标 + 已加载）。
        // 不在消费时立刻清：StrictMode 双跑 mount 时第一实例的清空会让第二实例
        // 看不到跳转（高亮丢失）。交给 TTL，过期后自动释放贴底。
        const nonce = ++jumpNonce
        set({ jump: { seq: link.seq, targetId: link.id, nonce }, notice: null })
        armJumpTimer(nonce, () => {
          if (get().jump?.nonce === nonce) set({ jump: null })
        })
      } else {
        // 群历史没有分页命令：目标不在最近 50 条里时如实说明，别假装跳了。
        set({ notice: `消息 #${link.seq} 不在已加载的历史里（会话已打开）` })
      }
      return true
    })()
    deepLinkInflight.set(key, run)
    try {
      return await run
    } finally {
      deepLinkInflight.delete(key)
    }
  },

  resumeOpenTarget() {
    // 离开 Hub 再回来：重新水合当前 target —— 回复可能是在没有订阅时落定的（B3），
    // 群还要重开 conversation/已读游标（B7）。但深链刚把 Hub 打开时，选择与跳转
    // 归深链所有：这里再 select 一次就会换代，把深链的选择作废（复审 B1）。
    // 注意用「本次打开」的所有权而不是一次性 claim：StrictMode 会双跑 mount effect，
    // 一次性 claim 第二次就被消费掉，照样 abort。closeHub/openHub 负责清所有权。
    if (get().deepLinkOwnedOpen) return
    const state = get()
    if (state.activeGroupId) void get().selectGroup(state.activeGroupId)
    else if (state.activeId) void get().select(state.activeId)
  },

  async copyDeepLink(kind, id, seq) {
    const link = formatHubDeepLink(kind, id, seq ?? 0)
    if (!link) {
      set({ error: '复制链接失败：缺少目标' })
      return false
    }
    const ok = await copyText(link)
    set(ok ? { notice: `已复制 ${link}`, error: null } : { error: `复制失败，请手动复制：${link}` })
    return ok
  },

  async loadPromptPreview(agentID, force, groupID) {
    const scope = normalizePromptScope(agentID, groupID)
    if (!scope.agentId) return
    const current = get().prompt
    if (!force && promptScopeMatches(current, scope)) {
      if (current?.loading) return // 同作用域在飞：复用这一次
      if (current?.loaded && !current.error) return // 只读缓存：切回来不重复请求
    }
    // 换作用域（DM↔群、群 A→群 B）不保留旧层：那些层是另一个作用域的正文。
    const keep = promptScopeMatches(current, scope) ? current?.layers ?? [] : []
    set({
      prompt: {
        agentId: scope.agentId,
        groupId: scope.groupId,
        layers: force ? [] : keep,
        note: '',
        totalBytes: 0,
        loading: true,
        loaded: false,
        error: null,
      },
    })
    // DM 不带 group_id：后端按「没带 = DM 变体」处理，与旧调用同形。
    const payload: Record<string, unknown> = { agent_id: scope.agentId }
    if (scope.groupId) payload.group_id = scope.groupId
    const r = await hubCommand<HubPromptPreviewData>('hub_agent_prompt_preview', payload)
    // 竞态：切到别的智能体**或别的群**后，旧响应不能写进新面板（只比 agentId 会让
    // 群 A 的迟到响应覆盖群 B）。前端只认**本次请求**的作用域，不读响应里的
    // scope/group_id —— 旧 bridge 不回显也不影响判定。
    if (!promptScopeMatches(get().prompt, scope)) return
    if (!r.ok) {
      set({
        prompt: {
          agentId: scope.agentId, groupId: scope.groupId, layers: [], note: '', totalBytes: 0,
          loading: false, loaded: false, error: r.error || '读取提示词失败',
        },
      })
      return
    }
    set({
      prompt: {
        agentId: scope.agentId,
        groupId: scope.groupId,
        layers: r.data?.layers ?? [],
        note: r.data?.note ?? '',
        totalBytes: Number(r.data?.total_bytes ?? 0) || 0,
        loading: false,
        loaded: true,
        error: null,
      },
    })
  },

  async loadTasks() {
    set({ tasksLoading: true })
    const r = await hubCommand<{ tasks?: HubTask[] }>('hub_task_list', { limit: 100 })
    if (!r.ok) {
      set({ tasksLoading: false, error: r.error || '读取任务失败' })
      return
    }
    set({ tasksLoading: false, tasks: r.data?.tasks ?? [] })
  },

  async loadNeeds() {
    set({ needsLoading: true })
    const r = await hubCommand<{ needs?: HubNeed[] }>('hub_need_list', { status: 'pending' })
    if (!r.ok) {
      set({ needsLoading: false, error: r.error || '读取 Needs you 失败' })
      return
    }
    set({ needsLoading: false, needs: r.data?.needs ?? [] })
  },

  // —— 场景2：Project 会话的待办快照（DM/群内联卡）——
  // 三个 action 都只读事件、只改前端状态：不写 hub_message、不写 refs、不发命令。
  // 为什么放在 store 而不是组件里：Hub 关闭时也要继续收集（用户回来时卡还在），
  // 组件只在打开 DM/群时才挂载；这份数据是 App 级订阅（attachHubEvents）的自然产物。
  applyTodoSnapshot(event) {
    const sessionId = String(event.session_id ?? '')
    const runId = String(event.run_id ?? '')
    if (!isProjectSession(sessionId) || !runId) return
    const key = hubRunKey(sessionId, runId)
    const prev = get().todoSnapshots[key]
    const next = mergeTodoSnapshot(prev, { ...event, arrival: ++todoArrival })
    if (!next || next === prev) return
    set({ todoSnapshots: putTodoSnapshot(get().todoSnapshots, next) })
  },

  rememberRunName(event) {
    const sessionId = String(event.session_id ?? '')
    const runId = String(event.run_id ?? '')
    if (!isProjectSession(sessionId) || !runId) return
    // 根 run 也记（root=true，名字恒为空）：只有见过 agent_start 的 run 才允许显示
    // 「主会话」——数不到的 run 显示 run_xxxx，绝不猜成主会话。
    const root = (event.parent_run_id ?? '') === '' && (event.depth ?? 0) === 0
    const next = putRunName(get().runNames, hubRunKey(sessionId, runId), { root, name: event.name })
    if (next === get().runNames) return
    set({ runNames: next })
  },

  forgetTodosForTask(task) {
    const all = get().todoSnapshots
    const next = forgetTodoSnapshots(all, {
      sessionId: task.target?.session_id,
      runIds: [task.dispatch?.run_id ?? ''].filter(Boolean),
    })
    if (next !== all) set({ todoSnapshots: next })
  },

  async cancelTask(taskID) {
    const r = await hubCommand<{ task?: HubTask }>('hub_task_cancel', { task_id: taskID })
    if (!r.ok) {
      set({ error: r.error || '取消失败' })
      return
    }
    if (r.data?.task) {
      set((s) => ({ tasks: s.tasks.map((t) => (t.id === taskID ? (r.data!.task as HubTask) : t)) }))
    }
    void get().loadTasks()
    set({ notice: '已请求取消任务' })
  },

  async retryTask(taskID) {
    const r = await hubCommand<{ task?: HubTask }>('hub_task_retry', { task_id: taskID })
    if (!r.ok) {
      set({ error: r.error || '重试失败' })
      return
    }
    set({ notice: '已创建重试任务' })
    void get().loadTasks()
  },

  async resolveNeed(needID, resolution, answers) {
    const r = await hubCommand<{ need?: HubNeed }>('hub_need_resolve', { need_id: needID, resolution, answers })
    if (!r.ok) {
      set({ error: r.error || '处理失败' })
      return
    }
    if (r.data?.need) {
      const resolved = r.data.need
      set((s) => ({ needs: s.needs.filter((n) => n.id !== needID), tasks: s.tasks }))
      if (resolved.status === 'resolved') {
        // 审批/提问不是写一条 Hub 消息，而是用 need 的 session_id + anchor 去 resolve
        // 对应 Project 会话里挂起的审批/提问；这里把落点明确告诉用户，避免「点完就没了」。
        // failure/budget/handoff 只是标记已读/交接状态，不能说成「提交到会话」。
        const target = resolved.target_session_id || resolved.session_id || ''
        const routed = resolved.kind === 'approval' || resolved.kind === 'question'
        set({ notice: routed && target ? `已提交到 ${target}` : '已处理' })
      }
    }
    void get().loadNeeds()
  },

  async clearContext() {
    const id = get().activeId
    if (!id) return
    const r = await hubCommand<{ session_id?: string }>('hub_clear', { target: { kind: 'dm', agent_id: id } })
    if (!r.ok) {
      set({ error: r.error || '清空上下文失败' })
      return
    }
    set({ live: '', rootRunId: null, runLog: [], actions: [], notice: '上下文已清空' })
    if (id) void get().select(id)
  },

  async loadGroups() {
    set({ groupsLoading: true })
    const startedAt = Date.now()
    const r = await hubCommand<{ groups?: HubGroup[]; progress?: Record<string, HubGroupProgress> }>('hub_group_list', {})
    if (!r.ok) {
      set({ groupsLoading: false, error: r.error || '读取群组失败' })
      return
    }
    // progress 是左栏预览/未读的唯一权威投影。但事件可能比响应先到（§7.5.7）：
    // 快照没有 revision，用「请求开始时间 vs 本地最近更新时间」挡住旧覆盖（N10），
    // 不只是比 last_seq（同 seq 的任务结论/状态也会被旧响应写回）。
    const progress = r.data?.progress ?? {}
    const completedAt = Date.now()
    set((s) => {
      const merged = { ...s.groupInfo }
      for (const [gid, p] of Object.entries(progress)) {
        if (shouldApplyGroupProgress(s.groupInfo[gid], p, groupInfoUpdatedAt.get(gid) ?? 0, startedAt)) {
          merged[gid] = p
          groupInfoUpdatedAt.set(gid, completedAt)
        }
      }
      return { groupsLoading: false, groups: r.data?.groups ?? [], groupInfo: merged }
    })
  },

  async createGroup(req) {
    const name = (req.name ?? '').trim()
    // 去空去重：UI 传的是勾选顺序，重复 id 会让后端「唯一 owner」校验报错。
    const memberIDs = [...new Set((req.member_ids ?? []).map((id) => String(id ?? '').trim()).filter(Boolean))]
    if (!name) {
      set({ error: '群名称不能为空' })
      return null
    }
    if (memberIDs.length === 0) {
      set({ error: '至少选择 1 个成员' })
      return null
    }
    // owner 缺省取第一个成员（与 bridge 的 hubGroupCreate 同一条语义）。
    const owner = (req.owner_agent_id ?? '').trim() || memberIDs[0]
    const r = await hubCommand<{ group?: HubGroup }>('hub_group_create', {
      name,
      member_ids: memberIDs,
      owner_agent_id: owner,
      policy: req.policy ?? { mode: 'ask' },
    })
    const group = r.data?.group
    if (!r.ok || !group) {
      set({ error: r.error || '创建群组失败' })
      return null
    }
    // 两步都不能省：先刷新左栏（计数/列表立刻出现新群），再 openGroup 让 Hub 打开并选中它
    //（openGroup 内部会按需 openHub —— 从别处新建群时也要落到群视图上）。
    await get().loadGroups()
    await get().openGroup(group.id)
    return group
  },

  async selectGroup(groupID) {
    if (!groupID) return
    groupSendGen++
    const gen = ++groupSelectGen
    const wasOpen = get().open
    set((s) => ({
      activeGroupId: groupID,
      selectionNonce: s.selectionNonce + 1,
      activeId: null, profile: null, sid: null,
      groupMessages: [], groupActivity: [], groupRounds: [], groupSending: false, groupDrafts: [],
      groupAssigning: false, groupAssignNote: null, groupConvStopping: false,
      // 打开即已读：先清本地未读点（游标在历史加载完之后再推进）。
      // Hub 已关闭时不在这里清 —— 事件/关闭期间的新消息必须留着未读（B4）。
      groupInfo: wasOpen
        ? { ...s.groupInfo, [groupID]: { ...(s.groupInfo[groupID] ?? {}), unread: 0 } }
        : s.groupInfo,
      groupReadSeq: { ...s.groupReadSeq },
      messages: [], live: '', rootRunId: null,
      error: null, failed: null, failedByCid: {}, loading: true,
      view: 'chat',
      // 手动切群/新深链都会走到这里：旧的跳转请求作废（深链在 selectGroup 之后再登记）。
      jump: null,
    }))
    const r = await hubCommand<{
      group?: HubGroup
      conversation?: HubConversation
      cursor?: { human_read_seq?: number }
    }>('hub_group_get', { group_id: groupID })
    if (get().activeGroupId !== groupID || groupSelectGen !== gen) return
    if (!r.ok) {
      set({ loading: false, error: r.error || '打开群组失败' })
      return
    }
    const serverRead = Number(r.data?.cursor?.human_read_seq ?? 0)
    if (r.data?.conversation) {
      const conversation = r.data.conversation
      set((s) => ({ groupConversations: { ...s.groupConversations, [groupID]: conversation } }))
    }
    if (serverRead > 0) {
      set((s) => ({
        groupReadSeq: { ...s.groupReadSeq, [groupID]: Math.max(serverRead, s.groupReadSeq[groupID] ?? 0) },
      }))
    }
    touchGroupInfo(groupID)
    const h = await hubCommand<{ messages?: HubMessage[] }>('hub_group_history', { group_id: groupID, limit: 50 })
    if (get().activeGroupId !== groupID || groupSelectGen !== gen) return
    const a = await hubCommand<{ activity?: HubGroupActivity[] }>('hub_group_activity', { group_id: groupID })
    if (get().activeGroupId !== groupID || groupSelectGen !== gen) return
    const history = h.ok ? (h.data?.messages ?? []) : []
    const historyIds = history
      .map((m) => m.id)
      .filter((id): id is string => typeof id === 'string' && id.length > 0)
    // 已读游标不能只看历史最大 seq：打开期间可能已有新消息/服务端游标更靠前。
    const latest = history.reduce(
      (max, m) => Math.max(max, m.seq),
      Math.max(get().groupInfo[groupID]?.last_seq ?? 0, get().groupReadSeq[groupID] ?? 0),
    )
    set((s) => {
      // 历史请求期间 hub_message 可能已经先到（§7.5.7 顺序）：按 seq 合并，
      // 不拿快照覆盖新消息；乐观/失败气泡（seq<=0）也保留。
      const pageSeqs = new Set(history.map((m) => m.seq))
      const extra = s.groupMessages.filter((m) => !pageSeqs.has(m.seq))
      const rec = reconcileDraftsWithMessages(s.groupDrafts, s.finalizedMessageIds, historyIds)
      const patch: Partial<HubState> = {
        loading: false,
        groupMessages: [...history, ...extra].sort((x, y) => x.seq - y.seq),
        groupActivity: a.ok ? (a.data?.activity ?? []) : [],
        groups: s.groups.map((g) => (g.id === groupID ? (r.data?.group ?? g) : g)),
      }
      // 断线期间错过的 final：历史里已有该 message_id → 丢掉残留草稿并记 finalized，
      // 防止重连后迟到的 delta 把幽灵气泡复活（P4-01）。
      if (rec.drafts !== s.groupDrafts) {
        patch.groupDrafts = rec.drafts
        patch.finalizedMessageIds = rec.finalized
      }
      return patch
    })
    // 打开群 = 已读（§7.5.7 群级共享游标）：历史加载完再推进，避免用旧 seq 覆盖新消息。
    // 只有 Hub 仍然打开才清未读/推进游标 —— 关闭后才落地的历史不能吃掉未读（B4）。
    if (latest > 0 && get().open) {
      set((s) => ({
        groupReadSeq: { ...s.groupReadSeq, [groupID]: Math.max(latest, s.groupReadSeq[groupID] ?? 0) },
        groupInfo: { ...s.groupInfo, [groupID]: { ...(s.groupInfo[groupID] ?? {}), unread: 0 } },
      }))
      touchGroupInfo(groupID)
      void markGroupRead(groupID, latest)
    }
  },

  async loadGroupActivity(groupID) {
    const gid = groupID ?? get().activeGroupId
    if (!gid) return
    const gen = groupSelectGen
    const a = await hubCommand<{ activity?: HubGroupActivity[] }>('hub_group_activity', { group_id: gid })
    // 同一群 A→B→A 也要用代际拒绝旧包，否则新读会被旧快照覆盖。
    if (get().activeGroupId !== gid || groupSelectGen !== gen) return
    if (a.ok) set({ groupActivity: a.data?.activity ?? [] })
  },

  clearGroup() {
    groupSendGen++
    set({
      activeGroupId: null, groupMessages: [], groupActivity: [], groupRounds: [], groupSending: false,
      groupDrafts: [], groupAssigning: false, groupAssignNote: null, groupConvStopping: false,
      failed: null, failedByCid: {},
      jump: null,
    })
  },

  async sendGroup(text, mentions, mentionAll, reuseClientMsgId, attachments, sourceDraft) {
    const { activeGroupId, groupSending } = get()
    const body = (text ?? '').trim()
    const draft0 = typeof sourceDraft === 'string' ? sourceDraft.trim() : undefined
    // 纯图片（正文为空）也允许发：群里没有 @ 时后端只存「[图片]」，@ 由用户在正文里给。
    const check = validateAttachments(attachments)
    const atts = check.ok ? check.attachments : []
    if (!activeGroupId || (!body && atts.length === 0) || groupSending) return false
    if (!check.ok) {
      set({ error: check.error || '附件不合法' })
      return false
    }
    const gen = ++groupSendGen
    // 与 DM 同一条幂等规则（HUB-IDEM-01）：失败记录保留首发 cid，重试/同正文重发沿用它，
    // 桥按 client_msg_id 去重，不会因「响应丢了」而把同一条再喂一次模型。
    // 同样按「同正文 + 同群 + 同附件指纹」找记录，而不是只看最新一条（复审 B3）。
    const fp = attachmentFingerprint(atts)
    const reuse = reuseClientMsgId || reuseCidFor(get().failedByCid, 'group', activeGroupId, body, fp)
    const n = ++optimisticSeq
    const clientMsgId = reuse || `g-${Date.now()}-${n}`
    set((s) => ({
      groupSending: true,
      error: null, notice: null,
      // 只摘这一条 cid，别的失败气泡还要能各自重试（复审 B3）。
      ...dropFailed(s.failedByCid, reuse),
      // 重发先摘掉失败气泡，别在时间线上叠第二条乐观气泡（HUB-FAIL-01）。
      groupMessages: [
        ...(reuse ? s.groupMessages.filter((m) => !(m.failed && m.clientMsgId === reuse)) : s.groupMessages),
        { seq: -n, sender: { kind: 'human' }, text: body, ...(atts.length > 0 ? { attachments: atts } : {}), pending: true, clientMsgId },
      ],
      // 与 DM 同一套乐观清空：气泡已进时间线，输入框不等响应（失败按 restoreDraft 放回）。
      ...(clearsDraft(s.groupDraftText[activeGroupId], draft0, body)
        ? { groupDraftText: { ...s.groupDraftText, [activeGroupId]: '' } }
        : {}),
    }))
    const payload: Record<string, unknown> = {
      group_id: activeGroupId, text: body, client_msg_id: clientMsgId,
    }
    if (atts.length > 0) payload.attachments = atts
    // 结构化 mentions 是人点选过的目标（§8.3.3）；正文里的 @ 仍由后端再解析一次，
    // 两者取并集 —— 前端删改正文不会留下「幽灵唤醒」。
    if (mentions && mentions.length > 0) payload.mentions = mentions
    if (mentionAll) payload.mention_all = true
    const r = await hubCommand<{ seq?: number; round?: HubRound; deliveries?: HubDelivery[] }>('hub_group_send', payload)
    // 切群/下一轮发送已发生：旧响应只负责自己的乐观气泡，不得复位新一轮的 groupSending。
    if (groupSendGen !== gen || get().activeGroupId !== activeGroupId) return false
    if (!r.ok) {
      // 失败同样留记录（含 cid + @ 目标）：群里之前只有一条「网络中断 ×」，没有重试入口。
      const rec: HubFailedSend = {
        kind: 'group', text: body, clientMsgId, groupId: activeGroupId, mentions, mentionAll,
        ...(draft0 === undefined ? {} : { sourceDraft: draft0 }),
        ...(atts.length > 0 ? { attachments: atts, attachmentsFingerprint: fp } : {}),
      }
      set((s) => {
        // 同 DM：乐观清空后失败，把原文放回群草稿（用户已输入新内容时不动）。
        const restore = restoreDraft(s.groupDraftText[activeGroupId], draft0, body)
        return {
          groupSending: false, error: r.error || '发送失败',
          ...rememberFailed(s.failedByCid, rec),
          groupMessages: s.groupMessages.map((m) => (m.clientMsgId === clientMsgId ? { ...m, pending: false, failed: true } : m)),
          ...(restore === null ? {} : { groupDraftText: { ...s.groupDraftText, [activeGroupId]: restore } }),
        }
      })
      return false
    }
    set((s) => ({
      groupSending: false,
      // 成功只清这一条 cid 的失败记录；草稿仍是这条正文才清（复审 B2/B3）。
      // 发送时已乐观清空，这里幂等兜底：等待期间用户新输入的正文不匹配，不会被抹掉。
      ...dropFailed(s.failedByCid, clientMsgId),
      ...(clearsDraft(s.groupDraftText[activeGroupId], draft0, body)
        ? { groupDraftText: { ...s.groupDraftText, [activeGroupId]: '' } }
        : {}),
      groupRounds: upsertRound(s.groupRounds, r.data?.round),
      groupMessages: s.groupMessages.map((m) => (m.clientMsgId === clientMsgId && r.data?.seq ? { ...m, seq: r.data.seq, pending: false } : m)),
    }))
    void get().loadGroupActivity(activeGroupId)
    return true
  },

  async sendGroupAssign(groupID, req) {
    const gid = (groupID || get().activeGroupId || '').trim()
    const instruction = (req.instruction ?? '').trim()
    if (!gid || !req.agent_id || !instruction || get().groupAssigning) return false
    const gen = groupSelectGen
    const clientMsgId = `assign-${Date.now()}-${++optimisticSeq}`
    set({ groupAssigning: true, groupAssignNote: null, error: null, notice: null })
    // §7.1.3 的键名是 agent_id（后端也兼容 to_agent）；显式带上 client_msg_id 做幂等。
    const r = await hubCommand<{ task_id?: string; status?: string; note?: string; seq?: number }>('hub_group_assign', {
      group_id: gid, agent_id: req.agent_id, instruction,
      ...(req.title ? { title: req.title } : {}),
      client_msg_id: clientMsgId,
    })
    if (groupSelectGen !== gen || get().activeGroupId !== gid) {
      set({ groupAssigning: false })
      return false
    }
    if (!r.ok) {
      set({ groupAssigning: false, groupAssignNote: null, error: r.error || '派任务失败' })
      return false
    }
    const status = r.data?.status || 'queued'
    const note = r.data?.note || (status === 'queued'
      ? '目标当前正忙：任务已入队，对方回合结束后派发。'
      : `任务已创建（${status}）。`)
    set({ groupAssigning: false, groupAssignNote: note })
    // 指派消息/任务事件会先到（emit 在 resp 之前）；这里兜底刷新一次活动与群列表。
    void get().loadGroupActivity(gid)
    return true
  },

  async stopGroupConversation(groupID) {
    const gid = (groupID || get().activeGroupId || '').trim()
    if (!gid || get().groupConvStopping) return false
    set({ groupConvStopping: true, error: null })
    const r = await hubCommand<{ conversation?: HubConversation; interrupted?: number }>('hub_conversation_stop', {
      group_id: gid,
    })
    set({ groupConvStopping: false })
    if (!r.ok) {
      set({ error: r.error || '停止自动对话失败' })
      return false
    }
    if (r.data?.conversation) {
      set((s) => ({ groupConversations: { ...s.groupConversations, [gid]: r.data!.conversation as HubConversation } }))
    }
    set({ notice: r.data?.interrupted ? `已停止自动对话（中断 ${r.data.interrupted} 个 run）` : '已停止自动对话' })
    return true
  },

  /** P6-04 ④：重置本群对话限制（清零 pair/深度/指纹，解除 waiting_human）。 */
  async resetGroupConversation(groupID) {
    const gid = (groupID || get().activeGroupId || '').trim()
    if (!gid) return false
    const r = await hubCommand<{ conversation?: HubConversation }>('hub_conversation_reset', { group_id: gid })
    if (!r.ok) {
      set({ error: r.error || '重置对话限制失败' })
      return false
    }
    if (r.data?.conversation) {
      set((s) => ({ groupConversations: { ...s.groupConversations, [gid]: r.data!.conversation as HubConversation } }))
    }
    set({ notice: '已重置本群对话限制' })
    return true
  },

  handleEvent(line) {
    let ev: {
      event_type?: string
      agent_id?: string
      session_id?: string
      workspace?: string
      run_id?: string
      parent_run_id?: string
      depth?: number
      seq?: number
      message?: HubMessage | string
      client_msg_id?: string
      content?: string
      text?: string
      status?: string
      task?: HubTask
      need?: HubNeed
      id?: string
      name?: string
      is_error?: boolean
      arguments?: string
      timestamp?: string
      ms?: number
      before?: number
      after?: number
      reason?: string
      group_id?: string
      scope?: string
      code?: string
      kind?: string
      severity?: string
      task_id?: string
      at?: string
      round?: HubRound
      message_id?: string
      index?: number
      delta?: string
      conversation?: HubConversation
      target_agent_id?: string
      target_kind?: string
      group?: HubGroup
      /** tool_run_end 的待办快照（属主 = 发出该调用的 run）。缺字段 = 无快照。 */
      todos?: unknown
    }
    try {
      ev = JSON.parse(line) as typeof ev
    } catch {
      return
    }
    // —— 场景2：Project 会话的内联待办 ——
    // 必须在下面所有 sid 闸门**之前**收：`sid` 是 Hub 自己的会话
    // （hub.<agent>.dm / hub.<agent>.g.<group>），Project 会话（s_…）永远对不上，
    // 先过闸门就等于把整条事件丢掉 —— 这也正是「Project 的 ToDo 一直看不到」的根因。
    // 只收 Project 会话：Hub 自己的工具事件不写（同一份清单已在转录/右栏待办面板里）。
    if (isProjectSession(ev.session_id)) {
      if (ev.event_type === 'agent_start') {
        get().rememberRunName({
          session_id: ev.session_id, run_id: ev.run_id, name: ev.name,
          parent_run_id: ev.parent_run_id, depth: ev.depth,
        })
      } else if (ev.event_type === 'tool_run_end' && 'todos' in ev) {
        // 只有**带** todos 字段才是快照（bridge 只在 todo_*/编排调用上注入）。
        // 缺字段不能当空清单：那样每条普通工具都会把一张活的卡清空。
        get().applyTodoSnapshot(ev)
      }
    }
    const { activeId, sid } = get()
    const isRoot = (ev.parent_run_id ?? '') === '' && (ev.depth ?? 0) === 0
    switch (ev.event_type) {
      case 'hub_message': {
        if (ev.group_id) {
          const msg = typeof ev.message === 'string' ? undefined : ev.message
          if (!msg) return
          const gid = ev.group_id
          // Hub 关闭时 activeGroupId 可能还留着：只有「打开 + 当前群」才算正在看（B4）。
          const active = isHubTargetActive(get().open, get().activeGroupId, gid)
          // final 覆盖草稿：即使消息属于别的群，也要把该 message_id 记入 finalized，
          // 否则切回该群时迟到的 delta 会重新长出草稿。
          const fin = finalizeHubDraft(get().groupDrafts, get().finalizedMessageIds, msg.id)
          set((s) => {
            const prev = s.groupInfo[gid] ?? {}
            const info = projectGroupMessage(prev, msg, active)
            const next: Partial<HubState> = { groupDrafts: fin.drafts, finalizedMessageIds: fin.finalized }
            if (info !== prev) {
              next.groupInfo = { ...s.groupInfo, [gid]: info }
              if (active && msg.seq > 0) {
                next.groupReadSeq = { ...s.groupReadSeq, [gid]: Math.max(msg.seq, s.groupReadSeq[gid] ?? 0) }
              }
            }
            if (!active) return next
            const withoutPending = ev.client_msg_id
              ? s.groupMessages.filter((m) => m.clientMsgId !== ev.client_msg_id)
              : s.groupMessages
            if (withoutPending.some((m) => m.seq === msg.seq)) return next
            return { ...next, groupMessages: [...withoutPending, msg].sort((a, b) => a.seq - b.seq) }
          })
          // 正在看的群：新消息到即已读（best-effort，不阻塞渲染、不逐条刷事件）。
          if (active && msg.seq > 0) {
            touchGroupInfo(gid)
            void markGroupRead(gid, msg.seq)
          }
          return
        }
        const agentID = ev.agent_id || ''
        const msg = typeof ev.message === 'string' ? undefined : ev.message
        if (!msg) return
        // final 覆盖草稿（场景2b）：DM 每轮 LLM 内容一条 hub_message，id 复用本轮的
        // 草稿 message_id。无论当前是否在看这个 DM 都要 finalize —— 否则切走再切回时
        // 迟到的 delta 会重新长出幽灵气泡（与群同一套 finalized 守卫）。
        const fin = finalizeHubDraft(get().groupDrafts, get().finalizedMessageIds, msg.id)
        // 左栏预览/未读同步：只有 Hub 打开且当前就是这个 DM 才不算未读。
        const dmActive = isHubTargetActive(get().open, activeId, agentID)
        touchDMInfo(agentID) // 刷新响应的竞态守卫要看到「这条消息比请求新」
        set((s) => {
          const dm = { ...s.dm }
          const info = { ...(dm[agentID] ?? {}) }
          info.last_seq = msg.seq
          info.last_text = (msg.text ?? '').replace(/\s+/g, ' ').slice(0, 80)
          info.last_sender = msg.sender?.kind
          info.last_at = msg.created_at ?? new Date().toISOString()
          if (dmActive) info.unread = 0
          else if (msg.sender?.kind === 'agent') info.unread = (info.unread ?? 0) + 1
          dm[agentID] = info
          const next: Partial<HubState> = { dm }
          // 只在真有草稿被删 / 有 id 入账时才改这两个字段：每个 DM final 都换引用会让
          // groupDrafts 的订阅者（群视图、DM 视图）白重渲染一次。
          if (fin.drafts !== s.groupDrafts) next.groupDrafts = fin.drafts
          // LRU 满 200 时长度不再变化，只能看队头是否换了（新 id 一定在队头）。
          if (fin.finalized[0] !== s.finalizedMessageIds[0]) next.finalizedMessageIds = fin.finalized
          return next
        })
        if (!dmActive) return
        set((s) => {
          // 乐观消息 → 真实消息：按 client_msg_id（人发的）或 seq（智能体回复）去重。
          const withoutPending = ev.client_msg_id
            ? s.messages.filter((m) => m.clientMsgId !== ev.client_msg_id)
            : s.messages
          if (withoutPending.some((m) => m.seq === msg.seq)) return { live: '' }
          const next = [...withoutPending.filter((m) => m.seq !== msg.seq), msg]
          return { messages: next.sort((a, b) => a.seq - b.seq), live: msg.sender?.kind === 'agent' ? '' : s.live }
        })
        return
      }
      case 'hub_message_delta': {
        if (ev.group_id) {
          if (!get().open || ev.group_id !== get().activeGroupId) return
          set((s) => ({
            groupDrafts: applyHubDelta(s.groupDrafts, ev as HubMessageDelta, s.finalizedMessageIds),
          }))
          return
        }
        // DM（场景2b）：group_id 是空串。草稿同样进 groupDrafts，靠 groupId==='' 与
        // 群草稿分桶；只有「Hub 打开 + 正在看这个 agent」才收 —— 别的 DM 的流式内容
        // 不能串到当前会话里（切过去时那段正文已经落成 final 消息，见 select 的对账）。
        const agentID = typeof ev.agent_id === 'string' ? ev.agent_id : ''
        if (!agentID || !get().open || agentID !== get().activeId) return
        set((s) => ({
          groupDrafts: applyHubDelta(s.groupDrafts, ev as HubMessageDelta, s.finalizedMessageIds),
        }))
        return
      }
      case 'content_chunk': {
        // 只收：① 当前会话 ② 根 run（子 agent 的 chunk 也带同一 session_id，run_id 不同）。
        if (!sid || (ev.session_id && ev.session_id !== sid)) return
        if (!isRoot) return
        const root = get().rootRunId
        if (root && ev.run_id && ev.run_id !== root) return
        const piece = ev.content ?? ev.text ?? ''
        if (!piece) return
        // mock 与真 bridge 在发 hub_message_delta 的同时**仍然**发 content_chunk（旧 live
        // 路径）。有草稿时 live 没人画（HubSurface 的 showLive=false；群视图只画群草稿），
        // 但每个 token 都写一次 live 就会换引用、把整棵 DM 视图（消息列表 + Markdown）
        // reconcile 一遍。running 同理：已经是 true 还 spread 一个新对象，订阅整张
        // running map 的左栏/群视图/右栏全部白重渲染。只在真有可见变化时才写 state。
        set((s) => {
          const aid = s.activeId
          // 群模式：live 不被 GroupView 使用（群正文走 groupDrafts）。
          // DM：当前 agent 已有草稿时草稿就是真身，live 只是「还没收到 delta」的兜底。
          const needLive = !s.activeGroupId
            && !s.groupDrafts.some((d) => !d.groupId && d.agentId === aid)
          // 已 true 时复用原 running 对象：引用不变 → 订阅者不通知。
          const needRunning = !!aid && s.running[aid] !== true
          // 两处都没有可见变化 → 原样返回（Zustand 不通知，零渲染）。
          if (!needLive && !needRunning) return s
          const next: Partial<HubState> = {}
          if (needLive) next.live = s.live + piece
          if (needRunning && aid) next.running = { ...s.running, [aid]: true }
          return next
        })
        return
      }
      case 'agent_start': {
        // run 名表（子智能体展示名）已在函数顶部按 Project 会话记过；这里仍然只管
        // 当前 DM 的运行点，口径不变（根 run 才亮环）。
        if (!isRoot) return
        if (!activeId || !sid || (ev.session_id && ev.session_id !== sid)) return
        set((s) => ({
          running: { ...s.running, [activeId]: true },
          live: '',
          rootRunId: ev.run_id ?? null,
        }))
        return
      }
      case 'agent_end': {
        if (!isRoot) return // 子 agent 结束不能熄根 run 的运行点
        // 群内空回复/中断没有 final hub_message：按群会话 id 清掉该 agent 的草稿。
        // 带上 groupId：同一个 agent 在自己的 DM 里可能也在流式，那条草稿不能一起清。
        const groupScope = groupScopeFromSession(ev.session_id)
        if (get().open && groupScope && groupScope.groupId === get().activeGroupId) {
          set((s) => ({
            groupDrafts: dropDraftsForAgent(s.groupDrafts, groupScope.agentId, groupScope.groupId),
          }))
        }
        // DM（场景2b）：hub.<agent>.dm 结束就清掉该 agent 的 DM 草稿（groupId===''），
        // 纯工具轮/中断没有 final，不能把半截气泡留在时间线里；群草稿一条都不动。
        const dmScope = dmScopeFromSession(ev.session_id)
        if (dmScope) {
          set((s) => ({ groupDrafts: dropDraftsForAgent(s.groupDrafts, dmScope.agentId, '') }))
        }
        if (!activeId || !sid || (ev.session_id && ev.session_id !== sid)) return
        set((s) => ({
          running: { ...s.running, [activeId]: false },
          live: '', // 纯工具轮/中断没有最终回复：半截流式气泡不能留着
          rootRunId: null,
        }))
        return
      }
      case 'hub_alert': {
        const alert = alertFromEvent(ev)
        set((s) => ({ alerts: addHubAlert(s.alerts, alert) }))
        return
      }
      case 'hub_task_update': {
        const task = ev.task
        const evt = ev as HubTaskUpdate
        const taskID = evt.task_id || task?.id || ''
        if (task?.id) {
          set((s) => {
            const rest = s.tasks.filter((t) => t.id !== task.id)
            return { tasks: [task, ...rest] }
          })
        }
        // 场景2：任务进终态就立刻收掉内联待办卡（显示层已按终态过滤，这里顺手回收
        // 快照，避免同一会话的下一条任务认领到上一轮的旧清单）。
        if (isTerminalTaskStatus(task?.status ?? evt.status)) {
          const known = task ?? get().tasks.find((t) => t.id === taskID)
          if (known) get().forgetTodosForTask(known)
        }
        // 左栏任务态：只更新「最近一张任务卡」的 status/summary；迟到的 update
        // 不能把被新消息清掉的 last_outcome 复活（N2，见 projectTaskUpdate）。
        const gid = ev.group_id || task?.target?.group_id || task?.origin?.group_id || ''
        if (!gid) return
        set((s) => {
          const prev = s.groupInfo[gid] ?? {}
          const info = projectTaskUpdate(prev, { ...evt, task_id: taskID || evt.task_id })
          if (info === prev) return {}
          touchGroupInfo(gid)
          return { groupInfo: { ...s.groupInfo, [gid]: info } }
        })
        return
      }
      case 'hub_conversation': {
        if (!ev.group_id || !ev.conversation) return
        const gid = ev.group_id
        const conversation = ev.conversation
        set((s) => ({ groupConversations: { ...s.groupConversations, [gid]: conversation } }))
        return
      }
      case 'hub_read': {
        // 目前 bridge 不主动发；为多端/手机端保留同一条归约路径：读到哪就清到哪。
        // seq<=0 一律忽略（恶意/空事件不能把未读清零，N10）。
        if (!ev.group_id) return
        const gid = ev.group_id
        const seq = Number(ev.seq ?? 0)
        set((s) => {
          const prev = s.groupInfo[gid] ?? {}
          const info = projectGroupRead(prev, seq)
          const next: Partial<HubState> = {}
          if (info !== prev) {
            next.groupInfo = { ...s.groupInfo, [gid]: info }
            touchGroupInfo(gid)
          }
          if (seq > 0) next.groupReadSeq = { ...s.groupReadSeq, [gid]: Math.max(seq, s.groupReadSeq[gid] ?? 0) }
          return next
        })
        return
      }
      case 'hub_group_update': {
        if (!ev.group?.id) return
        const next = ev.group
        set((s) => ({ groups: s.groups.map((g) => (g.id === next.id ? next : g)) }))
        return
      }
      case 'hub_need': {
        const need = ev.need
        if (!need?.id) return
        set((s) => {
          const rest = s.needs.filter((n) => n.id !== need.id)
          // 已处理/过期的不留在 pending 列表里（但保留事件通知能力由上层决定）。
          if (need.status && need.status !== 'pending') return { needs: rest }
          return { needs: [need, ...rest] }
        })
        return
      }
      case 'tool_run_start':
      case 'tool_run_end': {
        // 这里只管「Run 视图」的工具行：只认当前 DM 会话（Project 会话的工具事件
        // 不属于 Hub 的 Run 页）。待办快照**不在这里**收 —— 它在 handleEvent 顶部、
        // 这道 sid 闸门之前就落了表（见「场景2」注释）。
        if (!sid || (ev.session_id && ev.session_id !== sid)) return
        const key = ev.id || `${ev.name ?? 'tool'}-${ev.timestamp ?? Date.now()}`
        const started = ev.event_type === 'tool_run_start'
        set((s) => {
          if (started) {
            if (s.runLog.some((e) => e.id === key)) return {}
            return {
              runLog: [...s.runLog, { id: key, kind: 'tool' as const, name: ev.name, status: 'running', at: Date.now() }],
            }
          }
          const next = s.runLog.map((e) =>
            e.id === key ? { ...e, status: ev.is_error ? 'error' : 'done', ms: e.at ? Date.now() - e.at : undefined } : e,
          )
          if (!next.some((e) => e.id === key)) {
            next.push({ id: key, kind: 'tool', name: ev.name, status: ev.is_error ? 'error' : 'done', at: Date.now() })
          }
          return { runLog: next }
        })
        return
      }
      case 'compress_start': {
        if (!sid || (ev.session_id && ev.session_id !== sid)) return
        set((s) => ({
          runLog: [...s.runLog, { id: `compress-${Date.now()}`, kind: 'compress' as const, name: 'compress', status: 'running', at: Date.now() }],
        }))
        return
      }
      case 'compress_end': {
        if (!sid || (ev.session_id && ev.session_id !== sid)) return
        set((s) => ({
          runLog: [...s.runLog, {
            id: `compress-${Date.now()}`, kind: 'compress' as const, name: 'compress', status: 'done',
            summary: `${ev.before ?? '?'} → ${ev.after ?? '?'}`, at: Date.now(),
          }],
          actions: [...s.actions, {
            seq: -(s.actions.length + 1), sender: { kind: 'system' },
            text: `上下文已压缩：${ev.before ?? '?'} → ${ev.after ?? '?'} 条`,
            refs: { action: 'compress' }, created_at: new Date().toISOString(),
          }],
        }))
        return
      }
      case 'hub_round': {
        if (!ev.group_id || ev.group_id !== get().activeGroupId) return
        set((s) => ({ groupRounds: upsertRound(s.groupRounds, ev.round) }))
        void get().loadGroupActivity(ev.group_id)
        return
      }
      case 'hub_agent_status': {
        const agentID = ev.agent_id || ''
        if (!agentID) return
        const running = ev.status !== 'idle' && ev.status !== 'deleted'
        set((s) => ({ running: { ...s.running, [agentID]: running } }))
        return
      }
      default:
        return
    }
  },
}))

/** App 启动时订阅一次（离开 Hub 页也要收告警）；返回撤销函数。 */
export function attachHubEvents(): () => void {
  const transport = getTransport()
  return transport.onEvent((line) => useHubStore.getState().handleEvent(line))
}
