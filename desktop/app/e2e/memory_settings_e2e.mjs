#!/usr/bin/env node
// 设置面板 · 项目记忆开关 的客户端级 E2E。
//
// 为什么需要它：MemoryTab 从未被真正渲染过。此前只有 tsc（类型）与 vite build
// （import 可解析）两道检查，两者都**不会**发现运行时问题 —— 引用了不存在的 CSS
// 类、showToast 形状不对、i18n 键缺失、复制按钮报错，都会顺利通过这两道。
// 仓库里同类界面的既有教训（见 e2e/ 下的 _ctxpanel_shots 等）也是这个路子。
//
// 覆盖：
//   ① 设置弹窗能打开，「项目记忆」tab 存在且可点
//   ② tab 内出现开关与「下一个会话生效」提示
//   ③ 读到 bridge 的权威值（memory_dir 出现 = 命令往返成功）
//   ④ 点「关」后 UI 立即变化，且 ~/.go-code/memory.json 真的落盘
//   ⑤ 重开面板仍显示「关」（持久化）
//   ⑥ 切到别的 tab 再切回来，状态不丢
//   ⑦ **记忆浏览区**（官方 §2.7 的 `/memory`，2026-09-30 新增）：预置 3 个文件
//      ——正常的、索引里有但磁盘上没有的（missing）、磁盘上有但索引没登记的
//      （notIndexed）——逐一断言渲染出的行与标签，以及空状态。
//
// 隔离：HOME → 独立目录（不碰用户真实 ~/.go-code/memory.json）。
// 退出码：全通过 0。

import { _electron } from 'playwright'
import crypto from 'crypto'
import { createRequire } from 'module'
import { execSync, spawnSync } from 'child_process'
import fs from 'fs'
import os from 'os'
import path from 'path'
import { fileURLToPath } from 'url'

const require = createRequire(import.meta.url)
const __dirname = path.dirname(fileURLToPath(import.meta.url))
const APP = path.resolve(__dirname, '..')
const REPO = path.resolve(APP, '../..')
// 隔离靠 HAI_DATA_DIR 而**不是** HOME（electron/main.ts:53-59）：
//  - userData（localStorage）在 macOS 上由 `app.getPath('appData')` 决定，认的是
//    **OS 家目录**，$HOME 改不动它 —— 只隔离 HOME 会把本 E2E 的 localStorage 写进
//    用户真实存储（`~/Library/Application Support/hai-desktop`）。
//  - HAI_DATA_DIR 非空时，userData 追加数据目录后缀（多实例隔离），且 spawn bridge 时
//    把 HOME 指向它 → Go 侧 ~/.go-code 同步隔离，memory.json 落在隔离目录里。
const DATA = path.join(os.homedir(), '.go-code-e2e-memory-data')
const HOME = DATA
const WS = path.join(os.homedir(), '.go-code-e2e-memory-ws')
const ELECTRON = require('electron')
const GOMODCACHE = execSync('go env GOMODCACHE').toString().trim()

let pass = 0
const log = (...a) => console.log('[e2e-memory]', ...a)
function ok(label) {
  pass++
  console.log(`  ✅ ${label}`)
}
function bad(msg) {
  console.error(`  ❌ ${msg}`)
  process.exitCode = 1
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

async function waitFor(fn, { timeout = 30000, interval = 400, label = '条件' } = {}) {
  const end = Date.now() + timeout
  while (Date.now() < end) {
    try {
      if (await fn()) return true
    } catch { /* 继续等 */ }
    await sleep(interval)
  }
  throw new Error(`等待超时：${label}`)
}

function maxMtime(dir) {
  let max = 0
  try {
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name)
      if (e.isDirectory()) max = Math.max(max, maxMtime(p))
      else max = Math.max(max, fs.statSync(p).mtimeMs)
    }
  } catch { /* 目录缺失 */ }
  return max
}

function ensureBuild() {
  const index = path.join(APP, 'dist', 'index.html')
  const mainJs = path.join(APP, 'dist-electron', 'main.js')
  const srcMtime = Math.max(maxMtime(path.join(APP, 'src')), maxMtime(path.join(APP, 'electron')))
  const outMtime = Math.min(
    fs.existsSync(index) ? fs.statSync(index).mtimeMs : 0,
    fs.existsSync(mainJs) ? fs.statSync(mainJs).mtimeMs : 0,
  )
  if (!fs.existsSync(index) || !fs.existsSync(mainJs) || srcMtime > outMtime) {
    log('renderer/electron main 有更新或缺失，执行 npm run build …')
    const b = spawnSync('npm', ['run', 'build'], { cwd: APP, encoding: 'utf8' })
    if (b.status !== 0) throw new Error(`npm run build 失败:\n${b.stdout}\n${b.stderr}`)
  }
  const r = spawnSync('go', ['build', '-o', 'desktop/app/dist/hai-bridge', './desktop/bridge'], {
    cwd: REPO, encoding: 'utf8',
  })
  if (r.status !== 0) throw new Error(`go build bridge 失败:\n${r.stderr}`)
}

/**
 * 预置记忆文件，让浏览区有东西可列。
 *
 * 目录名（<记忆根>/repos/<id>）取决于工作区身份：WS 不是 git 仓库，所以走
 * **非 git 回落**（官方 memory#storage-location：「Outside a git repo, the project
 * root is used instead」），id = `path-<路径非字母数字折叠为->-<路径哈希12位>`。
 *
 * 哈希我们自己算不了（它是 shortHash(abs path)），所以**先让 bridge 告诉我们**：
 * 起一个只读的 bridge 子进程没有好处、也没必要——改为「先开面板，从 memory_dir
 * 读出根目录，再算出 repos/<id>」，那正是本 E2E 已经拿到的权威值（步骤③）。
 * 于是 seed 放在 ③ 之后。
 */
function seedMemoryFiles(memoryDir, workspace, indexLines, files) {
  const dir = path.join(memoryDir, 'repos', memoryRepoId(workspace))
  fs.mkdirSync(dir, { recursive: true })
  if (indexLines) fs.writeFileSync(path.join(dir, 'MEMORY.md'), indexLines)
  for (const [name, body] of Object.entries(files)) fs.writeFileSync(path.join(dir, name), body)
  return dir
}

/**
 * 复刻 memory.idFromPath（memory/identity.go）：非 git 工作区的记忆目录名。
 *
 * 为什么要在 E2E 里复刻：官方 memory#storage-location 规定「Outside a git repo,
 * the project root is used instead」，而这个 workspace 不是 git 仓库，所以记忆目录
 * 的最后一段是**路径折叠 + 路径哈希**，没有别的办法在不启动 bridge 的前提下算出它。
 *
 * 为什么敢复刻：下面紧接着的「浏览区列出 3 个文件」断言就是校验 —— 算法一旦与 Go
 * 侧漂移，这里算出的目录会是空的，那个断言会红并把算出的 id 一起打出来。所以这份
 * 复刻**坏掉的方式是显式失败，不会静默通过**。
 *
 * 对齐点（改动这里时必须同步改 memory/identity.go 的 idFromPath）：
 *   · abs  = EvalSymlinks(Abs(workdir))；再把分隔符统一成 '/'
 *   · 逐字符：保留 [A-Za-z0-9.]，其余一律换成 '-'
 *   · 首尾 '-' 去掉 → 连续 '-' 压成一个
 *   · 折叠后若只剩 "path-"（无可辨认部分）→ 只留哈希
 *   · 截断到 67 字符，再拼 '-' + sha256(abs) 的前 12 位十六进制
 */
const MEM_MAX_ID_LEN = 80
function memoryRepoId(workspace) {
  const abs = fs.realpathSync(workspace).split(path.sep).join('/')
  const shortHash = crypto.createHash('sha256').update(abs).digest('hex').slice(0, 12)
  let raw = 'path-'
  for (const ch of abs) {
    raw += /[A-Za-z0-9.]/.test(ch) ? ch : '-'
  }
  let body = raw.replace(/^-+|-+$/g, '').replace(/-{2,}/g, '-')
  if (!body || body.replace(/[path.-]/g, '') === '') return 'path-' + shortHash
  const cap = MEM_MAX_ID_LEN - 12 - 1
  if (body.length > cap) body = body.slice(0, cap)
  return body + '-' + shortHash
}

/** 读偏好文件；不存在返回 null */
function readPrefs() {
  const p = path.join(HOME, '.go-code', 'memory.json')
  if (!fs.existsSync(p)) return null
  return JSON.parse(fs.readFileSync(p, 'utf8'))
}

const app_ = (async () => {
  ensureBuild()
  fs.rmSync(DATA, { recursive: true, force: true })
  fs.mkdirSync(DATA, { recursive: true })
  fs.mkdirSync(WS, { recursive: true })

  log(`启动 Electron（HAI_DATA_DIR=${DATA}）…`)
  const app = await _electron.launch({
    executablePath: ELECTRON,
    args: ['.'],
    cwd: APP,
    env: { ...process.env, HOME: DATA, HAI_DATA_DIR: DATA, GOMODCACHE },
  })
  const win = await app.firstWindow()
  const rendererErrors = []
  win.on('console', (m) => {
    if (m.type() === 'error') rendererErrors.push(m.text())
  })
  win.on('pageerror', (e) => rendererErrors.push(String(e)))

  try {
    await win.waitForLoadState('domcontentloaded')
    await win.evaluate(([ws]) => {
      localStorage.setItem('go-code.projects', JSON.stringify({ paths: [ws], active: ws }))
    }, [WS])
    await win.reload()
    await waitFor(() => win.locator('.nav-item:has-text("设置")').isVisible(), {
      timeout: 30000, label: '主界面',
    })
    ok('主界面挂载')

    // —— ① 打开设置 → 项目记忆 tab ——
    await win.locator('.nav-item:has-text("设置")').click()
    await waitFor(() => win.locator('[role="dialog"][aria-label="设置"]').isVisible(), {
      timeout: 10000, label: '设置弹窗',
    })
    const memoryTab = win.locator('.settings-nav-item:has-text("项目记忆")')
    await waitFor(() => memoryTab.isVisible(), { timeout: 10000, label: '「项目记忆」tab' })
    ok('设置弹窗内存在「项目记忆」tab')
    await memoryTab.click()
    await sleep(600)

    // —— ② 开关 + 生效提示 ——
    const seg = win.locator('.settings-body .seg[role="radiogroup"]').first()
    await waitFor(() => seg.isVisible(), { timeout: 10000, label: '开关组' })
    const bodyText = (await win.locator('.settings-body').textContent()) ?? ''
    if (!bodyText.includes('下一个会话')) bad('缺少「下一个会话生效」说明 —— 用户会以为开关失灵')
    else ok('显示「下一个会话生效」说明')

    // —— ③ bridge 往返：记忆目录路径 = 命令响应回来了 ——
    const gotDir = await waitFor(async () => {
      const t = (await win.locator('.settings-body').textContent()) ?? ''
      return t.includes('.go-code/memory') ? true : false
    }, { timeout: 15000, label: 'memory_settings 响应（记忆目录）' }).then(() => true).catch(() => false)
    if (!gotDir) bad('没读到 bridge 返回的记忆目录 —— memory_settings 命令可能没生效')
    else ok('读到 bridge 的记忆目录（命令往返成功）')

    // —— ⑦a 预置记忆文件，让浏览区有东西可列 ——
    // 放在 ③ 之后：记忆根目录是 bridge 刚回给我们的权威值，而 repos/<id> 里的
    // <id> 由身份派生（WS 非 git → 路径折叠 + 路径哈希），测试自己算不出来。
    // 记忆根目录**必须从那个 <code> 元素读**，不能对 .settings-body 的 textContent
    // 用正则捞：DOM 相邻元素的无分隔拼接会把路径和后面按钮的文案粘成一坨
    // （".../memory" + "复制记忆文件" = ".../memory复制记忆文件…"），贪婪的 [^\s]*
    // 会一路吃到下一个空白为止，拿到一个根本不存在的路径。本文件上方那段注释
    // 早就为「查未翻译 i18n 键」记过同一个坑，这里却在取路径时又踩了一次。
    const memRoot = ((await win.locator('.settings-body code.mesh-code').first().textContent()) ?? '').trim()
    if (!memRoot.includes('/memory')) {
      bad(`界面上读不到记忆根目录路径（读到 ${JSON.stringify(memRoot)}），无法预置浏览区数据`)
    } else {
      if (!fs.existsSync(memRoot)) log(`注意：记忆根目录尚不存在，将由本次预置创建：${memRoot}`)
      const computedId = memoryRepoId(WS)
      log(`记忆目录 id（非 git 回落）= ${computedId}`)
      const seeded = seedMemoryFiles(
        memRoot,
        WS,
        '# Project Memory\n\n- relay_tunnel_id.md\n- ghost_topic.md\n',
        {
          // 正常条目：索引里有、磁盘上有、带 type 与 modified
          'relay_tunnel_id.md':
            '---\ntype: project\nmodified: 2026-09-30T02:11:03Z\n---\n\nThe identifier is `q7-tunnel-corp`.\n',
          // 磁盘上有但索引没登记：模型刚写完 topic 还没补索引的正常中间态
          'not_indexed_yet.md': '---\ntype: user\n---\n\n还没登记\n',
          // ghost_topic.md **故意不创建**：索引里有、磁盘上没有
        },
      )
      if (!seeded) bad('预置记忆文件失败')
      else {
        // 预置后立刻自检落盘：上面那次「预置成功但面板空」就是路径算错造成的，
        // 而当时没有任何断言能区分「写失败了」和「写到别处去了」。
        const written = fs.existsSync(path.join(seeded, 'MEMORY.md')) &&
          fs.existsSync(path.join(seeded, 'relay_tunnel_id.md')) &&
          fs.existsSync(path.join(seeded, 'not_indexed_yet.md'))
        if (!written) bad(`预置后文件不在预期位置：${seeded}`)
        else ok(`预置 3 个记忆文件场景并已落盘（${path.basename(seeded)}）`)
        // 刷新浏览清单
        const refreshBtn = win.locator('.settings-body button.btn', { hasText: /刷新|Refresh/ })
        await waitFor(() => refreshBtn.isVisible(), { timeout: 10000, label: '浏览区刷新按钮' })
        await refreshBtn.click()
        try {
          // 注意 `async` + `await`：写成同步的 `count() >= 3` 是拿 Promise 比数字，
          // 恒为 false —— 断言会一直等��超时，而界面上其实已经渲染出来了（第一次
          // 栽在这：面板文本里三行都在，断言却在喊「没列出」）。
          await waitFor(async () => (await win.locator('table.mem-table tbody tr').count()) >= 3, {
            timeout: 15000, label: '浏览区列出 3 个文件',
          })
        } catch (e) {
          const dbg = path.join(APP, 'e2e', '_memorytab_shots', 'browse-debug.png')
          fs.mkdirSync(path.dirname(dbg), { recursive: true })
          await win.screenshot({ path: dbg })
          const dump = await win.locator('.settings-body').textContent()
          log('—— 诊断：浏览区所在面板文本 ——')
          log(String(dump).slice(0, 1200))
          log(`诊断截图：${dbg}`)
          throw e
        }
        ok('浏览区列出 3 个文件')

        // 正常条目：文件名 + type + modified 都渲染出来
        const rowNormal = win.locator('table.mem-table tbody tr', { hasText: 'relay_tunnel_id.md' })
        const normalText = (await rowNormal.textContent()) ?? ''
        if (!normalText.includes('project')) bad(`正常条目未显示 type：${normalText}`)
        else ok('正常条目显示 frontmatter 的 type')
        if (!normalText.includes('2026-09-30')) bad(`正常条目未显示 modified：${normalText}`)
        else ok('正常条目显示 modified 时间戳')

        // missing：索引里有、磁盘上没有 → 标注 + 删除线，不能静默消失
        const rowGhost = win.locator('table.mem-table tbody tr', { hasText: 'ghost_topic.md' })
        if ((await rowGhost.count()) === 0) bad('missing 的条目被静默隐藏了（官方不做存在性校验）')
        else {
          const ghostText = (await rowGhost.textContent()) ?? ''
          if (!ghostText.includes('文件不存在')) bad(`missing 条目未标注：${ghostText}`)
          else ok('missing 条目标注「文件不存在」')
        }

        // notIndexed：磁盘上有、索引没登记 → 标注，不能被当成不存在
        const rowUn = win.locator('table.mem-table tbody tr', { hasText: 'not_indexed_yet.md' })
        if ((await rowUn.count()) === 0) bad('未登记的条目没有被列出')
        else {
          const unText = (await rowUn.textContent()) ?? ''
          if (!unText.includes('未登记')) bad(`未登记条目未标注：${unText}`)
          else ok('未登记条目标注「未登记」')
        }

        // 浏览区不得出现删除按钮：官方承诺的是"随时自己编辑或删除"，
        // 宿主再画一个删除入口就得配一套校验与确认，而官方没有这套机制。
        const delBtns = await win.locator('table.mem-table button').count()
        if (delBtns > 0) bad(`浏览区出现了 ${delBtns} 个按钮：浏览应是纯只读的`)
        else ok('浏览区无写操作按钮（纯只读）')

        // 浏览区正常时**不得**出现「项目记忆未启用」——那句话只在该状态为真时说。
        const browseAreaText = (await win.locator('.settings-body').textContent()) ?? ''
        if (browseAreaText.includes('项目记忆未启用')) {
          bad('浏览区有文件却显示「项目记忆未启用」——available 与 files 两种状态搞反了')
        } else ok('有文件时不显示「项目记忆未启用」')
      }
    }

    // —— ②b 不得出现未翻译的 i18n 键 ——
    // 上一版这里直接漏了：按钮写 t('settings.copy')，该键不存在，useT 原样返回键名，
    // 于是界面上真的显示「settings.copy」。tsc 与 vite build 都发现不了（键是字符串，
    // 类型对、import 也在），只有把界面渲染出来才看得见。
    // 必须**此刻重新取**文本：目录行是 bridge 响应回来之后才渲染的，用早先的
    // 快照会漏掉它（第一版就是这么漏的：断言"通过"，而界面上明摆着 settings.copy）。
    const nowText = (await win.locator('.settings-body').textContent()) ?? ''
    // 必须逐个**叶子节点**查，不能对整段 textContent 扫：DOM 元素相邻会被无分隔拼接
    // （".../memory" + "settings.copy" = "memorysettings.copy"），此时既无法用 \b 起首，
    // 也无法用「前一字符非标识符」断言 —— 路径尾巴和键名粘在一起，从拼接文本里根本
    // 分不出来。第一版两次都栽在这里（断言"通过"而界面上明摆着坏键）。
    const leafTexts = await win.locator('.settings-body *').evaluateAll((els) =>
      els.filter((e) => e.children.length === 0).map((e) => (e.textContent || '').trim()),
    )
    const keyRe = /^(?:settings|ctx|common|md|app)\.[a-zA-Z][\w.]*$/
    const untranslated = [...new Set(leafTexts.filter((t) => keyRe.test(t)))]
    if (untranslated.length) {
      bad(`界面出现未翻译的 i18n 键：${untranslated.join(', ')}（useT 会把缺失的键原样渲染）`)
    } else {
      ok('无未翻译的 i18n 键')
    }

    // 复制按钮必须真的存在且有文案（class 用的是仓库既有的 .btn，btn-ghost 并不存在）
    const copyBtn = win.locator('.settings-body button.btn', { hasText: /复制|Copy/ })
    if ((await copyBtn.count()) === 0) bad('记忆目录旁的复制按钮缺失或无文案')
    else ok('记忆目录复制按钮存在')

    // 默认应为「开」
    let on = await seg.locator('button.cur').textContent()
    if (!String(on).includes('开')) bad(`默认应为开启，实际 ${on}`)
    else ok('默认为开启')

    // —— ④ 点「关」→ UI 立即变化 + 落盘 ——
    await seg.locator('button:not(.cur)').click()
    await sleep(800)
    on = await seg.locator('button.cur').textContent()
    if (String(on).includes('开')) bad(`点关后 UI 仍显示「${on}」`)
    else ok('点「关」后 UI 立即变为关闭')

    const prefs = await waitFor(() => {
      const p = readPrefs()
      return p && p.enabled === false ? p : false
    }, { timeout: 10000, label: '~/.go-code/memory.json 落盘' }).catch(() => null)
    if (!prefs) bad('点「关」后 memory.json 未落盘（enabled:false）')
    else ok('~/.go-code/memory.json 已落盘 enabled:false')

    // —— ⑤ 关面板重开，状态应持久 ——
    await win.keyboard.press('Escape')
    await sleep(400)
    await win.locator('.nav-item:has-text("设置")').click()
    await waitFor(() => win.locator('[role="dialog"][aria-label="设置"]').isVisible(), { timeout: 10000, label: '设置弹窗(2)' })
    await win.locator('.settings-nav-item:has-text("项目记忆")').click()
    await sleep(800)
    const seg2 = win.locator('.settings-body .seg[role="radiogroup"]').first()
    await waitFor(() => seg2.isVisible(), { timeout: 10000, label: '开关组(2)' })
    const on2 = await seg2.locator('button.cur').textContent()
    if (String(on2).includes('开')) bad(`重开面板后显示「${on2}」，持久化失败`)
    else ok('重开面板仍为关闭（持久化生效）')

    // —— ⑥ 切走再切回，状态不丢 ——
    await win.locator('.settings-nav-item:has-text("通用")').click()
    await sleep(300)
    await win.locator('.settings-nav-item:has-text("项目记忆")').click()
    await sleep(600)
    const on3 = await win.locator('.settings-body .seg[role="radiogroup"]').first().locator('button.cur').textContent()
    if (String(on3).includes('开')) bad(`切 tab 往返后显示「${on3}」，状态丢了`)
    else ok('切 tab 往返后状态不丢')

    // 截图留证
    const shotDir = path.join(APP, 'e2e', '_memorytab_shots')
    fs.mkdirSync(shotDir, { recursive: true })
    await win.screenshot({ path: path.join(shotDir, 'memory-tab.png') })
    log(`截图：${shotDir}/memory-tab.png`)

    // renderer 错误汇总（只报与本功能相关的）
    const relevant = rendererErrors.filter((e) => /memory|Memory|设置|settings/i.test(e))
    if (relevant.length) {
      bad(`renderer 报错（${relevant.length} 条，与记忆/设置相关）：\n    ` + relevant.slice(0, 5).join('\n    '))
    } else {
      ok('renderer 无相关报错')
    }
  } finally {
    await app.close()
  }
})()

app_.catch((e) => {
  console.error('[e2e-memory] 失败:', e)
  process.exitCode = 1
})
