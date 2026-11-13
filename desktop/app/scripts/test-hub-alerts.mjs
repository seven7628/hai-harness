#!/usr/bin/env node
// P2.5-05：hub_alert 纯逻辑验收。
//   幂等键必须忽略 at（否则后端每次新 at 让重复告警永不合并）；
//   severity 只接受 info/warning/error，未知值归一到 warning。
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const entry = join(__dirname, '..', 'src', 'hub', 'alerts.ts')
const result = await build({
  entryPoints: [entry], bundle: true, platform: 'node', format: 'esm',
  write: false, logLevel: 'silent',
})
const source = result.outputFiles[0].text
const mod = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }

assert(mod.alertKey('blocked_budget', 'task_a', '预算到顶') === mod.alertKey('blocked_budget', 'task_a', '预算到顶'), '同 code/task/message 幂等')
await new Promise((r) => setTimeout(r, 5))
assert(mod.alertKey('blocked_budget', 'task_a', '预算到顶') === mod.alertKey('blocked_budget', 'task_a', '预算到顶'), '跨毫秒仍幂等（key 不含时钟）')
const evA = { code: 'notify_failed', task_id: 'task_a', message: 'x', at: '2026-01-01T00:00:00Z' }
const evB = { ...evA, at: '2026-01-01T00:00:05Z' }
assert(mod.alertFromEvent(evA).id === mod.alertFromEvent(evB).id, '事件重放（只有 at 不同）仍合并')
assert(mod.alertFromEvent(evA).id !== mod.alertFromEvent({ ...evA, message: 'y' }).id, 'message 不同不合并')
assert(mod.alertKey('blocked_budget', 'task_a', 'x') !== mod.alertKey('blocked_budget', 'task_b', 'x'), '不同 task 不合并')
assert(mod.alertKey('notify_failed', 'task_a', 'x') === mod.alertKey('notify_failed', 'task_a', 'x'), '重复 notify_failed 合并')
assert(mod.alertSeverity('error') === 'error' && mod.alertSeverity('info') === 'info', '已知 severity 原样')
assert(mod.alertSeverity('') === 'warning' && mod.alertSeverity(undefined) === 'warning', '空/未知 severity 归一为 warning')
const one = { id: mod.alertKey('settle_failed', 'task_a', 'x'), code: 'settle_failed' }
const two = { id: mod.alertKey('recovery_failed', 'task_b', 'y'), code: 'recovery_failed' }
let list = mod.addHubAlert([], one)
list = mod.addHubAlert(list, one)
assert(list.length === 1, '同 id 去重')
list = mod.addHubAlert(list, two)
assert(list[0].id === two.id && list.length === 2, '新告警置顶')
for (let i = 0; i < 30; i++) list = mod.addHubAlert(list, { id: `x:${i}`, code: 'x' })
assert(list.length === 20, '告警栈封顶 20')
assert(mod.alertSeverity('critical') === 'warning', '未知 severity 不产生无样式圆点')
console.log(`\n${pass} 项通过`)
