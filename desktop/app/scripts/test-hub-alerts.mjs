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
assert(mod.alertKey('blocked_budget', 'task_a', 'x') !== mod.alertKey('blocked_budget', 'task_b', 'x'), '不同 task 不合并')
assert(mod.alertKey('notify_failed', 'task_a', 'x') === mod.alertKey('notify_failed', 'task_a', 'x'), '重复 notify_failed 合并')
assert(mod.alertSeverity('error') === 'error' && mod.alertSeverity('info') === 'info', '已知 severity 原样')
assert(mod.alertSeverity('') === 'warning' && mod.alertSeverity(undefined) === 'warning', '空/未知 severity 归一为 warning')
assert(mod.alertSeverity('critical') === 'warning', '未知 severity 不产生无样式圆点')
console.log(`\n${pass} 项通过`)
