// providerCatalog-test.ts —— Provider 目录（搜索/分类/可用性/排序）纯逻辑单测。
//
// 为什么值得钉：目录是**用户找得到入口**的唯一路径。
//   · 搜索漏了别名（glm / grok / moonshot），用户会以为「我们不支持智谱」；
//   · 「可用」判定错了，用户会看到两条一模一样的 DeepSeek（或反过来什么都加不了）；
//   · 排序坏了不至于崩溃，但「当前」会掉到列表中间，用户一眼找不到自己在用哪个。
//
// 运行：esbuild bundle 后 node --test（见 package.json 的 test:provider-catalog）。
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { CATALOG, availableEntries, catalogByID, categoryCounts, countByHost, hostOfURL, nextFreeID, searchCatalog, sortConfigured, assertCatalogCoverage } from './providerCatalog'

test('搜索：别名命中（glm → 智谱、grok → xAI、moonshot → Kimi）', () => {
  const ids = (q: string) => searchCatalog(q).map((e) => e.id)
  assert.ok(ids('glm').includes('zhipu'))
  assert.ok(ids('grok').includes('xai'))
  assert.ok(ids('moonshot').includes('kimi'))
  assert.ok(ids('智谱').includes('zhipu'))
  assert.ok(ids('通义').includes('qwen'))
})

test('搜索：host 片段命中（bigmodel / opencode.ai）', () => {
  assert.ok(searchCatalog('bigmodel').some((e) => e.id === 'zhipu'))
  assert.ok(searchCatalog('opencode.ai').some((e) => e.id === 'opencode'))
})

test('搜索：i18n 显示名参与匹配，空查询返回全部', () => {
  const hit = searchCatalog('智谱 Zhipu', { displayNames: { zhipu: '智谱 Zhipu' } })
  assert.equal(hit.length, 1)
  assert.equal(hit[0].id, 'zhipu')
  assert.equal(searchCatalog('').length, CATALOG.length)
})

test('搜索 + 分类同时生效：订阅类里搜 claude 只出 anthropic', () => {
  const hit = searchCatalog('claude', { category: 'sub' })
  assert.deepEqual(hit.map((e) => e.id), ['anthropic'])
})

test('分类计数：all = 条目总数，各分类之和 + custom = all', () => {
  const c = categoryCounts()
  assert.equal(c.all, CATALOG.length)
  const sum = (c.sub ?? 0) + (c.vendor ?? 0) + (c.platform ?? 0) + (c.aggregator ?? 0) + (c.custom ?? 0)
  assert.equal(sum, c.all)
  assert.equal(c.custom, 2) // 两个自定义模板
})

test('可用条目：已添加的 id 被排除，自定义模板始终可选', () => {
  const left = availableEntries(['deepseek', 'opencode'])
  assert.ok(!left.some((e) => e.id === 'deepseek'))
  assert.ok(!left.some((e) => e.id === 'opencode'))
  assert.ok(left.some((e) => e.id === 'kimi'))
  assert.equal(left.filter((e) => e.id.startsWith('custom:')).length, 2)
})

test('删除后再添加（deleted 留在 settings 里）不影响可用判定 —— 判定只认 id', () => {
  // 这里刻意用「已添加」语义而非「未删除」：ProviderPage 传入的已添加 id 列表已过滤 deleted。
  const left = availableEntries([])
  assert.equal(left.length, CATALOG.length)
})

test('hostOfURL：协议/路径/端口/大小写都能归一', () => {
  assert.equal(hostOfURL('https://API.DeepSeek.com/v1'), 'api.deepseek.com')
  assert.equal(hostOfURL('api.moonshot.cn/v1'), 'api.moonshot.cn')
  assert.equal(hostOfURL('http://localhost:8080/v1'), 'localhost')
  assert.equal(hostOfURL(''), '')
  assert.equal(hostOfURL('::::'), '')
})

test('排序：当前在最前，其次有计量，其次有凭证，最后按名字', () => {
  const rows = [
    { id: 'b', active: false, hasKey: true, label: 'Bravo' },
    { id: 'a', active: true, hasKey: false, label: 'Alpha' },
    { id: 'c', active: false, hasKey: true, hasMeter: true, label: 'Charlie' },
    { id: 'd', active: false, hasKey: false, label: 'Delta' },
  ]
  assert.deepEqual(sortConfigured(rows).map((r) => r.id), ['a', 'c', 'b', 'd'])
})

test('目录与后端预设的一致性校验：缺失返回缺失项（不抛）', () => {
  const presetIDs = CATALOG.filter((e) => !e.id.startsWith('custom:')).map((e) => e.id)
  assert.deepEqual(assertCatalogCoverage(presetIDs), [])
  assert.deepEqual(assertCatalogCoverage(['deepseek']).sort(), presetIDs.filter((id) => id !== 'deepseek').sort())
  assert.deepEqual(assertCatalogCoverage([]), []) // 后端没数据（如 mock 未就绪）→ 不校验
})

test('catalogByID：自定义模板也能查到', () => {
  assert.equal(catalogByID('deepseek')?.category, 'vendor')
  assert.equal(catalogByID('custom:anthropic')?.custom?.protocol, 'anthropic')
  assert.equal(catalogByID('nope'), undefined)
})

// —— 「同一家可以加多个」 ——
// 用户实际用法：opencode 加三次（opencode/opencode2/opencode3）分别配不同 key 再改名。
// 这两条错了，第二次添加要么被目录卡片禁掉，要么撞 id 失败。
test('nextFreeID：空闲用原名，占用则递增 opencode → opencode2 → opencode3', () => {
  assert.equal(nextFreeID('opencode', []), 'opencode')
  assert.equal(nextFreeID('opencode', ['opencode']), 'opencode2')
  assert.equal(nextFreeID('opencode', ['opencode', 'opencode2']), 'opencode3')
  // 空洞也复用（原来加的 opencode2 被删了 → 下次就用 opencode2，不无限涨）
  assert.equal(nextFreeID('opencode', ['opencode', 'opencode3']), 'opencode2')
  // 与既有其它 provider 不冲突
  assert.equal(nextFreeID('deepseek', ['opencode2']), 'deepseek')
})

test('countByHost：按 host 归集「已添加几家」（改名/换 id 都还算同一家）', () => {
  const rows = [
    { base_url: 'https://opencode.ai/zen/go/v1' },
    { base_url: 'https://opencode.ai/zen/v1' },      // opencode3：同 host 不同路径
    { base_url: 'https://api.deepseek.com/v1' },
    { base_url: '' },                                 // 解析不出 host → 不计
    { base_url: 'not a url' },
  ]
  const m = countByHost(rows)
  assert.equal(m.get('opencode.ai'), 2)
  assert.equal(m.get('api.deepseek.com'), 1)
  assert.equal(m.size, 2)
  assert.equal(countByHost([]).size, 0)
})
