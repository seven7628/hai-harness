#!/usr/bin/env node
// 内联「任务等待你」卡的提问逻辑（N3）：多选/单选/预览的**口径**必须与
// AskUserQuestionPanel 一致 —— 多选 = 勾选项用「；」拼接，单选 = 原值。
// 纯逻辑，Node 直跑（无需浏览器/dev server）。组件层的行为在 check-hub-inline-needs.mjs。
import { build } from 'esbuild'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const __dirname = fileURLToPath(new URL('.', import.meta.url))
const hubDir = join(__dirname, '..', 'src', 'hub')
const result = await build({
  stdin: {
    contents: "export * as inline from './inlineNeedAnswers.ts'",
    resolveDir: hubDir,
    sourcefile: 'hub-inline-needs-entry.ts',
    loader: 'ts',
  },
  bundle: true,
  platform: 'node',
  format: 'esm',
  write: false,
  logLevel: 'silent',
})
const mod = await import(`data:text/javascript;base64,${Buffer.from(result.outputFiles[0].text).toString('base64')}`)
const { inline } = mod

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assert = (cond, m) => { if (!cond) throw new Error(`断言失败: ${m}`); ok(m) }
const eq = (got, want, m) => assert(got === want, `${m}（got=${JSON.stringify(got)} want=${JSON.stringify(want)}）`)

// —— 1) 解析：need.body 是 events.AskUserQuestion.Questions 的 JSON ——
{
  const qs = inline.parseInlineQuestions(JSON.stringify([
    { id: 'q1', question: '送谁看？', multi: true, options: [{ label: '客户' }, { label: '内部团队' }] },
    {
      id: 'q2', question: 'C 家没公开价，怎么写？',
      options: [{ label: '写「未公开」', preview: '价格列：未公开' }, { label: '用估算价', preview: '价格列：$45（估算）' }],
    },
  ]))
  eq(qs.length, 2, '解析出两题')
  eq(qs[0].multi, true, 'q1 是多选')
  eq(qs[0].options.length, 2, 'q1 选项数')
  eq(qs[1].multi, false, 'q2 默认单选')
  eq(qs[1].options[0].preview, '价格列：未公开', '选项 preview 保留')
  // 老格式/纯文本 → 空数组：调用方退回展示原始 body，不吞掉模型的提问。
  eq(inline.parseInlineQuestions('先看这两家，其余明天说').length, 0, '非 JSON 返回空')
  eq(inline.parseInlineQuestions('').length, 0, '空 body 返回空')
  eq(inline.parseInlineQuestions('{"a":1}').length, 0, '非数组返回空')
  // 纯字符串选项（模型最常见的写法）等价 {label}
  const strOpts = inline.parseInlineQuestions('[{"id":"q","question":"选","options":["A","B"]}]')
  eq(strOpts[0].options.map((o) => o.label).join(','), 'A,B', '字符串选项等价 label')
  // 缺 id 用下标兜底（回答要能对上题目）
  eq(inline.parseInlineQuestions('[{"question":"只有题干"}]')[0].id, '0', '缺 id 用下标')
}

// —— 2) 答案拼装：多选「；」拼接，单选原值（与 AskUserQuestionPanel.finalAnswer 同口径）——
{
  const [multi, single] = inline.parseInlineQuestions(JSON.stringify([
    { id: 'q1', question: '送谁看？', multi: true, options: [{ label: '客户' }, { label: '内部团队' }] },
    { id: 'q2', question: '怎么写？', options: [{ label: '写「未公开」' }, { label: '用估算价' }] },
  ]))
  eq(inline.composeAnswer(multi, ['客户', '内部团队'], ''), '客户；内部团队', '多选按“；”拼接')
  eq(inline.composeAnswer(multi, ['客户', '内部团队'], '先给老板看'), '客户；内部团队；先给老板看', '自由输入追加在多选末尾')
  eq(inline.composeAnswer(multi, [], '只打字'), '只打字', '多选只打字的场景')
  eq(inline.composeAnswer(multi, ['客户', '客户'], ''), '客户', '重复勾选去重')
  eq(inline.composeAnswer(single, [], '用估算价'), '用估算价', '单选原值')
  eq(inline.composeAnswer(single, ['客户'], ''), '', '单选不拿勾选集合当答案（这点是多选被做成单选的旧 bug 反面）')

  const answers = inline.composeAnswers([multi, single], { q1: ['客户'] }, { q2: '用估算价' })
  eq(JSON.stringify(answers), JSON.stringify({ q1: '客户', q2: '用估算价' }), '整张卡按 question_id 出答案')
  const skipped = inline.composeAnswers([multi, single], {}, {})
  eq(JSON.stringify(skipped), JSON.stringify({ q1: '', q2: '' }), '跳过也要带上题号（Go 侧才能写「用户没答」）')
}

// —— 3) 预览：选中项 → 它的 preview；没选 → 第一个带 preview 的选项 ——
{
  const [q] = inline.parseInlineQuestions(JSON.stringify([{
    id: 'q', question: '返回什么？',
    options: [{ label: '401' }, { label: '续期', preview: 'Set-Cookie: session=<new>' }],
  }]))
  eq(inline.previewOf(q, [])?.text, 'Set-Cookie: session=<new>', '没选时展示首个带预览的选项')
  // 与 AskUserQuestionPanel.previewFor 同规则：选中项没 preview 就回落到首个带预览的。
  eq(inline.previewOf(q, ['401'])?.label, '续期', '选中项无 preview 时回落到首个带预览的备选（与面板同规则）')
  eq(inline.previewOf(q, ['续期'])?.label, '续期', '预览跟着选中项')
  const [none] = inline.parseInlineQuestions('[{"id":"q","question":"无预览","options":["A"]}]')
  eq(inline.previewOf(none, ['A']), null, '没有 preview 的题不画预览块')
}

console.log(`\n✅ Hub 内联提问卡逻辑：${pass} 项全部通过`)
