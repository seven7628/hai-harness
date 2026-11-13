// renderer-send-test.ts —— 主进程 → 渲染进程统一发送通道的单测（node:test，不启动 Electron）。
//
// 钉住 BR-04 的两件事：
//   ① 窗口为 null / 窗口已销毁 / webContents 已销毁 / 发送瞬间被销毁 → **不抛**，只丢弃；
//      抛出去会从 readline 的 'line' 监听、16ms flush 定时器、子进程回调里冒成主进程
//      uncaughtException（现场表现：bridge 有输出但界面不动）。
//   ② 主进程里**没有**绕过这一层的裸 webContents.send（防止只修报错那一行、其余 5 处复发）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { createRendererSender, type SendableWindow } from './renderer-send'

// 源码守卫要读 .ts 源文件本身（bundle 到 /tmp 后 import.meta.url 不可用）：
// npm 脚本在 desktop/app 下运行，故 cwd 即基准；找不到就明确失败而不是静默跳过。
function sourcePath(file: string): string {
  const p = join(process.cwd(), 'electron', file)
  if (!existsSync(p)) throw new Error(`源码守卫找不到 ${p}（应在 desktop/app 下运行本测试）`)
  return p
}

/** 假窗口：记录送出的 (channel, args)；destroyed 可切换。 */
function fakeWindow() {
  const sent: Array<{ channel: string; args: unknown[] }> = []
  const wc = {
    destroyed: false,
    throwOnSend: false,
    isDestroyed() {
      return this.destroyed
    },
    send(channel: string, ...args: unknown[]) {
      if (this.throwOnSend) throw new Error('Object has been destroyed')
      sent.push({ channel, args })
    },
  }
  const win = {
    destroyed: false,
    webContents: wc,
    isDestroyed() {
      return this.destroyed
    },
  }
  return { win, wc, sent }
}

test('窗口正常：透传 channel 与载荷，sent 计数 +1', () => {
  const { win, sent } = fakeWindow()
  const s = createRendererSender(() => win)
  assert.equal(s.send('bridge:event', 'line-1'), true)
  assert.deepEqual(sent, [{ channel: 'bridge:event', args: ['line-1'] }])
  assert.deepEqual(s.stats(), { sent: 1, dropped: 0, failed: 0 })
})

test('载荷原样透传：对象 / 数组 / 字符串 / 多参数', () => {
  const { win, sent } = fakeWindow()
  const s = createRendererSender(() => win)
  const batch = { lines: ['a', 'b'] }
  s.send('bridge:event', batch)
  s.send('bridge:exit', { code: 1, err: 'boom' })
  s.send('hub:deep-link', 'hub://task/1')
  s.send('multi', 1, 'two', { three: 3 })
  assert.deepEqual(sent[0].args, [batch])
  assert.deepEqual(sent[1].args, [{ code: 1, err: 'boom' }])
  assert.deepEqual(sent[2].args, ['hub://task/1'])
  assert.deepEqual(sent[3].args, [1, 'two', { three: 3 }])
})

test('窗口为 null：丢弃，不抛', () => {
  const s = createRendererSender(() => null)
  assert.equal(s.send('bridge:event', 'x'), false)
  assert.deepEqual(s.stats(), { sent: 0, dropped: 1, failed: 0 })
  assert.equal(s.lastDropReason(), '窗口不存在')
})

test('窗口已销毁（关闭中）：丢弃，不抛', () => {
  const { win, sent } = fakeWindow()
  win.destroyed = true
  const s = createRendererSender(() => win)
  assert.equal(s.send('bridge:event', 'x'), false)
  assert.equal(sent.length, 0)
  assert.equal(s.lastDropReason(), '窗口已销毁')
})

test('webContents 已销毁（reload/导航窗口期）：丢弃，不抛', () => {
  const { win, wc, sent } = fakeWindow()
  wc.destroyed = true
  const s = createRendererSender(() => win)
  assert.equal(s.send('bridge:event', 'x'), false)
  assert.equal(sent.length, 0)
  assert.equal(s.lastDropReason(), 'webContents 已销毁')
})

test('webContents 缺失：丢弃，不抛', () => {
  const win = { webContents: null } as unknown as SendableWindow
  const s = createRendererSender(() => win)
  assert.equal(s.send('bridge:event', 'x'), false)
  assert.equal(s.lastDropReason(), 'webContents 不存在')
})

test('判空与发送之间被销毁（send 抛 Object has been destroyed）：计入 failed，不抛', () => {
  const { win, wc } = fakeWindow()
  wc.throwOnSend = true
  const s = createRendererSender(() => win)
  assert.equal(s.send('bridge:event', 'x'), false)
  assert.deepEqual(s.stats(), { sent: 0, dropped: 0, failed: 1 })
  assert.match(s.lastDropReason(), /Object has been destroyed/)
})

test('getter 抛错：按窗口不在处理，不冒泡', () => {
  const s = createRendererSender(() => {
    throw new Error('boom')
  })
  assert.equal(s.send('bridge:event', 'x'), false)
  assert.equal(s.stats().dropped, 1)
})

test('窗口重建后发到新窗口（getter 而不是捕获实例）', () => {
  let win: SendableWindow | null = null
  const s = createRendererSender(() => win)
  assert.equal(s.send('bridge:event', 'before'), false)
  const a = fakeWindow()
  win = a.win
  assert.equal(s.send('bridge:event', 'after'), true)
  assert.deepEqual(a.sent.map((x) => x.args[0]), ['after'])
})

test('readline 语义：窗口在事件流中途销毁，后续行照常处理且一个异常都不出', () => {
  // 复刻 main.ts 的 rl.on('line') 现场：早期行窗口还在，随后窗口被销毁（关闭/reload），
  // 但 bridge 仍在输出。旧实现（裸 send）在这一刻抛进监听 → 该行之后的事件全丢。
  const { win, wc } = fakeWindow()
  const s = createRendererSender(() => win)
  const lines = ['evt-1', 'evt-2', 'evt-3', 'evt-4']
  let handled = 0
  const onLine = (line: string) => {
    s.send('bridge:event', line) // 不判空、不包 try/catch —— 与修复后的调用点一致
    handled++
  }
  onLine(lines[0])
  wc.destroyed = true // 窗口销毁（webContents 先没，窗口对象还在）
  for (const line of lines.slice(1)) onLine(line)
  win.destroyed = true // 随后窗口也没了
  onLine('evt-5')
  assert.equal(handled, 5, '每一行都必须被处理完（不能因为窗口不在而中断事件流）')
  assert.equal(s.stats().sent, 1, '只有窗口活着时那一行送达')
  assert.equal(s.stats().dropped, 4)
})

/**
 * 源码守卫：主进程不得再有裸 webContents.send（除了通道实现本身）。
 *
 * 按「去注释 + 去空白」后的文本匹配，而不是逐行正则：`win.webContents\n  .send(...)`
 * 这类跨行写法与注释里的说明都不能骗过守卫（逐行版会同时误报注释、漏报跨行）。
 * 仍抓不到的情况（已知局限）：先把 webContents 取到别名再 send
 * （`const wc = win.webContents; wc.send(...)`）—— 静态文本分析看不到，只能靠 review；
 * 真要覆盖需要 AST 分析，超出本守卫的收益。
 */
test('主进程没有绕过统一通道的 webContents.send', () => {
  for (const file of ['main.ts', 'embed.ts']) {
    const src = readFileSync(sourcePath(file), 'utf8')
    const flat = src
      .replace(/\/\*[\s\S]*?\*\//g, '') // 块注释
      .replace(/(^|\n)\s*\/\/.*/g, '$1') // 行注释
      .replace(/\s+/g, '')
    const hits = flat.split('.webContents.send(').length - 1
    // 失败时给出逐行定位（只为可读性；判定用上面的 flattened 文本）
    const offenders = src
      .split('\n')
      .map((line, i) => ({ line: line.trim(), no: i + 1 }))
      .filter(({ line }) => /\.webContents\s*\.\s*$/.test(line) || /\.webContents\.send/.test(line))
      .map(({ line, no }) => `${file}:${no}: ${line}`)
    assert.equal(hits, 0, `这些调用点必须走 sendToRenderer：\n${offenders.join('\n')}`)
  }
})
