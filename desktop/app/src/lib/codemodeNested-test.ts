// codemodeNested-test.ts —— codemode 宿主渲染纯逻辑单测（node:test，纯 Node 无 DOM/Electron 依赖）。
//
// 为什么值得单测：这三条都是**回归性**极强的展示规则，且都有一条真实事故垫底
//（设计文档 §17 / §20.3）：
//   1. 折叠态只显示最后 8 条 + "…N earlier calls"（清单无限长会顶掉整屏对话）；
//   2. args 预览 200 字符 / 错误 500 字符（超限必须带省略标注）；
//   3. 输出预览按**视觉行**限行 —— pi 的 CHANGELOG.md:39：「一行 minified JSON 把折叠结果
//      撑满整屏」，修法就是「限折行后的行数，而不是逻辑行数」。按 \n 数行的实现会让
//      本文件的 `单行 100KB` 用例炸掉（变异验证见报告）。
//
// 运行：esbuild bundle 后 node --test（见 package.json 的 test:codemode-nested）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import {
  NESTED_ARGS_PREVIEW_CHARS,
  NESTED_COLLAPSED_LIMIT,
  NESTED_ERROR_PREVIEW_CHARS,
  OUTPUT_PREVIEW_LINES,
  charCells,
  hasScriptOutput,
  isCodemodeTool,
  oneLine,
  parseNestedCalls,
  pickNestedCalls,
  previewChars,
  previewVisualLines,
  spillPathFrom,
  textCells,
  visualLineCount,
  type NestedCallView,
} from './codemodeNested.ts'
// store 接线测试用：事件入口 + zustand store（node 下 getTransport() 惰性回落到 mock）
import { dispatchEvent, useAppStore } from '../store/useAppStore.ts'
import type { MsgBlock } from '../store/useAppStore.ts'

// 造 N 条嵌套记录（id/name 递增，便于断言「取的是最后 N 条」）。
function makeCalls(n: number, over: Partial<NestedCallView> = {}): NestedCallView[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `c${i}`,
    name: `tool_${i}`,
    args: '{}',
    result: '',
    isError: false,
    error: '',
    durMs: 1000 + i,
    ...over,
  }))
}

// —— 1. 折叠/展开选取 ————————————————————————————————————————————————

test('折叠态只显示最后 8 条，N = 被折叠的条数', () => {
  const calls = makeCalls(20)
  const folded = pickNestedCalls(calls, true)
  assert.equal(folded.visible.length, NESTED_COLLAPSED_LIMIT)
  assert.equal(folded.hidden, 12, '…N earlier calls 的 N 必须是**被藏起来的条数**')
  assert.deepEqual(
    folded.visible.map((c) => c.id),
    ['c12', 'c13', 'c14', 'c15', 'c16', 'c17', 'c18', 'c19'],
    '折叠掉的必须是**较早**的调用（最后几条才看得出脚本最终做了什么）',
  )
})

test('展开态显示全部（hidden 归零，不出现「…N earlier calls」）', () => {
  const calls = makeCalls(20)
  const all = pickNestedCalls(calls, false)
  assert.equal(all.visible.length, 20)
  assert.equal(all.hidden, 0)
})

test('条数不超过折叠上限时折叠态也全显示', () => {
  const calls = makeCalls(NESTED_COLLAPSED_LIMIT)
  const folded = pickNestedCalls(calls, true)
  assert.equal(folded.visible.length, NESTED_COLLAPSED_LIMIT)
  assert.equal(folded.hidden, 0)
  assert.equal(pickNestedCalls([], true).visible.length, 0)
})

// —— 2. args / 错误预览截断 ————————————————————————————————————————

test('args 预览超 200 字符即截断并带省略标注', () => {
  const long = 'a'.repeat(NESTED_ARGS_PREVIEW_CHARS + 50)
  const p = previewChars(long, NESTED_ARGS_PREVIEW_CHARS)
  assert.equal(p.truncated, true)
  assert.equal(p.text.length, NESTED_ARGS_PREVIEW_CHARS + 1, '200 字符 + 一个省略号')
  assert.ok(p.text.endsWith('…'))
  const short = previewChars('a'.repeat(NESTED_ARGS_PREVIEW_CHARS), NESTED_ARGS_PREVIEW_CHARS)
  assert.equal(short.truncated, false)
  assert.equal(short.text.length, NESTED_ARGS_PREVIEW_CHARS)
})

test('错误预览按 500 字符截断', () => {
  const e = previewChars('x'.repeat(900), NESTED_ERROR_PREVIEW_CHARS)
  assert.equal(e.truncated, true)
  assert.equal(e.text.length, NESTED_ERROR_PREVIEW_CHARS + 1)
})

test('args 单行化：换行/连续空白压成一个空格（工具行的一行摘要不能自己折行）', () => {
  assert.equal(oneLine('{\n  "path":\t"/a",\n\n  "n": 1\n}'), '{ "path": "/a", "n": 1 }')
  assert.equal(oneLine('   '), '')
})

test('截断不劈开代理对（否则预览尾部是半个字符的乱码）', () => {
  const p = previewChars('😀'.repeat(10), 3)
  assert.equal(p.text, '😀…', '第 3 个 UTF-16 单元是代理对的一半 → 退一格')
})

// —— 3. 视觉行限行（§20.3「单行输出不炸屏」） ————————————————————————

test('单行 100KB 的 JSON：折叠态预览必须限在 5 个视觉行内', () => {
  const width = 100 // 每行 100 个字符单元格
  const json = '[' + '{"a":1234567890,"b":"xxxxxxxx"},'.repeat(3500) + ']'
  assert.ok(json.length > 100_000, `用例前提：>= 100KB（实际 ${json.length}）`)
  const p = previewVisualLines(json, { lines: OUTPUT_PREVIEW_LINES, width })
  assert.equal(p.truncated, true)
  assert.equal(p.visualLines, OUTPUT_PREVIEW_LINES)
  assert.ok(p.text.length <= OUTPUT_PREVIEW_LINES * width, '预览文本不得超过 5 行宽度')
  assert.equal(p.text.includes('\n'), false, '原文没有换行 → 预览也不该凭空多出换行')
  assert.ok(p.hiddenVisualLines > 1000, '必须如实报出被折叠的视觉行数（供 UI 明示）')
})

test('视觉行 ≠ 逻辑行：长逻辑行按宽度折行后再数', () => {
  // 1 个逻辑行、300 个单元格、每行 100 → 3 个视觉行
  assert.equal(visualLineCount('a'.repeat(300), 100), 3)
  // 3 个逻辑行、每行都短 → 3 个视觉行
  assert.equal(visualLineCount('a\nb\nc', 100), 3)
  // 空行也占 1 行；末尾换行只是行终止符（'\n' 结尾的输出是 N 行内容）
  assert.equal(visualLineCount('a\n\nb', 100), 3)
  assert.equal(visualLineCount('a\n', 100), 1)
  assert.equal(visualLineCount('', 100), 0)
})

test('按 \n 数行的实现必须失败：3 个逻辑行里有 2 行超宽 → 折叠态只显示前 5 个视觉行', () => {
  const width = 10
  // 逻辑行：l0(25 格→3 行) / l1(25 格→3 行) / l2(短→1 行) = 7 个视觉行
  const text = 'a'.repeat(25) + '\n' + 'b'.repeat(25) + '\n' + 'tail'
  const p = previewVisualLines(text, { lines: OUTPUT_PREVIEW_LINES, width })
  assert.equal(p.truncated, true)
  assert.equal(p.visualLines, OUTPUT_PREVIEW_LINES)
  // 第 3 个逻辑行放不下（3+3=6 > 5）→ 在第 2 个逻辑行里折断
  assert.equal(p.text, 'a'.repeat(25) + '\n' + 'b'.repeat(20))
  assert.equal(p.hiddenVisualLines, 7 - OUTPUT_PREVIEW_LINES, '7 个视觉行只显示了 5 个')
})

test('内容没超预算时原样返回（不截断、不加省略号）', () => {
  const p = previewVisualLines('line1\nline2', { lines: OUTPUT_PREVIEW_LINES, width: 80 })
  assert.equal(p.text, 'line1\nline2')
  assert.equal(p.truncated, false)
  assert.equal(p.hiddenVisualLines, 0)
  assert.equal(p.visualLines, 2)
})

test('中日韩宽字符按 2 个单元格算（否则一行中文会被低估一半、多显示一倍内容）', () => {
  assert.equal(charCells('中'.codePointAt(0)!), 2)
  assert.equal(charCells('a'.codePointAt(0)!), 1)
  assert.equal(textCells('中文abc'), 7)
  // 20 个汉字 = 40 格 → 每行 10 格时是 4 个视觉行；按字符个数算会错判成 2 行
  assert.equal(visualLineCount('中'.repeat(20), 10), 4)
  const p = previewVisualLines('中'.repeat(20), { lines: 2, width: 10 })
  assert.equal(p.text, '中'.repeat(10), '2 行 × 每行 5 个汉字')
  assert.equal(p.visualLines, 2)
})

// —— 4. 事件字段 → 视图对象 ————————————————————————————————————————

test('parseNestedCalls：纳秒 → 毫秒，缺 name 的记录丢弃', () => {
  const v = parseNestedCalls([
    { id: 'c1', name: 'read_file', args: '{"path":"/a"}', result: 'ok', is_error: false, duration: 1_500_000 },
    { id: 'c2', name: 'bash', is_error: true, error: 'boom', duration: 2_000_000_000 },
    { id: 'c3' }, // 无名 → 丢弃
    null, // 脏数据 → 丢弃
  ])
  assert.ok(v)
  assert.equal(v.length, 2)
  assert.equal(v[0].durMs, 1.5)
  assert.equal(v[1].isError, true)
  assert.equal(v[1].error, 'boom')
  assert.equal(v[1].durMs, 2000, 'Go 的 time.Duration 序列化成纳秒整数')
  assert.equal(parseNestedCalls([]), undefined)
  assert.equal(parseNestedCalls(undefined), undefined)
  assert.equal(parseNestedCalls([{ name: 'x' }])?.length, 1)
})

// —— 5. 输出预览的显示判据 + spill 路径 ————————————————————————————

test('hasScriptOutput：编排型工具的输出才进折叠预览', () => {
  assert.equal(hasScriptOutput({ name: 'codemode', result: 'x' }), true)
  assert.equal(hasScriptOutput({ name: 'codemode', result: '' }), false, '无结果不预览')
  assert.equal(hasScriptOutput({ name: 'bash', result: 'x' }), false, '普通工具行为不变（折叠态不显示结果）')
  assert.equal(hasScriptOutput({ name: 'bash', result: 'x', spillPath: '/tmp/s.txt' }), true, '带 spill 路径算脚本输出')
  assert.equal(hasScriptOutput({ name: 'bash', result: 'x', nestedCalls: makeCalls(2) }), true, '带嵌套清单算脚本输出')
})

test('isCodemodeTool：含 codemode 即认（工具名还在 Wave 2，写死精确名会静默失效）', () => {
  assert.equal(isCodemodeTool('codemode'), true)
  assert.equal(isCodemodeTool('CodeMode'), true)
  assert.equal(isCodemodeTool('codemode_run'), true)
  assert.equal(isCodemodeTool('bash'), false)
})

test('spillPathFrom：pi 文案 / 本仓 slim 文案两种写法都认得，缺失时为 ""', () => {
  assert.equal(
    spillPathFrom('[Full output: /tmp/pi-codemode-abc.txt (read with offset/limit)]'),
    '/tmp/pi-codemode-abc.txt',
  )
  assert.equal(
    spillPathFrom('[完整输出已写 spill-run1-2.txt，以下为前 2048 字节预览]'),
    'spill-run1-2.txt',
  )
  // 结构化通道优先（bash 的 full_output_path / 未来的 codemode 结构化结果）
  assert.equal(spillPathFrom('无关文本', { full_output_path: '/tmp/full.txt' }), '/tmp/full.txt')
  assert.equal(spillPathFrom('无关文本', { spill_path: '/tmp/full.txt' }), '/tmp/full.txt')
  // 没有就**不显示**（绝不编造路径）
  assert.equal(spillPathFrom('{ ok: true }'), '')
  assert.equal(spillPathFrom('Full output: 见上文'), '', '不像路径的整串必须丢弃')
  assert.equal(spillPathFrom(''), '')
})

// —— 6. store 接线：事件字段 → 工具块字段（真实 dispatchEvent，不是模拟）————————————
//
// 这一段起的是「契约测试」的作用：引擎现在**还不发**嵌套工具事件（IMPLEMENTATION-SPEC
// §6.2.3：Phase 2 才接 ExecuteOne 的事件转发），所以行为契约只能靠构造事件来钉住 ——
// 将来引擎一开始发，宿主必须已经是「不建独立工具行 + 清单挂外层」的样子。
//
// 为什么能直接 import store：getTransport() 是惰性的（node 下无 window.desktop → mock），
// 事件入口 dispatchEvent 与 zustand store 都是纯 JS。构建侧的代价是 store 的 import 图里有
// vite 资源后缀（?url / ?no-inline / ?raw），esbuild 需要几个 loader（见 package.json）。
type ToolBlock = Extract<MsgBlock, { kind: 'tool' }>

function toolBlocks(sid: string): ToolBlock[] {
  const v = useAppStore.getState().views[sid]
  return (v?.blocks ?? []).filter((b): b is ToolBlock => b.kind === 'tool')
}

function view(sid: string) {
  const v = useAppStore.getState().views[sid]
  if (!v) throw new Error(`view ${sid} 不存在`)
  return v
}

test('store：带 parent_call_id 的 tool_start 不生成独立工具行（不建块 / 不建链路节点）', () => {
  const sid = 's-cm-start'
  dispatchEvent({ event_type: 'tool_start', session_id: sid, run_id: 'r1', id: 'outer', name: 'codemode', arguments: '{"code":"…"}' })
  const tsBefore = view(sid).toolStartTs
  dispatchEvent({ event_type: 'tool_start', session_id: sid, run_id: 'r1', id: 'nested', name: 'bash', arguments: '{"command":"ls"}', parent_call_id: 'outer', depth: 1 })

  const tools = toolBlocks(sid)
  assert.equal(tools.length, 1, '嵌套子调用不得生成独立工具行')
  assert.equal(tools[0].toolId, 'outer')
  const nodes = view(sid).runNodes.filter((n) => n.id === 'tool-nested')
  assert.equal(nodes.length, 0, '嵌套子调用不得生成链路节点')
  assert.equal(view(sid).toolStartTs['nested'], undefined, '嵌套子调用不得留下开始时刻（否则会算出一条没有行的耗时）')
  assert.deepEqual(view(sid).toolStartTs, tsBefore, '其它调用的开始时刻不受影响')
})

test('store：带 parent_call_id 的 tool_response 不改工具行，但会话级事实（diff/待办）照常入档', () => {
  const sid = 's-cm-end'
  dispatchEvent({ event_type: 'tool_start', session_id: sid, run_id: 'r1', id: 'outer2', name: 'codemode', arguments: '{"code":"…"}' })
  dispatchEvent({
    event_type: 'tool_response', session_id: sid, id: 'outer2', name: 'codemode', result: 'script done',
    nested_calls: [{ id: 'n1', name: 'read_file', args: '{}', result: 'ok', duration: 250_000 }],
  })
  // 脚本内的子调用收尾：带 parent + 一次真实文件变更 + 待办快照
  dispatchEvent({
    event_type: 'tool_response', session_id: sid, id: 'nested2', name: 'write_file', parent_call_id: 'outer2',
    result: 'written', diff: { path: 'a.txt', added: 1, removed: 0 },
    todos: [{ id: 't1', title: '脚本改的东西', done: true }],
  })

  const tools = toolBlocks(sid)
  assert.equal(tools.length, 1, '子调用的结果不得变成第二行')
  assert.equal(tools[0].result, 'script done', '外层结果不被子调用覆盖')
  assert.equal(view(sid).diffs.length, 1, '子调用的 diff 是真实发生的文件变更：必须入档（否则 Git 面板是错的）')
  assert.equal(view(sid).diffs[0].path, 'a.txt')
  assert.equal(view(sid).todos.length, 1, '子调用改过的待办快照照常入档')
})

test('store：codemode 的 nested_calls 落到工具块上（纳秒 → 毫秒），spill 路径一并提取', () => {
  const sid = 's-cm-fields'
  dispatchEvent({ event_type: 'tool_start', session_id: sid, id: 'outer3', name: 'codemode', arguments: '{"code":"…"}' })
  dispatchEvent({
    event_type: 'tool_response', session_id: sid, id: 'outer3', name: 'codemode',
    result: '{"ok":true}\n[完整输出已写 /tmp/hai-codemode-1.txt，以下为前 2048 字节预览]',
    nested_calls: [
      { id: 'n1', name: 'read_file', args: '{"path":"/a"}', result: 'ok', is_error: false, duration: 1_500_000 },
      { id: 'n2', name: 'bash', is_error: true, error: 'boom', duration: 2_000_000_000 },
    ],
  })

  const b = toolBlocks(sid)[0]
  assert.ok(b.nestedCalls, '工具块必须带上嵌套清单（渲染层的数据来源）')
  assert.equal(b.nestedCalls.length, 2)
  assert.equal(b.nestedCalls[0].durMs, 1.5)
  assert.equal(b.nestedCalls[1].isError, true)
  assert.equal(b.nestedCalls[1].error, 'boom')
  assert.equal(b.nestedCalls[1].durMs, 2000)
  assert.equal(b.spillPath, '/tmp/hai-codemode-1.txt')
})

test('store：普通工具（无 nested_calls / 非 codemode）两个字段都保持 undefined（零影响）', () => {
  const sid = 's-plain'
  dispatchEvent({ event_type: 'tool_start', session_id: sid, id: 'b1', name: 'bash', arguments: '{"command":"ls"}' })
  dispatchEvent({ event_type: 'tool_response', session_id: sid, id: 'b1', name: 'bash', result: 'a.txt' })
  const b = toolBlocks(sid)[0]
  assert.equal(b.nestedCalls, undefined)
  assert.equal(b.spillPath, undefined)
  assert.equal(hasScriptOutput(b), false, '普通工具的折叠态行为不变（不显示结果预览）')
})

test('store：重复投递同一条响应是幂等的（清单被覆盖而不是追加）', () => {
  const sid = 's-cm-dup'
  const resp = {
    event_type: 'tool_response', session_id: sid, id: 'outer4', name: 'codemode', result: 'done',
    nested_calls: [{ id: 'n1', name: 'bash', args: '{}', duration: 1_000_000 }],
  }
  dispatchEvent({ event_type: 'tool_start', session_id: sid, id: 'outer4', name: 'codemode', arguments: '{}' })
  dispatchEvent(resp)
  dispatchEvent(resp) // 重连补发 / 日志重放尾段
  const tools = toolBlocks(sid)
  assert.equal(tools.length, 1, '重复响应不得新增行')
  assert.equal(tools[0].nestedCalls?.length, 1, '重复响应不得让清单翻倍')
})

// —— 7. CSS 契约：折叠预览的折行模型必须与上面的 JS 预算一致 ————————————————
//
// 为什么值得一条「读 CSS 的测试」：这里有一个**浏览器实测**出来的坑 ——
// .tn-pre 若用 break-word（.tr-code 的写法），浏览器会优先在连字符/单词边界断行，
// 同样的宽度下真实折行数会**多于** JS 算出的预算，多出来的尾行被 max-height 悄悄裁掉
// （实测：5 行预算渲染成 6 行，'short' 那行看不见也没有任何提示）。改成 break-all 后
// 浏览器与 JS 是同一套「逐字符贪心填满一行」模型，实测 5 行 = 5 行、无裁剪。
// max-height 是第二道保险（测量失败时也不炸屏），必须在。
test('CSS 契约：.tn-pre 用 break-all（与 JS 逐字符折行模型一致）且有 max-height 兜底', () => {
  // bundle 产物落在 /tmp（见 package.json 的 outfile），所以路径只能按 cwd 取 ——
  // 测试脚本由 npm run（cwd = desktop/app）执行；换目录跑会在这里给出明确提示。
  const cssPath = resolve(process.cwd(), 'src/index.css')
  assert.ok(existsSync(cssPath), `找不到 ${cssPath}：请用 npm run test:codemode-nested（cwd = desktop/app）运行`)
  const css = readFileSync(cssPath, 'utf8')
  const at = css.indexOf('.tool-row .tn-pre {')
  assert.ok(at > 0, 'index.css 里必须有 .tool-row .tn-pre 规则')
  const block = css.slice(at, css.indexOf('}', at))
  assert.match(block, /word-break:\s*break-all/, 'break-word 会让真实折行多于预算 → 尾行被悄悄裁掉')
  assert.match(block, /max-height:\s*calc\(/, 'max-height 是「单行输出不炸屏」的第二道保险')
})

test('制表符按制表位（8 格）算宽度：按 1 格算会低估折行数 → 尾行被静默裁掉', () => {
  assert.equal(textCells('a\tb'), 9, 'a 占 1 格，\\t 补齐到 8，b 落在第 9 格')
  assert.equal(textCells('\t'), 8)
  assert.equal(textCells('12345678\t'), 16, '已在边界上 → 跳到下一个制表位')
  // 12 个制表符 = 96 格 → 每行 8 格 = 12 个视觉行（按 1 格算只会得到 2 行）
  assert.equal(visualLineCount('\t'.repeat(12), 8), 12)
  // 制表符行超预算时按制表位切（不劈开制表符本身）
  const p = previewVisualLines('ab\tcd', { lines: 1, width: 8 })
  assert.equal(p.visualLines, 1)
  assert.equal(p.truncated, true)
})
