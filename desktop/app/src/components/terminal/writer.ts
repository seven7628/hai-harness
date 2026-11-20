// [recovered] from /private/tmp/builtin-surfaces.test.cjs（模块标记 `// src/components/terminal/writer.ts`）
// [implemented] 依据 docs/TERMINAL_E2E_CONTRACTS.md「writer 批处理与屏障」：
//   切片里 `terminalWriter` 没有 export、类型全丢。本文件补导出名（TerminalPane.tsx 的
//   import：terminalWriter）与类型标注，**逻辑逐字未改**；移除 @ts-nocheck。
//
// 为什么需要这一层（docs/TERMINAL_DESIGN.md §7）：xterm 的 write 是异步回调式，
// 而 WS 的每个 message 都是一小块输出。直接逐块 write 会让「在飞的 write」无限堆积
// （xterm 内部队列暴涨、按键回声延迟）。这里做两件事：
//   ① **批处理**：队头开写后，紧邻的输出块合并成一次 write（顺序不变）；
//   ② **屏障**：函数（步骤）是合并的分界线 —— 它必须等前面那一格写完才执行，
//      也不许被后面的输出抢跑。`term.reset()` 这类「顺序敏感」动作就靠它排队。

export type WriterChunk = Uint8Array
/** 屏障：排队执行的同步动作（如 reset / 写一条提示行）。抛异常只丢它自己。 */
export type WriterStep = () => void
export type WriterItem = WriterChunk | WriterStep
/** 底层写入器（xterm 的 `term.write(data, done)`）。 */
export type WriteFn = (data: Uint8Array, done: () => void) => void

export interface TerminalWriter {
  push(item: WriterItem): void
  /** 丢弃排队内容并关闸；在飞的 write 取消不了，但它的 done 之后不再推进队列。 */
  clear(): void
}

function isStep(item: WriterItem): item is WriterStep {
  return typeof item === 'function'
}

export function terminalWriter(write: WriteFn): TerminalWriter {
  let queue: WriterItem[] = []
  let running = false
  let closed = false
  let pumping = false

  function pump(): void {
    // pumping：done 回调可能在 write 的**同步**返回里就调用（xterm 对空/极小数据会这样），
    // 那时再进 pump 会把同一个队列重入处理一遍 → 用闸门把重入变成「继续本轮 while」。
    if (pumping || closed) return
    pumping = true
    try {
      while (!closed && !running && queue.length > 0) {
        const head = queue[0]
        if (isStep(head)) {
          queue.shift()
          try {
            head()
          } catch {
            /* 屏障抛异常只丢它自己：队列继续 */
          }
          continue
        }
        // 合并到下一个屏障为止
        const batch: WriterChunk[] = []
        while (batch.length < queue.length) {
          const nxt = queue[batch.length]
          if (isStep(nxt)) break
          batch.push(nxt)
        }
        queue.splice(0, batch.length)
        let total = 0
        for (const chunk of batch) total += chunk.length
        if (total === 0) continue // 只有空块：一次也不写，不占一格
        const merged = new Uint8Array(total)
        let offset = 0
        for (const chunk of batch) {
          merged.set(chunk, offset)
          offset += chunk.length
        }
        running = true
        let settled = false
        write(merged, () => {
          if (settled) return // 同一个 done 被回调两次不该让两个 write 同时在飞
          settled = true
          if (closed) return
          running = false
          pump()
        })
      }
    } finally {
      pumping = false
    }
  }

  return {
    push(item: WriterItem): void {
      if (closed) return
      queue.push(item)
      pump()
    },
    clear(): void {
      closed = true
      queue = []
    },
  }
}
