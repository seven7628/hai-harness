#!/usr/bin/env node
// 手机页面静态审计（P4-03c）：不启动浏览器也能跑的硬约束。
//   - 触控目标：所有交互控件 >= 44px（N2）；
//   - 减少动态效果：prefers-reduced-motion 必须关掉光标/输入中/抽屉动画（N3）；
//   - 不拼 HTML：Hub 正文只走 DOM fragment + textContent（N9）；
//   - 已读游标：贴底 + 可见的纯函数守卫真的被接线（N8）；
//   - 发送失败：草稿在成功前不清空，失败有重试（N5）；
//   - 键入 @：input/composition 真的调用 mentionTypingQuery（N4）；
//   - 主题：system/ink/paper 持久化与 meta theme-color（P4-03c-1）；
//   - 390px：CSS 不写死超过 390px 的横向尺寸。
// 返回值是错误列表；空数组 = 通过。390 浏览器实测见 check-mobile-390.mjs。
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

const __dirname = dirname(fileURLToPath(import.meta.url))
const webDir = join(__dirname, '..', '..', 'bridge', 'mobile_web')

export const MOBILE_WEB_DIR = webDir

function read(name) {
  return readFileSync(join(webDir, name), 'utf8')
}

function cssRules(css) {
  const out = []
  const re = /([^{}]+)\{([^{}]*)\}/g
  let m
  while ((m = re.exec(css))) out.push({ selector: m[1].trim(), body: m[2] })
  return out
}

function px(body, prop) {
  const m = new RegExp(`${prop}\\s*:\\s*(\\d+(?:\\.\\d+)?)px`).exec(body)
  return m ? Number(m[1]) : 0
}

function targetRule(rules, selector) {
  return rules.filter((r) => r.selector.split(',').some((s) => s.trim() === selector))
}

function requireTarget(rules, selector, errors, needWidth = false) {
  const hits = targetRule(rules, selector)
  if (!hits.length) {
    errors.push(`44px: 找不到规则 ${selector}`)
    return
  }
  const height = Math.max(...hits.map((r) => Math.max(px(r.body, 'min-height'), px(r.body, 'height'))))
  if (height < 44) errors.push(`44px: ${selector} 高度 ${height}px < 44px`)
  if (needWidth) {
    const width = Math.max(...hits.map((r) => Math.max(px(r.body, 'min-width'), px(r.body, 'width'))))
    if (width < 44) errors.push(`44px: ${selector} 宽度 ${width}px < 44px`)
  }
}

function balancedBlock(src, openAt) {
  const start = src.indexOf('{', openAt)
  if (start < 0) return ''
  let depth = 0
  for (let i = start; i < src.length; i++) {
    if (src[i] === '{') depth++
    else if (src[i] === '}') {
      depth--
      if (depth === 0) return src.slice(start + 1, i)
    }
  }
  return ''
}

export function auditMobileWeb() {
  const errors = []
  const app = read('app.js')
  const model = read('hub_model.js')
  const avatar = read('avatar_model.js')
  const css = read('style.css')
  const html = read('index.html')
  const tokens = read('tokens.css')
  const cleanCss = css.replace(/\/\*[\s\S]*?\*\//g, '')
  const rules = cssRules(cleanCss)

  // —— 44px 触控目标 ——
  requireTarget(rules, 'button', errors)
  requireTarget(rules, '.icon', errors, true)
  requireTarget(rules, '#sessions li .more', errors, true)
  requireTarget(rules, '.sheet-actions button', errors)
  requireTarget(rules, '.hub-chip', errors)
  requireTarget(rules, '.hub-sheet-actions button', errors)
  requireTarget(rules, '.hub-load-more', errors)
  requireTarget(rules, '.hub-composer button', errors)
  requireTarget(rules, '.hub-at', errors, true)
  requireTarget(rules, '.agents-bar', errors)
  requireTarget(rules, '.jump', errors)
  requireTarget(rules, '.mention-item', errors)
  requireTarget(rules, '.theme-option', errors)
  if (!/\.mention-head \.icon[\s\S]{0,80}?(width|min-width)\s*:\s*(4[4-9]|[5-9]\d|\d{3,})px/.test(cleanCss)) {
    errors.push('44px: @ 选择器返回键不是 44px')
  }

  // —— reduced motion ——
  const rmAt = cleanCss.indexOf('@media (prefers-reduced-motion: reduce)')
  if (rmAt < 0) {
    errors.push('reduced-motion: 缺少 @media (prefers-reduced-motion: reduce)')
  } else {
    const rm = balancedBlock(cleanCss, rmAt)
    for (const marker of [
      'animation-duration: 0.01ms !important',
      'animation-iteration-count: 1 !important',
      'transition-duration: 0.01ms !important',
      '.cursor::after',
      '.hub-msg.draft .body::after',
      '.hub-typing .dots::after',
      '.sigil .arc',
      '.sheet',
      'animation: none !important',
    ]) {
      if (!rm.includes(marker)) errors.push(`reduced-motion: 块里缺少 ${marker}`)
    }
  }

  // —— 安全渲染 / XSS ——
  for (const [name, src] of [['app.js', app], ['hub_model.js', model], ['avatar_model.js', avatar]]) {
    if (/\.innerHTML\b|insertAdjacentHTML|outerHTML|document\.write\s*\(/.test(src)) {
      errors.push(`xss: ${name} 使用了 HTML 注入 API`)
    }
  }
  if (!/H\.bodyParts\(/.test(app)) errors.push('hub-md: app.js 未用 H.bodyParts 渲染定稿正文')
  // —— 头像 sigil v2（与桌面同一套算法）——
  if (!/HaiMobileAvatar/.test(avatar) || !/function deriveSigil/.test(avatar) || !/function planStack/.test(avatar)) {
    errors.push('avatar: avatar_model.js 缺少 deriveSigil/planStack')
  }
  if (!/A\.planStack\(/.test(app)) {
    errors.push('avatar: app.js 未接 avatar_model.js 的 planStack')
  }
  if (!/LUNA_FACE/.test(app) || !/data-luna-face/.test(app)) {
    errors.push('avatar: app.js 未接 Luna 引擎（LUNA_FACE/data-luna-face）')
  }
  if (!html.includes('src="mascot-bots.js"') || !html.includes('src="luna-face.js"')) {
    errors.push('avatar: index.html 未加载 mascot-bots.js/luna-face.js')
  }
  if (!html.includes('src="avatar_model.js"')) errors.push('avatar: index.html 未加载 avatar_model.js')
  if (cleanCss.includes('.hub-avatar')) errors.push('avatar: 仍在用旧 .hub-avatar 圆头像样式')
  if (!/\.sigil\.s24 \.trait\s*\{\s*display:\s*none/.test(cleanCss)) {
    errors.push('avatar: 24px 未隐藏内侧 trait')
  }
  if (!/function appendHubBody\(/.test(app)) errors.push('hub-md: 缺少 appendHubBody')
  if (!/appendHubBody\(body, m\.text/.test(app)) errors.push('hub-md: renderHubMessage 未接 appendHubBody')
  if (!/body\.textContent = d\.text/.test(app)) errors.push('hub-md: 流式草稿必须保持 textContent 纯文本')
  if (!/function renderHubDraft/.test(app)) errors.push('hub-md: 缺少 renderHubDraft')
  if (!/function markdownPlan|markdownPlan,/.test(model)) errors.push('hub-md: 模型缺少 markdownPlan')
  if (!/function bodyParts/.test(model)) errors.push('hub-md: 模型缺少 bodyParts')

  // —— N4 键入 @ ——
  if (!/H\.mentionTypingQuery\(/.test(app)) errors.push('mention: composer 未调用 H.mentionTypingQuery')
  if (!/compositionstart/.test(app) || !/compositionend/.test(app)) errors.push('mention: 缺少 IME composition 安全')
  if (!/openMentionPicker\(q\.query\)/.test(app)) errors.push('mention: 键入 query 未预填选择器')
  if (!/function mentionFromComposer/.test(app)) errors.push('mention: 缺少 mentionFromComposer')

  // —— N5 发送失败保草稿 ——
  if (!/H\.draftAfterSend\(/.test(app)) errors.push('draft: 成功路径未走 draftAfterSend')
  if (!/H\.draftAfterFailure\(/.test(app)) errors.push('draft: 失败路径未走 draftAfterFailure')
  if (!/appendHubSendError/.test(app) || !/重试/.test(app)) errors.push('draft: 失败没有重试入口')
  for (const [fn, end] of [
    ['async function sendHubGroup', 'async function stopHubConversation'],
    ['async function sendHubDM', '// ---- Hub 事件流'],
  ]) {
    const at = app.indexOf(fn)
    const stop = app.indexOf(end, at)
    const body = at >= 0 && stop > at ? app.slice(at, stop) : ''
    if (/input\.value\s*=\s*''/.test(body)) errors.push(`draft: ${fn} 在请求前清空输入框`)
  }

  // —— N8 已读游标 ——
  if (!/H\.shouldMarkRead\(/.test(app)) errors.push('read: 未用 H.shouldMarkRead 守卫')
  if (!/document\.hidden/.test(app)) errors.push('read: 未检查 document.hidden')
  if (!/visibilitychange/.test(app)) errors.push('read: 缺少 visibilitychange 重评估')
  if (!/addEventListener\('scroll', scheduleHubReadCheck/.test(app)) errors.push('read: 时间线滚动未重评估')
  if (!/mark_read:\s*false/.test(app)) errors.push('read: hub_open 不能在打开时无条件推进已读')

  // —— N6/N7 ——
  if (/case 'hub_group_update'/.test(app)) errors.push('events: 仍有死亡的 hub_group_update 分支')
  if (!/H\.pushHubAlert\(/.test(app)) errors.push('alerts: 未走 pushHubAlert 统一去重键')

  // —— 主题 ——
  if (!/hai\.theme/.test(app)) errors.push('theme: 未持久化到 hai.theme')
  if (!/documentElement\.dataset\.theme/.test(app)) errors.push('theme: 未写 documentElement.dataset.theme')
  if (!/prefers-color-scheme/.test(app)) errors.push('theme: 未读系统外观')
  if (!/themeMode === 'system'/.test(app)) errors.push('theme: 系统变化未限制在 system 模式')
  if (!/meta\[name="theme-color"\]/.test(app)) errors.push('theme: 未更新 meta theme-color')
  for (const marker of ['id="theme"', 'id="theme-sheet"', 'data-theme-mode="system"', 'data-theme-mode="ink"', 'data-theme-mode="paper"']) {
    if (!html.includes(marker)) errors.push(`theme: index.html 缺少 ${marker}`)
  }
  const metaColor = /<meta name="theme-color" content="(#[0-9a-fA-F]{6})">/.exec(html)
  if (!metaColor || !tokens.toLowerCase().includes(metaColor[1].toLowerCase())) {
    errors.push('theme: 初始 theme-color 必须来自 tokens.css')
  }
  for (const hex of ['#1a1614', '#fdfcfb']) {
    if (!tokens.toLowerCase().includes(hex)) errors.push(`theme: tokens.css 缺少 ${hex}`)
  }

  // —— 390px：不允许写死超过视口的横向尺寸 ——
  for (const m of cleanCss.matchAll(/(?:^|[;{\s])(min-)?width\s*:\s*(\d{3,})px/g)) {
    const v = Number(m[2])
    if (v > 390) errors.push(`390px: 固定宽度 ${v}px 超出 390 视口`)
  }
  if (!/@media \(max-width: 390px\)|@media \(max-width: 430px\)/.test(cleanCss)) {
    // 不是硬性要求（单列本身就该在 390 成立），但记录一下供人工看。
    // 不作为错误，避免为了通过审计加无意义的媒体查询。
  }

  return { errors, checks: rules.length }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  const { errors } = auditMobileWeb()
  if (errors.length) {
    for (const e of errors) console.error(`  ❌ ${e}`)
    process.exit(1)
  }
  console.log('手机页面静态审计通过：44px / reduced-motion / XSS / @ / 草稿 / 已读 / 主题 / 390px')
}
