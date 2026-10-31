// 沙箱 UI 回复协议 store 侧回归测试（真实 store + esbuild 打包，Node 直接跑）。
//
// 被测的是「widget → 草稿」这条链路的**落点**：markdown.tsx 完成 e.source / 版本 / shape
// 三层校验后调 store.receiveWidgetReply，Composer 消费后 clearWidgetReply。
// iframe 与 DOM 不在这个测试里（需要浏览器），这里只钉 store 的契约：
//   1. 合法回复进 widgetReply；
//   2. 同 id 重复投递被丢弃（iframe 重挂载 / 连点都会重发）；
//   3. 非法输入一律不进（长度/类型/id 形状）——store 是全局可写的，不能假设调用方守规矩；
//   4. clearWidgetReply 之后是干净的（一次性信号，否则会重复注入草稿）。
//
// 用法：npm run test:widget-reply
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const require = createRequire(import.meta.url)

// store 模块加载时读 localStorage（项目树/偏好） → shim
const mem = new Map()
global.localStorage = {
  getItem: (k) => (mem.has(k) ? mem.get(k) : null),
  setItem: (k, v) => mem.set(k, String(v)),
  removeItem: (k) => mem.delete(k),
}

const dir = mkdtempSync(join(tmpdir(), 'widget-reply-'))
const outfile = join(dir, 'store-bundle.cjs')
// mock transport 会 import 带 `?raw` / `?url` / `?no-inline` 后缀的静态资源
// （README、logo、截图）。esbuild 不认识这些后缀 → 必须显式配 loader，
// 否则本测试和 test:store 一样在打包阶段就挂（既有环境问题，此处一并修）。
const assetPlugin = {
  name: 'asset-suffix',
  setup(b) {
    b.onResolve({ filter: /\?(raw|url|no-inline)$/ }, () => ({
      path: 'asset',
      namespace: 'asset-stub',
    }))
    b.onLoad({ filter: /.*/, namespace: 'asset-stub' }, () => ({
      contents: 'module.exports = ""',
      loader: 'js',
    }))
  },
}

await build({
  entryPoints: [join(__dirname, '../src/store/useAppStore.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  plugins: [assetPlugin],
})
const { useAppStore } = require(outfile)

const st = () => useAppStore.getState()
const reply = () => st().widgetReply
let n = 0
const ok = (label, fn) => {
  fn()
  n++
  console.log(`  ✓ ${label}`)
}

console.log('widget reply protocol — store side')

ok('合法 choice 回复进通道', () => {
  st().receiveWidgetReply('选了方案 B', 'r-1-abc')
  assert.ok(reply(), '应写入 widgetReply')
  assert.equal(reply().text, '选了方案 B')
  assert.equal(reply().id, 'r-1-abc')
  assert.ok(reply().ts > 0, 'ts 必须写入（用于排查）')
})

ok('同 id 重复投递被丢弃', () => {
  const before = reply()
  st().receiveWidgetReply('选了方案 B（重复）', 'r-1-abc')
  assert.equal(reply(), before, '同 id 不覆盖（应整体丢弃，而不是更新文本）')
})

ok('新 id 覆盖旧回复', () => {
  st().receiveWidgetReply('改成方案 A', 'r-2-xyz')
  assert.equal(reply().text, '改成方案 A')
  assert.equal(reply().id, 'r-2-xyz')
})

ok('clear 之后是干净的（一次性信号）', () => {
  st().clearWidgetReply()
  assert.equal(reply(), undefined)
  st().clearWidgetReply() // 幂等：重复 clear 不炸
  assert.equal(reply(), undefined)
})

ok('空白正文被拒', () => {
  st().clearWidgetReply()
  st().receiveWidgetReply('   \n\t ', 'r-3-a')
  assert.equal(reply(), undefined, '纯空白不应进通道')
})

ok('超长正文被拒（>2000）', () => {
  st().clearWidgetReply()
  st().receiveWidgetReply('x'.repeat(2001), 'r-4-a')
  assert.equal(reply(), undefined)
  // 恰好 2000 应通过（边界包含）
  st().receiveWidgetReply('y'.repeat(2000), 'r-5-a')
  assert.ok(reply(), '恰好 2000 字应通过')
})

ok('非字符串正文被拒', () => {
  st().clearWidgetReply()
  for (const bad of [null, undefined, 42, {}, [], true]) {
    st().receiveWidgetReply(bad, 'r-6-a')
    assert.equal(reply(), undefined, `正文类型 ${typeof bad} 不应通过`)
  }
})

ok('非法 id 被拒', () => {
  st().clearWidgetReply()
  const badIds = ['', 'has space', 'x'.repeat(65), 'a/b', '问题', null, 42]
  for (const bad of badIds) {
    st().receiveWidgetReply('正文', bad)
    assert.equal(reply(), undefined, `id ${JSON.stringify(bad)} 不应通过`)
  }
  // 合法形状全部可用
  for (const good of ['a', 'A_1-b', 'x'.repeat(64), 'r-1-abc']) {
    st().receiveWidgetReply('正文', good)
    assert.ok(reply(), `id ${good} 应通过`)
    st().clearWidgetReply()
  }
})

ok('正文首尾空白被裁掉', () => {
  st().clearWidgetReply()
  st().receiveWidgetReply('  选了 B  ', 'r-7-a')
  assert.equal(reply().text, '选了 B', '应 trim 后存')
})

console.log(`\n${n} 组断言全部通过`)
