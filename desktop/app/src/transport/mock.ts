import type { Transport, BridgeExitInfo, AppSettings, ProviderSaveInput, ProviderCfg, MCPServerCfg, ExternalSkillsStatus, ExternalSkillsImportResult, OAuthStatus } from './types'
import type { BridgeCommand } from '../store/events'
import type { MCPProjectInfo, MCPProjectServer, HookSectionView } from '../store/useAppStore'
import currentProjectReadmeZh from '../../../../README.zh-CN.md?raw'
import haiLogoDarkUrl from '../../../../brand/hai-logo-dark.svg?no-inline'
import haiLogoLightUrl from '../../../../brand/hai-logo-light.svg?no-inline'
import screenshotMainUrl from '../../../../brand/screenshot-main.png?url'
import screenshotTraceUrl from '../../../../brand/screenshot-trace.png?url'

// mock 项目池：pickWorkspace 轮换返回，演示「项目→会话」树（浏览器无真实文件夹选择器）
const MOCK_PROJECTS = ['/mock/proj-auth', '/mock/proj-billing', '/mock/proj-search', '/mock/proj-notify']
// mock 里「真实存在」的工作区目录（真 bridge 用 os.Stat 判定；不在集合里 = 已删除 → 面板不展示）
const MOCK_EXISTING_PROJECTS = ['/mock/workspace', ...MOCK_PROJECTS]
const MOCK_TITLES = [
  '修复登录接口 JWT 过期分支',
  '重构计费结算状态机',
  '搜索服务 ES 索引迁移',
  '通知推送重试与幂等',
  '补齐订单导出并发测试',
  '权限中间件去重梳理',
]

// mock 技能（对齐真实 bridge skills 命令返回形状：name/description/enabled 真过滤）
const MOCK_SKILLS: { name: string; description: string; instructions: string; resources: string[]; enabled: boolean; source?: string }[] = [
  {
    name: 'code-review',
    description: '对变更做多维度审查（正确性/安全/性能/测试覆盖），逐条验证后输出结论。',
    instructions: '# 代码评审\n\n按正确性 → 安全性 → 性能 → 测试覆盖逐条审查变更，每条给严重度与证据，最后给结论。',
    resources: ['/mock/workspace/.agents/skills/code-review/references/guide.md'],
    enabled: true,
  },
  {
    name: 'git-workflow',
    description: '分支管理、提交规范检查、合并冲突分析，会话内 git 操作护栏。',
    instructions: '# Git 工作流\n\n分支命名、提交信息规范、合并冲突处理步骤。',
    resources: [],
    enabled: true,
  },
  {
    name: 'web-research',
    description: '联网检索最新资料，引原文 + 来源列表，结果落进会话。',
    instructions: '# 联网调研\n\n检索、取原文、列来源。',
    resources: [],
    enabled: false,
  },
  {
    name: 'doc-writer',
    description: '中文技术文档、README、变更日志的写作规范与模板。',
    instructions: '# 技术写作\n\n中文文档结构、标题层级、变更日志模板。',
    resources: [],
    enabled: false,
  },
]

// 已更新过的技能名：skill_update 后重浏览 → update_available=false（模拟真实 bridge 更新刷新 commit）
const MOCK_UPDATED = new Set<string>()

// mock 定时任务（CronPage；真 bridge 由 ~/.go-code/cron.json + cron_runs.jsonl 提供）
const MOCK_CRON: {
  id: string; cron: string; prompt: string; recurring: boolean; workspace: string
  created_at: string; next_run?: string; last_run?: string; last_status?: string; run_count: number
}[] = [
  { id: 'c1', cron: '3 9 * * *', prompt: '提醒我早上打卡', recurring: true, workspace: '/workspace/go-code', created_at: new Date().toISOString(), next_run: new Date().toISOString(), last_status: 'success', run_count: 3 },
  { id: 'c2', cron: '*/5 * * * *', prompt: '拉取行情并汇总', recurring: true, workspace: '/workspace/billing', created_at: new Date().toISOString(), next_run: new Date().toISOString(), last_status: 'running', run_count: 21 },
  { id: 'c3', cron: '30 14 19 8 *', prompt: '一周后提醒提交周报', recurring: false, workspace: '/workspace/go-code', created_at: new Date().toISOString(), run_count: 0 },
]
const MOCK_CRON_RUNS = MOCK_CRON.flatMap((j) =>
  Array.from({ length: 3 }, (_, i) => ({
    run_id: `r-${j.id}-${i}`, job_id: j.id, fired_at: new Date().toISOString(), session_id: `s-cron-${j.id}-${i}`,
    status: i === 0 ? j.last_status ?? 'success' : 'success', duration_ms: 3000 + i * 1200, final_result: `示例运行结果 ${i + 1}`,
  })))

interface MockSess {
  id: string
  title: string
}

// 每会话运行态（多 session 并行）：各自独立的 inbox（takeBatch 语义）/审批/提问/脚本进度。
// 事件按 session_id 路由进 views[sid]——背景会话的事件进自己 view，不打扰当前显示。
interface MockRunState {
  id: string
  busy: boolean // 模拟 SDK inbox 占用：运行中连发入队，脚本结束批量进下一轮
  askQueue: string[]
  pendingApproval: { id: string; runId: string } | null
  pendingQuestion: { batchId: string; toolId: string; runId: string } | null // ask_user 待答批次（answer_question 命令锚）
  lastAnswers: { questionId: string; answer: string }[] // 用户最近一次整批回答（模型流式回显）
  runSeq: number
  mode: string
  stopped: boolean // interrupt 置位：该会话主 run 脚本停止推进（after 拦截主 run 节点；afterBg 节点不受影响）
  promote?: { taskId: string; runId: string; name: string } // 运行中长工具（promote_task 命令等待锚）
  bgTool?: { taskId: string; runId: string } // 已 promote 的后台工具（interrupt_task 精确停止锚）
  toolDone?: boolean // 后台工具已正常完成
  toolInterrupted?: boolean // 后台工具已被精确中断（interrupt_task）
  failPromote?: boolean // 演示/测试钩子：promote_task 一律失败（验证前端乐观回滚 + 行内提示）
  curRunId?: string // 当前主 run id（对齐 SDK：llm_end 恒带 run_id，主 agent 归集到其 run）
}

let seq = 0
const ts = () => new Date().toISOString()

// —— 自定义 provider 校验（与 bridge validProviderName / electron isValidProviderName 同规则）——
const MOCK_BUILTIN_PROVIDERS = ['deepseek', 'openai', 'opencode', 'kimi', 'zhipu'] as const
function isValidProviderName(name: string): boolean {
  if (!name || name === 'active') return false
  return /^[A-Za-z][A-Za-z0-9_-]*$/.test(name)
}

type Emit = (line: string) => void

// mock 重放：事件名与工具参数全部对齐 SDK 真实结构（events 包 / tools·todo·subagent·plan·question）。
// 覆盖：多工具调用（list_dir/read_file+行区间/grep/edit_file/todo_add/todo_update）、
// HITL 审批（带文件 diff 预览）、HITL 反方向 ask_user 批量提问（一次问 2 条 → 列表 modal → 整批回答续跑）、
// 异步子 agent（agent_spawn + task_* 事件 + task_result_delivered 自动回传，无 agent_wait）、
// plan 模式只读调研 + plan_submit。
// 多 session 并行：每会话独立运行态 + 事件按 session_id 路由（对齐真 bridge 单进程多会话）；
// 切换活跃会话时背景会话的异步流程（子 agent/后台任务/提问/审批）继续推进到其自己的 view。
// Q1 待发送队列：运行中连发的消息全部即时推送到 mock（模拟 SDK inbox），mock 排队、当前脚本结束
// 后批量进入下一轮（对齐 SDK takeBatch）；前端 pendingQueue 镜像展示「待发送」清单。
// 事件严格串行：每个 LLM 轮 = llm_start→(think/流式)→llm_end，工具调用在其后
// —— store 的「关闭最后一条 assistant 块」依赖此顺序；每个 ask 发一个主 agent_start（runId 递增）。

// mock 里的「项目 README」（演示 / 宣传片 / 预览回归共用这一份真实内容，不用另造一份假文档）。
// 图片改写：README 里的图是**仓库相对路径**（brand/hai-logo-*.svg、brand/screenshot-*.png）。
// 真实环境由主进程 fs:read-image 读盘（见 lib/localAsset.ts），浏览器 mock 没有这条 IPC ——
// 原样渲染只会得到「读不到」的占位块（宣传片里 logo 与两张截图就是这么露出来的）。这里在 mock
// 侧把相对路径换成 Vite 打出来的资源 URL：图真能显示，形状仍与真 bridge 一致。
// logo 走 <picture>：srcset 在 <source> 上，src 在 <img> 上，两处都要改。
//
// 三个坑都在这几行里（都是实测踩出来的）：
//  ① Vite 给的是**根相对**地址（dev /@fs/…，build 因 base:'./' 是 ./assets/…）。它们没有 scheme，
//     会被 lib/localAsset.ts 的 isLocalAssetSrc 判成「本地文件路径」→ 又走回 fs:read-image →
//     浏览器 mock 里没有这条 IPC，还是读不到。补成 http(s) 绝对地址后才落到普通 <img> 分支。
//  ② svg 只有 451 字节，Vite 默认会**内联成 data URL**；而 data URL 里带逗号，srcset 解析会把它
//     当成「URL + 描述符」切开 → logo 的 <img src> 直接变空（表现为文档顶部一个破图图标）。
//     所以这两张图用 ?no-inline 拿到真实 URL。
//  ③ png 截图 400KB+，本来就不会内联，无需处理。
const assetUrl = (u: string): string => new URL(u, typeof location === 'undefined' ? 'http://localhost/' : location.href).href
const MOCK_README_ZH = currentProjectReadmeZh
  .replaceAll('brand/hai-logo-dark.svg', assetUrl(haiLogoDarkUrl))
  .replaceAll('brand/hai-logo-light.svg', assetUrl(haiLogoLightUrl))
  .replaceAll('brand/screenshot-main.png', assetUrl(screenshotMainUrl))
  .replaceAll('brand/screenshot-trace.png', assetUrl(screenshotTraceUrl))
  .replace(/\n$/, '') // 去掉文件末尾换行：行数与真实文件一致（wc -l = 426）

// —— mock 工作区文件（read_file / file_preview / 审批 diff 共用同一内容：真 bridge 从磁盘读同一文件）——
// read_file 读到几行就是几行（header 行数 = 内容实际行数）；edit_file 的 diff 只反映该工具自己的改动，
// 每个文件工具按各自参数给出不同结果（不共享同一份 canned 内容）。
const MOCK_FILES: Record<string, string[]> = {
  'auth.go': [
    'package auth',
    '',
    'import (',
    '\t"errors"',
    '\t"net/http"',
    '\t"time"',
    '',
    '\t"github.com/golang-jwt/jwt/v5"',
    ')',
    '',
    'var ErrUnauthorized = errors.New("unauthorized")',
    '',
    'func Parse(r *http.Request) (string, error) {',
    '\t// 解析 Bearer header…',
    '\tauth := r.Header.Get("Authorization")',
    '\tif !strings.HasPrefix(auth, "Bearer ") {',
    '\t\treturn "", ErrUnauthorized',
    '\t}',
    '\treturn auth[len("Bearer "):], nil',
    '}',
    '',
    'func Login(w http.ResponseWriter, r *http.Request) {',
    '\ttok, err := Parse(r)',
    '\tif err != nil {',
    '\t\tw.WriteHeader(500)',
    '\t\treturn',
    '\t}',
    '\t_ = tok',
    '\tw.WriteHeader(200)',
    '}',
  ],
  'auth_test.go': [
    'package auth',
    '',
    'import "testing"',
    '',
    'func TestLoginValid(t *testing.T) { … }',
    'func TestLoginNoToken(t *testing.T) { … }',
    '// 缺少：过期 token → 401、损坏 token → 401',
  ],
  'App.vue': [
    '<template>',
    '  <div class="app">',
    '    <h1>{{ msg }}</h1>',
    '    <button @click="count++">count is {{ count }}</button>',
    '  </div>',
    '</template>',
    '',
    '<script setup lang="ts">',
    'import { ref } from "vue"',
    'const msg = ref("hello")',
    'const count = ref(0)',
    '</script>',
    '',
    '<style scoped>',
    '.app { color: #42b883; }',
    'button { padding: 4px 12px; }',
    '</style>',
  ],
  // Markdown 样例：右侧「最终文件」的预览/纯文本双模式验证用（GFM 表格 + 列表 +
  // 围栏代码 + 足够长的正文以覆盖「预览容器可滚动」）。用独立文件而非 README.md ——
  // 产出卡片回归（e2e/verify_artifacts_card.mjs）以 README.md 为样本断言 .fp-code，
  // 复用同名会把那条用例的语义改掉。
  'guide.md': [
    '# 使用指南',
    '',
    '本文件用于验证 **Markdown 预览**模式。',
    '',
    '| 参数 | 说明 |',
    '| --- | --- |',
    '| level | 强度档位 |',
    '| model | 模型名 |',
    '',
    '- 支持 GFM 表格',
    '- 支持任务列表',
    '- 支持围栏代码高亮',
    '',
    '```go',
    'func main() {',
    '\tfmt.Println("hello")',
    '}',
    '```',
    '',
    '> 引用块：预览模式应保留语义结构。',
    '',
    ...Array.from({ length: 60 }, (_, i) => `第 ${i + 1} 段：用于验证预览容器可滚动、底部内容可达。`),
  ],
  // CSV 样例（右侧「最终文件」的表格/纯文本双模式验证用，见 e2e/verify_csv_preview.mjs）。
  // 刻意混入**真实世界才有的脏东西**，让 e2e 走的是与单测同一批边界：
  //   ① 字段内的逗号（"便宜, 常买"）      ② 字段内的换行（引号**从字段起始**才有引用语义）
  //   ③ 转义引号（""他说""）              ④ 管道符与 markdown 语法（*粗* / `码`）
  //   ⑤ 少一列的行（脏数据，末行只有 2 个字段）
  // 注意 ② 的写法：引号必须在**字段起始位**（",多行…"）才是「引用开始」，写在字段中部
  // （多行备注："…）按字面引号处理 → 换行仍会切成两行（Excel 同款，见 lib/csv.ts）。
  'sales.csv': [
    '产品,数量,单价,备注',
    '苹果,3,5.5,"便宜, 常买"',
    '梨,2,8,"多行备注：需要冷藏\n第二行说明"',
    '他说,1,0,"好的""没问题"""',
    '记号笔,4,12,*促销* `SKU-9527`',
    '凑数行,1,1',
  ],
  // 大文件闸：700 行数据（超 500 行上限）→ 验证「只渲染前 N 行 + 还有 M 行」提示。
  'big.csv': [
    'id,val',
    ...Array.from({ length: 700 }, (_, i) => `${i + 1},v${i + 1}`),
  ],
  // 项目 README：真实内容（见上面的 MOCK_README_ZH）。不用 README.md 这个名字 ——
  // 产出卡片回归（e2e/verify_artifacts_card.mjs）以 README.md 为样本断言 .fp-code，
  // 复用同名会把那条用例的语义改掉。
  'README.zh-CN.md': MOCK_README_ZH.split('\n'),
}

// —— mock 命令校验（与真 bridge 同规则：拒绝式失败，返回中文原因；null = 通过）——
// 放在模块级而非类内：纯函数、无状态，mock 里被 dispatch 直接调用。
const MOCK_HOOK_EVENTS = ['SessionStart', 'UserPromptSubmit', 'PreToolUse', 'PostToolUse', 'PostToolBatch', 'PreCompact', 'Stop']

export function validateMockMCPServers(servers: Record<string, MCPServerCfg>): string | null {
  for (const [name, c] of Object.entries(servers)) {
    if (!name.trim()) return '服务器名不能为空'
    const type = c?.type ?? 'stdio'
    if (!['stdio', 'http', 'streamable_http', 'sse'].includes(type)) return `未知 type ${type}`
    if (type === 'stdio' && !c?.command?.trim()) return `${name}: stdio 需 command`
    if (type !== 'stdio' && !c?.url?.trim()) return `${name}: 需 url`
  }
  return null
}

export function validateMockHooks(body: HookSectionView): string | null {
  for (const [ev, groups] of Object.entries(body.events ?? {})) {
    if (!MOCK_HOOK_EVENTS.includes(ev)) return `未知事件 ${ev}`
    for (const g of groups ?? []) {
      const m = g.matcher?.trim()
      if (m && !/^[A-Za-z0-9_\-|,]+$/.test(m)) {
        try {
          new RegExp(m)
        } catch {
          return `matcher 非法正则：${m}`
        }
      }
      for (const h of g.hooks ?? []) {
        const type = (h.type ?? '').trim().toLowerCase()
        if (type && type !== 'command') return `暂不支持的 handler 类型：${h.type}`
        if (!h.command?.trim()) return '命令不能为空'
        if (h.timeout != null && h.timeout < 0) return '超时不能为负数'
      }
    }
  }
  return null
}

// —— mock 虚拟文件树（@ 引用浏览/读取用；浏览器无真实文件系统，模拟工作区内外结构）——
const MOCK_FS_DIRS: Record<string, string[]> = {
  '/mock': ['workspace', 'proj-auth', 'proj-billing', 'proj-search', 'proj-notify'],
  '/mock/workspace': ['src', 'docs'],
  '/mock/workspace/src': [],
  '/mock/workspace/docs': [],
  '/mock/proj-auth': ['src'],
  '/mock/proj-auth/src': [],
  '/mock/proj-billing': ['src'],
  '/mock/proj-billing/src': [],
  '/mock/proj-search': ['src'],
  '/mock/proj-search/src': [],
  '/mock/proj-notify': ['src'],
  '/mock/proj-notify/src': [],
}
const MOCK_FS_FILES: Record<string, string[]> = {
  '/mock/workspace': ['auth.go', 'auth_test.go', 'README.md', 'README.zh-CN.md'],
  '/mock/proj-auth': ['auth.go', 'README.md', 'README.zh-CN.md'], // @ 面板首屏有文件条目可点选（verify 4a）
  '/mock/workspace/src': ['main.go', 'util.go'],
  '/mock/workspace/docs': ['guide.md'],
  '/mock/proj-auth/src': ['svc.go', 'App.vue'], // .vue 单文件组件：右侧文件栏高亮验证
  '/mock/proj-billing/src': ['billing.go'],
  '/mock/proj-search/src': ['search.go', 'index.go'],
  '/mock/proj-notify/src': ['notify.go'],
}

// mockFsChildren 虚拟目录子项（目录已知 → 子项列表；未知 → null 表示不存在）
function mockFsChildren(dir: string): { name: string; path: string; isDir: boolean }[] | null {
  const base = String(dir ?? '').replace(/\/+$/, '')
  if (!base.startsWith('/mock')) return null
  if (!Object.prototype.hasOwnProperty.call(MOCK_FS_DIRS, base)) return null
  const out: { name: string; path: string; isDir: boolean }[] = []
  for (const n of MOCK_FS_DIRS[base] ?? []) out.push({ name: n, path: `${base}/${n}`, isDir: true })
  for (const n of MOCK_FS_FILES[base] ?? []) out.push({ name: n, path: `${base}/${n}`, isDir: false })
  return out
}
function mockIsDir(p: string): boolean {
  return Object.prototype.hasOwnProperty.call(MOCK_FS_DIRS, p.replace(/\/+$/, ''))
}
function mockIsFile(p: string): boolean {
  const base = p.replace(/\/+$/, '')
  const i = base.lastIndexOf('/')
  const parent = base.slice(0, i)
  const name = base.slice(i + 1)
  return Boolean(MOCK_FS_FILES[parent]?.includes(name))
}

// mock 侧「算不算图片」的扩展名集合：与 electron/read-image.ts 的 IMAGE_MIMES 同集合，
// 但浏览器拿不到主进程模块，只能各自一份（漏一个的后果只是 mock 少回一种图，e2e 立刻能看见）。
const MOCK_IMAGE_EXTS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'avif', 'bmp', 'ico', 'svg'])

// read_file 工具结果：只显示请求的行区间（start/end 截断到文件实际行数；header 行数 = 内容实际行数）。
// 读到几行就是几行 —— 不同 read_file 调用按各自行区间给出不同内容。
function readResult(path: string, start: number, end: number): string {
  const lines = MOCK_FILES[path] ?? []
  const total = lines.length
  const s = Math.max(1, start || 1)
  const e = Math.min(total, end || total)
  const body = lines.slice(s - 1, e).map((ln, i) => `${s + i}: ${ln}`).join('\n')
  return `[read_file: ${path}, ${total} lines total, showing ${s}-${e}]\n${body}`
}

// edit_file（t-7）自己的 diff：只改 auth.go 的 Login 错误分支（与 t-7 的 old_string → new_string 一致）。
// 每个编辑/写文件工具展示它自己独立的变更，不与其他工具共享。
const EDIT_DIFF = [
  '--- auth.go',
  '+++ auth.go',
  '@@ -22,9 +22,14 @@',
  ' func Login(w http.ResponseWriter, r *http.Request) {',
  ' \ttok, err := Parse(r)',
  ' \tif err != nil {',
  '+\t\tif errors.Is(err, jwt.ErrTokenExpired) {',
  '+\t\t\tw.Header().Set("X-Auth-Error", "token_expired")',
  '+\t\t\tw.WriteHeader(401)',
  '+\t\t\treturn',
  '+\t\t}',
  ' \t\tw.WriteHeader(500)',
  ' \t\treturn',
  ' \t}',
  ' \t_ = tok',
  ' \tw.WriteHeader(200)',
  ' }',
].join('\n')

// MOCK_STATIC_PARTS 静态分区（系统提示词/技能/工具/MCP/其他）的组装字节估算：
// 口径同真 bridge —— 静态块每轮不变（固定展示），不随会话内容缩放。数值按 mock 自身
// 的演示量级（单轮 Input 200~1600）等比缩小，但保证 静态合计 < 最小锚点（否则面板会
// 进入「锚点过期」态，百分比合计 >100%，演示与 e2e 都会看到假告警）。
const MOCK_STATIC_PARTS: [string, number][] = [
  ['tools', 96], ['system_prompt', 58], ['skills', 16], ['other', 4], ['mcp', 14],
]

class MockTransport implements Transport {
  // 多订阅者（V2 P2-PROTOCOL-08）：onEvent 可注册多个回调，off 只移除自己的；
  // 单 cb 会被后注册覆盖、off 误伤所有订阅者（session_pin 一次性回调 + 长期订阅并存时丢事件）。
  private cbs = new Set<Emit>()
  private timers: ReturnType<typeof setTimeout>[] = []
  private persona = 'code'
  private model = 'deepseek-v4-flash-vision-exp'
  private mockProvider = 'deepseek' // Provider 设置热切换（mock：仅记录）
  private ws = '/mock/workspace' // 事件/响应回填 workspace：浏览器 mock 也走 store 事件门禁
  private sid = 'mock-1' // 当前活跃会话 id（事件流 session_id 缺省值）
  private states = new Map<string, MockRunState>() // 每会话运行态（多 session 并行，独立推进）
  // 每会话「当前上下文占用」锚点 = 最近一次 llm_end 的 Input（真 bridge = reducer.CtxAnchor）：
  // context_breakdown 据此结算分区的消息残差，保证 mock 下面板百分比与 ctx 环同源。
  // ctxEstimated 标记锚点口径（compress_end 的字符估算 → true，llm_end 真实 usage → false）。
  private ctxTokens = new Map<string, number>()
  private ctxEstimated = new Map<string, boolean>()
  private sessions = new Map<string, MockSess[]>() // workspace path → 该项目的会话列表
  private pickIdx = 0
  private titleIdx = 0

  start(_ws: string): Promise<{ ok: boolean; error?: string }> {
    return Promise.resolve({ ok: true })
  }

  // 浏览器无真实文件夹选择器：轮换假项目，演示项目树多项目；轮完回到已存在项目 → 该项目下再加会话
  async pickWorkspace(): Promise<string | null> {
    return MOCK_PROJECTS[this.pickIdx++ % MOCK_PROJECTS.length] ?? '/mock/workspace'
  }

  // 主目录：mock 无真实 home，「新建文件夹…」modal 默认父目录用 /mock
  homeDir(): Promise<string> {
    return Promise.resolve('/mock')
  }

  // 新建文件夹：浏览器无真 mkdir，直接返回合成路径（store 注册为工作区，会话树照常工作）
  async createWorkspaceFolder(parent: string, name: string): Promise<{ ok: boolean; path?: string; error?: string }> {
    const clean = name.trim()
    if (!clean || clean.includes('/')) return { ok: false, error: '无效的文件夹名称' }
    return { ok: true, path: `${parent.replace(/\/$/, '')}/${clean}` }
  }

  // —— 全局设置（mock：localStorage 假持久化；API key 不存明文，只记布尔 hasKey）——
  // MCP 配置（mock：演示两个服务器；mcp_list 状态由此派生）
  private mockMCPCfg: Record<string, MCPServerCfg> = {
    github: { type: 'stdio', command: 'npx', args: ['-y', '@modelcontextprotocol/server-github'], enabled: true },
    memory: { type: 'http', url: 'http://localhost:8080/mcp', enabled: true },
  }
  // 已保存市场（mock：初始一个演示市场；market_* 命令维护）
  private mockMarkets: { name: string; url: string; added_at: string }[] = [
    { name: 'skill-market', url: 'github.com/awesome/skill-market', added_at: ts() },
  ]
  // MCP 服务器状态：source 标注配置分层来源（user=全局 settings，project=项目文件）。
  // 演示一台项目层服务器（真 bridge 由 {ws}/.mcp.json 提供）——UI 的项目维度查看据此可验。
  private mockProjectMCP: { name: string; type: string; command: string }[] = [
    { name: 'repo-tools', type: 'stdio', command: 'npx -y @acme/repo-mcp' },
  ]
  private mcpStatus(cfg: Record<string, MCPServerCfg>): { name: string; type: string; command: string; enabled: boolean; state: string; tool_count: number; error: string; source: string }[] {
    const rows: { name: string; type: string; command: string; enabled: boolean; state: string; tool_count: number; error: string; source: string }[] = Object.entries(cfg).map(([name, c]) => {
      const args = c.args ?? []
      return {
        name,
        type: c.type ?? 'stdio',
        command: c.type === 'stdio' ? `${c.command ?? ''}${args.length ? ' ' + args.join(' ') : ''}` : (c.url ?? ''),
        enabled: c.enabled ?? true,
        state: 'connected',
        tool_count: Math.max(1, (name.length * 3) % 12),
        error: '',
        source: 'user',
      }
    })
    for (const p of this.mockProjectMCP) {
      rows.push({ ...p, enabled: true, state: 'connected', tool_count: 2, error: '', source: 'project' })
    }
    return rows
  }

  // 配置层状态（真 bridge mcp_list.layers：三层文件 + 损坏恢复可见）。mock 固定演示：
  // 用户层有效、项目 .mcp.json 有效（1 台）、原生项目文件未创建。
  private mockMCPLayers(ws: string) {
    return [
      { path: `${this.mockHome()}/.go-code/settings.json`, source: 'user', present: true, ok: true, stale: false, servers: Object.keys(this.settings().mcpServers).length, updated_at: ts() },
      { path: `${ws}/.mcp.json`, source: 'project', present: true, ok: true, stale: false, servers: this.mockProjectMCP.length, updated_at: ts() },
      { path: `${ws}/.go-code/settings.json`, source: 'project', present: false, ok: true, stale: false, servers: 0 },
    ]
  }
  private mockHome(): string {
    return '/mock/home'
  }

  // —— 项目级 MCP / hooks 文件（mock）——
  // 真 bridge 读/写 {ws}/.mcp.json（只读）与 {ws}/.go-code/settings.json（可编辑）、
  // {ws}/.go-code/hooks.json（可编辑）；mock 用一张内存表模拟，让「全局 × 项目」两级面板可验。
  private mockProjectFiles: Record<string, { mcpJson?: Record<string, MCPServerCfg>; goCode?: Record<string, MCPServerCfg>; hooks?: HookSectionView; claudeHooks?: HookSectionView }> = {
    '/mock/workspace': {
      mcpJson: { 'repo-tools': { type: 'stdio', command: 'npx', args: ['-y', '@acme/repo-mcp'] } },
      goCode: { 'scratch-notes': { type: 'stdio', command: 'uvx', args: ['notes-mcp'] } },
      hooks: { events: { PreToolUse: [{ matcher: 'bash|write_file', hooks: [{ type: 'command', name: 'lint', command: '/mock/workspace/.go-code/hooks/pre-tool.sh', timeout: 10 }] }] } },
      claudeHooks: { events: { PostToolUse: [{ matcher: 'write_file', hooks: [{ type: 'command', command: 'echo claude-compat', timeout: 5 }] }] } },
    },
    '/mock/proj-auth': { mcpJson: { 'auth-db': { type: 'stdio', command: 'npx', args: ['-y', '@acme/auth-mcp'] } } },
    '/mock/proj-billing': {
      hooks: { events: { Stop: [{ hooks: [{ type: 'command', name: 'notify', command: '/mock/proj-billing/.go-code/hooks/stop.sh', timeout: 600 }] }] } },
    },
  }
  // 用户级 hooks（~/.go-code/settings.json 的 hooks 段；可编辑）
  private mockUserHooks: HookSectionView = {
    allowProjectHooks: true,
    events: {
      UserPromptSubmit: [{ hooks: [{ type: 'command', name: 'works-task-start', command: '/mock/home/.codex/hooks/works-task-start.sh', timeout: 600 }] }],
      Stop: [{ hooks: [{ type: 'command', name: 'works-task-stop', command: '/mock/home/.codex/hooks/stop.sh', timeout: 600 }] }],
    },
  }
  // Claude Code 兼容层（只读；~/.claude/settings.json）
  private mockClaudeUserHooks: HookSectionView = {
    events: { SessionStart: [{ hooks: [{ type: 'command', command: 'echo claude-session-start', timeout: 5 }] }] },
  }
  // 信任库（mock）：命令 → 状态；未登记的项目命令默认 untrusted（安全默认，与真实现一致）
  private mockTrust: Record<string, 'managed' | 'trusted' | 'untrusted' | 'modified'> = {}

  // mockProjectMCPInfo 一个项目的项目层 MCP 视图（契约同真 bridge mcp_projects）。
  private mockProjectMCPInfo(path: string): MCPProjectInfo {
    const f = this.mockProjectFiles[path] ?? {}
    const mcpJson = f.mcpJson ?? {}
    const goCode = f.goCode ?? {}
    const running = path === this.ws
    const rows: MCPProjectServer[] = []
    const push = (name: string, c: MCPServerCfg, file: string, editable: boolean) => {
      const exist = rows.findIndex((r) => r.name === name)
      const row: MCPProjectServer = {
        name,
        type: c.type ?? 'stdio',
        command: c.command,
        args: c.args,
        url: c.url,
        env: c.env,
        enabled: c.enabled ?? true,
        file,
        editable,
        ...(running ? { state: 'connected', tool_count: Math.max(1, name.length % 5) } : {}),
      }
      if (exist >= 0) rows[exist] = row // 同名：后层（settings.json）覆盖前层（.mcp.json）
      else rows.push(row)
    }
    for (const [n, c] of Object.entries(mcpJson)) push(n, c, `${path}/.mcp.json`, false)
    for (const [n, c] of Object.entries(goCode)) push(n, c, `${path}/.go-code/settings.json`, true)
    return {
      path,
      name: path.split('/').filter(Boolean).pop() ?? path,
      // 存在性：mock 已知项目 = 存在；其余（例如已删除/已移除的项目路径）= 不存在 → 前端过滤掉
      exists: MOCK_EXISTING_PROJECTS.includes(path),
      running,
      layers: [
        { path: `${path}/.mcp.json`, present: Object.keys(mcpJson).length > 0, ok: true, stale: false, servers: Object.keys(mcpJson).length },
        { path: `${path}/.go-code/settings.json`, present: Object.keys(goCode).length > 0, ok: true, stale: false, servers: Object.keys(goCode).length },
      ],
      servers: rows,
    }
  }

  // mockHooksInfo hooks 来源汇总（契约同真 bridge hooks_list）。
  private mockHooksInfo(paths: string[]) {
    const commands = (b?: HookSectionView): string[] => {
      const out: string[] = []
      for (const groups of Object.values(b?.events ?? {})) {
        for (const g of groups ?? []) for (const h of g.hooks ?? []) if (h.command) out.push(h.command)
      }
      return out
    }
    const trust: Record<string, string> = {}
    for (const c of commands(this.mockUserHooks)) trust[c] = this.mockTrust[c] ?? 'trusted' // 用户级默认可信
    for (const c of commands(this.mockClaudeUserHooks)) trust[c] = this.mockTrust[c] ?? 'trusted'
    for (const p of paths) {
      const f = this.mockProjectFiles[p] ?? {}
      for (const c of [...commands(f.hooks), ...commands(f.claudeHooks)]) trust[c] = this.mockTrust[c] ?? 'untrusted'
    }
    const body = (b?: HookSectionView) => (b ?? {})
    return {
      user: {
        id: 'user', kind: 'user', title: '用户配置', path: `${this.mockHome()}/.go-code/settings.json`,
        present: true, ok: true, editable: true, body: body(this.mockUserHooks),
      },
      claudeUser: {
        id: 'claude-user', kind: 'claude-user', title: 'Claude 兼容（只读）', path: `${this.mockHome()}/.claude/settings.json`,
        present: true, ok: true, editable: false, body: body(this.mockClaudeUserHooks),
      },
      projects: paths.map((p) => {
        const f = this.mockProjectFiles[p] ?? {}
        return {
          id: `project:${p}`, kind: 'project', title: p.split('/').filter(Boolean).pop() ?? p,
          workspace: p, path: `${p}/.go-code/hooks.json`, exists: true,
          present: Boolean(f.hooks), ok: true, editable: true, body: body(f.hooks),
          claude: f.claudeHooks ? [{ path: `${p}/.claude/settings.json`, present: true, ok: true, body: f.claudeHooks }] : [],
        }
      }),
      // 全局开关的「合并视角」：真 bridge 由 hooks.Load 合并各层得出；mock 以用户层为准
      // （项目层自带的 allowProjectHooks 在真实实现里也算数，mock 不模拟这一档）
      effective: {
        disableAllHooks: Boolean(this.mockUserHooks.disableAllHooks),
        allowProjectHooks: Boolean(this.mockUserHooks.allowProjectHooks),
        requireTrust: Boolean(this.mockUserHooks.requireTrust),
        readClaudeSettings: this.mockUserHooks.readClaudeSettings !== false,
        sources: [`${this.mockHome()}/.go-code/settings.json`, `${this.mockHome()}/.claude/settings.json`],
      },
      trust,
      warnings: [],
    }
  }

  // 内置预设模型价表（mock 版：与真 bridge list_provider_presets 的 models_cost 对齐，
  // 让 mock 模式打开设置即回显内置价）。deepseek 直连价 + opencode 网关价。
  private builtinModelsCost(presets: { id: string; models: string[] }[]): Record<string, Record<string, { input: number; cache_read: number; cache_write: number; output: number }>> {
    const priceOf: Record<string, { input: number; cache_read: number; output: number }> = {
      'deepseek-v4-flash': { input: 0.14, cache_read: 0.014, output: 0.28 },
      'deepseek-v4-flash-vision-exp': { input: 0.14, cache_read: 0.014, output: 0.28 },
      'deepseek-v4-pro': { input: 0.435, cache_read: 0.003625, output: 0.87 },
      'deepseek-chat': { input: 0.28, cache_read: 0.028, output: 0.56 },
      'glm-5.1': { input: 1.4, cache_read: 0.26, output: 4.4 },
      'glm-5.2': { input: 1.4, cache_read: 0.26, output: 4.4 },
      'glm-5.3': { input: 1.4, cache_read: 0.26, output: 4.4 },
      'kimi-k3': { input: 3, cache_read: 0.3, output: 15 },
      'kimi-k2.7-code': { input: 0.95, cache_read: 0.19, output: 4 },
      'kimi-k2.6': { input: 0.95, cache_read: 0.16, output: 4 },
      'mimo-v2.6-pro': { input: 0.435, cache_read: 0.003625, output: 0.87 },
      'mimo-v2.6-flash': { input: 0.14, cache_read: 0.0028, output: 0.28 },
      'mimo-v2.5': { input: 0.14, cache_read: 0.0028, output: 0.28 },
      'mimo-v2.5-pro': { input: 0.435, cache_read: 0.003625, output: 0.87 },
      'hy3': { input: 0.14, cache_read: 0.035, output: 0.58 },
    }
    const out: Record<string, Record<string, { input: number; cache_read: number; cache_write: number; output: number }>> = {}
    for (const p of presets) {
      const costs: Record<string, { input: number; cache_read: number; cache_write: number; output: number }> = {}
      for (const m of p.models) {
        const pr = priceOf[m]
        if (pr) costs[m] = { input: pr.input, cache_read: pr.cache_read, cache_write: 0, output: pr.output }
      }
      if (Object.keys(costs).length) out[p.id] = costs
    }
    return out
  }
  private settings(): AppSettings {
    const DEF: AppSettings = {
      theme: 'dark',
      lang: 'zh',
      browser_engine: 'mcp',
      provider: {
        active: 'deepseek',
        deepseek: {
          base_url: 'https://api.deepseek.com/v1',
          models: { 'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000, 'deepseek-chat': 64000 },
          max_tokens: { 'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000, 'deepseek-chat': 8192 },
          protocol: 'chat_completions',
          input_types: { 'deepseek-v4-flash-vision-exp': ['text', 'image'] }, // DeepSeek 视觉模型
        },
        openai: {
          base_url: 'https://api.openai.com/v1',
          models: { 'gpt-4o': 128000 },
          max_tokens: { 'gpt-4o': 16384 },
          protocol: 'chat_completions',
          input_types: { 'gpt-4o': ['text', 'image'], 'gpt-4o-mini': ['text', 'image'], 'gpt-4.1': ['text', 'image'] },
        },
        opencode: {
          base_url: 'https://opencode.ai/zen/go/v1',
          models: { 'deepseek-v4-flash': 1000000, 'deepseek-v4-pro': 1000000, 'deepseek-v4-flash-vision-exp': 1000000, 'glm-5.1': 202752, 'glm-5.2': 1000000, 'glm-5.3': 1000000, 'glm-5.3-flash': 1000000, 'kimi-k3': 1048576, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 262144, 'kimi-k2.5': 262144, 'mimo-v2.6-pro': 1048576, 'mimo-v2.6-flash': 1048576, 'mimo-v2.5': 1000000, 'mimo-v2.5-pro': 1048576, 'hy3': 256000, 'hy4-preview': 1024000, 'qwen3.6-plus': 1000000, 'qwen3.7-max': 1000000, 'qwen3.7-plus': 1000000, 'qwen3.8-max': 1000000, 'minimax-m2.7': 204800, 'minimax-m3': 1000000, 'gpt-5.6-luna': 1050000, 'grok-4.6': 500000, 'muse-spark-1.2-contributor': 1048576, 'longcat-2.0': 1000000 },
          max_tokens: { 'deepseek-v4-flash': 384000, 'deepseek-v4-pro': 384000, 'deepseek-v4-flash-vision-exp': 384000, 'glm-5.1': 32768, 'glm-5.2': 131072, 'glm-5.3': 131072, 'glm-5.3-flash': 131072, 'kimi-k3': 131072, 'kimi-k2.7-code': 262144, 'kimi-k2.6': 65536, 'kimi-k2.5': 262144, 'mimo-v2.6-pro': 131072, 'mimo-v2.6-flash': 131072, 'mimo-v2.5': 128000, 'mimo-v2.5-pro': 128000, 'hy3': 64000, 'hy4-preview': 64000, 'qwen3.6-plus': 65536, 'qwen3.7-max': 65536, 'qwen3.7-plus': 65536, 'qwen3.8-max': 131072, 'minimax-m2.7': 131072, 'minimax-m3': 131072, 'gpt-5.6-luna': 128000, 'grok-4.6': 500000, 'muse-spark-1.2-contributor': 131072, 'longcat-2.0': 131072 },
          protocol: 'chat_completions',
          input_types: {}, // 默认纯文本；多模态模型可在 Provider 设置勾选「图片」
        },
        kimi: {
          base_url: 'https://api.moonshot.cn/v1',
          models: { 'kimi-k2.6': 262144, 'kimi-k2.7-code': 262144, 'kimi-k2.7-code-highspeed': 262144, 'kimi-k2.7': 262144, 'kimi-k2.5': 262144, 'kimi-k2-thinking': 262144, 'kimi-k3': 1048576 },
          max_tokens: { 'kimi-k2.6': 262144, 'kimi-k2.7-code': 262144, 'kimi-k2.7-code-highspeed': 262144, 'kimi-k2.7': 262144, 'kimi-k2.5': 262144, 'kimi-k2-thinking': 262144, 'kimi-k3': 131072 },
          protocol: 'chat_completions',
          input_types: {}, // 默认纯文本；多模态模型可在 Provider 设置勾选「图片」
        },
        zhipu: {
          base_url: 'https://open.bigmodel.cn/api/paas/v4',
          models: { 'glm-4.6': 204800, 'glm-4.7': 204800, 'glm-5-turbo': 200000, 'glm-5.1': 202752, 'glm-5.2': 1000000, 'glm-5.2-highspeed': 1000000, 'glm-5.3': 1000000, 'glm-5.3-flash': 1000000, 'glm-5.3-highspeed': 1000000, 'glm-5': 1000000, 'glm-5v-turbo': 200000, 'glm-4.5-flash': 131072, 'glm-4.5': 131072, 'glm-4.5-air': 131072 },
          max_tokens: { 'glm-4.6': 131072, 'glm-4.7': 131072, 'glm-5-turbo': 131072, 'glm-5.1': 32768, 'glm-5.2': 131072, 'glm-5.2-highspeed': 131072, 'glm-5.3': 131072, 'glm-5.3-flash': 131072, 'glm-5.3-highspeed': 131072, 'glm-5': 131072, 'glm-5v-turbo': 131072, 'glm-4.5-flash': 98304, 'glm-4.5': 98304, 'glm-4.5-air': 98304 },
          protocol: 'chat_completions',
          input_types: {}, // 默认纯文本；多模态模型可在 Provider 设置勾选「图片」
        },
      },
      mcpServers: this.mockMCPCfg,
      agent: { reminder_rounds: 30, max_subagents: 1000, max_explore_subagents: 1000 },
      runtime: { stop_background_on_interrupt: false }, // 主停止只停主会话（S3-B mock 缺省 false）
      stt: { enabled: false }, // 语音输入默认关闭
    }
    try {
      const raw = JSON.parse(localStorage.getItem('go-code.settings') ?? '{}') as Partial<AppSettings>
      const provider: AppSettings['provider'] = { ...DEF.provider, ...(raw.provider ?? {}) }
      for (const k of MOCK_BUILTIN_PROVIDERS) provider[k] = { ...DEF.provider[k], ...(raw.provider?.[k] ?? {}) }
      // 自定义 provider：补齐 ProviderCfg 缺省（避免 UI 读 undefined）
      for (const [k, v] of Object.entries(provider)) {
        if (k === 'active' || (MOCK_BUILTIN_PROVIDERS as readonly string[]).includes(k)) continue
        const cfg = v as ProviderCfg | undefined
        // 白名单重建：与 electron provider:save / bridge loadSettings 读取的字段保持一致 ——
        // 漏字段会让 mock 静默丢配置（usage_input_includes_cache = usage 口径标注、
        // prices = 每模型价表覆盖，两者都曾被这里丢掉）。
        provider[k] = {
          base_url: cfg?.base_url ?? '', models: cfg?.models ?? {}, max_tokens: cfg?.max_tokens ?? {},
          protocol: cfg?.protocol ?? 'chat_completions', openai_compat: cfg?.openai_compat,
          input_types: cfg?.input_types ?? {}, auth_type: cfg?.auth_type ?? 'api_key',
          // 可选字段用条件展开而不是 `key: undefined`：electron 侧「清除标注」是 delete
          // 字段（不存在 ≠ false），mock 必须同样**不留键**，否则 UI/测试读到的「自动」
          // 状态与真实持久化不一致。
          ...(cfg?.prices ? { prices: cfg.prices } : {}),
          ...(typeof cfg?.usage_input_includes_cache === 'boolean' ? { usage_input_includes_cache: cfg.usage_input_includes_cache } : {}),
          ...(typeof cfg?.cache_ttl_1h === 'boolean' ? { cache_ttl_1h: cfg.cache_ttl_1h } : {}),
        }
      }
      return {
        ...DEF,
        ...raw,
        provider,
        mcpServers: raw.mcpServers ?? DEF.mcpServers,
        agent: { ...DEF.agent!, ...(raw.agent ?? {}) }, // DEF.agent 类型是可选（AppSettings.agent?），! 断言必有（mock 恒有默认）
      }
    } catch {
      return DEF
    }
  }
  private saveSettings(s: AppSettings): void {
    try {
      localStorage.setItem('go-code.settings', JSON.stringify(s))
    } catch {
      /* 配额/隐私模式忽略 */
    }
  }
  private hasKeys(): Record<string, boolean> {
    try {
      return JSON.parse(localStorage.getItem('go-code.provider-keys') ?? '{}')
    } catch {
      return {}
    }
  }
  private setHasKey(provider: string, v: boolean): void {
    try {
      localStorage.setItem('go-code.provider-keys', JSON.stringify({ ...this.hasKeys(), [provider]: v }))
    } catch {
      /* ignore */
    }
  }

  async settingsGet(): Promise<AppSettings> {
    return this.settings()
  }

  async externalSkillsStatus(): Promise<ExternalSkillsStatus> {
    // 浏览器 mock 模拟「两个外部工具目录均存在」的可用状态，便于设置页演示。
    return {
      enabled: true,
      sources: [
        { name: 'codex', root: '/mock/.codex', skills_dir: '/mock/.codex/skills', root_exists: true, skills_dir_exists: true, skill_count: 2 },
        { name: 'claude', root: '/mock/.claude', skills_dir: '/mock/.claude/skills', root_exists: true, skills_dir_exists: true, skill_count: 1 },
      ],
    }
  }

  async importExternalSkills(): Promise<{ ok: boolean; result?: ExternalSkillsImportResult; error?: string }> {
    return {
      ok: true,
      result: {
        status: await this.externalSkillsStatus(),
        imported: [
          { source: 'codex', name: 'code-review', target: '/mock/.agents/skills/code-review' },
          { source: 'claude', name: 'doc-writer', target: '/mock/.agents/skills/doc-writer' },
        ],
      },
    }
  }

  // mock：无法弹系统「选择应用程序」对话框，返回固定演示 App（验证 UI 选择流程）。
  async pickApp(): Promise<{ name: string; bundleId?: string } | null> {
    return { name: 'Calculator', bundleId: 'com.apple.Calculator' }
  }
  async settingsSet(patch: Partial<AppSettings>): Promise<{ ok: boolean; error?: string; settings?: AppSettings }> {
    const cur = this.settings()
    const provider: AppSettings['provider'] = { ...cur.provider }
    for (const [k, v] of Object.entries(patch.provider ?? {})) {
      if (k === 'active') provider.active = v as string
      else if (isValidProviderName(k)) provider[k] = { ...((cur.provider[k] as ProviderCfg | undefined) ?? {}), ...(v as ProviderCfg) }
    }
    const next: AppSettings = { ...cur, ...patch, provider }
    this.saveSettings(next)
    return { ok: true, settings: next }
  }
  // mock：浏览器直连 OpenAI 兼容端点（无 CORS 保护；仅开发调试用，真实走 Electron 主进程）
  async sttTranscribe(audio: ArrayBuffer, mimeType: string): Promise<{ ok: boolean; text?: string; error?: string }> {
    const stt = this.settings().stt
    const baseUrl = (stt?.base_url ?? '').trim().replace(/\/+$/, '')
    const model = (stt?.model ?? '').trim()
    const apiKey = (stt?.api_key ?? '').trim()
    if (!baseUrl || !model || !apiKey) return { ok: false, error: '语音模型未配置' }
    try {
      const fd = new FormData()
      const ext = mimeType.includes('webm') ? 'webm' : mimeType.includes('ogg') ? 'ogg' : 'wav'
      fd.append('file', new Blob([audio], { type: mimeType }), `recording.${ext}`)
      fd.append('model', model)
      const res = await fetch(`${baseUrl}/audio/transcriptions`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${apiKey}` },
        body: fd,
      })
      if (!res.ok) return { ok: false, error: `转写失败（HTTP ${res.status}）` }
      const data = (await res.json().catch(() => null)) as { text?: string } | null
      const text = (data?.text ?? '').trim()
      return text ? { ok: true, text } : { ok: false, error: '转写结果为空' }
    } catch (e) {
      return { ok: false, error: e instanceof Error ? e.message : String(e) }
    }
  }
  async providerSave(p: ProviderSaveInput): Promise<{ ok: boolean; hasKey?: boolean; error?: string }> {
    if (!isValidProviderName(p.provider)) return { ok: false, error: '无效 provider 名称' }
    if (p.active && !isValidProviderName(p.active)) return { ok: false, error: '无效 provider 名称' }
    const cur = this.settings()
    if (p.active) cur.provider.active = p.active
    const pc = (cur.provider[p.provider] as ProviderCfg | undefined) ?? { base_url: '', models: {}, max_tokens: {}, protocol: 'chat_completions', input_types: {}, auth_type: 'api_key' }
    delete pc.deleted // 重新保存 = 重新启用（对齐 electron provider:save）
    if (p.base_url) pc.base_url = p.base_url
    if (p.openai_compat && typeof p.openai_compat === 'object') pc.openai_compat = p.openai_compat
    if (p.protocol === 'chat_completions' || p.protocol === 'responses' || p.protocol === 'anthropic') pc.protocol = p.protocol
    if (p.models) pc.models = p.models
    if (p.max_tokens) pc.max_tokens = p.max_tokens
    if (p.prices) pc.prices = p.prices // 每模型价表覆盖（USD/1M tokens）——与 electron provider:save 一致
    if (p.input_types) pc.input_types = p.input_types // 模型输入类型（图片门控）——与 electron provider:save 一致
    if (p.auth_type === 'api_key' || p.auth_type === 'oauth') pc.auth_type = p.auth_type // OAuth 鉴权方式——与 electron provider:save 一致
    // usage 口径标注：bool = 写；显式 null = 清除（回自动）——与 electron provider:save 一致
    if (p.usage_input_includes_cache === true || p.usage_input_includes_cache === false) pc.usage_input_includes_cache = p.usage_input_includes_cache
    else if (p.usage_input_includes_cache === null) delete pc.usage_input_includes_cache
    if (p.cache_ttl_1h === true || p.cache_ttl_1h === false) pc.cache_ttl_1h = p.cache_ttl_1h
    else if (p.cache_ttl_1h === null) delete pc.cache_ttl_1h
    if (p.api_key) pc.api_key = p.api_key // 与 electron provider:save 一致：明文写回 settings.provider（bridge 读取主通道）
    cur.provider[p.provider] = pc
    this.saveSettings(cur)
    if (p.api_key) this.setHasKey(p.provider, true)
    return { ok: true, hasKey: this.hasKeys()[p.provider] ?? false }
  }
  async providerRemove(provider: string): Promise<{ ok: boolean; error?: string }> {
    if (!isValidProviderName(provider)) return { ok: false, error: '无效 provider 名称' }
    const cur = this.settings()
    // 内置 5 家：与 electron provider:remove 一致 —— deleted 标记（重启不复活，保留默认值供重新添加）
    if ((MOCK_BUILTIN_PROVIDERS as readonly string[]).includes(provider)) {
      const pc = cur.provider[provider]
      const cfg = pc && typeof pc !== 'string' ? pc : ({} as ProviderCfg)
      cur.provider[provider] = {
        base_url: cfg.base_url ?? '', models: cfg.models ?? {}, max_tokens: cfg.max_tokens ?? {},
        protocol: cfg.protocol ?? 'chat_completions', openai_compat: cfg.openai_compat,
        input_types: cfg.input_types ?? {}, prices: cfg.prices,
        auth_type: cfg.auth_type ?? 'api_key',
        ...(cfg.api_key ? { api_key: cfg.api_key } : {}),
        deleted: true,
      } as ProviderCfg
    } else {
      delete cur.provider[provider]
    }
    if (cur.provider.active === provider) cur.provider.active = 'deepseek'
    this.saveSettings(cur)
    if (this.hasKeys()[provider]) this.setHasKey(provider, false)
    return { ok: true }
  }
  async providerKeyStatus(provider: string): Promise<boolean> {
    return this.hasKeys()[provider] ?? false
  }
  // OAuth 订阅登录（2026-08 P2）：mock 环境用本地模拟（localStorage 标记登录态）。
  async oauthLogin(provider: string): Promise<{ ok: boolean; error?: string }> {
    // 模拟登录成功：记录 oauth 登录态
    const cur = this.settings()
    const pc = (cur.provider[provider] as ProviderCfg | undefined) ?? { base_url: '', models: {}, max_tokens: {} }
    pc.auth_type = 'oauth'
    cur.provider[provider] = pc
    this.saveSettings(cur)
    this.setHasKey(provider, true) // mock 视 OAuth 登录 = 已配置
    return { ok: true }
  }
  async oauthLogout(provider: string): Promise<{ ok: boolean; error?: string }> {
    const cur = this.settings()
    const pc = (cur.provider[provider] as ProviderCfg | undefined)
    if (pc) {
      pc.auth_type = 'api_key'
      cur.provider[provider] = pc
      this.saveSettings(cur)
    }
    this.setHasKey(provider, false)
    return { ok: true }
  }
  async oauthStatus(provider: string): Promise<OAuthStatus> {
    const pc = this.settings().provider[provider] as ProviderCfg | undefined
    const loggedIn = pc?.auth_type === 'oauth'
    return { provider, logged_in: loggedIn, supports_oauth: true, source: loggedIn ? 'oauth' : undefined }
  }
  async oauthPromptAnswer(provider: string, value: string, requestId?: string): Promise<{ ok: boolean; error?: string }> {
    return { ok: true } // mock：直接接受
  }
  async mcpSave(servers: Record<string, MCPServerCfg>): Promise<{ ok: boolean; error?: string }> {
    const cur = this.settings()
    cur.mcpServers = servers
    this.saveSettings(cur)
    return { ok: true }
  }

  // —— @ 引用文件系统（浏览器 mock：虚拟目录树，对齐 Electron 主进程 fs 返回形状）——
  async fsList(dir: string): Promise<{ ok: boolean; path: string; entries: { name: string; path: string; isDir: boolean; size?: number }[]; error?: string }> {
    const entries = mockFsChildren(dir)
    if (!entries) return { ok: false, path: dir, entries: [], error: 'mock 无此目录' }
    return { ok: true, path: dir, entries }
  }

  async fsReadRef(req: { path: string; workspace: string }): Promise<{ ok: boolean; content?: string; error?: string }> {
    const target = String(req?.path ?? '')
    if (!mockIsFile(target) && !mockIsDir(target)) return { ok: false, error: `mock 文件/目录不存在: ${target}` }
    const ws = String(req?.workspace ?? '')
    const rel = (abs: string) => {
      const r = abs.startsWith(ws + '/') ? abs.slice(ws.length) : abs
      return r && !r.startsWith('/') ? './' + r : r
    }
    if (mockIsDir(target)) {
      const lines = mockFsChildren(target)
        ?.filter((e) => !e.isDir)
        .map((e) => '  ' + rel(e.path)) ?? []
      return { ok: true, content: `Dir: ${rel(target)}\nFiles:\n` + (lines.length ? lines.join('\n') : '  (空目录)') }
    }
    const name = target.split('/').pop() ?? ''
    const content = (MOCK_FILES[name] ?? [`mock ${name}`]).join('\n')
    return { ok: true, content: `FilePath: ${rel(target)}\nFileContent:\n${content}` }
  }

  // 与 fsReadRef 同类（渲染进程给路径、主进程读盘），但只读图片、回 data URL 给 <img> 用。
  // 刻意**不**查虚拟文件树是否存在该路径：图片是预览的附属资源，e2e 关心的是「<img> 能不能
  // 拿到可渲染的 data URL」，而不是 mock 里有没有这个文件（存在性/敏感目录判定是真实主进程的事）。
  // 图片一律回同一张 SVG（含 .png 请求）：浏览器按 data URL 的 mime 解析，不看路径扩展名，
  // 这样每种图片扩展名在 e2e 里都拿到**同一尺寸**可测量。
  // 固定 120×40 是给 e2e 的锚点（断言 naturalWidth），别改。
  async fsReadImage(req: { path: string; workspace: string }): Promise<{ ok: boolean; dataUrl?: string; mime?: string; error?: string }> {
    const target = String(req?.path ?? '')
    const ext = (target.split('.').pop() ?? '').toLowerCase()
    if (!target.includes('.') || !MOCK_IMAGE_EXTS.has(ext)) return { ok: false, error: `不是图片文件: ${target}` }
    const name = target.split('/').pop() ?? ''
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="120" height="40"><rect width="120" height="40" fill="#d97a66"/><text x="8" y="26" font-size="14" fill="#ffffff">${name}</text></svg>`
    const mime = 'image/svg+xml'
    // utf8 → base64：文件名可能是中文，btoa 直接吃非 Latin-1 字符会炸。
    const b64 = btoa(Array.from(new TextEncoder().encode(svg), (b) => String.fromCharCode(b)).join(''))
    return { ok: true, dataUrl: `data:${mime};base64,${b64}`, mime }
  }

  async pickFsFile(): Promise<string | null> {
    return '/mock/workspace/auth.go'
  }

  // 多选（mock）：两个路径 —— 覆盖「添加文件」多选批量入 docRefs 的路径。
  async pickFsFiles(): Promise<string[]> {
    return ['/mock/workspace/auth.go', '/mock/workspace/README.md']
  }

  async pickFsDir(): Promise<string | null> {
    return '/mock/workspace/src'
  }

  // 浏览器 mock 无真实路径：给可读的假路径（覆盖「文档附件」全链路 UI）。
  // 空串语义（非磁盘文件）在 mock 下无法复现，浏览器 e2e 需要时注入 window.desktop。
  getPathForFile(file: File): string {
    return '/mock/' + (file.name || 'file')
  }

  async fsStat(p: string): Promise<{ ok: boolean; size?: number; isFile?: boolean; error?: string }> {
    const path = String(p ?? '').replace(/\/+$/, '')
    const name = path.split('/').pop() ?? ''
    if (mockIsFile(path)) return { ok: true, size: (MOCK_FILES[name] ?? []).join('\n').length, isFile: true }
    if (mockIsDir(path)) return { ok: true, size: 0, isFile: false }
    // getPathForFile 造出的 /mock/<name> 不在虚拟树里：给一个**确定性**的假体积，
    // 让附件条的「文件名 + 大小」分支在浏览器里能被完整渲染/断言（不是静默 0）。
    if (path.startsWith('/mock/')) return { ok: true, size: 1024 * (1 + (name.length % 9)), isFile: true }
    return { ok: false, error: `mock 路径不存在: ${p}` }
  }

  // xlsx 工作簿预览（mock）：不真转（无 Python/浏览器渲染），直接给一个失败结果并说明
  // 原因 —— 比「假成功 + 打不开的白面板」更诚实；e2e 需要成功路径时注入 window.desktop.watchXlsx。
  async watchXlsx(): Promise<{ ok: boolean; html_path?: string; error?: string }> {
    return { ok: false, error: '浏览器预览模式不支持 xlsx 转换（需桌面端受管运行时）' }
  }

  // docx 文档预览（mock）：同上 —— 不真转，直接给失败结果并说明原因，比「假成功 +
  // 打不开的白面板」更诚实；e2e 需要成功路径时注入 window.desktop.watchDocx。
  async watchDocx(): Promise<{ ok: boolean; html_path?: string; error?: string }> {
    return { ok: false, error: '浏览器预览模式不支持 docx 转换（需桌面端受管运行时）' }
  }

  // pptx 演示文稿预览（mock）：同上 —— 不真转，直接给失败结果并说明原因；e2e 需要成功
  // 路径时注入 window.desktop.watchPptx。
  async watchPptx(): Promise<{ ok: boolean; html_path?: string; error?: string }> {
    return { ok: false, error: '浏览器预览模式不支持 pptx 转换（需桌面端受管运行时）' }
  }

  private genSid(wsPath: string, n: number): string {
    return `mock-${wsPath.split('/').pop() || 'ws'}-${n}`
  }

  private nextTitle(): string {
    return MOCK_TITLES[this.titleIdx++ % MOCK_TITLES.length]
  }

  // 会话运行态（惰性创建；key = session_id，与 store views 路由一致）
  private state(sid: string): MockRunState {
    let st = this.states.get(sid)
    if (!st) {
      st = { id: sid, busy: false, askQueue: [], pendingApproval: null, pendingQuestion: null, lastAnswers: [], runSeq: 1, mode: 'auto', stopped: false }
      this.states.set(sid, st)
    }
    return st
  }

  // 命令归属会话：payload.session_id 优先（并行会话路由的锚），缺省回退当前活跃会话
  private cmdSid(cmd: BridgeCommand): string {
    const sid = cmd.payload?.session_id
    return typeof sid === 'string' && sid ? sid : this.sid
  }

  onEvent(cb: Emit): () => void {
    this.cbs.add(cb)
    return () => {
      this.cbs.delete(cb) // 只移除自己的回调
    }
  }
  onExit(_cb: (info: BridgeExitInfo) => void): () => void {
    return () => undefined
  }

  send(cmd: BridgeCommand): Promise<{ ok: boolean; error?: string }> {
    const ws = cmd.payload?.workspace
    if (typeof ws === 'string' && ws) this.ws = ws
    switch (cmd.type) {
      case 'list': {
        const wsPath = this.ws
        this.respond(cmd, { sessions: this.sessions.get(wsPath) ?? [] })
        break
      }
      case 'new_session': {
        const wsPath = this.ws
        const list = this.sessions.get(wsPath) ?? []
        const id = String(cmd.payload?.id ?? '')
        const existing = id ? list.find((s) => s.id === id) : undefined
        const wasLive = Boolean(existing && this.states.has(existing.id))
        let sess: MockSess
        if (existing) {
          sess = existing // 恢复已有会话（切项目/切会话）
        } else {
          sess = { id: id || this.genSid(wsPath, list.length + 1), title: this.nextTitle() }
          this.sessions.set(wsPath, [...list, sess])
        }
        this.sid = sess.id
        this.respond(cmd, { session_id: sess.id, sessions: this.sessions.get(wsPath) ?? [sess], todos: [], live: wasLive })
        break
      }
      case 'delete_session': {
        // 删除会话（mock：内存摘除 + session_closed 事件；真 bridge 另删磁盘日志）
        const wsPath = this.ws
        const sid = String((cmd.payload?.session_id as string) ?? '')
        const list = this.sessions.get(wsPath) ?? []
        this.sessions.set(wsPath, list.filter((s) => s.id !== sid))
        this.states.delete(sid) // 摘除运行态（脚本推进/队列一并丢弃）
        if (sid) this.emit({ event_type: 'session_closed', session_id: sid, timestamp: ts() })
        this.respond(cmd, { sessions: this.sessions.get(wsPath) ?? [] })
        break
      }
      case 'delete_workspace': {
        // 删除工作区（mock：内存摘除；真 bridge 另删 {ws}/.go-code 下的 skills.json 元数据，保留用户文件与项目配置）
        this.sessions.delete(this.ws)
        this.respond(cmd, {})
        break
      }
      case 'ask': {
        const sid = this.cmdSid(cmd)
        const text = String((cmd.payload?.text as string) || '')
        this.respond(cmd, {})
        this.ask(sid, text)
        break
      }
      case 'ask_batch': {
        // 客户端暂存队列一次性批量推送：每条消息 = 内容块数组（text/image），取各 text 块文本喂入
        const sid = this.cmdSid(cmd)
        const msgs = Array.isArray(cmd.payload?.messages) ? (cmd.payload.messages as Record<string, unknown>[]) : []
        const texts: string[] = []
        for (const m of msgs) {
          const blocks = Array.isArray(m?.content) ? (m.content as Record<string, unknown>[]) : []
          const t = blocks.filter((b) => b && b.type === 'text').map((b) => String(b.content)).join('\n').trim()
          if (t) texts.push(t)
        }
        this.respond(cmd, {})
        if (texts.length) this.askBatch(sid, texts)
        break
      }
      case 'approve': {
        const sid = this.cmdSid(cmd)
        const st = this.state(sid)
        const ok = Boolean(cmd.payload?.approve)
        this.respond(cmd, {})
        if (st.pendingApproval && ok) {
          const runId = st.pendingApproval.runId
          this.emit({
            event_type: 'tool_run_end', session_id: sid, id: st.pendingApproval.id, run_id: runId,
            name: 'edit_file', result: 'edited (+5 -0)', is_error: false,
            diff: { path: 'auth.go', added: 5, removed: 0, unified: EDIT_DIFF }, timestamp: ts(),
          })
          st.pendingApproval = null
          this.after(240, () => this.postApproval(sid, runId), sid)
        } else if (st.pendingApproval) {
          const runId = st.pendingApproval.runId
          this.emit({ event_type: 'tool_run_end', session_id: sid, id: st.pendingApproval.id, run_id: runId, name: 'edit_file', result: 'user rejected the edit (approval denied)', is_error: true, timestamp: ts() })
          st.pendingApproval = null
          this.after(200, () => this.postApproval(sid, runId), sid)
        }
        break
      }
      case 'answer_question': {
        // 用户回答 ask_user 提问批次：一次命令注入整批 → 工具结束（解阻）→ 继续执行
        const sid = this.cmdSid(cmd)
        const st = this.state(sid)
        const batchId = String((cmd.payload?.batch_id as string) || '')
        const answers = Array.isArray(cmd.payload?.answers)
          ? (cmd.payload?.answers as { question_id?: unknown; answer?: unknown }[]).map((a) => ({ questionId: String(a.question_id ?? ''), answer: String(a.answer ?? '') }))
          : []
        this.respond(cmd, {})
        if (st.pendingQuestion && batchId === st.pendingQuestion.batchId) {
          const runId = st.pendingQuestion.runId
          st.lastAnswers = answers
          const summary = answers.map((a) => a.answer).filter(Boolean).join('；') || '（全部跳过）'
          this.emit({ event_type: 'tool_run_end', session_id: sid, id: st.pendingQuestion.toolId, run_id: runId, name: 'ask_user', result: `用户回答：${summary}`, is_error: false, timestamp: ts() })
          st.pendingQuestion = null
          this.after(220, () => this.postQuestion(sid, runId), sid)
        }
        break
      }
      case 'interrupt': {
        const sid = this.cmdSid(cmd)
        this.state(sid).stopped = true
        this.respond(cmd, {})
        break
      }
      case 'promote_task': {
        // 运行中长工具 → 后台任务：匹配等待锚 → 发 task_promoted（工具行切后台态）+ 续跑脚本。
        // failPromote 钩子（演示文本含「promote失败」）：模拟真实失败时序（工具已结束/批已释放，
        // bridge 返回 task not found）→ 前端回滚乐观置位 + 行内错误提示（问题 1 回归断言用）。
        const sid = this.cmdSid(cmd)
        const taskId = String((cmd.payload?.task_id as string) || '')
        const st = this.state(sid)
        const p = st.promote
        if (st.failPromote || !p || p.taskId !== taskId) {
          this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error: `task not found: ${taskId}`, data: {}, workspace: this.ws })
          break
        }
        this.respond(cmd, {})
        st.promote = undefined
        st.bgTool = { taskId, runId: p.runId } // interrupt_task 精确停止锚（runTools 返回后仍有效）
        this.emit({ event_type: 'task_promoted', session_id: sid, task_id: taskId, kind: 'tool', name: p.name, timestamp: ts() })
        // 主 loop 已拿到占位 {task_id,status:running} → 模型继续；后台工具稍后完成 → 自动回传。
        // afterBg：promote 后用户立刻主停止（120ms 内）也不吞掉后台任务回传（对齐新语义）
        this.afterBg(120, () => this.promoteContinue(sid, p.runId), sid)
        break
      }
      case 'interrupt_task': {
        // 按 taskId 精确中断异步任务（右侧任务栏「停止」）：只影响目标任务，不停主会话。
        // 锚：已 promote 的后台工具（bgTool）或仍在等待 promote 的运行中工具（promote）。
        const sid = this.cmdSid(cmd)
        const taskId = String((cmd.payload?.task_id as string) || '')
        const st = this.state(sid)
        const bg = st.bgTool
        const p = st.promote
        const isBg = (bg && bg.taskId === taskId) || (p && p.taskId === taskId)
        if (!isBg) {
          this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error: `task not found: ${taskId}`, data: {}, workspace: this.ws })
          break
        }
        if (st.toolDone || st.toolInterrupted) {
          this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error: `task already finished: ${taskId}`, data: {}, workspace: this.ws })
          break
        }
        this.respond(cmd, {})
        st.toolInterrupted = true
        st.promote = undefined
        const runId = bg ? bg.runId : p!.runId
        // 后台工具被精确中断：工具立即结束（取消错误）+ 终态 interrupted 自动回传；
        // 主会话不停（promote 后主 loop 已继续，后续 llmTurn 照常推进）。
        // afterBg：终态交付属任务自身生命周期收敛，主停止在途也不吞（防任务卡卡「中断中」）
        this.emit({ event_type: 'tool_run_end', session_id: sid, id: 'b-1', run_id: runId, name: 'bash', result: 'context canceled', is_error: true, timestamp: ts() })
        this.afterBg(60, () => {
          this.emit({ event_type: 'task_result_delivered', session_id: sid, task_id: taskId, status: 'interrupted', result: 'context canceled', error: 'context canceled', name: 'bash', timestamp: ts() })
          st.bgTool = undefined
        }, sid)
        break
      }
      case 'compact': {
        const sid = this.cmdSid(cmd)
        this.respond(cmd, {})
        this.emit({ event_type: 'compress_start', session_id: sid, before: 45000 })
        this.after(180, () => {
          // 压缩后上下文占用下降（面板锚点同步，同真 bridge）。口径 = 字符估算（含
          // 静态前缀：系统提示词/技能/工具 schema），故仍高于静态合计 → 面板不 degraded。
          this.ctxTokens.set(sid, 268)
          this.ctxEstimated.set(sid, true)
          this.emit({ event_type: 'compress_end', session_id: sid, before: 45000, after: 21000, ctx_tokens: 268 })
        }, sid)
        break
      }
      case 'switch_mode': {
        const sid = this.cmdSid(cmd)
        this.state(sid).mode = String((cmd.payload?.mode as string) || 'auto')
        this.respond(cmd, {})
        break
      }
      case 'switch_persona': {
        const persona = String((cmd.payload?.persona as string) || 'code')
        this.persona = persona
        this.respond(cmd, { persona })
        break
      }
      case 'switch_model': {
        const model = String((cmd.payload?.model as string) || 'deepseek-v4-flash-vision-exp')
        this.model = model
        this.respond(cmd, { model })
        break
      }
      case 'set_provider': {
        // Provider 设置热切换（mock：只记 active，模型窗口走 settings）
        const prov = String((cmd.payload?.provider as string) || 'deepseek')
        this.mockProvider = prov
        this.respond(cmd, { active: prov })
        break
      }
      case 'list_models': {
        // Provider 面板「拉取模型列表」（mock：返回假模型清单 + 注册表风格 capabilities，
        // 含 cost 价表 —— 与真 bridge list_models 对齐，前端价格编辑器空覆盖回显内置价）
        const prov = String((cmd.payload?.provider as string) || 'deepseek')
        const fake = prov === 'openai'
          ? ['gpt-4o', 'gpt-4o-mini', 'gpt-4.1']
          : prov === 'opencode'
            ? ['deepseek-v4-flash', 'deepseek-v4-pro', 'glm-5.1', 'glm-5.2', 'glm-5.3', 'kimi-k3', 'kimi-k2.7-code', 'kimi-k2.6', 'mimo-v2.6-pro', 'mimo-v2.6-flash', 'mimo-v2.5', 'mimo-v2.5-pro', 'hy3']
            : ['deepseek-v4-flash-vision-exp', 'deepseek-chat', 'deepseek-reasoner']
        const caps: Record<string, { inputs?: string[]; context_window?: number; max_tokens?: number; cost?: { input?: number; cache_read?: number; cache_write?: number; output?: number } }> = {}
        const priceOf = this.builtinModelsCost([{ id: prov, models: fake }])[prov] ?? {}
        for (const m of fake) {
          caps[m] = { inputs: ['text'], context_window: 1_000_000, max_tokens: 8192 }
          const p = priceOf[m]
          if (p) caps[m]!.cost = { input: p.input, cache_read: p.cache_read, cache_write: 0, output: p.output }
        }
        this.respond(cmd, { provider: prov, models: fake, capabilities: caps })
        break
      }
      case 'list_provider_presets': {
        // 内置 Provider 预设（对齐真 bridge ← providers.json；mock 派生自 DEF 内置 5 家 + 常用预设）
        const DEF = this.settings()
        const presets = [
          { name: 'DeepSeek', id: 'deepseek', base_url: 'https://api.deepseek.com/v1', protocol: 'chat_completions', models: Object.keys(DEF.provider.deepseek.models ?? {}) },
          { name: 'OpenAI', id: 'openai', base_url: 'https://api.openai.com/v1', protocol: 'chat_completions', models: Object.keys(DEF.provider.openai.models ?? {}) },
          { name: 'OpenCode Go', id: 'opencode', base_url: 'https://opencode.ai/zen/go/v1', protocol: 'chat_completions', models: Object.keys(DEF.provider.opencode.models ?? {}) },
          { name: 'Kimi (Moonshot)', id: 'kimi', base_url: 'https://api.moonshot.cn/v1', protocol: 'chat_completions', models: Object.keys(DEF.provider.kimi.models ?? {}) },
          { name: '智谱 Zhipu', id: 'zhipu', base_url: 'https://open.bigmodel.cn/api/paas/v4', protocol: 'chat_completions', models: Object.keys(DEF.provider.zhipu.models ?? {}) },
          { name: 'Anthropic', id: 'anthropic', base_url: 'https://api.anthropic.com', protocol: 'anthropic', models: [], oauth: { name: 'Anthropic (Claude Pro/Max)', flow: 'anthropic', is_subscription: true } },
          { name: 'xAI (Grok)', id: 'xai', base_url: 'https://api.x.ai/v1', protocol: 'responses', models: [], oauth: { name: 'xAI (Grok/X subscription)', flow: 'xai', is_subscription: true, login_label: 'Sign in with SuperGrok or X Premium' } },
          { name: 'MiniMax', id: 'minimax', base_url: 'https://api.minimax.io/anthropic', protocol: 'anthropic', models: [] },
          { name: 'Fireworks', id: 'fireworks', base_url: 'https://api.fireworks.ai/inference', protocol: 'anthropic', models: [] },
          { name: 'OpenRouter', id: 'openrouter', base_url: 'https://openrouter.ai/api/v1', protocol: 'chat_completions', models: [], oauth: { name: 'OpenRouter', flow: 'openrouter', key_instead: true } },
          { name: 'OpenAI Codex', id: 'openai-codex', base_url: 'https://chatgpt.com/backend-api', protocol: 'responses', models: [], oauth: { name: 'OpenAI (ChatGPT Plus/Pro)', flow: 'openai-codex', is_subscription: true } },
          { name: 'GitHub Copilot', id: 'github-copilot', base_url: 'https://api.individual.githubcopilot.com', protocol: 'anthropic', models: [], oauth: { name: 'GitHub Copilot', flow: 'github-copilot', is_subscription: true } },
          { name: 'Kimi Coding', id: 'kimi-coding', base_url: 'https://api.kimi.com/coding', protocol: 'anthropic', models: [], oauth: { name: 'Kimi Code (subscription)', flow: 'kimi-coding', is_subscription: true, login_label: 'Sign in with Kimi Code' } },
        ]
        this.respond(cmd, { presets, models_cost: this.builtinModelsCost(presets) })
        break
      }
      case 'refresh_prices': {
        // 「从价源补价」（mock：假价目 + 与真桥同语义的合并 —— 默认只补「无价」模型，
        // overwrite=true 才覆盖已有价；prices 回传**整表**供前端 providerSave 落盘，
        // 与 desktop/bridge/price_refresh.go 的响应契约一致）
        const prov = String((cmd.payload?.provider as string) || this.settings().provider.active)
        // 与真桥一致：由 payload.overwrite 决定（store 默认发 true —— 补价即覆盖）
        const overwrite = Boolean(cmd.payload?.overwrite)
        const cfg = this.settings().provider[prov] as ProviderCfg | undefined
        const models = Object.keys(cfg?.models ?? {})
        const prices: Record<string, { input: number; cache_read: number; cache_write: number; output: number }> = { ...(cfg?.prices ?? {}) }
        const filled: string[] = []
        let skipped = 0
        for (const m of models) {
          if (prices[m] && !overwrite) {
            skipped++ // 已有价（手填或上次补的）：保守不动
            continue
          }
          prices[m] = { input: 0.5, cache_read: 0.05, cache_write: 0.625, output: 1.5 }
          filled.push(m)
        }
        this.emit({
          event_type: 'command_response', id: cmd.id, ok: true, workspace: this.ws,
          data: {
            provider: prov, source: 'mock', filled, filled_count: filled.length,
            skipped_count: skipped, total: models.length, prices,
            persisted: false, note: '价已即时生效（内存）；持久化请由前端经 providerSave 写 settings.json',
          },
        })
        break
      }

      case 'test_connection': {
        // Provider 设置「测试连接」（mock：有 key → 假成功 + 假模型数；无 key → 失败）
        const base = String((cmd.payload?.base_url as string) || '')
        const key = String((cmd.payload?.api_key as string) || '')
        if (!key) {
          this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error: '未配置 API key（请先在设置里填写）', data: {}, workspace: this.ws })
          break
        }
        const prov = String((cmd.payload?.provider as string) || 'deepseek')
        const fake = prov === 'openai'
          ? ['gpt-4o', 'gpt-4o-mini', 'gpt-4.1']
          : prov === 'opencode'
            ? ['deepseek-v4-flash', 'deepseek-v4-pro', 'glm-5.1', 'glm-5.2', 'glm-5.3', 'kimi-k3', 'kimi-k2.7-code', 'kimi-k2.6', 'mimo-v2.6-pro', 'mimo-v2.6-flash', 'mimo-v2.5', 'mimo-v2.5-pro', 'hy3']
            : ['deepseek-v4-flash-vision-exp', 'deepseek-chat', 'deepseek-reasoner']
        this.respond(cmd, { provider: prov, base_url: base, models: fake, count: fake.length })
        break
      }
      case 'file_preview': {
        const path = String((cmd.payload?.path as string) || '')
        // 二进制分支：真 bridge 会读头部做 LookBinary 判定，二进制返回 binary=true + 空 content
        // （避免前端把二进制喂给 shiki）。这里按扩展名模拟同一契约 —— 否则 e2e 覆盖不到
        // 「文件树点开 .xlsx」这条路径（产出卡入口另有专门分支，两者是不同的入口）。
        const ext = (path.split('.').pop() ?? '').toLowerCase()
        if (['xlsx', 'xlsm', 'xls', 'docx', 'doc', 'pptx', 'ppt', 'pdf', 'png', 'jpg', 'jpeg', 'gif'].includes(ext)) {
          this.respond(cmd, { path, content: '', lines: 0, truncated: false, binary: true })
          break
        }
        // 预览与 read_file 同源（MOCK_FILES）：点击工具里的文件路径 → 展示该文件真实内容。
        // 兼容两种路径形态：工具参数短名（"auth.go"）与文件树完整路径（"/mock/proj-auth/src/App.vue"）。
        const lines = MOCK_FILES[path] ?? MOCK_FILES[path.split('/').pop() ?? '']
        const content = lines ? lines.join('\n') : `// ${path}\n// （mock 示例：真 bridge 读取工作区文件）\n`
        this.respond(cmd, { path, content, lines: lines ? lines.length : 2, truncated: false, binary: false })
        break
      }
      case 'switch_effort':
      case 'shutdown':
        this.respond(cmd, {})
        if (cmd.type === 'shutdown') this.emit({ event_type: 'session_closed', session_id: this.sid, timestamp: ts() })
        break
      case 'skills': {
        // 工作区技能清单（SkillsPage + /skill 面板数据源；含启用标志）
        this.respond(cmd, {
          skills: MOCK_SKILLS.map((s) => ({ name: s.name, description: s.description, enabled: s.enabled })),
        })
        break
      }
      case 'context_breakdown': {
        // 上下文构成分区（Composer ctx 环 hover 面板）。口径与真 bridge 一致（2026-09-20
        // 重定）：静态分区按组装字节**固定**，消息分区 = 锚点 − 静态合计（残差），总量 =
        // 锚点（= 本会话最近一次 llm_end 的 Input / 压缩后的估算）。尚无锚点 → 纯估算
        // （estimated: true、anchor_source: none，与真 bridge 同语义）。
        const sid = this.cmdSid(cmd)
        const anchor = this.ctxTokens.get(sid) ?? 0
        const staticSum = MOCK_STATIC_PARTS.reduce((a, [, t]) => a + t, 0)
        const est = this.ctxEstimated.get(sid) === true
        const messages = anchor > 0 ? Math.max(0, anchor - staticSum) : 156
        const parts = [{ key: 'messages', tokens: messages }, ...MOCK_STATIC_PARTS.map(([key, tokens]) => ({ key, tokens }))]
        this.respond(cmd, {
          parts,
          total: anchor > 0 ? anchor : staticSum + messages,
          estimated: anchor <= 0 || est,
          anchor_source: anchor <= 0 ? 'none' : est ? 'estimate' : 'real',
          degraded: anchor > 0 && anchor < staticSum,
          message_count: 24,
        })
        break
      }
      case 'skill_toggle': {
        const name = String(cmd.payload?.name ?? '')
        const on = Boolean(cmd.payload?.enabled)
        const sk = MOCK_SKILLS.find((s) => s.name === name)
        this.respond(cmd, sk ? {} : {})
        if (sk) sk.enabled = on
        break
      }
      case 'skill_install': {
        // 远程技能下载（mock：把 URL 来源的技能加进清单；真 bridge 走 exec git）
        const url = String(cmd.payload?.url ?? '')
        const scope = String(cmd.payload?.scope ?? 'global')
        const name = String(cmd.payload?.name ?? (url.split('/').pop()?.split('#').shift()?.replace(/\.git$/, '') || 'remote-skill'))
        if (!MOCK_SKILLS.some((s) => s.name === name)) {
          MOCK_SKILLS.push({
            name, description: `远端技能（${scope}）：${url}`, instructions: `# ${name}\n\n来源：${url}\n（mock 示例：真 bridge 从 GitHub 拉取并校验）`,
            resources: [], enabled: true,
          })
        }
        this.respond(cmd, { skills: [{ name, scope }] })
        break
      }
      case 'skill_browse': {
        // 远程市场浏览（mock：按 URL 造几个假技能 + 已装/可更新标记；真 bridge 克隆仓库只读解析）
        const url = String(cmd.payload?.url ?? '')
        const base = url.split('/').pop()?.split('#').shift()?.replace(/\.git$/, '') || 'repo'
        const fake = [`${base}-lint`, `${base}-review`, `${base}-doc`].map((n, i) => ({
          name: n,
          description: `市场技能 ${i + 1}（mock 示例：${url}）`,
          installed: MOCK_SKILLS.some((s) => s.name === n),
          // 模拟：已安装的第二个技能有远端新 commit → 可更新（演示市场行「更新」路径；更新后消失）
          update_available: MOCK_SKILLS.some((s) => s.name === n) && i === 1 && !MOCK_UPDATED.has(n),
        }))
        this.respond(cmd, { skills: fake })
        break
      }
      case 'skill_uninstall': {
        const name = String(cmd.payload?.name ?? '')
        const idx = MOCK_SKILLS.findIndex((s) => s.name === name)
        if (idx >= 0) MOCK_SKILLS.splice(idx, 1)
        this.respond(cmd, {})
        break
      }
      case 'skill_update':
        MOCK_UPDATED.add(String(cmd.payload?.name ?? ''))
        this.respond(cmd, {})
        break
      // —— 市场管理（SkillsPage）：保存/移除/清单 + 按市场更新 ——
      case 'market_add': {
        const url = String(cmd.payload?.url ?? '')
        if (url && !this.mockMarkets.some((m) => m.url === url)) {
          const name = url.split('/').pop()?.replace(/\.git$/, '') || 'market'
          this.mockMarkets.push({ name, url, added_at: ts() })
        }
        this.respond(cmd, {})
        break
      }
      case 'market_remove': {
        const url = String(cmd.payload?.url ?? '')
        this.mockMarkets = this.mockMarkets.filter((m) => m.url !== url)
        this.respond(cmd, {})
        break
      }
      case 'market_list':
        this.respond(cmd, { markets: this.mockMarkets })
        break
      case 'skill_update_market': {
        const url = String(cmd.payload?.url ?? '')
        const updated = MOCK_SKILLS.filter((s) => s.source?.includes(url.split('/').pop()?.replace(/\.git$/, '') ?? '')).map((s) => s.name)
        this.respond(cmd, { updated: updated.length ? updated : undefined })
        break
      }
      case 'skill_get': {
        const name = String(cmd.payload?.name ?? '')
        const sk = MOCK_SKILLS.find((s) => s.name === name)
        if (sk) this.respond(cmd, { name, instructions: sk.instructions, resources: sk.resources })
        else this.respond(cmd, {})
        break
      }
      case 'load_skill': {
        // 产品主动加载技能进上下文：/skill 面板 → 发 command_result 事件回显（对齐 SDK /skills load）
        const sid = this.cmdSid(cmd)
        const name = String(cmd.payload?.name ?? '')
        const sk = MOCK_SKILLS.find((s) => s.name === name)
        this.respond(cmd, {})
        if (!sk) this.emit({ event_type: 'command_result', session_id: sid, name: 'skills', error: `skill ${name} not found`, timestamp: ts() })
        else if (!sk.enabled) this.emit({ event_type: 'command_result', session_id: sid, name: 'skills', error: `skill ${name} is disabled`, timestamp: ts() })
        else this.emit({ event_type: 'command_result', session_id: sid, name: 'skills', result: `skill "${name}" loaded into context`, timestamp: ts() })
        break
      }
      case 'mcp_list': {
        // MCP 服务器清单 + 状态 + 配置层状态（SettingsModal MCP tab 数据源；契约对齐真 bridge）
        const ws = String(cmd.payload?.workspace ?? '/mock/workspace')
        this.respond(cmd, { servers: this.mcpStatus(this.settings().mcpServers), layers: this.mockMCPLayers(ws) })
        break
      }
      case 'mcp_set': {
        // 全量更新 MCP 配置（mock：存进 settings + 持久化；状态演示为 connected）
        const servers = (cmd.payload?.servers as Record<string, MCPServerCfg> | undefined) ?? {}
        const cur = this.settings()
        cur.mcpServers = servers
        this.saveSettings(cur)
        this.respond(cmd, {})
        break
      }
      case 'mcp_refresh': {
        this.respond(cmd, {})
        break
      }
      // mcp_projects：多项目项目层汇总（MCP 页「项目」区；不依赖运行态，exists=false 由前端过滤）
      case 'mcp_projects': {
        const list = (cmd.payload?.workspaces as string[] | undefined) ?? []
        const projects = (list.length ? list : [this.ws]).map((p) => this.mockProjectMCPInfo(p))
        this.respond(cmd, { projects })
        break
      }
      // mcp_project_set：写某项目的 {ws}/.go-code/settings.json（mock：内存 + 回拉列表）
      case 'mcp_project_set': {
        const ws = String(cmd.payload?.workspace ?? '')
        const servers = (cmd.payload?.servers as Record<string, MCPServerCfg> | undefined) ?? {}
        if (!ws) {
          this.respondErr(cmd, 'workspace 为空')
          break
        }
        const bad = validateMockMCPServers(servers)
        if (bad) {
          this.respondErr(cmd, bad)
          break
        }
        this.mockProjectFiles[ws] = { ...(this.mockProjectFiles[ws] ?? {}), goCode: servers }
        this.respond(cmd, { changed: true })
        break
      }
      // hooks_list：hooks 配置来源汇总（钩子页数据源；契约对齐真 bridge）
      case 'hooks_list': {
        const list = (cmd.payload?.workspaces as string[] | undefined) ?? []
        this.respond(cmd, this.mockHooksInfo(list.length ? list : [this.ws]))
        break
      }
      // hooks_set：写某来源（mock：内存 + 回拉）
      case 'hooks_set': {
        const scope = String(cmd.payload?.scope ?? 'user')
        const ws = String(cmd.payload?.workspace ?? '')
        const body = (cmd.payload?.body as HookSectionView | undefined) ?? {}
        const bad = validateMockHooks(body)
        if (bad) {
          this.respondErr(cmd, bad)
          break
        }
        if (scope === 'project') {
          if (!ws) {
            this.respondErr(cmd, 'workspace 为空')
            break
          }
          this.mockProjectFiles[ws] = { ...(this.mockProjectFiles[ws] ?? {}), hooks: body }
        } else {
          this.mockUserHooks = body
        }
        this.respond(cmd, { changed: true })
        break
      }
      // hooks_trust：信任命令（mock：按命令记 trusted）
      case 'hooks_trust': {
        const commands = (cmd.payload?.commands as string[] | undefined) ?? []
        for (const c of commands) this.mockTrust[c] = 'trusted'
        this.respond(cmd, { trusted: commands.length })
        break
      }
      case 'command': {
        // 斜杠命令（/技能名 /summary /clear）：对齐真 bridge command 解析
        const sid = this.cmdSid(cmd)
        const line = String(cmd.payload?.line ?? '')
        const raw = line.startsWith('/') ? line.slice(1) : line
        const sk = MOCK_SKILLS.find((s) => s.name === raw)
        if (raw === 'summary') {
          this.respond(cmd, {})
          this.ctxTokens.set(sid, 244) // 压缩后锚点同步（估算口径：含静态前缀）
          this.ctxEstimated.set(sid, true)
          this.emit({ event_type: 'compress_end', session_id: sid, before: 12, after: 3, ctx_tokens: 244, timestamp: ts() })
        } else if (raw === 'clear') {
          this.respond(cmd, {})
          this.emit({ event_type: 'command_result', session_id: sid, name: 'clear', result: 'context cleared', timestamp: ts() })
        } else if (raw === 'reload_skills') {
          // 对齐真 bridge：重扫技能注册表（mock 即当前清单），刷新技能列表
          this.respond(cmd, {})
          this.emit({
            event_type: 'command_result', session_id: sid, name: 'reload_skills',
            result: `已重载技能注册表（全局 ~/.agents/skills + ~/.go-code/skills + 工作区 .agents/skills + .go-code/skills），当前共 ${MOCK_SKILLS.length} 个技能`, timestamp: ts(),
          })
        } else if (!sk) {
          // 未知命令 → ok:false（对齐 bridge：SDK 拒绝 unknown command）
          this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error: `unknown command "${raw}"`, data: {}, workspace: this.ws })
        } else if (!sk.enabled) {
          this.respond(cmd, {})
          this.emit({ event_type: 'command_result', session_id: sid, name: 'skills', error: `skill ${raw} is disabled`, timestamp: ts() })
        } else {
          this.respond(cmd, {})
          this.emit({ event_type: 'command_result', session_id: sid, name: 'skills', result: `skill "${raw}" loaded into context`, timestamp: ts() })
        }
        break
      }
      case 'metrics': {
        // 使用统计（设置页「使用统计」+ 空会话近 30 天图表）；真 bridge 由 ~/.go-code/metrics.jsonl 聚合
        this.respond(cmd, { report: mockMetricsReport(String(cmd.payload?.from ?? '')) })
        break
      }
      // —— 定时任务（CronPage；mock 内存态；mock 走 localStorage 的开关镜像）——
      case 'cron_list': {
        const cs = this.settings().cron ?? { enabled: true, auto_clean: true }
        this.respond(cmd, { enabled: cs.enabled, auto_clean: cs.auto_clean, jobs: MOCK_CRON })
        break
      }
      case 'cron_create': {
        const expr = String(cmd.payload?.cron ?? '')
        const prm = String(cmd.payload?.prompt ?? '')
        if (!/^\S+\s+\S+\s+\S+\s+\S+\s+\S+$/.test(expr)) {
          this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error: '非法 cron 表达式', data: {}, workspace: this.ws })
          break
        }
        const recurring = cmd.payload?.recurring !== false
        const id = `c${MOCK_CRON.length + 1}`
        const now = ts()
        MOCK_CRON.unshift({ id, cron: expr, prompt: prm, recurring, workspace: String(cmd.payload?.workspace ?? ''), created_at: now, next_run: now, run_count: 0 })
        this.respond(cmd, { id })
        break
      }
      case 'cron_update': {
        const id = String(cmd.payload?.id ?? '')
        const j = MOCK_CRON.find((x) => x.id === id)
        if (!j) { this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error: `任务 ${id} 不存在`, data: {}, workspace: this.ws }); break }
        if (cmd.payload?.prompt) j.prompt = String(cmd.payload.prompt)
        if (cmd.payload?.cron) j.cron = String(cmd.payload.cron)
        if (typeof cmd.payload?.recurring === 'boolean') j.recurring = cmd.payload.recurring
        this.respond(cmd, { id })
        break
      }
      case 'cron_delete': {
        const id = String(cmd.payload?.id ?? '')
        const i = MOCK_CRON.findIndex((x) => x.id === id)
        if (i < 0) { this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error: `任务 ${id} 不存在`, data: {}, workspace: this.ws }); break }
        MOCK_CRON.splice(i, 1)
        this.respond(cmd, {})
        break
      }
      case 'cron_runs': {
        const jobId = String(cmd.payload?.job_id ?? '')
        const runs = MOCK_CRON_RUNS.filter((r) => !jobId || r.job_id === jobId)
        this.respond(cmd, { runs: runs.slice(0, 50) })
        break
      }
      case 'cron_runs_detail': {
        // 某次运行的本地化历史详情（mock：生成演示消息；真 bridge 读 cron_sessions 快照）
        const jobId = String(cmd.payload?.job_id ?? '')
        const runId = String(cmd.payload?.run_id ?? '')
        const run = MOCK_CRON_RUNS.find((r) => r.job_id === jobId && r.run_id === runId)
        if (!run) {
          this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error: `运行记录不存在: ${runId}`, data: {}, workspace: this.ws })
          break
        }
        const job = MOCK_CRON.find((j) => j.id === jobId)
        const messages = [
          { role: 'user', text: job?.prompt ?? `定时任务触发（${jobId}）` },
          { role: 'assistant', text: `已执行完成：${run.final_result ?? '（无结果摘要）'}。\n\n本次运行状态：${run.status}，耗时 ${(run.duration_ms ?? 0) / 1000}s。` },
        ]
        this.respond(cmd, {
          job_id: jobId, run_id: runId, session_id: run.session_id, found: true,
          status: run.status, fired_at: run.fired_at, duration_ms: run.duration_ms,
          final_result: run.final_result, messages,
        })
        break
      }
      case 'cron_clean':
        this.respond(cmd, { cleaned: 0 })
        break
      case 'git_snapshot':
        // 工作区级/会话级 git 探测：mock 项目非 git 仓库（响应形状对齐真实 bridge）
        this.respond(cmd, {
          session_id: String(cmd.payload?.session_id ?? ''),
          git: { is_repo: false, ahead: 0, behind: 0, staged: 0, modified: 0, untracked: 0, conflicted: 0, clean: true },
          files: [],
        })
        break
      case 'git_history':
        // mock 历史返回一条演示提交（has_more=false；形状对齐真实 bridge）
        this.respond(cmd, {
          session_id: String(cmd.payload?.session_id ?? ''),
          has_more: false,
          commits: [{
            hash: 'abcdef1234567890abcdef1234567890abcdef12',
            short_hash: 'abcdef1',
            subject: 'mock: 初始化演示提交',
            author: 'mock',
            date: new Date().toISOString(),
            graph: '*',
          }],
        })
        break
      case 'git_commit_diff': {
        const hash = String(cmd.payload?.hash ?? '')
        this.respond(cmd, {
          session_id: String(cmd.payload?.session_id ?? ''),
          hash,
          diff: 'diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1,2 @@\n mock demo\n+second line\n',
        })
        break
      }
      case 'git_file_diff': {
        const path = String(cmd.payload?.path ?? '')
        this.respond(cmd, {
          session_id: String(cmd.payload?.session_id ?? ''),
          path,
          diff: `diff --git a/${path} b/${path}\n--- a/${path}\n+++ b/${path}\n@@ -1 +1,2 @@\n mock demo\n+changed line\n`,
        })
        break
      }
      case 'git_stage':
      case 'git_unstage':
      case 'git_discard': {
        // mock 写操作：返回计数（不改变 mock 快照；响应形状对齐真实 bridge）
        const n = Array.isArray(cmd.payload?.paths) ? (cmd.payload.paths as unknown[]).length : 0
        const key = cmd.type === 'git_stage' ? 'staged' : cmd.type === 'git_unstage' ? 'unstaged' : 'discarded'
        this.respond(cmd, { session_id: String(cmd.payload?.session_id ?? ''), [key]: n })
        break
      }
      case 'git_commit':
        this.respond(cmd, { session_id: String(cmd.payload?.session_id ?? ''), hash: 'm0ck123' })
        break
      case 'git_branch_list':
        // mock：单分支 main 且为当前（形状对齐真实 bridge）
        this.respond(cmd, { session_id: String(cmd.payload?.session_id ?? ''), branches: [{ name: 'main', current: true }] })
        break
      case 'git_checkout':
        this.respond(cmd, { session_id: String(cmd.payload?.session_id ?? ''), branch: String(cmd.payload?.branch ?? '') })
        break
      case 'git_init':
        this.respond(cmd, { session_id: String(cmd.payload?.session_id ?? ''), created: true })
        break
      // IM 集成（SettingsModal IM tab 预览数据）
      case 'im_config_get':
        this.respond(cmd, {
          config: {
            enabled: true,
            gateways: [
              {
                id: 'feishu-main', type: 'feishu', enabled: true, config: { app_id: 'cli_mock', mode: 'websocket' },
                routes: [{ chat: { gateway: 'feishu', chat_id: 'oc_mock_group' }, workspace: '~/workspace/go-code' }],
                allow_users: ['ou_mock_user'],
              },
            ],
            mirrors: [],
            security: { require_bind_confirm: true },
          },
        })
        break
      case 'im_schema_list':
        this.respond(cmd, {
          schemas: [
            {
              type: 'feishu',
              name: '飞书',
              description: '企业自建应用机器人：长连接接收、卡片审批、话题支持',
              groups: [
                {
                  title: '基础',
                  fields: [
                    { key: 'app_id', label: 'App ID', type: 'string', required: true, help: '飞书开放平台 → 开发者后台 → 凭证与基础信息' },
                    { key: 'app_secret', label: 'App Secret', type: 'password', required: true, secret: true },
                    {
                      key: 'mode', label: '接收模式', type: 'select', required: true, default: 'websocket',
                      options: [
                        { value: 'websocket', label: '长连接（推荐，无需公网）' },
                        { value: 'webhook', label: 'Webhook（需公网 + 加密配置）' },
                      ],
                    },
                  ],
                },
                {
                  title: '高级',
                  fields: [
                    { key: 'encrypt_key', label: 'Encrypt Key', type: 'password', secret: true, advanced: true },
                    { key: 'base_url', label: '自定义 API 域名', type: 'string', advanced: true, default: 'https://open.feishu.cn' },
                  ],
                },
              ],
            },
          ],
        })
        break
      case 'im_status':
        this.respond(cmd, { enabled: true, gateways: [{ id: 'feishu-main', type: 'feishu', enabled: true, status: 'connected' }] })
        break
      case 'im_config_set':
        this.respond(cmd, {})
        break
      case 'im_chat_list': // 群列表（路由绑定下拉）
        this.respond(cmd, {
          chats: [
            { chat_id: 'oc_group_a', name: 'go-code 开发群' },
            { chat_id: 'oc_group_b', name: '架构评审群' },
            { chat_id: 'oc_group_c', name: '发布通知群' },
          ],
        })
        break
      case 'im_simulate_bind': // 仅 mock：模拟收到陌生 IM 消息 → 触发绑定确认弹窗
        this.emit({
          event_type: 'im_bind_confirm_requested',
          workspace: this.ws,
          data: {
            chat: { gateway: 'feishu', chat_id: 'oc_new_group' },
            user_id: 'ou_new_user',
            text: '你好，能帮我修个 bug 吗？',
          },
        })
        this.respond(cmd, {})
        break
      case 'mesh_status': {
        // mock：本机节点 + 空链接/peers（真 bridge 返回完整状态）
        this.respond(cmd, {
          session_enabled: true,
          remote_enabled: true,
          node_id: 'mock-node',
          listen_addr: '127.0.0.1:17890',
          tls: false,
          connect_urls: ['ws://127.0.0.1:17890'],
          links: [],
          peers: [],
          remote_sessions: [],
        })
        break
      }
      case 'mesh_ping': {
        // mock：对配置 peer 假 ping 成功（无真实链路）
        this.respond(cmd, {
          node_id: String((cmd.payload?.node_id as string) ?? ''),
          ok: true,
          rtt_ms: 1,
        })
        break
      }
      case 'browser_engine_get': {
        // mock：浏览器引擎 + ego 状态（形状对齐 bridge egoStatusToMap，供插件页/测试使用）。
        // 默认引擎 mcp + 未安装；e2e 可注入 window.__mockBrowserEngine / __mockEgoStatus
        // 覆盖各状态分层（未安装 / 已安装未运行 / 已安装未引导 / 可用）。
        const g = globalThis as { __mockEgoStatus?: Record<string, unknown>; __mockBrowserEngine?: string }
        const engine = g.__mockBrowserEngine === 'ego' ? 'ego' : 'mcp'
        this.respond(cmd, {
          browser_engine: engine,
          ego: g.__mockEgoStatus ?? { installed: false, available: false, app_running: false, cli_found: false, error: 'ego-browser 命令未找到（未安装 ego lite 或未完成 onboarding）' },
        })
        break
      }
      case 'trace': {
        const p = cmd.payload as Record<string, unknown> | undefined
        this.mockTraceData(cmd, String(p?.run_id || ''), p?.latest === 1 || p?.latest === true)
        break
      }
      default:
        this.respond(cmd, {})
    }
    return Promise.resolve({ ok: true })
  }

  // —— 链路 tab：trace 命令（mock：返回当前会话的示例 trace，供 UI 联调/回归）——
  // 与 bridge 同形：不带 run_id → 摘要列表；带 run_id → 该 trace 的完整 span。
  private mockTraceData(cmd: BridgeCommand, runId: string, wantLatest = false): void {
    const spans: unknown[] = []
    const mk = (o: Record<string, unknown>): void => { spans.push(o) }
    mk({ kind: 'run', run_id: 'mock-run', label: '给链路 tab 加时间轴瀑布图', status: 'done', depth: 0, start_at: 0, dur_ms: 20000 })
    mk({ kind: 'llm', id: 'mock-req-1', run_id: 'mock-run', label: 'llm', model: 'mock-model', status: 'done', start_at: 40, dur_ms: 3200,
      in_tok: 1200, out_tok: 180, cache_tok: 900, cache_write_tok: 140, reason_tok: 120, cost_usd: 0.0021 })
    mk({ kind: 'tool', id: 'mock-tool-1', run_id: 'mock-run', name: 'read_file', label: 'read_file', status: 'done', start_at: 3300, dur_ms: 420 })
    mk({ kind: 'tool', id: 'mock-tool-2', run_id: 'mock-run', name: 'bash', label: 'bash', status: 'error', start_at: 3800, dur_ms: 1500, is_error: true })
    // 与上一条区间重叠 → 触发并行窗口底纹。真实数据实测 22.8% 的 trace 有此形态，
    // 但原示例恰好没有重叠 → 该渲染路径一直是零覆盖（补上后 e2e 可断言）。
    mk({ kind: 'tool', id: 'mock-tool-4', run_id: 'mock-run', name: 'grep', label: 'grep', status: 'done', start_at: 4000, dur_ms: 1100 })
    mk({ kind: 'llm', id: 'mock-req-2', run_id: 'mock-run', label: 'llm', model: 'mock-model', status: 'done', start_at: 5400, dur_ms: 2600,
      in_tok: 2400, out_tok: 260, cache_tok: 2000, cost_usd: 0.0035, retries: 1 })
    mk({ kind: 'compress', run_id: 'mock-run', label: 'compress', status: 'done', start_at: 8100, dur_ms: 1200, before: 42, after: 12, ctx_tokens: 3400 })
    mk({ kind: 'run', run_id: 'mock-sub', parent_id: 'mock-run', label: 'worker', status: 'done', depth: 1, start_at: 9500, dur_ms: 6000 })
    mk({ kind: 'llm', id: 'mock-req-3', run_id: 'mock-sub', label: 'llm', model: 'mock-model', status: 'done', start_at: 9600, dur_ms: 2100, in_tok: 300, out_tok: 40 })
    mk({ kind: 'tool', id: 'mock-tool-3', run_id: 'mock-sub', name: 'grep', label: 'grep', status: 'running', start_at: 11800, dur_ms: 900, task_id: 'tooltask-mock' })
    mk({ kind: 'wait', id: 'mock-q-1', run_id: 'mock-run', label: 'ask_user', status: 'done', start_at: 15600, dur_ms: 2600, wait_type: 'ask_user', questions: 2 })
    const summary = {
      id: 'mock-run', label: '给链路 tab 加时间轴瀑布图', status: 'done', start_at: Date.parse('2026-09-10T12:00:00Z'),
      wall_ms: 20000, spans: spans.length, tools: 4, turns: 3, subs: 1, errors: 1,
      in_tok: 3900, out_tok: 480, cost_usd: 0.0056, meta_bytes: JSON.stringify(spans).length,
    }
    // 列表按「最近在上」：index 0 = 最新（前端点它 = 跟随实时投影），
    // 故这里把示例 trace 放在 index 1（历史位）—— 与真实的「一个会话多条 trace」同形，
    // 便于验证「选中历史 trace → 命令拉详情 → 渲染五类 span」这条路径。
    const older = { ...summary, id: 'mock-run-prev', label: '（历史）给链路 tab 加时间轴瀑布图', start_at: summary.start_at - 600000, meta_bytes: summary.meta_bytes }
    const live = { ...summary, id: 'mock-run-live', label: '（最新）发起对话后这里显示实时链路', start_at: summary.start_at + 60000, wall_ms: 1000, spans: 1, tools: 0, turns: 0, subs: 0, errors: 0, in_tok: 0, out_tok: 0, cost_usd: 0, meta_bytes: 40 }
    const payload: Record<string, unknown> = { traces: [live, older], total: 2, has_more: false }
    // latest=1：列表请求附带最新一条详情（与 bridge 同形；前端刷新兜底用）
    if (wantLatest) payload.latest = { summary: live, spans: spans.slice(0, 1) }
    if (runId && runId === older.id) { payload.found = true; payload.trace = { summary: older, spans } }
    else if (runId && runId === live.id) { payload.found = true; payload.trace = { summary: live, spans: spans.slice(0, 1) } }
    else payload.found = false
    this.respond(cmd, payload)
  }

  private respond(cmd: BridgeCommand, data: Record<string, unknown>) {
    this.emit({ event_type: 'command_response', id: cmd.id, ok: true, data })
  }

  // respondErr：命令失败响应（真 bridge 同形：ok:false + error；store 侧透出给面板）
  private respondErr(cmd: BridgeCommand, error: string) {
    this.emit({ event_type: 'command_response', id: cmd.id, ok: false, error })
  }

  // emit：事件行注入 workspace + seq；session_id 由调用方显式带（按会话路由）
  private emit(e: Record<string, unknown>) {
    const line = JSON.stringify({ ...e, workspace: this.ws, seq: ++seq })
    for (const cb of this.cbs) cb(line)
  }

  // after：延迟 fn；sid 决定「该会话是否已 stopped」+ fn 闭包用 sid 发事件。
  // 默认拦截主 run 脚本节点（对齐真 bridge：主停止只 abort 主 run）。
  private after(ms: number, fn: () => void, sid = this.sid, stoppedOk = false) {
    this.timers.push(setTimeout(() => {
      if (stoppedOk || !this.state(sid).stopped) fn()
    }, ms))
  }

  // afterBg：后台任务脚本节点（脱离主 run 存活的部分：promoted 工具回传、子 agent
  // task_* 事件流）——不受主停止 stopped 拦截。对齐 2026-08-30 语义：主「停止」
  // 不影响异步任务；后台任务只经 interrupt_task 精确停止（由任务自身终态标志门控）。
  private afterBg(ms: number, fn: () => void, sid = this.sid) {
    this.after(ms, fn, sid, true)
  }

  private stream(text: string, step = 22, gap = 22, requestId?: string, runId?: string, sid = this.sid, bg = false) {
    let i = 0
    const push = () => {
      if ((!bg && this.state(sid).stopped) || i >= text.length) return
      const chunk = text.slice(i, i + step)
      i += step
      this.emit({ event_type: 'content_chunk', session_id: sid, content: chunk, ...(requestId ? { request_id: requestId } : {}), ...(runId ? { run_id: runId } : {}), timestamp: ts() })
      this.timers.push(setTimeout(push, gap))
    }
    push()
  }

  private think(text: string, at: number, requestId?: string, runId?: string, sid = this.sid, bg = false) {
    this.after(at, () => this.emit({ event_type: 'reasoning_chunk', session_id: sid, content: text, ...(requestId ? { request_id: requestId } : {}), ...(runId ? { run_id: runId } : {}), timestamp: ts() }), sid, bg)
  }

  private llmStart(model = 'deepseek-v4-flash-vision-exp', requestId?: string, runId?: string, sid = this.sid) {
    this.emit({ event_type: 'llm_start', session_id: sid, model, ...(requestId ? { request_id: requestId } : {}), ...(runId ? { run_id: runId } : {}), timestamp: ts() })
  }
  private llmEnd(over: Record<string, unknown>, sid = this.sid) {
    // cost_usd 模拟真 bridge 打标（同价表：Input$0.14 / CacheRead$0.014 / Output$0.28 每 M）
    const u = (over.usage as { Input?: number; CacheRead?: number; CacheWrite?: number; Output?: number } | undefined) ?? {}
    // 上下文占用锚点（context_breakdown 用）：与真 bridge 的 reducer 同口径 —— 最近一次
    // llm_end 的 Input 即当前上下文占用（压缩/清空后会由后续轮次覆盖）。
    if (typeof u.Input === 'number' && u.Input > 0) {
      this.ctxTokens.set(sid, u.Input)
      this.ctxEstimated.set(sid, false) // provider 真实 usage → 真实口径
    }
    const costUsd = ((u.Input ?? 0) * 0.14 + (u.CacheRead ?? 0) * 0.014 + (u.CacheWrite ?? 0) * 0.175 + (u.Output ?? 0) * 0.28) / 1e6
    this.emit({ event_type: 'llm_end', session_id: sid, model: this.model, timestamp: ts(), cost_usd: Number(costUsd.toFixed(8)), ...over })
  }
  private toolStart(id: string, name: string, args: string, runId: string, at: number, sid = this.sid, bg = false) {
    this.after(at, () => this.emit({ event_type: 'tool_run_start', session_id: sid, id, run_id: runId, name, arguments: args, timestamp: ts() }), sid, bg)
  }
  private toolEnd(id: string, name: string, result: string, runId: string, at: number, sid = this.sid, extra?: Record<string, unknown>, bg = false) {
    this.after(at, () => this.emit({ event_type: 'tool_run_end', session_id: sid, id, run_id: runId, name, result, is_error: false, ...(extra ?? {}), timestamp: ts() }), sid, bg)
  }

  // 一整个 LLM 轮：llm_start → think×n → 流式 → llm_end。at = 起始时刻。
  // requestId 使 store 按请求归属流式块（多并发 agent 各回各家）；runId 使子 agent 输出/用量归集到其卡片。
  // 主 agent 轮不显式传 runId → 默认当前主 run（对齐 SDK：llm_end 恒带 run_id）。
  // stoppedOk：主 run 停止（stopped）后仍照常发出的轮（子 agent 后台输出；对齐真实 SDK
  // —— 子 agent 不随主 run abort）。
  private llmTurn(at: number, opts: { think?: string[]; thinkGap?: number; stream?: string; endAt: number; end: Record<string, unknown>; requestId?: string; runId?: string; stoppedOk?: boolean }, sid = this.sid) {
    const runId = opts.runId ?? this.state(sid).curRunId
    const okStop = Boolean(opts.stoppedOk && opts.runId) // 必须显式传 runId（子 agent 轮）才豁免主停止拦截
    // requestId 缺省自动生成：对齐真实 bridge（每轮事件都带 request_id）——
    // 缺省时 store 按 lastLLMBlockId 兜底关联，块级 TTFT 无法结算（content_chunk
    // 的 ttftMs 只在能按 request_id 定位块时挂），消息底部耗时行/输出速度会缺失。
    const rid = opts.requestId ?? `req-${runId}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`
    // llm_start 也带 run_id（与 SDK 事件自包含一致）：子 agent 轮不建叙述块，输出走其卡片
    this.after(at, () => this.llmStart(this.model, rid, runId, sid), sid, okStop)
    let t = at + 40
    const gap = opts.thinkGap ?? 70
    for (const th of opts.think ?? []) {
      this.think(th, t, rid, runId, sid, okStop)
      t += gap
    }
    if (opts.stream) this.after(t, () => this.stream(opts.stream!, 22, 22, rid, runId, sid, okStop), sid, okStop)
    this.after(opts.endAt, () => this.llmEnd({ ...(runId ? { run_id: runId } : {}), ...(rid ? { request_id: rid } : {}), ...opts.end }, sid), sid, okStop)
  }

  // 某会话收到用户输入：空闲 → 立即开 run；运行中 → 入队（模拟 SDK inbox），脚本结束批量下一轮
  private ask(sid: string, text: string) {
    if (!text.length) return
    this.askBatch(sid, [text])
  }

  // 批量输入（客户端暂存队列整队推送）：空闲 → 一批开 run；运行中 → 整批入队（SDK takeBatch 语义）
  private askBatch(sid: string, texts: string[]) {
    if (!texts.length) return
    const st = this.state(sid)
    st.stopped = false // 中断（interrupt 硬停）后下一条输入恢复运行（对齐真 bridge 语义）
    if (st.busy) {
      st.askQueue.push(...texts)
      return
    }
    this.startRun(sid, texts)
  }

  // 一次 run 处理一批（对齐 SDK takeBatch + 插话 Poll）：先发 user_inputs_consumed（本批已消费
  // 进本轮，前端据此把待发送移入对话页），再 agent_start。
  private startRun(sid: string, texts: string[]) {
    const st = this.state(sid)
    st.busy = true
    const runId = `main-${st.runSeq++}`
    st.curRunId = runId
    if (texts.length > 0) {
      this.emit({ event_type: 'user_inputs_consumed', session_id: sid, user_texts: texts, timestamp: ts() })
    }
    this.emit({ event_type: 'agent_start', session_id: sid, run_id: runId, index: 0, content: '处理用户输入', model: this.model, timestamp: ts() })
    const joined = texts.join(' ')
    if (st.mode === 'plan') this.planScript(sid, runId)
    else if (/长任务|sleep 30|移到后台/.test(joined)) {
      st.failPromote = /promote失败/.test(joined) // 「promote失败」钩子：promote_task 命令一律失败（验证回滚）
      this.promoteScript(sid, runId) // 长工具 promote 演示
    }
    else if (/vue/.test(joined)) this.vueScript(sid, runId) // mock 钩子：读 .vue 组件（右侧文件栏高亮验证）
    else if (/readme/i.test(joined)) this.readmeScript(sid, runId) // mock 钩子：读项目 README（右侧 Markdown 预览 + 相对路径图片）
    else if (/html|网页预览/.test(joined)) this.htmlScript(sid, runId) // mock 钩子：输出 html 代码块（iframe 实时预览验证）
    else if (/写个网页|写html|write_html/.test(joined)) this.writeHtmlScript(sid, runId) // mock 钩子：write_file 落盘 .html（「打开」右侧浏览器验证）
    else if (/mermaid|流程图|架构图/.test(joined)) this.mermaidScript(sid, runId, /错误|坏/.test(joined)) // mock 钩子：输出 mermaid 图表（markdown 渲染验证）；含「错误/坏」→ 输出语法错误的图（错误回退验证）
    else if (/宽表/.test(joined)) this.wideTableScript(sid, runId) // mock 钩子：输出超宽表格（表格横滚验证；输入「宽表格」命中）
    else if (/长会话|长历史/.test(joined)) this.longHistoryScript(sid, runId) // mock 钩子：一次性注入大量历史消息（切会话滚动定位验证）
    else this.normalScript(sid, runId)
  }

  // 脚本结束：运行中如有待发送 → 批量进入下一轮（SDK takeBatch 语义）
  private finishRun(sid: string) {
    const st = this.state(sid)
    if (st.stopped) return
    st.busy = false
    if (st.askQueue.length > 0) {
      const batch = st.askQueue.slice()
      st.askQueue.length = 0
      this.after(80, () => { if (!this.state(sid).stopped) this.startRun(sid, batch) }, sid)
    }
  }

  // ---------- plan 模式：只读调研 + plan_submit ----------
  private planScript(sid: string, runId: string) {
    this.llmTurn(0, {
      think: ['plan 模式：只读调研当前错误处理路径，产出计划，不修改任何文件。'],
      stream: '我先梳理现状：确认 `auth.go` 的错误处理路径。',
      endAt: 700,
      end: { finish_reason: 'tool_call', usage: { Input: 480, Output: 55, CacheRead: 430, CacheWrite: 60, CacheWrite1h: 60, TotalTokens: 535 }, tool_calls: [{ id: 'p-1', name: 'list_dir', arguments: '{"path":"."}' }] },
    }, sid)
    this.toolStart('p-1', 'list_dir', '{"path":"."}', runId, 720, sid)
    this.toolEnd('p-1', 'list_dir', 'auth.go\nauth_test.go\ngo.mod\nREADME.md', runId, 790, sid)
    this.llmTurn(820, {
      think: ['读取 auth.go 前 200 行，确认 Login 的错误分支。'],
      stream: '读取 `auth.go`。',
      endAt: 1300,
      end: { finish_reason: 'tool_call', usage: { Input: 640, Output: 40, CacheRead: 600, CacheWrite: 40, TotalTokens: 680 }, tool_calls: [{ id: 'p-2', name: 'read_file', arguments: '{"path":"auth.go","start_line":1,"end_line":200}' }] },
    }, sid)
    this.toolStart('p-2', 'read_file', '{"path":"auth.go","start_line":1,"end_line":200}', runId, 1320, sid)
    this.toolEnd('p-2', 'read_file', readResult('auth.go', 1, 200), runId, 1400, sid)
    this.llmTurn(1440, {
      think: ['确认 jwt 库是否已导出 ErrTokenExpired：grep 全仓。'],
      stream: 'grep 确认 `jwt.ErrTokenExpired` 是否可用。',
      endAt: 1900,
      end: { finish_reason: 'tool_call', usage: { Input: 720, Output: 36, CacheRead: 680, TotalTokens: 756 }, tool_calls: [{ id: 'p-3', name: 'grep', arguments: '{"pattern":"jwt.ErrTokenExpired","path":"."}' }] },
    }, sid)
    this.toolStart('p-3', 'grep', '{"pattern":"jwt.ErrTokenExpired","path":"."}', runId, 1920, sid)
    this.toolEnd('p-3', 'grep', '命中：jwt 库已导出 jwt.ErrTokenExpired', runId, 1980, sid)
    this.llmTurn(2020, {
      think: ['方案确定：Login 错误分支先判 jwt.ErrTokenExpired → 401 + token_expired 错误码，其余错误仍回 500；go test 回归。'],
      stream: '**修复计划**\n\n1. `auth.go`：`Login` 错误分支先判 `jwt.ErrTokenExpired` → 401 + `token_expired` 错误码，其余错误仍回 500；\n2. `go test ./auth` 回归。',
      endAt: 2600,
      end: { finish_reason: 'tool_call', usage: { Input: 1100, Output: 120, CacheRead: 1000, CacheWrite: 100, CacheWrite1h: 100, TotalTokens: 1220 }, tool_calls: [{ id: 'p-4', name: 'plan_submit', arguments: '{"plan":"1. Login 过期分支 → 401 + token_expired…2. 回归"}' }] },
    }, sid)
    this.toolStart('p-4', 'plan_submit', '{"plan":"1. Login 过期分支 → 401 + token_expired…"}', runId, 2620, sid)
    this.toolEnd('p-4', 'plan_submit', 'plan submitted: 1. Login 过期分支 → 401 + token_expired 错误码…', runId, 2700, sid)
    this.llmTurn(2740, {
      stream: '计划已提交，等待确认后进入执行。',
      endAt: 3200,
      end: { finish_reason: 'stop', usage: { Input: 200, Output: 30, CacheRead: 180, TotalTokens: 230 } },
    }, sid)
    this.after(3220, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 1, content: '计划已提交', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.after(3260, () => this.finishRun(sid), sid)
  }

  // ---------- vue 钩子：读 .vue 单文件组件（右侧文件栏高亮验证） ----------
  private vueScript(sid: string, runId: string) {
    this.llmTurn(0, {
      think: ['读取 App.vue，确认组件结构。'],
      stream: '读取 `App.vue`。',
      endAt: 700,
      end: { finish_reason: 'tool_call', usage: { Input: 300, Output: 30, CacheRead: 260, TotalTokens: 330 }, tool_calls: [{ id: 'v-1', name: 'read_file', arguments: '{"path":"App.vue","start_line":1,"end_line":200}' }] },
    }, sid)
    this.toolStart('v-1', 'read_file', '{"path":"App.vue","start_line":1,"end_line":200}', runId, 720, sid)
    this.toolEnd('v-1', 'read_file', readResult('App.vue', 1, 200), runId, 800, sid)
    this.llmTurn(840, {
      stream: 'App.vue 已读完：template + script setup + scoped style 三块齐全。',
      endAt: 1500,
      end: { finish_reason: 'stop', usage: { Input: 420, Output: 40, CacheRead: 380, TotalTokens: 460 } },
    }, sid)
    this.after(1560, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 1, content: '读取 App.vue 完成', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.after(1600, () => this.finishRun(sid), sid)
  }

  // ---------- README 钩子：读项目 README（右侧 Markdown 预览 + 相对路径图片验证） ----------
  // 走的是真实内容（MOCK_README_ZH）：图是 brand/ 下的相对路径引用，mock 已把它们改写成
  // Vite 资源 URL，所以点开工具行里的路径就能看到「文档 + 相对路径图片」完整生效 ——
  // 演示、宣传片截图与预览回归共用这一条。
  private readmeScript(sid: string, runId: string) {
    this.llmTurn(0, {
      think: ['用户要看项目 README：直接读工作区里那份真实文档，预览交给右侧文件面板。'],
      stream: '读取项目里的 `README.zh-CN.md`（426 行）。',
      endAt: 700,
      end: { finish_reason: 'tool_call', usage: { Input: 320, Output: 32, CacheRead: 280, TotalTokens: 352 }, tool_calls: [{ id: 'r-1', name: 'read_file', arguments: '{"path":"README.zh-CN.md","start_line":1,"end_line":120}' }] },
    }, sid)
    this.toolStart('r-1', 'read_file', '{"path":"README.zh-CN.md","start_line":1,"end_line":120}', runId, 720, sid)
    this.toolEnd('r-1', 'read_file', readResult('README.zh-CN.md', 1, 120), runId, 900, sid)
    this.llmTurn(940, {
      think: ['README 是 Markdown：默认走渲染预览，不是高亮源码；图是仓库相对路径，预览里应当直接出图。'],
      stream: '已读完前 120 行。点开上面的 `README.zh-CN.md` 可看完整文档：标题、表格、徽标和 `brand/` 下的截图都是渲染后的效果，不是源码。',
      endAt: 1900,
      end: { finish_reason: 'stop', usage: { Input: 640, Output: 70, CacheRead: 600, TotalTokens: 710 } },
    }, sid)
    this.after(1960, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 1, content: 'README 预览已就绪', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.after(2000, () => this.finishRun(sid), sid)
  }

  // ---------- html 钩子：输出 ```html 代码块（iframe 沙箱实时预览验证） ----------
  // 三种形态：完整 <html> 文档 / HTML 片段（自动包骨架）/ 带内联 <script> 交互。
  private htmlScript(sid: string, runId: string) {
    const demo1 = '下面是一个 **完整 HTML 文档**（深色卡片 + 按钮 hover 动效）：\n\n'
      + '```html\n'
      + '<!doctype html>\n'
      + '<html lang="zh">\n'
      + '<head>\n'
      + '<meta charset="utf-8">\n'
      + '<style>\n'
      + '  body { font-family: system-ui, sans-serif; background: #1e1e2e; display: flex; justify-content: center; align-items: center; min-height: 200px; margin: 0; }\n'
      + '  .card { background: #2a2a3c; color: #e4e4f0; border-radius: 12px; padding: 24px 32px; box-shadow: 0 8px 24px rgba(0,0,0,.4); text-align: center; }\n'
      + '  .card h2 { margin: 0 0 8px; color: #8be9fd; }\n'
      + '  .card button { margin-top: 12px; padding: 8px 20px; border: 0; border-radius: 8px; background: #50fa7b; color: #1e1e2e; font-size: 14px; font-weight: 600; cursor: pointer; transition: transform .15s, box-shadow .15s; }\n'
      + '  .card button:hover { transform: translateY(-2px); box-shadow: 0 4px 12px rgba(80,250,123,.4); }\n'
      + '  .card button:active { transform: translateY(0); }\n'
      + '</style>\n'
      + '</head>\n'
      + '<body>\n'
      + '  <div class="card">\n'
      + '    <h2>实时预览</h2>\n'
      + '    <p>这个 HTML 运行在沙箱 iframe 里</p>\n'
      + '    <button onclick="document.querySelector(\'p\').textContent = \'按钮被点击了！\'">点我试试</button>\n'
      + '  </div>\n'
      + '</body>\n'
      + '</html>\n'
      + '```\n\n'
      + '再来一个 **HTML 片段**（无 <html> 骨架，自动补齐；带内联脚本做交互）：\n\n'
      + '```html\n'
      + '<style>\n'
      + '  .counter { font-family: system-ui; text-align: center; padding: 20px; }\n'
      + '  .counter .num { font-size: 48px; font-weight: 700; color: #e74c3c; }\n'
      + '  .counter button { font-size: 18px; padding: 6px 18px; margin: 4px; border-radius: 8px; border: 1px solid #bbb; cursor: pointer; background: #f8f9fa; }\n'
      + '  .counter button:hover { background: #e9ecef; }\n'
      + '</style>\n'
      + '<div class="counter">\n'
      + '  <div class="num" id="n">0</div>\n'
      + '  <button onclick="document.getElementById(\'n\').textContent = +document.getElementById(\'n\').textContent + 1">+1</button>\n'
      + '  <button onclick="document.getElementById(\'n\').textContent = 0">归零</button>\n'
      + '</div>\n'
      + '```\n\n'
      + '最后一个 **长页面 demo**（内容超高 → 预览封顶 560px 内滚，可点「在浏览器打开」到右侧浏览器看完整）：\n\n'
      + '```html\n'
      + '<!doctype html>\n'
      + '<html lang="zh">\n'
      + '<head>\n'
      + '<meta charset="utf-8">\n'
      + '<style>\n'
      + '  body { font-family: system-ui, sans-serif; max-width: 640px; margin: 0 auto; padding: 24px; color: #333; line-height: 1.7; }\n'
      + '  h1 { color: #2c3e50; border-bottom: 2px solid #3498db; padding-bottom: 8px; }\n'
      + '  h2 { color: #3498db; margin-top: 28px; }\n'
      + '  .box { background: #ecf9ff; border-left: 4px solid #3498db; padding: 12px 16px; border-radius: 0 8px 8px 0; margin: 16px 0; }\n'
      + '  code { background: #f4f4f4; padding: 2px 6px; border-radius: 4px; font-size: 14px; }\n'
      + '  table { border-collapse: collapse; width: 100%; margin: 16px 0; }\n'
      + '  th, td { border: 1px solid #ddd; padding: 8px 12px; text-align: left; }\n'
      + '  th { background: #f8f9fa; }\n'
      + '</style>\n'
      + '</head>\n'
      + '<body>\n'
      + '  <h1>长页面演示</h1>\n'
      + '  <p>这个页面内容很长，用于验证预览高度上限。滚动看全，或点「在浏览器打开」到右侧浏览器独立查看。</p>\n'
      + '  <div class="box">\n'
      + '    <strong>提示：</strong>预览区封顶 560px，超高内容在框内滚动；点「在浏览器打开」在右侧浏览器独立查看。\n'
      + '  </div>\n'
      + '  <h2>第一节：背景</h2>\n'
      + Array.from({ length: 6 }, (_, k) => `  <p>第 ${k + 1} 段背景介绍文字。这里展开项目的来龙去脉、技术选型与架构决策，让页面足够高以展示滚动行为。上下文包括需求分析、竞品调研与初步方案设计，内容较长以撑满可视区域。</p>`).join('\n')
      + '  <h2>第二节：实现细节</h2>\n'
      + Array.from({ length: 6 }, (_, k) => `  <p>第 ${k + 1} 段实现细节。描述核心模块划分、接口设计与数据流，附关键代码片段 <code>func main() {}</code> 与部署注意事项。同样以充实的内容填充页面高度。</p>`).join('\n')
      + '  <h2>数据对比</h2>\n'
      + '  <table>\n'
      + '    <tr><th>方案</th><th>延迟</th><th>成本</th><th>维护</th></tr>\n'
      + '    <tr><td>方案 A</td><td>12ms</td><td>低</td><td>中</td></tr>\n'
      + '    <tr><td>方案 B</td><td>8ms</td><td>高</td><td>低</td></tr>\n'
      + '    <tr><td>方案 C</td><td>20ms</td><td>极低</td><td>高</td></tr>\n'
      + '  </table>\n'
      + '  <h2>第三节：结论</h2>\n'
      + Array.from({ length: 4 }, (_, k) => `  <p>第 ${k + 1} 段结论与后续规划。总结当前进展、风险与下一步里程碑，同时给出版本演进路线与回滚策略，收束整篇长文。</p>`).join('\n')
      + '  <p style="text-align:center;color:#999;margin-top:24px">— 文档结束 —</p>\n'
      + '</body>\n'
      + '</html>\n'
      + '```'
    this.llmTurn(0, {
      think: ['用 html 代码块做实时预览演示。'],
      stream: demo1,
      endAt: 1600,
      end: { finish_reason: 'stop', usage: { Input: 300, Output: 220, CacheRead: 260, TotalTokens: 580 } },
    }, sid)
    this.after(1700, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 1, content: 'html 预览已输出', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.after(1740, () => this.finishRun(sid), sid)
  }

  // ---------- write_html 钩子：write_file 落盘 .html（工具卡「打开」→ file:// 验证） ----------
  // 写一个真实 html 文件（MOCK_WRITE_HTML 内容）→ tool_run_end 带 diff.path。
  private writeHtmlScript(sid: string, runId: string) {
    const HTML_FILE = 'demo-page.html'
    const content = '<!doctype html>\n<html lang="zh">\n<head>\n<meta charset="utf-8">\n' +
      '<style>\n  body { font-family: system-ui, sans-serif; background: #f0f2f5; display: flex; justify-content: center; align-items: center; min-height: 100vh; margin: 0; }\n' +
      '  .card { width: 300px; background: #fff; border-radius: 16px; padding: 24px; box-shadow: 0 10px 30px rgba(0,0,0,.08); text-align: center; }\n' +
      '  .card h2 { margin: 0 0 8px; color: #2c3e50; }\n' +
      '  .card p { color: #666; font-size: 14px; }\n' +
      '  .card button { margin-top: 12px; padding: 8px 20px; border: 0; border-radius: 8px; background: #3498db; color: #fff; cursor: pointer; }\n' +
      '</style>\n</head>\n<body>\n  <div class="card">\n' +
      '    <h2>这是落盘的 HTML 文件</h2>\n    <p>由 write_file 写入 demo-page.html，经 file:// 在右侧浏览器加载。</p>\n' +
      '    <button onclick="this.textContent=\'已点击 ✓\'">点我</button>\n  </div>\n</body>\n</html>'
    this.llmTurn(0, {
      think: ['生成一个演示 HTML 页面并落盘为 demo-page.html。'],
      stream: '我来写一个演示页面并保存到 `demo-page.html`。',
      endAt: 700,
      end: { finish_reason: 'tool_call', usage: { Input: 200, Output: 40, CacheRead: 180, TotalTokens: 240 }, tool_calls: [{ id: 'w-1', name: 'write_file', arguments: `{"path":"${HTML_FILE}"}` }] },
    }, sid)
    this.toolStart('w-1', 'write_file', `{"path":"${HTML_FILE}"}`, runId, 720, sid)
    this.toolEnd('w-1', 'write_file', 'wrote demo-page.html (+21 -0)', runId, 800, sid, {
      diff: { path: HTML_FILE, added: 21, removed: 0, unified: content.split('\n').map((l) => '+' + l).join('\n') },
    })
    this.llmTurn(840, {
      stream: `页面已写入 \`${HTML_FILE}\` —— 工具卡上点「打开」即可在右侧浏览器查看。`,
      endAt: 1500,
      end: { finish_reason: 'stop', usage: { Input: 260, Output: 30, CacheRead: 230, TotalTokens: 290 } },
    }, sid)
    this.after(1560, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 1, content: '写 html 完成', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.after(1600, () => this.finishRun(sid), sid)
  }

  // ---------- 宽表格钩子：输出超宽 markdown 表格（表格横滚验证） ----------
  // 复现用户实测场景：单元格里是不可断的长 token（长路径 / 长标识符）→ 表格 min-content
  // 宽度超过对话列宽。修复前表格被 .narrative{overflow-x:hidden} 裁掉（右侧列看不全、
  // 也滑不动）；修复后由 .md-table-wrap 横滚。
  private wideTableScript(sid: string, runId: string) {
    const table = [
      '审计结果',
      '',
      '| # | 端点 | 身份来源 | 写入业务行 | 审计表 operator_user | 备注 |',
      '| --- | --- | --- | --- | --- | --- |',
      '| 1 | `internal/api/v2/report/offline_handler.go` | ✅ user_id + tenant_id | ❌ 仅落 user_id | ✅（tenant_id > 0） | — |',
      '| 2 | `internal/api/v2/release/offline_handler.go` | ✅ user_id + tenant_id | ❌ 仅 trigger message | ✅（tenant_id > 0） | 与审计行同样漏 tenant_id |',
      '| 3 | `internal/workflow/agents/offline_handler.go` | ⚠️ 仅 user_id | ❌ modify_by / release_user / editor_id | ❌ 断链 | 漏 tenant_id |',
      '| 4 | `internal/robot/release/offline_handler.go` | ✅ user_id + tenant_id | modify_by / release_user / editor_id | ✅ | 覆盖 |',
      '',
      '两点需要你决策：',
      '',
      '1. **#3 漏传 tenant_id** 是明确的缺陷 —— 服务层代码写好了却进不去分支。按仓库规范这属于可复现缺陷，应走 spec → plan → 修。',
      '2. **#1/#2 的 user_id 未落业务表**是与既有实现对齐的现状，不算迁移偏差；但 #1 的 `OfflineInput.UserID` 属于纯死参数，可以考虑清掉或补写。',
      '',
      '窄表（列少、可断词）仍应占满列宽，不出现横条：',
      '',
      '| 项 | 值 |',
      '| --- | --- |',
      '| 状态 | 等待确认 |',
      '| 影响面 | 仅 offline 链路 |',
    ].join('\n')
    this.llmTurn(0, {
      think: ['汇总四条 offline 链路的操作人来源与落库情况，用宽表呈现。'],
      stream: table,
      endAt: 900,
      end: { finish_reason: 'stop', usage: { Input: 320, Output: 260, CacheRead: 280, TotalTokens: 580 } },
    }, sid)
    this.after(1000, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 1, content: '宽表格已输出', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.after(1040, () => this.finishRun(sid), sid)
  }

  // ---------- mermaid 钩子：输出含图表的 markdown（MermaidBlock 渲染验证） ----------
  // bad=true → 输出语法错误的图（验证错误回退显示源码）。
  // 否则输出 mermaid 全类型演示（flowchart / sequence / class / state / er / gantt / pie / mindmap / journey）。
  private mermaidScript(sid: string, runId: string, bad = false) {
    const diagram = bad
      ? '这个图语法有误：\n\n```mermaid\ngraph TD\n  A[开始] -->\n  B --x 悬挂边\n```\n\n下面是确定无法解析的图：\n\n```mermaid\nbogusdiagram\n  this is not valid\n```'
      : `这里是一次输出的 mermaid 全类型演示，从上到下依次是：

**1. flowchart 流程图（分支 + 子图）**

\`\`\`mermaid
flowchart TB
  subgraph 认证
    A[收到请求] --> B{校验 token}
    B -->|过期| C[401 token_expired]
    B -->|损坏| D[401 token_invalid]
    B -->|有效| E[放行]
  end
  subgraph 审计
    C --> F[记录审计]
    D --> F
    E --> G[记录访问]
  end
\`\`\`

**2. sequenceDiagram 时序图**

\`\`\`mermaid
sequenceDiagram
  participant C as 客户端
  participant S as 服务端
  participant DB as 数据库
  C->>S: POST /login
  S->>DB: 查用户
  DB-->>S: 用户+凭证
  alt 凭证有效
    S-->>C: 200 OK
  else 凭证无效
    S-->>C: 401 token_expired
  end
\`\`\`

**3. classDiagram 类图**

\`\`\`mermaid
classDiagram
  class User {
    +string id
    +string name
    +Login() bool
    +Logout() void
  }
  class Session {
    +string token
    +expiresAt time
    +Validate() bool
  }
  class AuthService {
    +Login(req) Resp
    +Refresh(token) Resp
  }
  User --> Session : 持有
  AuthService --> User : 操作
  AuthService --> Session : 签发
\`\`\`

**4. stateDiagram 状态图**

\`\`\`mermaid
stateDiagram-v2
  [*] --> 待支付
  待支付 --> 已支付: 支付成功
  待支付 --> 已取消: 用户取消
  已支付 --> 已发货: 商家发货
  已发货 --> 已完成: 确认收货
  已取消 --> [*]
  已完成 --> [*]
\`\`\`

**5. erDiagram ER 图**

\`\`\`mermaid
erDiagram
  USER ||--o{ ORDER : 下单
  ORDER ||--|{ ORDER_ITEM : 包含
  PRODUCT ||--o{ ORDER_ITEM : 被购买
  USER {
    string id PK
    string name
  }
  ORDER {
    string id PK
    string user_id FK
    string status
  }
  PRODUCT {
    string id PK
    string title
    float price
  }
\`\`\`

**6. gantt 甘特图**

\`\`\`mermaid
gantt
  title 项目排期
  dateFormat YYYY-MM-DD
  section 设计
    需求分析 :a1, 2026-09-01, 3d
    原型设计 :a2, after a1, 4d
  section 开发
    后端开发 :b1, after a2, 7d
    前端开发 :b2, after a2, 7d
  section 测试
    集成测试 :c1, after b1, 3d
    上线发布 :c2, after c1, 2d
\`\`\`

**7. pie 饼图**

\`\`\`mermaid
pie title 流量来源占比
  "直接访问" : 45
  "搜索引擎" : 30
  "社交媒体" : 15
  "外部链接" : 10
\`\`\`

**8. mindmap 思维导图**

\`\`\`mermaid
mindmap
  root((登录系统))
    认证
      密码校验
      Token 签发
      刷新机制
    安全
      限流
      审计日志
      风控策略
    前端
      登录页
      会话管理
\`\`\`

**9. journey 用户旅程**

\`\`\`mermaid
journey
  title 登录体验旅程
  section 打开应用
    启动速度: 5: 用户
    看到登录页: 4: 用户
  section 登录
    输入账号: 3: 用户
    点击登录: 4: 用户
    等待响应: 2: 用户
    进入首页: 5: 用户
\`\`\`

**10. 数学公式（KaTeX，行内 + 块级）**

行内公式：质能方程 $E = mc^2$，欧拉恒等式 $e^{i\\pi} + 1 = 0$，勾股定理 $a^2 + b^2 = c^2$。

块级公式（高斯积分）：

$$
\\int_{-\\infty}^{\\infty} e^{-x^2} dx = \\sqrt{\\pi}
$$

再来看一个贝叶斯定理：

$$
P(A|B) = \\frac{P(B|A) \\cdot P(A)}{P(B)}
$$

以及级数展开：

$$
e^x = \\sum_{n=0}^{\\infty} \\frac{x^n}{n!} = 1 + x + \\frac{x^2}{2!} + \\frac{x^3}{3!} + \\cdots
$$`
    this.llmTurn(0, {
      think: ['用 mermaid 输出全类型图表 + KaTeX 数学公式演示。'],
      stream: diagram,
      endAt: 2600,
      end: { finish_reason: 'stop', usage: { Input: 400, Output: 800, CacheRead: 320, TotalTokens: 1200 } },
    }, sid)
    this.after(2700, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 1, content: 'mermaid 全类型图表 + 数学公式已输出', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.after(2740, () => this.finishRun(sid), sid)
  }

  // ---------- 长会话钩子：一次性注入大量历史消息（content-visibility 滚动定位验证） ----------
  // 模拟「恢复一个很长的会话」：逐个 llm 轮（llm_start → 一次性 content_chunk →
  // llm_end）快速生成 25 条长 assistant 历史，撑出远超一屏的内容。
  // 切回该会话时离屏历史块用 contain-intrinsic-size 估算占位 —— scrollHeight
  // 先偏小、真实布局异步落定后才变大。用于验证切换会话的持续校准贴底。
  private longHistoryScript(sid: string, runId: string) {
    this.emit({ event_type: 'user_inputs_consumed', session_id: sid, user_texts: ['长会话'], timestamp: ts() })
    for (let i = 0; i < 25; i++) {
      const rid = `long-${i}`
      this.emit({ event_type: 'llm_start', session_id: sid, request_id: rid, run_id: runId, model: this.model, timestamp: ts() })
      this.emit({
        event_type: 'content_chunk',
        session_id: sid, request_id: rid, run_id: runId,
        content: `历史消息 ${i + 1}\n\n` + Array.from({ length: 14 }, (_, k) => `这是第 ${i + 1} 条消息的第 ${k + 1} 段正文，用于撑高会话内容，模拟真实长会话的渲染体积与离屏估算。`).join('\n\n'),
        timestamp: ts(),
      })
      this.emit({
        event_type: 'llm_end', session_id: sid, request_id: rid, run_id: runId, model: this.model, ts: ts(),
        finish_reason: 'stop',
        usage: { Input: 200, Output: 100, CacheRead: 180, CacheWrite: 20, TotalTokens: 300 },
        timestamp: ts(),
      })
    }
    this.after(50, () => {
      this.emit({ event_type: 'agent_start', session_id: sid, run_id: runId, index: 1, content: '长会话已恢复', model: this.model, timestamp: ts() })
      this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 1, content: '长会话已恢复', finish_reason: 'end', model: this.model, timestamp: ts() })
      this.finishRun(sid)
    }, sid)
  }

  // ---------- 常规模式：TODO + 多工具 + 审批 diff + ask_user + 异步子 agent（自动回传） ----------
  private normalScript(sid: string, runId: string) {
    this.llmTurn(0, {
      think: [
        '先读 auth.go 确认当前错误处理路径。/api/login 唯一入口，Login 里只有一处 errors.Is 判断。',
        'JWT 过期的 sentinel 是 jwt.ErrTokenExpired，和 ErrUnauthorized 是两个不同 error，errors.Is 不会命中 —— 401 被吞成 500 的根因。',
        '还要看 auth_test.go 现有覆盖，确认改动范围。',
      ],
      stream: '我先建两条待办，再逐个读文件确认现状：确认错误路径 → 修复并补用例 → 回归。',
      endAt: 900,
      end: { finish_reason: 'tool_call', usage: { Input: 520, Output: 60, CacheRead: 480, Reasoning: 22, TotalTokens: 580 }, tool_calls: [{ id: 't-1', name: 'todo_add', arguments: '{"title":"确认 auth.go 错误路径"}' }, { id: 't-2', name: 'todo_add', arguments: '{"title":"修复过期分支并补用例"}' }] },
    }, sid)
    this.toolStart('t-1', 'todo_add', '{"title":"确认 auth.go 错误路径"}', runId, 920, sid)
    this.toolStart('t-2', 'todo_add', '{"title":"修复过期分支并补用例"}', runId, 930, sid)
    this.toolEnd('t-1', 'todo_add', '[todo: 1 open, 0 done]\n- t1 [ ] 确认 auth.go 错误路径', runId, 970, sid, { todos: [{ id: 't1', title: '确认 auth.go 错误路径', done: false }] })
    this.toolEnd('t-2', 'todo_add', '[todo: 2 open, 0 done]\n- t1 [ ] 确认 auth.go 错误路径\n- t2 [ ] 修复过期分支并补用例', runId, 980, sid, { todos: [{ id: 't1', title: '确认 auth.go 错误路径', done: false }, { id: 't2', title: '修复过期分支并补用例', done: false }] })

    this.llmTurn(1000, {
      think: ['先看工作区结构，确认 auth 包位置。'],
      stream: '工作区结构：`auth.go` 与 `auth_test.go` 在同包。开始逐个读取。',
      endAt: 1500,
      end: { finish_reason: 'tool_call', usage: { Input: 700, Output: 40, CacheRead: 650, TotalTokens: 740 }, tool_calls: [{ id: 't-3', name: 'list_dir', arguments: '{"path":"."}' }] },
    }, sid)
    this.toolStart('t-3', 'list_dir', '{"path":"."}', runId, 1520, sid)
    this.toolEnd('t-3', 'list_dir', 'auth.go\nauth_test.go\ngo.mod\nREADME.md', runId, 1580, sid)

    this.llmTurn(1620, {
      think: ['读取 auth.go 前 200 行，确认 Login 实现与错误分支。'],
      stream: '读取 `auth.go`（1-200 行）。',
      endAt: 2100,
      end: { finish_reason: 'tool_call', usage: { Input: 900, Output: 45, CacheRead: 850, TotalTokens: 945 }, tool_calls: [{ id: 't-4', name: 'read_file', arguments: '{"path":"auth.go","start_line":1,"end_line":200}' }] },
    }, sid)
    this.toolStart('t-4', 'read_file', '{"path":"auth.go","start_line":1,"end_line":200}', runId, 2120, sid)
    this.toolEnd('t-4', 'read_file', readResult('auth.go', 1, 200), runId, 2200, sid)

    this.llmTurn(2240, {
      think: ['确认测试文件覆盖：读 auth_test.go 前 120 行。'],
      stream: '再读 `auth_test.go` 确认现有用例。',
      endAt: 2700,
      end: { finish_reason: 'tool_call', usage: { Input: 1020, Output: 40, CacheRead: 950, TotalTokens: 1060 }, tool_calls: [{ id: 't-5', name: 'read_file', arguments: '{"path":"auth_test.go","start_line":1,"end_line":120}' }] },
    }, sid)
    this.toolStart('t-5', 'read_file', '{"path":"auth_test.go","start_line":1,"end_line":120}', runId, 2720, sid)
    this.toolEnd('t-5', 'read_file', readResult('auth_test.go', 1, 120), runId, 2800, sid)

    this.llmTurn(2840, {
      think: ['确认 ErrTokenExpired 是否已定义：grep 全仓。'],
      stream: 'grep 确认 `ErrTokenExpired`。',
      endAt: 3300,
      end: { finish_reason: 'tool_call', usage: { Input: 1120, Output: 36, CacheRead: 1050, TotalTokens: 1156 }, tool_calls: [{ id: 't-6', name: 'grep', arguments: '{"pattern":"ErrTokenExpired","path":"."}' }] },
    }, sid)
    this.toolStart('t-6', 'grep', '{"pattern":"ErrTokenExpired","path":"."}', runId, 3320, sid)
    this.toolEnd('t-6', 'grep', '无匹配：ErrTokenExpired 尚未定义', runId, 3380, sid)

    // 根因确认后，「过期 token 对外语义」与「改动范围」都是产品决策——模型不瞎猜，
    // 用 ask_user 一次问两条（HITL 反方向：SDK 批量工具 → 前端提问列表 modal）
    // 第一条问题的选项带 preview（「必须看见才能选」的备选：三条返回形态各不相同，
    // 描述文字说不清，直接给报文头/响应体）。第二条仍用纯字符串 —— 演示里同时覆盖
    // 预览版面与字符串兼容形态（见 lib/questionOptions）。
    const askArgs = '{"questions":[{"question":"过期 token 的对外行为应如何处理？","options":[' +
      '{"label":"401 + token_expired 错误码","preview":"HTTP/1.1 401 Unauthorized\\n\\n{\\"code\\": \\"token_expired\\"}"},' +
      '{"label":"静默自动续期","preview":"HTTP/1.1 200 OK\\nSet-Cookie: session=<new>\\n\\n(调用方无感，拿到新 session)"},' +
      '{"label":"记录日志继续放行","preview":"HTTP/1.1 200 OK\\n\\n(log) token expired → pass-through"}]},' +
      '{"question":"改动范围？","options":["仅 auth 包","连同其余 500 兜底一起收敛"]}]}'
    this.llmTurn(3420, {
      think: [
        '根因确认：`errors.Is(err, ErrUnauthorized)` 漏掉 JWT 过期分支，过期 token 落入通用 500。',
        '但「过期 token 的对外语义」与「改动范围」是产品决策，不该猜 —— 用 ask_user 一次问两条。',
        '第一条的三个备选返回形态不同，光看标签选不出来：给每个选项配 preview，让用户看着报文头拍板。',
      ],
      stream: '根因确认，但有两个设计决策需要你一并拍板…',
      endAt: 4200,
      end: { finish_reason: 'tool_call', usage: { Input: 1600, Output: 120, CacheRead: 1450, Reasoning: 38, TotalTokens: 1720 }, tool_calls: [{ id: 't-q', name: 'ask_user', arguments: askArgs }] },
    }, sid)
    this.toolStart('t-q', 'ask_user', askArgs, runId, 4220, sid)
    this.after(4260, () => {
      // 提问批次事件（id = batch_id 锚，单题含独立 id）；SDK 注册先行（Begin）→ 事件 → 工具阻塞等回答
      this.emit({
        event_type: 'ask_user_question', session_id: sid, run_id: runId, id: 'q-1',
        questions: [
          {
            id: 'q-1-1', question: '过期 token 的对外行为应如何处理？',
            options: [
              { label: '401 + token_expired 错误码', preview: 'HTTP/1.1 401 Unauthorized\n\n{"code": "token_expired"}' },
              { label: '静默自动续期', preview: 'HTTP/1.1 200 OK\nSet-Cookie: session=<new>\n\n(调用方无感，拿到新 session)' },
              { label: '记录日志继续放行', preview: 'HTTP/1.1 200 OK\n\n(log) token expired → pass-through' },
            ],
          },
          // 纯字符串形态（历史事件/回放/模型简写）——前端归一化成 {label}
          { id: 'q-1-2', question: '改动范围？', options: ['仅 auth 包', '连同其余 500 兜底一起收敛'] },
        ], timestamp: ts(),
      })
      this.state(sid).pendingQuestion = { batchId: 'q-1', toolId: 't-q', runId }
    }, sid)
  }

  // 用户回答 ask_user 批次之后：按整批选择执行编辑 + 触发审批（原 edit 决策轮移到这里，answer_question 命令驱动）
  private postQuestion(sid: string, runId: string) {
    const st = this.state(sid)
    if (st.stopped) return
    const summary = st.lastAnswers.map((a) => a.answer).filter(Boolean).join('、') || '跳过'
    this.llmTurn(0, {
      think: ['已收到用户整批回答：' + st.lastAnswers.map((a) => a.answer).join('；') + '。按此执行：Login 错误分支先判 jwt.ErrTokenExpired → 401 + token_expired 错误码，其余错误仍回 500。'],
      stream: `按你的选择（${summary}）处理：\`auth.go\` 的 \`Login\` 错误分支先判令牌过期，过期 → 401 + \`token_expired\` 错误码，其余仍回 500。`,
      endAt: 700,
      end: { finish_reason: 'tool_call', usage: { Input: 1600, Output: 150, CacheRead: 1450, Reasoning: 38, TotalTokens: 1750 }, tool_calls: [{ id: 't-7', name: 'edit_file', arguments: '{"path":"auth.go","old_string":"if err != nil { w.WriteHeader(500); return }","new_string":"if errors.Is(err, jwt.ErrTokenExpired) { 401 + token_expired; return }"}' }] },
    }, sid)
    this.toolStart('t-7', 'edit_file', '{"path":"auth.go","old_string":"if err != nil { w.WriteHeader(500); return }","new_string":"…401 + token_expired 分支…"}', runId, 720, sid)
    // 审批只在「用户确认」（hitl/manual）模式弹出：auto（自主判断）下 write/edit 工具自声明自动、
    // full-access（完全访问）全自动——普通编辑/写文件无风险直接执行（对齐真 bridge parseMode）。
    const needsApproval = st.mode === 'hitl' || st.mode === 'manual'
    this.after(760, () => {
      if (needsApproval) {
        this.emit({
          event_type: 'tool_approval_requested', session_id: sid, id: 't-7', index: 0, name: 'edit_file',
          run_id: runId, arguments: '{"path":"auth.go","old_string":"…","new_string":"…"}',
          diff: { path: 'auth.go', added: 5, removed: 0, unified: EDIT_DIFF }, timestamp: ts(),
          // 与真 bridge 对齐：审批事件带等待窗口（approvalWindow，2026-09-20 起 30 分钟），
          // 前端倒计时读它；缺它则回退 APPROVAL_TIMEOUT=60s（老行为）
          timeout_ms: 30 * 60 * 1000,
        })
        this.state(sid).pendingApproval = { id: 't-7', runId }
      } else {
        // 直接执行：edit_file 完成（带它自己的 diff 落变更 tab），不弹审批
        this.emit({
          event_type: 'tool_run_end', session_id: sid, id: 't-7', run_id: runId, name: 'edit_file',
          result: 'edited (+5 -0)', is_error: false,
          diff: { path: 'auth.go', added: 5, removed: 0, unified: EDIT_DIFF }, timestamp: ts(),
        })
        this.postApproval(sid, runId)
      }
    }, sid)
  }

  // 审批之后：标记待办 + 派两个并发异步子 agent（测试/审查，各 3-4s 交错流式）+ 结果自动回传 + 收尾
  private postApproval(sid: string, runId: string) {
    const st = this.state(sid)
    if (st.stopped) return
    // main 决策轮：todo_update + 双 agent_spawn
    this.llmTurn(0, {
      requestId: `r-${runId}-1`, runId,
      think: ['改动已批准。标记待办完成，并派两个异步子 agent 并行：跑测试 + 审查改动。'],
      stream: '标记 `t1` 完成，异步派两个 agent：`go test ./auth` 与「审查改动」，我这边等它们并行收尾。',
      endAt: 700,
      end: {
        finish_reason: 'tool_call', usage: { Input: 1850, Output: 70, CacheRead: 1700, TotalTokens: 1920 },
        tool_calls: [
          { id: 't-8', name: 'todo_update', arguments: '{"id":"t1","done":true}' },
          { id: 't-9', name: 'agent_spawn', arguments: '{"task":"go test ./auth"}' },
          { id: 't-10', name: 'agent_spawn', arguments: '{"task":"审查 auth 改动"}' },
        ],
      },
    }, sid)
    this.toolStart('t-8', 'todo_update', '{"id":"t1","done":true}', runId, 720, sid)
    this.toolStart('t-9', 'agent_spawn', '{"task":"go test ./auth"}', runId, 730, sid)
    this.toolStart('t-10', 'agent_spawn', '{"task":"审查 auth 改动"}', runId, 740, sid)
    this.toolEnd('t-8', 'todo_update', '[todo: 1 open, 1 done]\n- t1 [x] 确认 auth.go 错误路径\n- t2 [ ] 修复过期分支并补用例', runId, 780, sid, { todos: [{ id: 't1', title: '确认 auth.go 错误路径', done: true }, { id: 't2', title: '修复过期分支并补用例', done: false }] })
    this.toolEnd('t-9', 'agent_spawn', 'task a1 started (background)', runId, 790, sid)
    this.toolEnd('t-10', 'agent_spawn', 'task a2 started (background)', runId, 800, sid)
    // ---- 异步子 agent：脱离主 run 存活（对齐 2026-08-30 语义：主停止不影响后台任务）----
    // 以下后台任务节点全部走 afterBg/stoppedOk：主「停止」拦截主 run（main 轮/收尾），
    // 但 task_*/agent_* 事件流与结果回传照常推进（子 agent 只经 interrupt_task 精确停止）。
    this.afterBg(820, () => this.emit({ event_type: 'task_started', session_id: sid, run_id: runId, task_id: 'a1', parent_run_id: runId, timestamp: ts() }), sid)
    this.afterBg(830, () => this.emit({ event_type: 'task_started', session_id: sid, run_id: runId, task_id: 'a2', parent_run_id: runId, timestamp: ts() }), sid)
    // 两个后台 agent 同时开跑（并发卡片）
    this.afterBg(840, () => this.emit({ event_type: 'agent_start', session_id: sid, run_id: `sub-${runId}-A`, parent_run_id: runId, depth: 1, content: 'subagent · go test ./auth', timestamp: ts() }), sid)
    this.afterBg(850, () => this.emit({ event_type: 'agent_start', session_id: sid, run_id: `sub-${runId}-B`, parent_run_id: runId, depth: 1, content: 'subagent · 审查 auth 改动', timestamp: ts() }), sid)

    // ---- sub-A（测试，~3.2s）：两轮，与 sub-B 交错流式 ----
    this.llmTurn(900, {
      requestId: `r-${runId}-A1`, runId: `sub-${runId}-A`, stoppedOk: true,
      think: ['读 auth_test.go 确认用例结构，再跑测试。'],
      stream: '运行 `go test ./auth`，先确认用例…',
      endAt: 2000,
      end: { finish_reason: 'tool_call', usage: { Input: 420, Output: 30, CacheRead: 380, TotalTokens: 450 }, tool_calls: [{ id: 't-11', name: 'read_file', arguments: '{"path":"auth_test.go","start_line":1,"end_line":200}' }] },
    }, sid)
    this.toolStart('t-11', 'read_file', '{"path":"auth_test.go","start_line":1,"end_line":200}', `sub-${runId}-A`, 2020, sid, true)
    this.toolEnd('t-11', 'read_file', readResult('auth_test.go', 1, 200), `sub-${runId}-A`, 2100, sid, undefined, true)
    this.llmTurn(2140, {
      requestId: `r-${runId}-A2`, runId: `sub-${runId}-A`, stoppedOk: true,
      think: ['结果收集完成，输出结论。'],
      stream: 'auth 包测试全部通过，无回归。',
      endAt: 3000,
      end: { finish_reason: 'stop', usage: { Input: 520, Output: 40, CacheRead: 480, TotalTokens: 560 } },
    }, sid)
    this.afterBg(3060, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: `sub-${runId}-A`, parent_run_id: runId, depth: 1, content: 'subagent · go test ./auth', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.afterBg(3080, () => this.emit({ event_type: 'task_end', session_id: sid, task_id: 'a1', run_id: runId, result: 'ok', usage: { Input: 940, Output: 70, CacheRead: 860, TotalTokens: 1010, Cost: { Input: 11, CacheRead: 12, Output: 20, Total: 43 } }, timestamp: ts() }), sid)
    // 结果自动回传（无 agent_wait）：任务完成 → 推入主会话 → task_result_delivered 事件（客户端渲染折叠块）
    this.afterBg(3100, () => this.emit({ event_type: 'task_result_delivered', session_id: sid, task_id: 'a1', result: 'ok', usage: { Input: 940, Output: 70, CacheRead: 860, TotalTokens: 1010, Cost: { Input: 11, CacheRead: 12, Output: 20, Total: 43 } }, timestamp: ts() }), sid)

    // ---- sub-B（审查，~4.3s，更久）：两轮，与 sub-A 交错 ----
    this.llmTurn(1100, {
      requestId: `r-${runId}-B1`, runId: `sub-${runId}-B`, stoppedOk: true,
      think: ['读取 auth.go 改动，逐一核对错误分支语义。'],
      stream: '审查 `auth.go` 错误分支：过期/损坏/内部错误三路是否各归其位…',
      endAt: 2600,
      end: { finish_reason: 'tool_call', usage: { Input: 500, Output: 34, CacheRead: 460, TotalTokens: 534 }, tool_calls: [{ id: 't-12', name: 'read_file', arguments: '{"path":"auth.go","start_line":1,"end_line":200}' }] },
    }, sid)
    this.toolStart('t-12', 'read_file', '{"path":"auth.go","start_line":1,"end_line":200}', `sub-${runId}-B`, 2620, sid, true)
    this.toolEnd('t-12', 'read_file', readResult('auth.go', 1, 200), `sub-${runId}-B`, 2700, sid, undefined, true)
    this.llmTurn(2740, {
      requestId: `r-${runId}-B2`, runId: `sub-${runId}-B`, stoppedOk: true,
      think: ['分支语义正确，用例覆盖两条路径，无回归风险。'],
      stream: '审查通过：401/500 边界正确。',
      endAt: 3800,
      end: { finish_reason: 'stop', usage: { Input: 600, Output: 50, CacheRead: 540, TotalTokens: 650 } },
    }, sid)
    this.afterBg(3860, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: `sub-${runId}-B`, parent_run_id: runId, depth: 1, content: 'subagent · 审查 auth 改动', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.afterBg(3880, () => this.emit({ event_type: 'task_end', session_id: sid, task_id: 'a2', run_id: runId, result: '审查通过', usage: { Input: 1100, Output: 84, CacheRead: 1000, TotalTokens: 1184, Cost: { Input: 14, CacheRead: 14, Output: 24, Total: 52 } }, timestamp: ts() }), sid)
    this.afterBg(3900, () => this.emit({ event_type: 'task_result_delivered', session_id: sid, task_id: 'a2', result: '审查通过', usage: { Input: 1100, Output: 84, CacheRead: 1000, TotalTokens: 1184, Cost: { Input: 14, CacheRead: 14, Output: 24, Total: 52 } }, timestamp: ts() }), sid)

    // ---- main：结果自动回传后模型续跑（无 agent_wait——模型看到 task_result 继续）----
    this.llmTurn(3960, {
      requestId: `r-${runId}-2`, runId,
      think: ['两个后台任务结果已自动回传：go test ./auth 通过、审查确认 401/500 边界正确。据此继续收尾。'],
      stream: '收到两个子 agent 结果（自动回传）：`go test ./auth` 通过 + 审查确认无回归。',
      endAt: 4800,
      end: { finish_reason: 'stop', usage: { Input: 1400, Output: 60, CacheRead: 1300, TotalTokens: 1460 } },
    }, sid)

    // ---- main：总结 ----
    this.llmTurn(4840, {
      requestId: `r-${runId}-3`, runId,
      think: ['全部完成，写总结。'],
      stream: '完成 ✅ 已改 1 个文件（auth.go），+5 -0。\n\n**改动要点**\n- `auth.go`：`Login` 错误分支先判 `jwt.ErrTokenExpired`，令牌过期单独映射为 401 + `X-Auth-Error: token_expired`；其余错误仍回 500。\n\n```go\nif err != nil {\n\tif errors.Is(err, jwt.ErrTokenExpired) {\n\t\tw.Header().Set("X-Auth-Error", "token_expired")\n\t\tw.WriteHeader(401)\n\t\treturn\n\t}\n\tw.WriteHeader(500)\n\treturn\n}\n```\n\n两个异步 agent 并行完成：`go test ./auth` 通过 + 审查确认无回归。',
      endAt: 5800,
      end: { finish_reason: 'stop', usage: { Input: 2100, Output: 180, CacheRead: 1900, Reasoning: 12, TotalTokens: 2280 } },
    }, sid)
    this.after(5820, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 2, content: '完成', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    // 第二个根 run：触发「旧 run 自动折叠」
    this.after(5840, () => this.emit({ event_type: 'agent_start', session_id: sid, run_id: `${runId}-2`, index: 0, content: '把 auth 用例并入全量测试', model: this.model, timestamp: ts() }), sid)
    this.llmTurn(5880, {
      requestId: `r-${runId}-4`, runId: `${runId}-2`,
      stream: '已并入全量测试，`go test ./...` 全部通过。',
      endAt: 6400,
      end: { finish_reason: 'stop', usage: { Input: 500, Output: 40, CacheRead: 460, TotalTokens: 540 } },
    }, sid)
    this.after(6420, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: `${runId}-2`, index: 1, content: '全量通过', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.after(6460, () => this.finishRun(sid), sid)
  }

  // ---------- 长工具 promote 演示（docs/ASYNC_TOOLS_AND_PROMOTE.md）：bash sleep 30 → 用户「移到后台」→ 自动回传 ----------
  // 用户在输入里带「长任务 / sleep 30 / 移到后台」触发：工具卡在运行态，等 promote_task 命令。
  private promoteScript(sid: string, runId: string) {
    const st = this.state(sid)
    this.llmTurn(0, {
      think: ['跑一个长时间构建：bash sleep 30 会持续约 30 秒——运行中可被用户移到后台，主线不阻塞。'],
      stream: '开始跑长时间构建 `sleep 30` —— 会持续约 30 秒。你可以把它移到后台，我这边继续。',
      endAt: 700,
      end: { finish_reason: 'tool_call', usage: { Input: 320, Output: 30, CacheRead: 280, TotalTokens: 350 }, tool_calls: [{ id: 'b-1', name: 'bash', arguments: '{"command":"sleep 30"}' }] },
    }, sid)
    this.after(720, () => {
      // tool_run_start 带 task_id（引擎批内自增 tooltask-N；前端据此显示「后台」按钮）
      st.promote = { taskId: 'tooltask-1', runId, name: 'bash' }
      this.emit({ event_type: 'tool_run_start', session_id: sid, id: 'b-1', run_id: runId, name: 'bash', task_id: 'tooltask-1', arguments: '{"command":"sleep 30"}', timestamp: ts() })
    }, sid)
    // 工具在此「卡住」（长时运行）——等用户点工具行的「后台」→ promote_task 命令 → promoteContinue
  }

  // promote_task 之后：主 loop 已拿占位 {task_id,status:running} → 模型继续；后台工具完成 → 自动回传。
  // 后台工具回传节点（tool_run_end / task_result_delivered / 后台完成说明轮）走 afterBg：
  // 主「停止」不影响已 promote 的后台任务（2026-08-30 语义），其终止只经 interrupt_task
  // 精确中断（由 toolInterrupted 终态标志门控，见 interrupt_task 分支）。
  private promoteContinue(sid: string, runId: string) {
    const st = this.state(sid)
    // 主 run 的「已移到后台」说明轮无需入口 guard：其事件节点均为 stopped 门控
    //（主停止后不再输出主 run 叙事，但后台任务回传照常，见下方 afterBg）。
    // 模型下轮：看到「已移到后台」，不重复调用，继续干活（提示词注记引导）
    this.llmTurn(0, {
      think: ['bash 已移到后台运行（占位结果 {status:running,task_id:tooltask-1}）。不重复调用，先推进其余收尾。'],
      stream: '`bash` 已移到后台运行，不阻塞主线 —— 先整理收尾步骤，结果完成后会自动回传。',
      endAt: 700,
      end: { finish_reason: 'stop', usage: { Input: 380, Output: 40, CacheRead: 340, TotalTokens: 420 } },
    }, sid)
    // 后台工具完成：tool_run_end（前端工具行更新，两路并发）+ task_result_delivered（模型续跑折叠块）
    this.afterBg(900, () => {
      if (st.toolInterrupted) {
        // 已被 interrupt_task 精确停止：完成交付已由中断分支发出，不再重复
        st.toolDone = true
        st.bgTool = undefined
        return
      }
      st.toolDone = true
      st.bgTool = undefined
      this.emit({ event_type: 'tool_run_end', session_id: sid, id: 'b-1', run_id: runId, name: 'bash', result: 'sleep done (30s)', is_error: false, timestamp: ts() })
      // 任务终态（真桥：toolTaskSink 发 TaskEnd；普通工具不花模型钱 → 无 usage 字段）
      this.emit({ event_type: 'task_end', session_id: sid, task_id: 'tooltask-1', status: 'completed', result: 'sleep done (30s)', timestamp: ts() })
      this.emit({ event_type: 'task_result_delivered', session_id: sid, task_id: 'tooltask-1', result: 'sleep done (30s)', timestamp: ts() })
    }, sid)
    this.afterBg(1000, () => {
      if (st.toolInterrupted) return // 中断分支已另发说明轮
      this.llmTurn(0, {
        think: ['后台任务 tooltask-1 完成（sleep done）→ 据此继续收尾。'],
        stream: '后台任务 `tooltask-1` 已完成：`sleep done (30s)`。收尾总结。',
        endAt: 1700,
        end: { finish_reason: 'stop', usage: { Input: 400, Output: 45, CacheRead: 360, TotalTokens: 445 } },
      }, sid)
    }, sid)
    this.afterBg(1760, () => this.emit({ event_type: 'agent_end', session_id: sid, run_id: runId, index: 2, content: '长任务后台完成', finish_reason: 'end', model: this.model, timestamp: ts() }), sid)
    this.afterBg(1800, () => this.finishRun(sid), sid)
  }
}

// —— 使用统计示例数据（浏览器 mock；真 bridge 由 ~/.go-code/metrics.jsonl 聚合）——
// 形状与 Go MetricsReport 完全对齐（Stats/Total/ByDay+段落 CumTokens/ByDayModel/ByModel/
// ByWorkspace），这样设置页与空会话图表在浏览器里能按真实布局渲染。确定性伪随机（固定种子）
// → 每次刷新曲线一致，便于截图比对。
const MOCK_STATS_MODELS = [
  { key: 'deepseek/deepseek-v4.1-flash', weight: 0.62, inTok: 26000, outTok: 900, price: { in: 0.14, out: 0.28 } },
  { key: 'deepseek/deepseek-v4-pro', weight: 0.2, inTok: 41000, outTok: 1400, price: { in: 0.435, out: 0.87 } },
  { key: 'opencode/glm-5.2', weight: 0.13, inTok: 52000, outTok: 1800, price: { in: 1.4, out: 4.4 } },
  { key: 'anthropic/claude-opus-4.8', weight: 0.05, inTok: 68000, outTok: 2600, price: { in: 15, out: 75 } },
]

function mockMetricsReport(from: string): Record<string, unknown> {
  const day = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
  const today = new Date()
  today.setHours(0, 0, 0, 0)
  const days: { key: string; date: Date; total: number; cost: number; models: { model: string; inTok: number; outTok: number; cost: number }[] }[] = []
  let seed = 20260919
  const rnd = () => ((seed = (seed * 1103515245 + 12345) & 0x7fffffff) / 0x7fffffff)
  // 活跃度：**不是每天都有用量**（真实 metrics.jsonl 近一年 37 天有记录）——工作日约六成活跃、
  // 周末一成多，另有几段连续断档（出差/休假）：热力图才有留白与连续天数分段，不是一片实心。
  const GAPS = [[40, 47], [118, 124], [196, 198], [268, 275]]
  for (let i = 364; i >= 0; i--) {
    const date = new Date(today)
    date.setDate(date.getDate() - i)
    const dow = date.getDay()
    const weekend = dow === 0 || dow === 6
    let scale = 0
    if (rnd() < (weekend ? 0.16 : 0.64)) scale = (weekend ? 0.2 : 0.5) + rnd() * 0.55
    if (GAPS.some(([a, b]) => i >= a && i <= b)) scale = 0 // 连续断档
    if (i === 12) scale = 1.9 // 历史峰值日
    if (i === 2) scale = 1.15
    if (i === 1) scale = 1.0
    if (i === 0) scale = 0.35
    const turns = scale === 0 ? 0 : 2 + Math.round(scale * 14 + rnd() * 9)
    const models: { model: string; inTok: number; outTok: number; cost: number }[] = []
    let total = 0
    let cost = 0
    if (scale === 0) {
      days.push({ key: day(date), date, total: 0, cost: 0, models }) // 未使用：不产数据（热力图留白）
      continue
    }
    for (const m of MOCK_STATS_MODELS) {
      const share = m.weight * (0.7 + rnd() * 0.6)
      const n = Math.max(1, Math.round(turns * share))
      const inTok = Math.round(n * m.inTok * (0.85 + rnd() * 0.3))
      const outTok = Math.round(n * m.outTok * (0.85 + rnd() * 0.3))
      const c = (inTok * m.price.in + outTok * m.price.out) / 1e6
      models.push({ model: m.key, inTok, outTok, cost: c })
      total += inTok + outTok
      cost += c
    }
    if (total > 0) days.push({ key: day(date), date, total, cost, models })
  }

  const cut = from || '0000-00-00'
  const inWindow = days.filter((d) => d.key >= cut)
  let cum = 0
  const cumByDay = new Map<string, number>()
  for (const d of days) {
    cum += d.total
    cumByDay.set(d.key, cum)
  }
  const byDayModel = new Map<string, { Day: string; Model: string; Input: number; Output: number; CacheRead: number; CostUsd: number }>()
  for (const d of inWindow) {
    for (const m of d.models) {
      const k = `${d.key}\u0000${m.model}`
      const row = byDayModel.get(k) ?? { Day: d.key, Model: m.model, Input: 0, Output: 0, CacheRead: 0, CostUsd: 0 }
      row.Input += m.inTok
      row.Output += m.outTok
      row.CacheRead += Math.round(m.inTok * 0.86)
      row.CostUsd += m.cost
      byDayModel.set(k, row)
    }
  }
  const modelsTotal = MOCK_STATS_MODELS.map((m) => ({
    key: m.key,
    Input: days.reduce((a, d) => a + (d.models.find((x) => x.model === m.key)?.inTok ?? 0), 0),
    Output: days.reduce((a, d) => a + (d.models.find((x) => x.model === m.key)?.outTok ?? 0), 0),
    CostUsd: days.reduce((a, d) => a + (d.models.find((x) => x.model === m.key)?.cost ?? 0), 0),
  }))
  const peak = days.reduce((a, d) => (d.total > a.total ? d : a), days[0] ?? { key: '', total: 0 })
  const activeDays = new Set(days.filter((d) => d.total > 0).map((d) => d.key)) // 0 值日 = 未使用
  let currentStreak = 0
  for (let i = 0; ; i++) {
    const d = new Date(today)
    d.setDate(d.getDate() - i)
    if (!activeDays.has(day(d))) break
    currentStreak++
  }
  let longestStreak = 0
  let run = 0
  for (let i = 364; i >= 0; i--) {
    const d = new Date(today)
    d.setDate(d.getDate() - i)
    run = activeDays.has(day(d)) ? run + 1 : 0
    if (run > longestStreak) longestStreak = run
  }
  const totalIn = days.reduce((a, d) => a + d.models.reduce((x, m) => x + m.inTok, 0), 0)
  const totalOut = days.reduce((a, d) => a + d.models.reduce((x, m) => x + m.outTok, 0), 0)
  const totalCost = days.reduce((a, d) => a + d.cost, 0)
  const winIn = inWindow.reduce((a, d) => a + d.models.reduce((x, m) => x + m.inTok, 0), 0)
  const winOut = inWindow.reduce((a, d) => a + d.models.reduce((x, m) => x + m.outTok, 0), 0)
  const winCost = inWindow.reduce((a, d) => a + d.cost, 0)
  // 有价性三分桶（2026-09-23）：mock 也按真桥口径给 —— 大部分有价，最后一个模型当作
  // 未收录价（无价），另留少量旧行（unknown）。前端「无价调用占比」卡片因此能在浏览器里渲染。
  const mockCalls = Math.max(1, Math.round(days.reduce((a, d) => a + 1 + d.models.length, 0) * 1.6))
  const unpricedCalls = Math.max(1, Math.round(mockCalls * 0.12))
  const unknownCalls = Math.max(1, Math.round(mockCalls * 0.05))
  const pricedCalls = Math.max(0, mockCalls - unpricedCalls - unknownCalls)
  const unpricedKey = modelsTotal.length > 0 ? modelsTotal[modelsTotal.length - 1].key : ''
  return {
    Stats: {
      TotalTokens: totalIn + totalOut,
      TotalCostUsd: totalCost,
      PeakDayTokens: peak.total,
      PeakDay: peak.key,
      LongestChatMs: 4 * 3600e3 + 12 * 60e3, // 4h12m（示例会话跨度）
      LongestChatAt: `${days[days.length - 2]?.key ?? ''}T18:20:00+08:00`,
      CurrentStreak: currentStreak,
      LongestStreak: longestStreak,
      ActiveDays: activeDays.size,
      Sessions: Math.round(days.reduce((a, d) => a + 1 + d.models.length, 0) * 1.6),
      FirstDay: days[0]?.key ?? '',
      LastDay: days[days.length - 1]?.key ?? '',
      PricedCalls: pricedCalls,
      UnpricedCalls: unpricedCalls,
      UnknownPricingCalls: unknownCalls,
    },
    // Reasoning ⊆ Output、CacheWrite1h ⊆ CacheWrite（2026-09-23 补齐维度）：mock 按固定
    // 比例派生（真桥是上游回报的真实值），让统计页的「思考占比」在浏览器里可渲染
    Total: {
      Input: winIn, Output: winOut, Reasoning: Math.round(winOut * 0.35),
      CacheRead: Math.round(winIn * 0.86), CacheWrite: Math.round(winIn * 0.05),
      CacheWrite1h: Math.round(winIn * 0.05), CostUsd: winCost,
    },
    ByDay: inWindow.map((d) => {
      const out = d.models.reduce((a, m) => a + m.outTok, 0)
      const inTok = d.models.reduce((a, m) => a + m.inTok, 0)
      return {
        Key: d.key, Input: inTok, Output: out, Reasoning: Math.round(out * 0.35),
        CacheRead: Math.round(inTok * 0.86), CacheWrite: Math.round(inTok * 0.05),
        CacheWrite1h: Math.round(inTok * 0.05), CostUsd: d.cost,
        CumTokens: cumByDay.get(d.key) ?? 0,
      }
    }),
    ByDayModel: [...byDayModel.values()],
    ByModel: modelsTotal
      .map((m) => ({
        Key: m.key,
        Input: m.Input,
        Output: m.Output,
        Reasoning: Math.round(m.Output * 0.35),
        CacheRead: Math.round(m.Input * 0.86),
        CacheWrite: Math.round(m.Input * 0.05),
        CacheWrite1h: Math.round(m.Input * 0.05),
        CostUsd: m.CostUsd,
        // 最后一个模型当作「价表未收录」：无价调用计数（卡片 hint 的主要来源）
        UnpricedCalls: m.key === unpricedKey ? unpricedCalls : 0,
      }))
      .sort((a, b) => a.Key.localeCompare(b.Key)),
    ByWorkspace: [
      { Key: '/mock/proj-auth', Input: Math.round(totalIn * 0.46), Output: Math.round(totalOut * 0.46), CacheRead: 0, CostUsd: totalCost * 0.42 },
      { Key: '/mock/proj-billing', Input: Math.round(totalIn * 0.31), Output: Math.round(totalOut * 0.31), CacheRead: 0, CostUsd: totalCost * 0.33 },
      { Key: '/mock/proj-search', Input: Math.round(totalIn * 0.23), Output: Math.round(totalOut * 0.23), CacheRead: 0, CostUsd: totalCost * 0.25 },
    ],
  }
}

export function mockTransport(): Transport {
  return new MockTransport()
}
