import type { AgentItem, MsgBlock } from '../store/useAppStore'

// 子智能体转录 → 主会话会话块：**让子会话页直接是主会话页**。
//
// 用户 2026-10 的原话：「子会话完全可以复用主会话的页面样式与功能；不需要有独立的了」。
// 之前是「两套宿主用同一批渲染原语」（AgentTranscript 调 NarrativeBlocks 的 ToolRow/Thinking），
// 样式对齐了，但**页面级**的东西仍然是两份：滚动/贴底跟随/历史渐入/加载更早/空态 ——
// 这些住在 Narrative 里，子会话那份只能各写一遍或干脆没有（于是「下方一大片空白、
// 也不能贴底跟随」）。正确做法不是把 Narrative 的能力抄一份，而是**让子会话就是 Narrative**：
// 把它的数据换成本函数的输出，组件本身一个字节都不分支。
//
// 为什么不是「在 store 里造一个虚拟 view」：那会把子智能体的转录写进 views（受 LRU 淘汰、
// 参与 checkpoint 归一化、还要在手势/分页里区分真假 view），为了复用一屏渲染而污染状态源。

/**
 * agentItemsToBlocks 把子智能体的活动记录映射成主会话的会话块。
 *
 * 映射规则（目标是「视觉上与主会话里同一条记录完全一致」）：
 *   · thinking + 紧随的 text → **一个** assistant 块（主会话里思考本来就是 assistant
 *     的字段而不是独立块；分成两块会多出一行「谁在说话」的作者行）
 *   · 单独的 thinking（后面跟的是工具）→ 只有 thinking 的 assistant 块；
 *     Narrative 对「无正文无流式」的块会跳过，但**有 thinking** 就渲染思考块 —— 正是要的
 *   · 单独 text → assistant 块（error 透传：LLM 失败提示在卡片内是醒目红字）
 *   · tool → tool 块（字段逐个对应；status 只有 running/done 两态，error 由 isError 表达）
 *   · compression → compression 块（子智能体自己的上下文压缩，与主会话同一个渲染件）
 *
 * id 用**负数**（-1 起递减）：这些是渲染期合成块，不是会话里真实存在的块。负数保证
 * 不会与 store 的 blockSeq（正数）撞 id —— React key 撞了会复用错节点，表现是「展开了
 * 这个工具的详情，另一个工具跟着展开」。
 */
export function agentItemsToBlocks(items: readonly AgentItem[]): MsgBlock[] {
  const out: MsgBlock[] = []
  let id = -1
  for (let i = 0; i < items.length; i++) {
    const it = items[i]
    if (it.kind === 'thinking') {
      const next = items[i + 1]
      if (next?.kind === 'text') {
        out.push({
          kind: 'assistant', id: id--, text: next.text, thinking: it.text, streaming: false,
          ...(it.thinkStart != null ? { thinkStart: it.thinkStart } : {}),
        })
        i++ // 已并入本块
        continue
      }
      out.push({
        kind: 'assistant', id: id--, text: '', thinking: it.text, streaming: false,
        ...(it.thinkStart != null ? { thinkStart: it.thinkStart } : {}),
      })
      continue
    }
    if (it.kind === 'user') {
      // 子运行上下文里的一条消息（用户 / 模型 / 孙任务结果）。映射成主会话的 **user 块** ——
      // 这样「我对它说的」和主会话里「我对主 agent 说的」长得一模一样，而不是任务结果那种卡。
      out.push({ kind: 'user', id: id--, text: it.text, ...(it.startedAt != null ? { ts: it.startedAt } : {}) })
      continue
    }
    if (it.kind === 'text') {
      // 失败/超限提示（llm_error 挂在卡片上，不污染主叙述）：原样保留「红字提示」的语义。
      // 映射成主会话的 **system + tone:error** 块，而不是 assistant 正文 ——
      // assistant 没有错误位，映射过去会悄悄变成普通段落，等于丢掉「这轮失败了」。
      if (it.error) {
        out.push({ kind: 'system', id: id--, text: it.text, tone: 'error' })
        continue
      }
      out.push({ kind: 'assistant', id: id--, text: it.text, thinking: '', streaming: false })
      continue
    }
    if (it.kind === 'tool') {
      out.push({
        kind: 'tool', id: id--, toolId: it.toolId, name: it.name,
        ...(it.args != null ? { args: it.args } : {}),
        ...(it.result != null ? { result: it.result } : {}),
        ...(it.errorInfo ? { errorInfo: it.errorInfo } : {}),
        isError: it.status === 'error',
        // 子 agent 的转录只回放历史（live 由 Narrative 的 streaming 决定），
        // 因此非 running 一律视作 done —— tool 块没有 error 态（错误走 isError）。
        status: it.status === 'running' ? 'running' : 'done',
        ...(it.startedAt != null ? { startedAt: it.startedAt } : {}),
        ...(it.durMs != null ? { durMs: it.durMs } : {}),
        ...(it.taskId ? { taskId: it.taskId } : {}),
        ...(it.diff ? { diff: it.diff } : {}),
        ...(it.images ? { images: it.images } : {}),
        ...(it.usage ? { usage: it.usage } : {}),
        ...(it.nestedCalls ? { nestedCalls: it.nestedCalls } : {}),
        ...(it.spillPath ? { spillPath: it.spillPath } : {}),
        ...(it.hostOps ? { hostOps: it.hostOps } : {}),
        ...(it.todos ? { todos: it.todos } : {}),
      })
      continue
    }
    if (it.kind === 'compression') {
      out.push({
        kind: 'compression', id: id--, before: it.before, after: it.after,
        ...(it.ctxTokens != null ? { ctxTokens: it.ctxTokens } : {}),
        ...(it.reason ? { reason: it.reason } : {}),
        ...(it.model ? { model: it.model } : {}),
        ...(it.summary ? { summary: it.summary } : {}),
        ...(it.analysis ? { analysis: it.analysis } : {}),
        ...(it.usage ? { usage: it.usage } : {}),
        ...(it.error ? { error: it.error } : {}),
        ...(it.aborted ? { aborted: it.aborted } : {}),
        ...(it.durMs != null ? { durMs: it.durMs } : {}),
        ...(it.active ? { active: it.active } : {}),
        ...(it.startedAt != null ? { startedAt: it.startedAt } : {}),
      })
      continue
    }
  }
  return out
}

/**
 * withAgentToolBlocks 把「子智能体转录里的工具调用」并进会话级块列表。
 *
 * 为什么需要：会话级聚合只看 v.blocks（文件活动 / 变更定位就是如此），而子运行的
 * 工具调用活在 agent 卡片的 items 上。少了它们，**子会话里点 write/edit 的路径
 * 「查看变更」就找不到那次调用**，只能退回「最终文件」——用户看到的差异是
 * 「主会话能看变更，子智能体点开看不到」（2026-10 报障）。
 *
 * 只并 tool 块：其它类型（assistant/thinking）进会话级聚合没有意义，白白放大列表。
 */
export function withAgentToolBlocks(blocks: readonly MsgBlock[]): MsgBlock[] {
  const cards = blocks.filter((b): b is Extract<MsgBlock, { kind: 'agent' }> => b.kind === 'agent')
  if (!cards.length) return blocks as MsgBlock[]
  const extra: MsgBlock[] = []
  for (const c of cards) {
    // 按 items 数组缓存：流式期间 blocks 每个 chunk 都换新（调用方 useMemo 因此每次都跑），
    // 而一张卡片的 items 只在它自己更新时换新。没有这层缓存，长会话里每来一个 chunk 都要
    // 把全部卡片的 items（最多 50×N 条）重新映射一遍。
    let tools = toolCache.get(c.items)
    if (!tools) {
      tools = agentItemsToBlocks(c.items).filter((b) => b.kind === 'tool')
      toolCache.set(c.items, tools)
    }
    extra.push(...tools)
  }
  return extra.length ? [...blocks, ...extra] : (blocks as MsgBlock[])
}

const toolCache = new WeakMap<readonly AgentItem[], MsgBlock[]>()
