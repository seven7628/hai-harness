// viewBudget 估算器的**标定**（2026-11-12）—— 不是测试，是一次性的量。
//
// 为什么需要：实测渲染进程有 **978 MB 的 V8 堆**，而按估算器算只占预算的
// 一小半 ⇒ 估算器**低估**了 ⇒ 预算机制根本没触发降级。
//
// 怎么标定：Node 的 V8 与 Electron 渲染进程同款 ⇒ 在 Node 里造 N 个真实形状的
// 块对象，量 `process.memoryUsage().heapUsed` 的增量，再和 roughSize 的
// 估算比。
//
// 跑法：
//   npx esbuild src/store/viewBudget-calibrate.ts --bundle --platform=node \
//     --format=cjs --outfile=/tmp/vb-cal.cjs && node /tmp/vb-cal.cjs
import { roughSize, blockBytes } from './viewBudget'

type MsgBlock = { kind: string; id: number; text: string; items: unknown[]; [k: string]: unknown }

// 真实形状的块（按 useAppStore 的 MsgBlock 联合裁出来的典型字段）。
function makeBlock(i: number, textLen: number, items: number): MsgBlock {
  const it: Record<string, unknown>[] = []
  for (let j = 0; j < items; j++) {
    it.push({
      kind: 'tool',
      toolId: `t${i}-${j}`,
      name: 'bash',
      args: '{"cmd":"ls -la /tmp"}',
      result: 'x'.repeat(400),
      status: 'done',
      durMs: 1234,
      runId: `r${i}`,
    })
  }
  return {
    kind: 'assistant',
    id: i,
    text: 'y'.repeat(textLen),
    streaming: false,
    thinking: 'z'.repeat(textLen / 2),
    model: 'deepseek-chat',
    runId: `r${i}`,
    thinkMs: 100,
    durMs: 2000,
    items: it,
  }
}

const N = 5000
global.gc?.()
const before = process.memoryUsage().heapUsed
const blocks: MsgBlock[] = []
for (let i = 0; i < N; i++) blocks.push(makeBlock(i + 1, 600, 6))
global.gc?.()
const after = process.memoryUsage().heapUsed

let estimated = 0
for (const b of blocks) estimated += blockBytes(b as never)

const actual = after - before
console.log(`块数        ${N}`)
console.log(`真实堆增量  ${(actual / 1e6).toFixed(1)} MB  → ${Math.round(actual / N)} B/块`)
console.log(`估算        ${(estimated / 1e6).toFixed(1)} MB  → ${Math.round(estimated / N)} B/块`)
console.log(`低估倍数    ${(actual / estimated).toFixed(2)}×`)
console.log(`单个 roughSize: ${roughSize(makeBlock(1, 600, 6))} B`)
