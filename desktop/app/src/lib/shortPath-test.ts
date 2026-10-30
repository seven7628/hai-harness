import { test } from 'node:test'
import assert from 'node:assert/strict'
import { shortName, shortPath } from './shortPath'

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

test('shortName：短名字原样返回（绝大多数文件不该被改动）', () => {
  assert.equal(shortName('main.go'), 'main.go')
  assert.equal(shortName('a'.repeat(24)), 'a'.repeat(24))
})

test('shortName：长名字中间截断，尾部（扩展名 + 末段）保住', () => {
  const out = shortName('conversation_router_helper_test.go')
  assert.ok(out.length <= 24, `长度必须 <= 24，实际 ${out.length}: ${out}`)
  assert.ok(out.includes('…'), '必须出现省略号')
  assert.ok(out.endsWith('_test.go'), `尾巴要能区分测试/源码，实际 ${out}`)
  assert.ok(out.startsWith('conversation_r'), `头部要保留辨识度，实际 ${out}`)
  // 同一目录下的「源码 / 测试」两个文件截断后必须还能区分
  assert.notEqual(shortName('conversation_router_helper_test.go'), shortName('conversation_router_helper.go'))
})

test('shortName：没有扩展名的长名字照样截断', () => {
  const out = shortName('a'.repeat(40))
  assert.ok(out.length <= 24)
  assert.ok(out.includes('…'))
})
