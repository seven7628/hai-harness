// [recovered] from /private/tmp/term-writer.test.cjs（模块标记 `// src/components/terminal/writer-test.ts`）
// [implemented] 依据 docs/TERMINAL_E2E_CONTRACTS.md「writer 批处理与屏障 6 条不变式」：
//   切线把 writer.ts 内联进来了；本文件改为 `import` 真模块，断言逐条保留（不删不改）。
import test from 'node:test'
import assert from 'node:assert/strict'

import { terminalWriter } from './writer'

const bytes = (text: string): Uint8Array => new TextEncoder().encode(text)

/** 假写入器：记录每次 write 的文本，done 由测试手动放行（于是「在飞」可观测）。 */
function sink() {
  const calls: { text: string; done: () => void }[] = []
  const log: string[] = []
  let live = 0
  let peak = 0
  const write = (data: Uint8Array, done: () => void): void => {
    const text = new TextDecoder().decode(data)
    live += 1
    peak = Math.max(peak, live)
    log.push(`write:${text}`)
    let counted = false
    calls.push({
      text,
      done: () => {
        if (!counted) {
          counted = true
          live -= 1
        }
        done()
      },
    })
  }
  const settle = (): void => {
    const last = calls[calls.length - 1]
    assert.ok(last, '没有 in-flight 的 write 可以放行')
    last.done()
  }
  return {
    write,
    calls,
    log,
    settle,
    peak: () => peak,
    texts: () => calls.map((c) => c.text),
  }
}

test('不变式 1：done 之前不写下一块，同时最多一个 in-flight', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  w.push(bytes('a'))
  assert.deepEqual(s.texts(), ['a'], '队头立刻开写')
  w.push(bytes('b'))
  s.settle()
  w.push(bytes('c'))
  s.settle()
  assert.deepEqual(s.texts(), ['a', 'b', 'c'], '每放行一次才推进一步')
  assert.equal(s.peak(), 1)
})

test('不变式 2：紧邻的输出块合并成一次 write', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  w.push(bytes('a'))
  w.push(bytes('b'))
  w.push(bytes('c'))
  s.settle()
  assert.deepEqual(s.texts(), ['a', 'bc'], '排队的两块合成一次 write 且顺序不变')
  assert.equal(s.peak(), 1)
})

test('不变式 2：步骤是合并的分界，跨不过去', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  w.push(bytes('a'))
  w.push(bytes('b'))
  w.push(() => {})
  w.push(bytes('c'))
  w.push(bytes('d'))
  s.settle()
  s.settle()
  assert.deepEqual(s.texts(), ['a', 'b', 'cd'], '步骤前后的输出块各自成一次 write')
})

test('不变式 3：输出 A → 步骤 → 输出 B，调用顺序与合并情况', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  const step = (): number => s.log.push('step')
  w.push(bytes('A'))
  w.push(bytes('B'))
  w.push(step)
  w.push(bytes('C'))
  w.push(bytes('D'))
  assert.deepEqual(s.log, ['write:A'], '步骤必须等到它前面那一格写完')
  s.settle()
  assert.deepEqual(s.log, ['write:A', 'write:B'], '步骤也不许被后面的输出抢先')
  s.settle()
  assert.deepEqual(s.log, ['write:A', 'write:B', 'step', 'write:CD'])
})

test('不变式 3：空闲时 push 的步骤立刻执行，输出跟在它后面', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  w.push(() => {
    s.log.push('step')
  })
  w.push(bytes('a'))
  assert.deepEqual(s.log, ['step', 'write:a'])
})

test('不变式 4：clear 丢弃排队内容，后续 push 全被忽略', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  w.push(bytes('a'))
  w.push(bytes('b'))
  w.clear()
  s.settle()
  w.push(bytes('c'))
  assert.deepEqual(s.texts(), ['a'], 'clear 之后一个字都不再写')
})

test('不变式 4：clear 之后迟到的 done 不会执行队列里的步骤', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  let ran = 0
  w.push(bytes('a'))
  w.push(() => {
    ran += 1
  })
  w.clear()
  s.settle()
  assert.equal(ran, 0)
  assert.deepEqual(s.texts(), ['a'])
})

test('不变式 5：空 Uint8Array 不产生 write', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  w.push(new Uint8Array(0))
  assert.deepEqual(s.log, [], '只有空块时一次也不写')
  w.push(bytes('a'))
  w.push(new Uint8Array(0))
  w.push(bytes('b'))
  s.settle()
  assert.deepEqual(s.texts(), ['a', 'b'], '空块被跳过，不占一格')
})

test('不变式 6：步骤抛异常只丢它自己，队列继续', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  w.push(bytes('a'))
  w.push(() => {
    throw new Error('步骤炸了')
  })
  w.push(bytes('b'))
  assert.doesNotThrow(() => s.settle())
  assert.deepEqual(s.texts(), ['a', 'b'])
  assert.equal(s.peak(), 1)
})

test('同一个 done 被回调两次不会让两个 write 同时在飞', () => {
  const s = sink()
  const w = terminalWriter(s.write)
  w.push(bytes('a'))
  w.push(bytes('b'))
  w.push(bytes('c'))
  const first = s.calls[0]
  first.done()
  first.done()
  assert.equal(s.calls.length, 2, '第二次 done 不该再推进一步')
  assert.equal(s.peak(), 1)
  s.settle()
  assert.deepEqual(s.texts(), ['a', 'bc'])
})
