// inputHistory-test.ts —— 输入框历史（↑/↓ 回溯、100 条上限、Command/文件/图片快照）的纯单测。
//
// 钉住四条容易看起来"没问题"的判定：
//   ① 未进入历史模式时，↑ 只在**光标位于开头**才取历史（否则多行正文无法移动光标）；
//   ② 历史导航的边界：到最旧不越界，↓ 越过最新必须回到进入前的草稿（草稿由调用方保存）；
//   ③ 每个作用域最多 100 条，重复输入移到最新而不是占新槽位；
//   ④ 条目是**输入快照**：/命令、文档引用 chips、图片指纹各自保留，且图片字节不落 localStorage。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  INPUT_HISTORY_ENTRY_CHARS,
  INPUT_HISTORY_MAX,
  InputHistoryStore,
  appendInputHistory,
  findHistoryImages,
  historyDirection,
  historyItem,
  imageFingerprints,
  imageKey,
  inputHistoryScope,
  itemKey,
  mergeInputHistories,
  parseInputHistory,
  pruneInputHistory,
  recallNewer,
  recallOlder,
  type InputHistoryItem,
  type InputHistoryStorage,
} from './inputHistory'

function memStorage(initial: Record<string, string> = {}): InputHistoryStorage & { data: Record<string, string> } {
  const data = { ...initial }
  return {
    data,
    getItem: (k) => (k in data ? data[k] : null),
    setItem: (k, v) => { data[k] = v },
  }
}

const textItem = (text: string): InputHistoryItem => ({ text })
const image = (dataUrl: string, mime = 'image/png') => ({ type: 'image', content: dataUrl, mimeType: mime })

test('作用域：主会话 / 子智能体 / 无会话工作区分开，且共享可回收前缀', () => {
  const ws = '/work/demo'
  const main = inputHistoryScope(ws, 's1')
  const other = inputHistoryScope(ws, 's2')
  const agent = inputHistoryScope(ws, 's1', 'task-9')
  const bare = inputHistoryScope(ws)
  assert.notEqual(main, other, '不同会话不共用一个历史')
  assert.notEqual(main, agent, '子智能体输入不并进主会话')
  assert.ok(agent.startsWith(`${main}\u0000`), '子智能体作用域挂在会话之下（可随会话一起删）')
  assert.ok(main.startsWith(bare), '会话作用域挂在工作区之下')
  assert.notEqual(main, bare, '无会话作用域是单独一条')
})

test('追加：去重移到最后、100 条上限、空输入不记', () => {
  let items: InputHistoryItem[] = []
  for (let i = 1; i <= INPUT_HISTORY_MAX + 5; i++) items = appendInputHistory(items, textItem(`msg-${i}`))
  assert.equal(items.length, INPUT_HISTORY_MAX)
  assert.equal(items[0].text, 'msg-6', '超限时丢最旧的')
  assert.equal(items.at(-1)?.text, `msg-${INPUT_HISTORY_MAX + 5}`)

  items = appendInputHistory(items, textItem('msg-50'))
  assert.equal(items.at(-1)?.text, 'msg-50', '重复输入移到最新')
  assert.equal(items.filter((x) => x.text === 'msg-50').length, 1, '旧位置不残留')

  assert.deepEqual(appendInputHistory([textItem('a')], { text: '   ' }), [textItem('a')], '纯空白不记')
  assert.deepEqual(appendInputHistory([], { text: '  x  ' }), [textItem('x')], '存储时 trim')
})

test('超大条目：拒绝入库，不驱逐已有历史', () => {
  const huge = 'x'.repeat(INPUT_HISTORY_ENTRY_CHARS + 1)
  const items = appendInputHistory([textItem('保留')], textItem(huge))
  assert.deepEqual(items, [textItem('保留')], '超大条目直接丢弃；旧历史不受影响')
})

test('条目快照：文本、文档引用、图片指纹同时保留；同文不同附件是两条', () => {
  const ref = { path: '/w/a.md', name: 'a.md', size: 12 }
  const img1 = image('data:image/png;base64,AAAA')
  const img2 = image('data:image/png;base64,BBBB')
  const a: InputHistoryItem = { text: '看下这个', refs: [ref], images: imageFingerprints([img1]) }
  const b: InputHistoryItem = { text: '看下这个', refs: [ref], images: imageFingerprints([img2]) }

  let items = appendInputHistory([], a)
  items = appendInputHistory(items, b)
  assert.equal(items.length, 2, '文本相同但图片不同 → 两条')
  assert.notEqual(itemKey(a), itemKey(b))

  const again = appendInputHistory(items, a)
  assert.equal(again.length, 2, '完全相同的输入 → 去重')
  assert.equal(again.at(-1)?.text, '看下这个')
  assert.deepEqual(again.at(-1)?.images, imageFingerprints([img1]), '附件指纹随条目保留')
})

test('图片指纹：按图片块提取、忽略文本块；字节不写进持久化条目', () => {
  const huge = `data:image/jpeg;base64,${'Z'.repeat(30_000)}`
  const fps = imageFingerprints([
    { type: 'text', content: '正文' },
    image(huge, 'image/jpeg'),
  ])
  assert.equal(fps.length, 1)
  assert.equal(fps[0].mime, 'image/jpeg')
  assert.equal(fps[0].len, huge.length)
  assert.ok(fps[0].hash.length > 0)

  const kv = memStorage()
  const store = new InputHistoryStore(kv)
  store.add('s', { text: '图', images: fps })
  const raw = kv.data['go-code.inputHistory']
  assert.ok(raw.length < 1000, `持久化只留指纹，实际 ${raw.length} 字符`)
  assert.ok(!raw.includes('ZZZZ'), '图片 data URL 不落 localStorage')
})

test('解析：坏 JSON / 未知版本 / 非法条目 → 空或丢弃，不抛；v1 纯文本自动迁移', () => {
  assert.deepEqual(parseInputHistory('{oops'), { v: 2, scopes: {} })
  assert.deepEqual(parseInputHistory(JSON.stringify({ v: 99, scopes: { a: { t: 1, items: ['x'] } } })), { v: 2, scopes: {} })
  assert.deepEqual(parseInputHistory('null'), { v: 2, scopes: {} })

  const legacy = parseInputHistory(JSON.stringify({ v: 1, scopes: { a: { t: 5, items: ['旧消息'] } } }))
  assert.deepEqual(legacy.scopes.a.items, [{ text: '旧消息' }], 'v1 string[] 迁移成 v2 条目')

  const ok = parseInputHistory(JSON.stringify({
    v: 2,
    scopes: {
      a: {
        t: 5,
        items: [
          { text: ' x ', refs: [{ path: '/w/a', name: 'a', size: 3 }], images: [{ mime: 'image/png', len: 4, hash: 'h' }] },
          { text: '   ' },
          42,
        ],
      },
      empty: { t: 1, items: [] },
      bad: 'nope',
    },
  }))
  assert.deepEqual(ok.scopes.a.items, [{
    text: 'x',
    refs: [{ path: '/w/a', name: 'a', size: 3 }],
    images: [{ mime: 'image/png', len: 4, hash: 'h' }],
  }], '合法条目保留，空条目/非法条目丢弃')
  assert.equal(ok.scopes.empty, undefined, '空作用域不保留')
  assert.equal(ok.scopes.bad, undefined)
})

test('裁剪：作用域数量按最近使用淘汰，结果不超上限', () => {
  const scopes: Record<string, { t: number; items: InputHistoryItem[] }> = {}
  for (let i = 0; i < 50; i++) scopes[`s${i}`] = { t: i, items: [textItem(`msg-${i}`)] }
  const pruned = pruneInputHistory({ v: 2, scopes }, 40)
  assert.equal(Object.keys(pruned.scopes).length, 40)
  assert.equal(pruned.scopes.s0, undefined, '最久未用的被淘汰')
  assert.ok(pruned.scopes.s49, '最近用过的保留')
})

test('仓库：跨实例读回（持久化）、同文去重、会话隔离、完整快照往返', () => {
  const kv = memStorage()
  const a = new InputHistoryStore(kv)
  const ref = { path: '/w/a.md', name: 'a.md', size: 12 }
  const fps = imageFingerprints([image('data:image/png;base64,AAAA')])
  a.add('s1', { text: '/summary 帮我压缩', refs: [ref], images: fps }, 1000)
  a.add('s1', { text: '第二条' }, 1001)
  a.add('s1', { text: '/summary 帮我压缩', refs: [ref], images: fps }, 1002) // 重复 → 移到最新
  a.add('s2', { text: '另一个会话' }, 1003)

  const wantS1 = [
    { text: '第二条', at: 1001 },
    { text: '/summary 帮我压缩', at: 1002, refs: [ref], images: fps },
  ]
  assert.deepEqual(a.get('s1'), wantS1)
  assert.deepEqual(a.get('s2'), [{ text: '另一个会话', at: 1003 }])

  const b = new InputHistoryStore(kv) // 模拟重启：从同一份 localStorage 读
  assert.deepEqual(b.get('s1'), wantS1, 'Command/文件/图片指纹全部读回')
})

test('仓库：无会话历史并入新会话（不重复、保留顺序）', () => {
  const store = new InputHistoryStore(memStorage())
  const ws = '/w'
  store.add(inputHistoryScope(ws), { text: '首条消息' }, 2000)
  store.add(inputHistoryScope(ws, 's1'), { text: '会话里已有的' }, 1000)
  store.adopt(inputHistoryScope(ws), inputHistoryScope(ws, 's1'), 2001)
  assert.deepEqual(store.get(inputHistoryScope(ws, 's1')), [
    { text: '会话里已有的', at: 1000 },
    { text: '首条消息', at: 2000 },
  ])
  assert.deepEqual(store.get(inputHistoryScope(ws)), [], '并完即清，避免下次再灌一遍')
})

test('仓库：删会话只回收该会话（含它的子智能体），不误伤兄弟会话', () => {
  const store = new InputHistoryStore(memStorage())
  const ws = '/w'
  store.add(inputHistoryScope(ws, 's1'), { text: 'a' }, 3000)
  store.add(inputHistoryScope(ws, 's1', 'task-1'), { text: 'a-sub' }, 3001)
  store.add(inputHistoryScope(ws, 's2'), { text: 'b' }, 3002)
  store.add(inputHistoryScope(ws), { text: 'bare' }, 3003)

  store.forget(inputHistoryScope(ws, 's1'))
  assert.deepEqual(store.get(inputHistoryScope(ws, 's1')), [])
  assert.deepEqual(store.get(inputHistoryScope(ws, 's1', 'task-1')), [])
  assert.deepEqual(store.get(inputHistoryScope(ws, 's2')), [{ text: 'b', at: 3002 }], '兄弟会话不受影响')
  assert.deepEqual(store.get(inputHistoryScope(ws)), [{ text: 'bare', at: 3003 }], '工作区作用域不受影响')

  store.forgetWorkspace(ws)
  assert.deepEqual(store.get(inputHistoryScope(ws, 's2')), [], '删工作区回收全部')
  assert.deepEqual(store.get(inputHistoryScope(ws)), [], '无会话历史一并回收')
})

test('仓库：存储不可用 / 写入抛错时不崩（历史非关键数据）', () => {
  const broken: InputHistoryStorage = {
    getItem: () => { throw new Error('denied') },
    setItem: () => { throw new Error('quota') },
  }
  const store = new InputHistoryStore(broken)
  assert.deepEqual(store.get('s'), [])
  assert.doesNotThrow(() => store.add('s', { text: 'hello' }))
  assert.deepEqual(store.get('s'), [], '落盘失败时读回空，但进程不崩')

  const offline = new InputHistoryStore(null)
  assert.doesNotThrow(() => offline.add('s', { text: 'hello' }))
  assert.deepEqual(offline.get('s'), [])
})

test('合并：文件与本地镜像按内容去重、按 at 排序，旧数据不会被新记录压掉', () => {
  const remote = [{ text: '文件里的', at: 2000 }]
  const local = [{ text: '本地更早的', at: 1000 }, { text: '文件里的', at: 2500 }, { text: '本地新发的', at: 3000 }]
  const merged = mergeInputHistories(remote, local)
  assert.deepEqual(merged, [
    { text: '本地更早的', at: 1000 },
    { text: '文件里的', at: 2500 }, // 同一内容取 at 较新的
    { text: '本地新发的', at: 3000 },
  ])
  // 缺 at 的旧数据按 0 处理：排在最前，不会挤掉新记录
  const legacy = mergeInputHistories([], [{ text: '旧数据' }, { text: '新数据', at: 4000 }])
  assert.deepEqual(legacy, [{ text: '旧数据' }, { text: '新数据', at: 4000 }])
})

test('导航：↑ 从最新开始，到最旧不越界；↓ 越过最新 → null（调用方恢复草稿）', () => {
  const history = [textItem('一'), textItem('二'), { text: '三', images: imageFingerprints([image('data:image/png;base64,AAAA')]) }]
  const n1 = recallOlder(null, history)
  assert.ok(n1)
  assert.equal(historyItem(n1).text, '三', '第一次 ↑ 取最新一条')

  const n2 = recallOlder(n1, history)
  assert.equal(historyItem(n2!).text, '二')
  const n3 = recallOlder(n2, history)
  assert.equal(historyItem(n3!).text, '一')
  const n4 = recallOlder(n3, history)
  assert.equal(n4, n3, '到最旧后不再越界（返回同一个状态）')

  const back = recallNewer(n3)
  assert.equal(historyItem(back!).text, '二')
  const newest = recallNewer(back)
  assert.equal(historyItem(newest!).text, '三')
  assert.equal(recallNewer(newest), null, '越过最新 → null')
})

test('导航：没有历史时 ↑ 什么都不做', () => {
  assert.equal(recallOlder(null, []), null)
  assert.equal(recallNewer(null), null)
})

test('键盘判定：未进入历史时 ↑ 只在光标位于开头才取历史', () => {
  const noMods = {}
  assert.equal(historyDirection('ArrowUp', noMods, false, 0, 0, false), 'older')
  assert.equal(historyDirection('ArrowUp', noMods, false, 3, 3, false), null, '光标在中间 → 原生移动')
  assert.equal(historyDirection('ArrowUp', noMods, false, 1, 4, false), null, '有选区 → 原生')
  assert.equal(historyDirection('ArrowDown', noMods, false, 0, 0, false), null, '未进入历史时 ↓ 不取历史')
})

test('键盘判定：组合键 / 输入法组合态一律不碰', () => {
  assert.equal(historyDirection('ArrowUp', { shiftKey: true }, false, 0, 0, false), null, 'Shift+↑ 是选区')
  assert.equal(historyDirection('ArrowUp', { ctrlKey: true }, false, 0, 0, false), null)
  assert.equal(historyDirection('ArrowUp', { metaKey: true }, false, 0, 0, false), null)
  assert.equal(historyDirection('ArrowUp', { altKey: true }, false, 0, 0, false), null)
  assert.equal(historyDirection('ArrowUp', {}, true, 0, 0, true), null, 'IME 组合态')
  assert.equal(historyDirection('ArrowUp', {}, false, 0, 0, true), 'older', '已进入历史后光标位置不再限制 ↑')
  assert.equal(historyDirection('ArrowDown', {}, false, 9, 9, true), 'newer', '已进入历史后 ↓ 也是选择')
})

test('图片指纹键：同一张图稳定，不同图不同', () => {
  const a = imageFingerprints([image('data:image/png;base64,AAAA')])
  const b = imageFingerprints([image('data:image/png;base64,AAAA')])
  const c = imageFingerprints([image('data:image/png;base64,BBBB')])
  assert.equal(imageKey(a), imageKey(b))
  assert.notEqual(imageKey(a), imageKey(c))
})

test('图片找回：按指纹从候选内容块（转录 / 暂存队列）定位原图', () => {
  const red = image('data:image/png;base64,AAAA')
  const blue = image('data:image/png;base64,BBBB')
  const candidates = [
    [{ type: 'text', content: '别的消息' }],
    [{ type: 'text', content: '正文' }, red],
    [blue],
  ]
  assert.deepEqual(findHistoryImages(imageFingerprints([red]), candidates), [red], '跳过文本块，命中含图消息')
  assert.deepEqual(findHistoryImages(imageFingerprints([blue]), candidates), [blue], '不同候选也能命中')
  assert.equal(findHistoryImages(imageFingerprints([image('data:image/png;base64,CCCC')]), candidates), undefined, '找不到返回 undefined')
})
