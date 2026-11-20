// [recovered] from /private/tmp/term-frame.test.cjs（模块标记 `// src/components/terminal/frame-test.ts`）
// [implemented] 依据 docs/TERMINAL_E2E_CONTRACTS.md「帧编解码 6 条」：
//   切线是 esbuild 产物，模块被**内联**在同一个文件里（没有 import/export，靠 bundle 注入
//   `import_node_test` 之类）。本文件转成仓库测试：`import` 真模块、去掉 @ts-nocheck、
//   断言逐条保留（不删不改）。运行：`npm run test:terminal`（esbuild 打包 → node --test）。
import test from 'node:test'
import assert from 'node:assert/strict'

import { encodeCtrl, decodeFrame } from './frame'

const enc = new TextEncoder()
const dec = new TextDecoder()

test('控制帧：0x00 开头 + JSON，往返一致', () => {
  const frame = encodeCtrl({ t: 'resize', cols: 120, rows: 40 })
  assert.equal(frame[0], 0)
  const { ctrl, data } = decodeFrame(frame)
  assert.equal(data, undefined)
  assert.deepEqual(ctrl, { t: 'resize', cols: 120, rows: 40 })
})

test('数据帧：非 0x00 开头原样返回（含中间的 NUL 也算数据）', () => {
  const raw = new Uint8Array([104, 0, 105]) // "h\0i"
  const { ctrl, data } = decodeFrame(raw)
  assert.equal(ctrl, undefined)
  assert.deepEqual(data, raw)
})

test('畸形控制帧按数据帧处理（不判死连接）', () => {
  const cases = [
    new Uint8Array([0]), // 只有控制字节
    new Uint8Array([0, 123]), // "{"（JSON 截断）
    new Uint8Array([0, 120, 121]), // "xy"（不是 JSON）
    enc.encode('\0{"t":""}'), // t 为空
  ]
  for (const c of cases) {
    const { ctrl, data } = decodeFrame(c)
    assert.equal(ctrl, undefined)
    assert.deepEqual(data, c)
  }
})

test('超长输出不被 JSON 解析（只认首字节）', () => {
  const big = new Uint8Array(256 << 10).fill(97)
  const { ctrl, data } = decodeFrame(big)
  assert.equal(ctrl, undefined)
  assert.equal(data?.length, big.length)
})

test('UTF-8 多字节字符原样透传（终端输出会切在字符中间）', () => {
  const raw = enc.encode('中文日志')
  const { data } = decodeFrame(raw)
  assert.equal(dec.decode(data), '中文日志')
})

test('ArrayBuffer 与 Uint8Array 两种入参等价', () => {
  const frame = encodeCtrl({ t: 'exit', code: 3 })
  const ab = new ArrayBuffer(frame.byteLength)
  new Uint8Array(ab).set(frame)
  const fromBuf = decodeFrame(ab)
  const fromU8 = decodeFrame(frame)
  assert.deepEqual(fromBuf.ctrl, fromU8.ctrl)
})
