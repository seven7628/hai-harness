// test-golden.mjs —— **不在默认链**的 golden 前置测试：需要 Go 侧先生成 golden。
//
// 为什么摘出来（R05a3b「测试资产转正」）：
//   `test:trace-parity` 比对 Go 与 TS 两套 trace 投影（docs/TRACE_CHAIN_REDESIGN.md），
//   它读的 `desktop/bridge/testdata/trace_fixture.json.go.golden.json` 由 Go 测试生成。
//   两道前置在干净检出里都不成立：
//     1. 生成 golden 的 Go 测试文件 `desktop/bridge/trace_crossimpl_test.go` 是 **未入库**
//        的（F37 遗留的 61 个 _test.go 之一）——干净检出里没有它，`go test -run` 只会
//        打印 `[no tests to run]`（实测）；
//     2. `testdata/` 被 .gitignore 忽略，golden 本身也进不了库。
//   默认链必须零外部前置，故两者都挪到这里。
//
// 前置条件（全满足才可跑）：
//   1. Go 工具链（go 1.x）在 PATH 上；
//   2. `desktop/bridge/trace_crossimpl_test.go` 存在（未入库，需从本机工作区带上）；
//   3. 先跑 `go test ./desktop/bridge -run TestTraceProjectionCrossImpl` 生成
//      `desktop/bridge/testdata/trace_fixture.json.go.golden.json`（本脚本会先替你跑这一步）。
//
// 退出码：0 = 通过；1 = 测试失败（前置齐全）；2 = 前置缺失（不是测试失败）。
import { spawnSync, spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const APP = join(dirname(fileURLToPath(import.meta.url)), '..')
const REPO = join(APP, '..', '..')
const GO_TEST = join(REPO, 'desktop', 'bridge', 'trace_crossimpl_test.go')
const FIXTURE = join(REPO, 'desktop', 'bridge', 'testdata', 'trace_fixture.json')
const GOLDEN = FIXTURE + '.go.golden.json'

const missing = (msg) => {
  console.error('⚠ 前置缺失：' + msg)
  console.error('  先满足 `npm run test:golden` 脚本头列出的三条前置，再重试。')
  process.exit(2)
}

if (!existsSync(GO_TEST)) {
  missing(`${GO_TEST}（生成 golden 的 Go 测试，未入库）`)
}
if (!existsSync(GOLDEN)) {
  console.log('▸ golden 缺失，先让 Go 侧生成：go test ./desktop/bridge -run TestTraceProjectionCrossImpl')
  const gen = spawnSync('go', ['test', './desktop/bridge', '-run', 'TestTraceProjectionCrossImpl'], {
    cwd: REPO, stdio: 'inherit',
  })
  if (gen.error) missing('go 工具链不在 PATH 上')
  if (gen.status !== 0) {
    console.error(`✗ golden 生成失败（go test 退出 ${gen.status}）`)
    process.exit(1)
  }
  if (!existsSync(GOLDEN)) {
    console.error(`✗ go test 跑完了但没产出 golden（${GOLDEN}）—— 测试文件可能不含生成分支`)
    process.exit(1)
  }
}

const r = spawn('node', [join(APP, 'scripts', 'test-trace-parity.mjs')], { cwd: APP, stdio: 'inherit' })
r.on('close', (code) => process.exit(code ?? 1))
