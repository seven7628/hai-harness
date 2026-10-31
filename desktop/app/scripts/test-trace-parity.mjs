// 交叉实现一致性：Go 与 TS 两套 trace 投影喂**同一份事件流**，逐字段比对输出。
//
// 背景（docs/TRACE_CHAIN_REDESIGN.md）：Go（desktop/bridge/trace.go）负责历史 trace，
// TS（src/store/trace.ts）负责当前 trace 的实时投影 —— 同一套折叠规则写了两遍。
// 若两者漂移，用户切换 trace 时瀑布图会跳变（同一 run 前后不一致）。
// 单侧单测各自能过，只有交叉比对能发现不一致。
//
// Go 侧 golden 由 desktop/bridge/trace_crossimpl_test.go 生成：
//   go test ./desktop/bridge -run TestTraceProjectionCrossImpl
//
// 用法：npm run test:trace-parity
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync, readFileSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const APP = join(__dirname, '..')
const REPO = join(APP, '..', '..')
const FIXTURE = join(REPO, 'desktop', 'bridge', 'testdata', 'trace_fixture.json')
const GOLDEN = FIXTURE + '.go.golden.json'

if (!existsSync(GOLDEN)) {
  console.error(`✗ 缺少 Go golden：${GOLDEN}`)
  console.error('  先生成：go test ./desktop/bridge -run TestTraceProjectionCrossImpl')
  process.exit(1)
}

// 打包 TS 投影模块（纯函数，无 store 依赖）
const dir = mkdtempSync(join(tmpdir(), 'trace-parity-'))
const outfile = join(dir, 'trace.cjs')
await build({
  entryPoints: [join(APP, 'src', 'store', 'trace.ts')],
  bundle: true, format: 'cjs', platform: 'node', outfile, logLevel: 'silent',
})
const require2 = createRequire(import.meta.url)
const { projectAllTraces, TRACE_SPAN_TYPES } = require2(outfile)

// —— 读 fixture（与 Go 侧同源的 23 行事件）——
const rawEvents = JSON.parse(readFileSync(FIXTURE, 'utf8'))
// 与 Go parseTraceEvent 同口径：白名单过滤后才进投影
const events = rawEvents.filter((e) => TRACE_SPAN_TYPES.has(String(e.event_type)))

const traces = projectAllTraces(events)

// —— 归一化为与 Go golden 相同的形状（字段名/顺序一致）——
const pick = (s) => {
  const o = { kind: s.kind }
  const opt = {
    id: s.id, run_id: s.run_id, parent_id: s.parent_id, name: s.name, label: s.label,
    depth: s.depth, dur_ms: s.dur_ms, model: s.model, in_tok: s.in_tok, out_tok: s.out_tok,
    cache_tok: s.cache_tok, cache_write_tok: s.cache_write_tok, cost_usd: s.cost_usd, before: s.before, after: s.after,
    ctx_tokens: s.ctx_tokens, wait_type: s.wait_type, questions: s.questions,
    is_error: s.is_error, retries: s.retries,
  }
  for (const [k, v] of Object.entries(opt)) {
    if (v === undefined || v === null || v === 0 || v === false || v === '') continue
    o[k] = v
  }
  o.status = s.status
  o.start_at = s.start_at ?? 0
  return o
}
const norm = traces.map((t) => ({
  id: t.summary.id,
  label: t.summary.label,
  status: t.summary.status,
  spans: t.spans.map(pick),
}))

const golden = JSON.parse(readFileSync(GOLDEN, 'utf8'))

// —— 逐字段比对 ——
let pass = 0
const ok = (m) => { pass++; console.log(`  ✓ ${m}`) }
function fail(msg, detail) {
  console.error(`  ✗ ${msg}`)
  if (detail) console.error(detail)
  process.exit(1)
}

console.log('交叉实现一致性（Go golden ←→ TS 投影）')
assert(traces.length > 0, 'TS 投影结果不应为空')
ok(`trace 条数一致（${traces.length}）`)
assert.strictEqual(norm.length, golden.length, `trace 条数：TS ${norm.length} vs Go ${golden.length}`)

for (let i = 0; i < golden.length; i++) {
  const g = golden[i], t = norm[i]
  const where = `trace[${i}] ${g.id}`
  assert.strictEqual(t.id, g.id, `${where} id`)
  if (t.label !== g.label) fail(`${where} label 不一致`, `  TS: ${JSON.stringify(t.label)}\n  Go: ${JSON.stringify(g.label)}`)
  if (t.status !== g.status) fail(`${where} status 不一致`, `  TS: ${t.status}  Go: ${g.status}`)
  if (t.spans.length !== g.spans.length) {
    fail(`${where} span 条数不一致`, `  TS ${t.spans.length}: ${t.spans.map((s) => s.kind).join(',')}\n  Go ${g.spans.length}: ${g.spans.map((s) => s.kind).join(',')}`)
  }
  for (let j = 0; j < g.spans.length; j++) {
    const ts = t.spans[j], gs = g.spans[j]
    const keys = new Set([...Object.keys(ts), ...Object.keys(gs)])
    for (const k of keys) {
      const a = ts[k] ?? (k === 'start_at' ? 0 : undefined)
      const b = gs[k] ?? (k === 'start_at' ? 0 : undefined)
      if (JSON.stringify(a) !== JSON.stringify(b)) {
        fail(`${where} span[${j}](${gs.kind}) 字段 ${k} 不一致`, `  TS: ${JSON.stringify(a)}\n  Go: ${JSON.stringify(b)}\n  完整 TS: ${JSON.stringify(ts)}\n  完整 Go: ${JSON.stringify(gs)}`)
      }
    }
  }
  ok(`${where}：${g.spans.length} 个 span 逐字段一致`)
}

const tsSpans = norm.reduce((a, t) => a + t.spans.length, 0)
const goSpans = golden.reduce((a, t) => a + t.spans.length, 0)
assert.strictEqual(tsSpans, goSpans, `span 总数一致（${tsSpans}）`)
assert(!JSON.stringify(norm).includes('正文不应进投影'), '正文不泄漏（TS）')

console.log(`\n✅ 交叉实现一致：${pass} 项断言，${norm.length} 条 trace / ${tsSpans} 个 span 逐字段相同`)
