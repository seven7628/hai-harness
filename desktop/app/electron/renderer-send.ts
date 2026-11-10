// 主进程 → 渲染进程的**唯一**发送通道（BR-04）。
//
// 为什么必须有这一层：`webContents.send` 在窗口关闭/刷新途中（webContents 已销毁）会**抛异常**，
// 而主进程的调用点大多在 readline 的 'line' 监听、16ms flush 定时器、子进程 exit 回调里 ——
// 这些位置抛出的异常没有任何 try/catch 接住，会直接变成 main process 的 uncaughtException
//（'Object has been destroyed'）。表现是「bridge 有输出但界面不动」甚至拖垮主进程，
// 而根因只是「这一刻窗口不在」。
//
// 这一层的语义只有一条：**送不到就丢掉**（drop），绝不把窗口生命周期问题升级成命令失败
// 或进程异常。窗口在、webContents 活 → 照常发，返回值告诉调用方是否送达（缓冲/去重可选）。
//
// 判空顺序（三层都必要）：
//   1. 窗口引用为 null（启动中／已关闭）；
//   2. 窗口 isDestroyed（close 已发出、GC 前）；
//   3. webContents.isDestroyed（reload／导航／崩溃恢复窗口期，窗口对象还在但内容没了）；
//   4. 即便以上都通过，发送瞬间仍可能被销毁 —— 故 send 本身包 try/catch（race 兜底）。

/** 发送目标的最小形状（真实 BrowserWindow 结构兼容；测试用假对象即可）。 */
export interface SendableWindow {
  isDestroyed?: () => boolean
  webContents:
    | {
        isDestroyed?: () => boolean
        send: (channel: string, ...args: unknown[]) => void
      }
    | null
    | undefined
}

/** 发送计数（诊断用：dropped/failed 长期增长说明窗口生命周期或订阅时机有问题）。 */
export interface RendererSendStats {
  sent: number
  dropped: number
  failed: number
}

export interface RendererSender {
  /** 发送一次。true = 已交给 webContents；false = 丢弃（窗口不在/已销毁/发送抛错）。 */
  send: (channel: string, ...args: unknown[]) => boolean
  stats: () => RendererSendStats
  /** 最近一次丢弃/失败的原因（仅供排障与测试断言；没有则空串）。 */
  lastDropReason: () => string
}

/**
 * createRendererSender：把「取当前窗口」的 getter 包成带判空的 sender。
 *
 * 传 getter 而不是窗口实例：主窗口会被重建（关闭→重新打开），捕获实例会永远发到旧对象。
 */
export function createRendererSender(getWindow: () => SendableWindow | null | undefined): RendererSender {
  const stats: RendererSendStats = { sent: 0, dropped: 0, failed: 0 }
  let lastReason = ''

  const drop = (reason: string): false => {
    stats.dropped++
    lastReason = reason
    return false
  }

  const send = (channel: string, ...args: unknown[]): boolean => {
    let win: SendableWindow | null | undefined
    try {
      win = getWindow()
    } catch {
      // getter 自身不该抛；真抛了也按「窗口不在」处理（绝不让它冒泡到 readline/定时器）
      win = null
    }
    if (!win) return drop('窗口不存在')
    try {
      if (win.isDestroyed?.()) return drop('窗口已销毁')
    } catch {
      return drop('窗口状态不可读')
    }
    const wc = win.webContents
    if (!wc) return drop('webContents 不存在')
    try {
      if (wc.isDestroyed?.()) return drop('webContents 已销毁')
    } catch {
      return drop('webContents 状态不可读')
    }
    try {
      wc.send(channel, ...args)
      stats.sent++
      return true
    } catch (e) {
      // 判空与发送之间被销毁（reload/关闭的 race）：发送失败不抛，只记一次。
      stats.failed++
      lastReason = `发送失败: ${(e as Error)?.message ?? String(e)}`
      return false
    }
  }

  return { send, stats: () => ({ ...stats }), lastDropReason: () => lastReason }
}
