// CSV/TSV 解析与 markdown 表格化回归测试（纯函数 + esbuild 打包，Node 直接跑，无需浏览器）。
//
// 背景（2026-09）：产出卡片与右栏「最终文件」此前把 .csv/.tsv 当**纯文本**渲染 ——
// 列对齐全靠逗号，中文错位，表格结构完全看不出来。改为转成 GFM 表格走既有 Markdown
// 管线（react-markdown + remark-gfm，零新依赖）。
//
// 本脚本锁住的关键行为（每条都对应一个「实现错了就会静默出错」的点）：
//   T1  基本解析（含表头）：字段正确切分、行列数对上
//   T2  字段内逗号：`"a,b",c` → 2 个字段（split(',') 会给 3 个，这是最经典的错法）
//   T3  字段内换行：引号包裹的多行字段不按行切；展示时折叠为 ↵（GFM 单元格不能有真换行）
//   T4  转义引号：`""` → 一个字面引号；引号只在字段起始位才是引用开始
//   T5  脏数据：行列数不一致不崩、不丢列，并如实标记 ragged
//   T6  markdown 注入防御：`|` 不撑坏表格；`*`/`_`/反引号/`[...]()`/`&copy;`/`$..$` 不被当语法
//   T7  空文件 → null；只有表头 → 表头渲染、0 数据行
//   T8  大文件闸门：行上限 / 列上限 / 总格数上限，且 hiddenRows 如实计数
//   T9  分隔符推断（.tsv / 分号 CSV / 引号内的分隔符不参与推断）+ BOM 剥离
//
// 用法：npm run test:csv
import { build } from 'esbuild'
import assert from 'node:assert'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createRequire } from 'node:module'
import React from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'

const __dirname = dirname(fileURLToPath(import.meta.url))
const require = createRequire(import.meta.url)

// 1) esbuild 打包纯函数模块（无 React / 无 store 依赖 → 直接 require 真实源码）
const dir = mkdtempSync(join(tmpdir(), 'csv-test-'))
const outfile = join(dir, 'csv.cjs')
await build({
  entryPoints: [join(__dirname, '../src/lib/csv.ts')],
  bundle: true,
  format: 'cjs',
  platform: 'node',
  outfile,
  logLevel: 'silent',
  loader: { '.md': 'text', '.svg': 'text', '.png': 'dataurl' }, // mock.ts 的 ?raw/?url 资源
})
const { parseDelimited, pickDelimiter, buildCsvTable, toGfmTable, escapeCell, isCsvPath, MAX_TABLE_ROWS, MAX_TABLE_COLS, MAX_TABLE_CELLS } = require(outfile)

let pass = 0
const ok = (m) => { pass++; console.log(`  ✅ ${m}`) }
const assertEq = (actual, expected, m) => {
  assert.deepStrictEqual(actual, expected, `${m}\n  实际: ${JSON.stringify(actual)}\n  期望: ${JSON.stringify(expected)}`)
  ok(m)
}

// 把 markdown 表格喂**真实渲染管线**（与组件同一条：react-markdown + remark-gfm），
// 从产出的 HTML 反查结构 —— 只比对 markdown 字符串会漏掉「解析器其实不认这段表格」的情况。
const renderMd = (md) => renderToStaticMarkup(React.createElement(ReactMarkdown, { remarkPlugins: [remarkGfm] }, md))
const tableOf = (md) => {
  const html = renderMd(md)
  const rows = [...html.matchAll(/<tr>([\s\S]*?)<\/tr>/g)].map((r) => [...r[1].matchAll(/<t[hd]>([\s\S]*?)<\/t[hd]>/g)].map((c) => c[1]))
  return { html, rows, isTable: html.includes('<table>') }
}
// HTML 文本节点还原（只做断言用得到的那几个实体）
const unescapeHtml = (s) => s.replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&quot;/g, '"').replace(/&#x27;/g, "'").replace(/&amp;/g, '&')

console.log('\n[T1] 基本解析（含表头）')
{
  const text = 'name,age,city\n张三,30,北京\n李四,25,上海\n'
  assertEq(parseDelimited(text, ','), [
    ['name', 'age', 'city'],
    ['张三', '30', '北京'],
    ['李四', '25', '上海'],
  ], 'parseDelimited：3 行 × 3 列，末尾换行不产生空行')
  const t = buildCsvTable(text, 'people.csv')
  assertEq(t.header, ['name', 'age', 'city'], '首行作表头')
  assertEq(t.rows, [['张三', '30', '北京'], ['李四', '25', '上海']], '数据行（不含表头）')
  assertEq([t.cols, t.dataRows, t.hiddenRows, t.ragged, t.multiline], [3, 2, 0, false, false], '统计字段')
  const r = tableOf(toGfmTable(t))
  assert(r.isTable, '渲染出 <table>')
  assertEq(r.rows.map((row) => row.map(unescapeHtml)), [
    ['name', 'age', 'city'], ['张三', '30', '北京'], ['李四', '25', '上海'],
  ], 'GFM 渲染后单元格逐一对上（中文不错位）')
  assert(isCsvPath('a/b/data.csv') && isCsvPath('DATA.TSV') && !isCsvPath('a.md') && !isCsvPath('csv'), 'isCsvPath 判定（大小写不敏感、无扩展名不算）')
}

console.log('\n[T2] 字段内的逗号（引号包裹）')
{
  const text = '"a,b",c\n"x, y",z\n'
  assertEq(parseDelimited(text, ','), [['a,b', 'c'], ['x, y', 'z']], '引号内的逗号不切字段（split(",") 会给 4 段）')
  const t = buildCsvTable(text, 't.csv')
  assertEq(t.header, ['a,b', 'c'], '表头含逗号的字段完整')
  assertEq(t.cols, 2, '列数 = 2（未被字段内的逗号撑成 4）')
  const r = tableOf(toGfmTable(t))
  assertEq(r.rows.map((row) => row.map(unescapeHtml)), [['a,b', 'c'], ['x, y', 'z']], '渲染后仍是 2 列且逗号保留')
}

console.log('\n[T3] 字段内的换行（引号包裹的多行字段）')
{
  const text = 'id,note\n1,"第一行\n第二行"\n2,单行\n'
  assertEq(parseDelimited(text, ','), [['id', 'note'], ['1', '第一行\n第二行'], ['2', '单行']], '引号内的换行属字段内容（不按行切）')
  const t = buildCsvTable(text, 't.csv')
  assertEq([t.dataRows, t.cols], [2, 2], '多行字段不额外产生数据行')
  assert(t.multiline, '标记 multiline（UI 据此显示「↵ = 单元格内换行」说明）')
  const r = tableOf(toGfmTable(t))
  assertEq(r.rows.length, 3, '渲染后仍是 3 行（含表头）—— 换行未把行拆开')
  assertEq(unescapeHtml(r.rows[1][1]), '第一行↵第二行', '单元格内换行折叠为 ↵')
  // 未闭合引号（被截断的文件）：不崩，按「引用到末尾」收尾
  assertEq(parseDelimited('a,b\n1,"未闭合\n', ','), [['a', 'b'], ['1', '未闭合\n']], '引号未闭合不抛错（按引用到末尾收尾）')
}

console.log('\n[T4] 转义引号 "" → 一个字面引号')
{
  const text = 'name,quote\n他说,"好的""没问题"""\n'
  assertEq(parseDelimited(text, ','), [['name', 'quote'], ['他说', '好的"没问题"']], '"" 折叠为一个引号')
  const t = buildCsvTable(text, 't.csv')
  const r = tableOf(toGfmTable(t))
  assertEq(unescapeHtml(r.rows[1][1]), '好的"没问题"', '渲染后引号正确（不是两个）')
  // 引号只在字段起始位才是引用开始：Excel 的宽容行为（a"b 里的引号是字面量）
  assertEq(parseDelimited('a"b,c\n', ','), [['a"b', 'c']], '字段中部的引号按字面量处理')
}

console.log('\n[T5] 行列数不一致（脏数据）')
{
  // 少列 / 多列 / 表头比数据短，三种混在一起
  const text = 'a,b\n1\n2,3,4\n5,6,7,8\n'
  const t = buildCsvTable(text, 't.csv')
  assert(t !== null, '脏数据仍能建表（不返回 null）')
  assertEq(t.cols, 4, '列数取全表最宽记录（4），不丢多出的列')
  assert(t.ragged, '标记 ragged（UI 据此提示行字段数不一致）')
  assertEq(t.rows, [['1', '', '', ''], ['2', '3', '4', ''], ['5', '6', '7', '8']], '短行补空串对齐，长行不截断')
  assertEq(t.header, ['a', 'b', '', ''], '表头也补齐到最宽列数')
  const r = tableOf(toGfmTable(t))
  assertEq(r.rows.length, 4, '渲染后 4 行')
  assertEq(r.rows.map((row) => row.length), [4, 4, 4, 4], '每行列数一致（表格不被脏数据撑坏）')
  assertEq(r.rows[1].map(unescapeHtml), ['1', '', '', ''], '少列的行补空单元格（不是被吞掉一行）')
  // 空行不算记录（与 Go encoding/csv 一致）
  assertEq(parseDelimited('a,b\n\n1,2\n', ','), [['a', 'b'], ['1', '2']], '空行跳过（不产生列数为 1 的假脏行）')
}

console.log('\n[T6] markdown 注入防御（含 | 的单元格不会撑坏表格）')
{
  assertEq(escapeCell('a|b'), 'a\\|b', '管道符被转义')
  assertEq(escapeCell('C:\\Users\\x'), 'C:\\\\Users\\\\x', '反斜杠先转义（Windows 路径不串味）')
  assertEq(escapeCell('*em* _u_ `c` ~s~'), '\\*em\\* \\_u\\_ \\`c\\` \\~s\\~', '强调/删除线/行内代码标记被转义')
  assertEq(escapeCell('[x](http://e.com)'), '\\[x\\](http://e.com)', '链接语法被转义')
  assertEq(escapeCell('<b>&copy;</b>'), '\\<b>\\&copy;\\</b>', 'HTML 标签与实体被转义（&copy; 不会被解成 ©）')
  assertEq(escapeCell('$PATH:$HOME'), '\\$PATH:\\$HOME', '美元符号被转义（KaTeX 不会吞成公式）')
  assertEq(escapeCell('多\n行'), '多↵行', '换行折叠为 ↵')
  assertEq(escapeCell('a\u0000b\u001fc'), 'abc', '控制字符剔除')

  // 最关键的一条：含 | 的单元格**不破坏表格结构**（修复前该行会被切成 3 格、后续行全部右移）
  const t = buildCsvTable('名前,値\nA|B,1\nC,2\n', 't.csv')
  const r = tableOf(toGfmTable(t))
  assertEq(r.rows.map((row) => row.map(unescapeHtml)), [
    ['名前', '値'], ['A|B', '1'], ['C', '2'],
  ], '含 | 的单元格渲染后仍是 2 列、值原样（A|B），表格结构未破')

  // 一行「全是语法」的敌对数据：渲染后必须是**纯文本原样**，不产生任何元素级语法
  const hostile = 'a,b\n*粗*,_斜_,`码`\n'
  const hr = tableOf(toGfmTable(buildCsvTable(hostile, 't.csv')))
  assertEq(hr.rows[1].map(unescapeHtml), ['*粗*', '_斜_', '`码`'], '敌意数据原样显示')
  assert(!/<(strong|em|code|del|a)\b/.test(hr.html), '未生成 strong/em/code/del/a 等元素（无 markdown 格式化）')

  // 分隔行/表头本身也走同一转义（表头里带 | 或 --- 不改变表格宽度）
  const ht = tableOf(toGfmTable(buildCsvTable('x|y,---\n1,2\n', 't.csv')))
  assertEq(ht.rows[0].map(unescapeHtml), ['x|y', '---'], '表头里的 | 与 --- 不改变列数/不产生分隔语义')
  assertEq(ht.rows[1].map(unescapeHtml), ['1', '2'], '数据行仍对齐（2 列）')
}

console.log('\n[T7] 空文件 / 只有表头')
{
  assertEq(buildCsvTable('', 'e.csv'), null, '空文件 → null（UI 显示「文件为空」，不是空表格）')
  assertEq(buildCsvTable('\n\n', 'e.csv'), null, '只有空行的文件 → null')
  const t = buildCsvTable('a,b,c\n', 'h.csv')
  assertEq([t.header, t.rows, t.dataRows, t.cols], [['a', 'b', 'c'], [], 0, 3], '只有表头：表头在、0 数据行')
  const r = tableOf(toGfmTable(t))
  assert(r.isTable && r.rows.length === 1, '渲染出一张只有表头的表（不崩、不是空字符串）')
  assertEq(r.rows[0], ['a', 'b', 'c'], '表头单元格正确')
  // 单元格全空但不为 null
  const e = buildCsvTable(',,\n,,\n', 'e.csv')
  assert(e !== null && e.cols === 3 && e.dataRows === 1, '全空单元格的表仍能渲染（3 列表头 + 1 行）')
}

console.log('\n[T8] 大文件闸门（防卡死）')
{
  const rows = Array.from({ length: 1200 }, (_, i) => `${i},v${i}`)
  const t = buildCsvTable(`id,val\n${rows.join('\n')}\n`, 'big.csv')
  assertEq([t.dataRows, t.rows.length, t.hiddenRows], [1200, MAX_TABLE_ROWS, 1200 - MAX_TABLE_ROWS], `行闸 ${MAX_TABLE_ROWS}：只渲染前 ${MAX_TABLE_ROWS} 行并如实报未显示数`)
  assertEq(t.rows[0], ['0', 'v0'], '前 N 行内容正确')
  // 宽表：总格数预算进一步收敛行数（实测 500×40 = 2 万格 ≈ 6.5s，必卡）
  const wideCols = Array.from({ length: 200 }, (_, i) => `c${i}`)
  const wideRows = Array.from({ length: 300 }, () => wideCols.map((_, i) => `v${i}`))
  const w = buildCsvTable([wideCols.join(','), ...wideRows.map((r) => r.join(','))].join('\n') + '\n', 'wide.csv')
  assertEq(w.cols, MAX_TABLE_COLS, `列闸 ${MAX_TABLE_COLS}（200 列被裁到 ${MAX_TABLE_COLS}）`)
  assert(w.wide, '标记 wide（UI 提示只显示前 N 列）')
  assert(
    w.rows.length * w.cols <= MAX_TABLE_CELLS && w.rows.length < MAX_TABLE_ROWS,
    `总格数闸 ${MAX_TABLE_CELLS}：宽表行数被进一步收敛到 ${w.rows.length} 行（< ${MAX_TABLE_ROWS}）`,
  )
  assertEq(w.hiddenRows, 300 - w.rows.length, 'hiddenRows 与收敛后的行数一致（不虚报）')
  assertEq(w.rows[0].length, MAX_TABLE_COLS, '每行补齐到列闸值')
  // 宽表不得被误判为脏数据（最宽值裁过之后 ragged 应按真实最宽值比）
  assert(!w.ragged, '等宽宽表不误报 ragged')
}

console.log('\n[T9] 分隔符推断 + BOM')
{
  assertEq(pickDelimiter('a\tb\tc\n1\t2\t3\n', ','), '\t', '首行有制表符 → 推断为 TSV')
  assertEq(pickDelimiter('a;b;c\n1;2;3\n', ','), ';', '首行有分号 → 推断为分号 CSV')
  assertEq(pickDelimiter('a,b\n1,2\n', '\t'), ',', '.tsv 但内容其实是逗号 → 回退逗号')
  assertEq(pickDelimiter('a,"b,c"\n', ','), ',', '引号内的分隔符不参与推断之外的干扰（仍认出逗号）')
  assertEq(pickDelimiter('single\n', ','), ',', '没有任何候选 → 保留首选分隔符（单列表格）')
  assertEq(buildCsvTable('a\tb\n1\t2\n', 'x.tsv').delimiter, '\t', '.tsv 首选制表符')
  assertEq(buildCsvTable('a;b\n1;2\n', 'x.csv').delimiter, ';', '.csv 但内容是分号 → 推断生效（否则整表只有一列）')
  const bom = buildCsvTable('\uFEFFname,age\n张三,30\n', 'b.csv')
  assertEq(bom.header, ['name', 'age'], 'BOM 剥离（表头不带不可见字符）')
  assertEq(buildCsvTable('name,age\r\n张三,30\r\n', 'crlf.csv').rows, [['张三', '30']], 'CRLF 归一化（Excel 导出常见）')
}

console.log(`\n✅ CSV 解析与 markdown 表格化：${pass} 项全部通过`)
