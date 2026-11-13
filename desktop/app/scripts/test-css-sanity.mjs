#!/usr/bin/env node
// 样式表完整性：**注释必须闭合**。
//
// 为什么值得一条脚本（2026-10 用户报障「这个左侧样式不是没有，而是被影响了」）：
// 多写一个注释结束符，后面几行就变成游离的 CSS 记号 —— 浏览器把它们当成一个**无效
// 选择器**，紧跟着的那条规则（连同它的所有声明）整块被丢弃。表现极难定位：
//   · DOM 结构完全正常（e2e 的选择器断言全绿）；
//   · 只有那几条样式静默消失（实测：.agent-tree 丢掉 margin 与 --atree-step →
//     连线列塌成 0 宽，树看起来"平"在会话行同一层）；
//   · 编辑器/TS/ESLint 都不报。
//
// 判据：解析后若出现「选择器里带中文」的规则，几乎一定是这种情况 ——
// 正文跑到注释外面了。真实的英文选择器集合是白名单式的稳定集合。
import { readFileSync } from 'fs'
import path from 'path'
import { fileURLToPath } from 'url'
import postcss from 'postcss'

const APP = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const FILES = [
  'src/index.css',
  'src/narrative-skin.css',
  'src/hub/hub.css',
  'src/hub/HubInlineTodos.css',
  'src/hub/CollapsibleMarkdown.css',
]

let bad = 0
for (const rel of FILES) {
  const css = readFileSync(path.join(APP, rel), 'utf8')
  let root
  try {
    root = postcss.parse(css)
  } catch (e) {
    console.error(`❌ ${rel}: 解析失败 —— ${e.message}`)
    bad++
    continue
  }
  const offenders = []
  root.walkRules((r) => {
    if (/[\u3000-\u9fff\uff00-\uffef]/.test(r.selector)) offenders.push(r.selector.replace(/\s+/g, ' ').slice(0, 100))
  })
  if (offenders.length) {
    bad += offenders.length
    console.error(`❌ ${rel}: ${offenders.length} 条规则的选择器里带中文（= 注释没闭合，正文档到外面去了）`)
    for (const o of offenders) console.error(`   · ${o}`)
  } else {
    console.log(`  ✅ ${rel}: 注释闭合正常（${root.nodes.length} 个顶层节点）`)
  }
}

if (bad) {
  console.error('\n修复方式：把漏写的那条注释真正闭合，正文留在注释里。')
  process.exit(1)
}
console.log('\nCSS_SANITY_TEST_PASS')
