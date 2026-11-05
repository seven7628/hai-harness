// 资料页 patch 构造回归（P1-08/09 复核 B1：撤下工具被静默清空）。
//
// 用法：npm run test:hub-patch
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const out = join(mkdtempSync(join(tmpdir(), 'hub-patch-')), 'agentPatch.mjs')
await build({
  entryPoints: [join(__dirname, '../src/hub/agentPatch.ts')],
  bundle: true,
  format: 'esm',
  outfile: out,
  logLevel: 'silent',
})
const { buildAgentPatch, buildAgentProfile, normalizeDisable } = await import(out)

const base = {
  id: 'ag_aaaaaaaa', name: 'Researcher', accent: '#7bc199', role: '竞品研究员',
  description: '负责竞品', instructions: '只信一手来源',
  provider: 'deepseek', model: 'deepseek-v4-flash', effort: 'low', compress_threshold: 0.8,
  memory: { enabled: true },
  tools: { preset: 'all', disable: ['browser_*', 'mcp__github__*'], allow: ['read_file'], bash_roots: ['/data'], mcp: { disable: false } },
  avatar: { kind: 'emoji', value: '🟢' },
}

function form(over = {}) {
  return {
    name: base.name, emoji: base.avatar.value, accent: base.accent, role: base.role,
    description: base.description, instructions: base.instructions,
    provider: base.provider, model: base.model, effort: base.effort,
    threshold: base.compress_threshold, memoryOn: true,
    preset: base.tools.preset, disable: base.tools.disable.join(' '),
    ...over,
  }
}

// 1) 什么都没改 → 空 patch（连 tools 都不出现）
assert.deepEqual(buildAgentPatch(form(), base), {}, 'unchanged form must produce an empty patch')

// 2) 只改人格 → patch 只有 instructions
assert.deepEqual(buildAgentPatch(form({ instructions: '新口径' }), base), { instructions: '新口径' })

// 3) 只改名字（用户没打开工具页）→ 不出现 tools（今天以前会被清空 disable）
const renamed = buildAgentPatch(form({ name: 'Researcher2' }), base)
assert.deepEqual(renamed, { name: 'Researcher2' })
assert.ok(!('tools' in renamed), 'rename must not touch tools')
assert.deepEqual(base.tools.disable, ['browser_*', 'mcp__github__*'], 'base object must not be mutated')

// 4) 用户真的清空撤下工具 → tools.disable 为空数组，且 allow/mcp 原样保留
const cleared = buildAgentPatch(form({ disable: '' }), base)
assert.deepEqual(cleared.tools.disable, [])
assert.deepEqual(cleared.tools.allow, ['read_file'])
assert.deepEqual(cleared.tools.bash_roots, ['/data'])
assert.deepEqual(cleared.tools.mcp, { disable: false })

// 5) 只改 preset → tools 里带 preset，disable 仍是原值
const presetOnly = buildAgentPatch(form({ preset: 'readonly' }), base)
assert.equal(presetOnly.tools.preset, 'readonly')
assert.deepEqual(presetOnly.tools.disable, ['browser_*', 'mcp__github__*'])

// 6) 头像/色未改 → 不出现 avatar/accent（避免把 image 头像降级成 emoji）
const mirrored = { ...base, avatar: { kind: 'image', path: '/x.png' } }
assert.ok(!('avatar' in buildAgentPatch(form(), mirrored)))

// 7) 归一化：空格/逗号/重复
assert.deepEqual(normalizeDisable(' a, b  a\nc '), ['a', 'b', 'c'])

// 8) 新建 profile 带全字段（服务端分配 id/version）
const created = buildAgentProfile(form({ name: '  New  ' }))
assert.equal(created.name, 'New')
assert.equal(created.avatar.value, '🟢')
assert.deepEqual(created.tools, { preset: 'all', disable: ['browser_*', 'mcp__github__*'] })

console.log('agent patch: 8/8 assertions passed')
