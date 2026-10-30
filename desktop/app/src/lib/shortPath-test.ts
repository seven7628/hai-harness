import { test } from 'node:test'
import assert from 'node:assert/strict'
import { shortPath } from './shortPath'

test('短路径原样返回', () => {
  assert.equal(shortPath('auth.go'), 'auth.go')
  assert.equal(shortPath('/tmp/arch-preview.html'), '/tmp/arch-preview.html')
})

test('超长路径收成 …/最后两段（段本身也截断）', () => {
  const p = '/tmp/hai-multi-9668/.agents/skills/visual-ui/references/architecture-and-execution-chain.md'
  const out = shortPath(p)
  assert.ok(out.startsWith('…/'), out)
  assert.ok(out.length < p.length, out)
  assert.ok(out.includes('references/'), out)
  assert.ok(out.endsWith('…'), out)
  assert.ok(!out.includes('hai-multi-9668'), out)
})

test('read_file 的行号后缀不吞掉', () => {
  const out = shortPath('/Users/admin/workspace/go-code/desktop/app/src/components/NarrativeBlocks.tsx:120-140')
  assert.ok(out.endsWith(':120-140'), out)
  assert.ok(out.startsWith('…/'), out)
})

test('单段超长（没有斜杠）也能截断', () => {
  const out = shortPath('a'.repeat(80) + '.ts')
  assert.ok(out.length <= 27 && out.endsWith('…'), out)
})

test('空串 / 带尾斜杠都不炸', () => {
  assert.equal(shortPath(''), '')
  assert.ok(shortPath('/very/long/'.repeat(8) + 'x').startsWith('…/'))
})
