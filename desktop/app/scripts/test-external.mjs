// test-external.mjs —— **不在默认链**的外部前置测试：需要仓库外/未入库的资产。
//
// 为什么摘出来（R05a3b「测试资产转正」）：
//   `link-check:embed-v2` 指向 `../../scripts/embed_v2_link_check.mjs`，而 `/scripts/` 在
//   .gitignore:26 被整目录忽略 —— 干净检出里这个文件**根本不存在**，默认链跑到第 45 步必
//   定 MODULE_NOT_FOUND。它还是个本机脚本：内部写死
//   `cd /Users/admin/workspace/go-code/desktop/app`，换台机器就不成立。
//
// 默认链的口径是「干净检出 + 只管 npm i 就能跑完」，所以这类东西不能留在里面。
//
// 前置条件（满足才可跑）：
//   1. 仓库根 `scripts/embed_v2_link_check.mjs` 存在（本机才有；未入库）；
//   2. 该脚本里的绝对路径指向**当前**仓库（换机器要改成自己的路径）；
//   3. 需要 desktop/app/node_modules 里的 esbuild（它就是靠 esbuild 打包 embed_v2.ts）。
//
// 退出码：0 = 通过；1 = 测试失败（前置齐全）；2 = 前置缺失（不是测试失败）。
import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = join(dirname(fileURLToPath(import.meta.url)), '..')
const REPO = join(APP, '..', '..')
const CHECKER = join(REPO, 'scripts', 'embed_v2_link_check.mjs')

if (!existsSync(CHECKER)) {
  console.error('⚠ 前置缺失：' + CHECKER)
  console.error('  该文件未入库（/scripts/ 被 .gitignore:26 整目录忽略），干净检出里没有。')
  console.error('  跳过。要跑它：在拥有该脚本的本机工作区执行，或把它入库（需同时去掉写死的绝对路径）。')
  process.exit(2)
}

const r = spawn('node', [CHECKER], { cwd: REPO, stdio: 'inherit' })
r.on('close', (code) => process.exit(code ?? 1))
