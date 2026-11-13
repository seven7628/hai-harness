# P4 最终复盘与已知限制

> 状态：P4-05 收口中。P4-01 回滚开关已补齐（`2b44d4e`），待终审 delta。
> 基线：`agent-hub` 分支（本地 main 已合并：`e38ba34` + 语义修复 `3e050ff`）。
> 终审：独立 Final Review `task-43keebnw`（CHANGES_REQUESTED，无 P4 功能回归；两个收口缺口见 §4/§5）。

## 1. P4 交付矩阵

| 任务 | 交付 | 实现 commit | 独立 Review |
|---|---|---|---|
| P4-01 | 长会话 memo、流式/typing 对账、弱网不变量、性能基准、**运行时回滚开关** | `8051305` + `2b44d4e` | `task-czrhx4tx` passed；终审 B1 回滚开关已补，delta 复核中 |
| P4-02 | Context 面板（触发上下文/提示词层/跳转）、`hub://` 深链、群/任务 Markdown 导出 | `f72e8ca` + `54dd883` | `task-uydfsps6` PASS（delta） |
| P4-03a | 手机 Hub 21 命令白名单、`scope=hub` WS、全事件单点扇出、delta 可丢/控制优先 | `195667f` | `task-upzkha5h` passed |
| P4-03b | 手机 Hub 列表/时间线/全屏 @/任务卡/任务板/Needs/已读/停止 | `46de6ae` | `task-eq9fgqy7` passed |
| P4-03c | system/ink/paper 主题、44px、reduced-motion、390px、IME @、失败保草稿、mdLite | `b8858f6` | `task-bb34tpm4` passed |
| P4-03d | 真实配对/HTTP/WS/FileStore E2E、任务/停止/重试幂等、390 Chromium | `9436882` | `task-v2rzhzy8` passed |
| P4-04a | Hub 用户级外部 Stop/AgentEnd hook、默认关、fail-open、仅用户级来源、热替换 | `fbbf251` + `40f5527` + `4f96a3a` + `df1b1cd` | `task-k7hqg4dz` PASS（4 轮 delta） |
| P4-04b | 消息搜索 / 归档 | — | 显式 defer（待产品拍板） |
| P4-05 | 全期终审、全量回归、性能/成本、已知限制、复盘、看板收尾 | 本文件 + 看板 | `task-43keebnw` 终审 → 修复 B1/B2 后 delta |

P4 的代码/测试没有“只有 review note、没有实现”的任务；P4-04b 是唯一未实现项，且已在拆分时明确 deferred。

## 2. 回归证据

### 2.1 通过的 gates

| Gate | 结果 | 时长/证据 |
|---|---|---|
| `go build ./...` | PASS | 14.4s（终审） |
| `go vet ./...` | PASS | 9.6s |
| `go test ./... -run '^$'` | PASS | 全仓编译门 21.1s |
| `go test ./agenthub/... ./session/... -count=1` | PASS | agenthub 17.9s / session 11.2s（终审） |
| `go test ./desktop/bridge -run 'TestMobileP4E2E' -count=3 -race` | PASS | 21.1s |
| `go test ./desktop/bridge -run 'TestHubHooks\|TestHubSessionStopHook' -count=2 -race` | PASS | 19.3s |
| `test:hub` | PASS | 14 项 |
| `test:hub-p3b` | PASS | 239 项 |
| `test:hub-stream` | PASS | 22 项 |
| `test:hub-context` | PASS | 191 项 |
| `test:scroll-follow` | PASS | 32 项 |
| `test:mobile-hub` | PASS | 238 项 |
| `test:agent-session` | PASS | 11 项 |
| `npx tsc --noEmit` + `tsc -p tsconfig.electron.json` | PASS | 双配置 |
| `npm run build` | PASS | Vite + electron + bridge；本机 16.7s，终审 30.9s |
| `npm run test:mobile-390 -- --require-browser` | PASS | Chromium 151，390×844，4 屏，`scrollWidth=390`，按钮 ≥44px，主题/动效/@/草稿/已读 |
| `npm run bench:hub-long` | PASS | production Vite + Chromium 151，见 §3 |
| `npm run lint` | 仅 2 条既有 lab 解析错误 + 58 warnings | 无 P4 Hub 文件新增错误 |

### 2.2 全量 `go test ./...` 的已知红项（waiver）

`go test ./... -count=1` 在当前机器/干净 worktree 上不是全绿，原因均已定位到 P4 之前的基线或环境，不是 P4 diff：

1. **`TestP0GuidanceCausesModelToWriteMemory`**（`desktop/bridge/memory_p0_test.go`）
   - 真实 provider `opencode2` / `space-bunny-free` 探活 400 `Model is unavailable`。
   - 这是“凭证在但网关模型不可用”时要求响亮失败的测试；`HAI_EVAL_ALLOW_PING_SKIP=1` 显式降级后 PASS。
   - 属于环境/供应商可用性，不是代码回归。
2. **`TestReducerLLMEndDedupAndUsage`**（`desktop/bridge/view_reducer_test.go`）
   - 已知全局 price-table 污染；单跑 `-count=2` PASS。
   - 属于测试间全局状态，不是 P4 代码。
3. **本机全量并行/沙箱噪声**（终审干净串行运行未复现）
   - `TestAssignTaskPumpCanceledNote` 单跑 PASS；
   - `execproc` 的 2 项单跑 PASS，`TestExecuteScriptUnsatisfiableAwaitIsBoundedByHostClock` 在主分支同样失败（预存在）；
   - `codemode` 的 `TestWallClockTimeoutKillsLoopAndLeavesNoResidue` 单跑 PASS；`TestApprovalWaitDoesNotConsumeWallClock/self-declared` 在主分支同样失败（预存在沙箱握手噪声）。

因此 P4 收口的可复现命令是：

```bash
# 全仓编译门
go test ./... -run '^$'

# 领域 + P4 关键行为（P4 不碰 provider 计费/价格表）
go test ./agenthub/... ./session/... -count=1
go test ./desktop/bridge -run 'TestMobileP4|TestHubHooks|TestHubSessionStopHook|TestMobileHub|TestMobileOutLine|TestMobileCmd|TestMobilePair' -count=3

# 前端 P4 纯逻辑 + 真实 390 浏览器
cd desktop/app
npm run test:hub && npm run test:hub-p3b && npm run test:hub-stream && \
npm run test:hub-context && npm run test:scroll-follow && \
npm run test:mobile-hub && npm run test:agent-session
npm run test:mobile-390 -- --require-browser
```

**`npm test` 环境限制**：`desktop/app/e2e/` 被 `.gitignore` 忽略，干净 worktree 没有 `e2e/safe_cleanup.test.mjs`，所以 `npm test` 在第一个 e2e 脚本就失败（0.66s）。主工作树里存在该目录时才能跑全量。P4 验收使用上面的 P4 子集 + `test:mobile-390`；发布前若要求全量 `npm test`，需先把稳定 e2e fixture 入库或提供 CI-safe 子集脚本。

## 3. 性能与成本

### 3.1 P4-01 长会话基准（production build，Chromium 151，1280×800）

Fixture：220 条消息 / 21 轮 / 3 条并行流式草稿 / 22,946 字符。

| 指标 | 本机 rerun（`BENCH_RUNS=3`） | 终审独立 rerun |
|---|---|---|
| mount → first frame | 166.9ms | 92.5ms |
| delta commit p50 / p95 / max | 0.3 / 0.6 / 0.9ms | 0.3 / 0.4 / 0.8ms |
| delta forced layout p50 / p95 / max | 0.6 / 0.8 / 1.8ms | 0.5 / 0.8 / 1.4ms |
| keystroke handler p50 / p95 | 0.5 / 1.1ms | 0.5 / 0.6ms |
| keystroke → next frame p50 / p95 | 5.2 / 8.5ms | 5.8 / 8.9ms |
| DOM thread / total | 3081 / 3136 | 3081 / 3136 |
| thread HTML hash | `37edea6` | `37edea6` |
| scroll rAF p50 / p95 | 8.3 / 9.2ms | 8.3 / 9.2ms |
| scroll frames >50ms / long tasks | 1 / 1（max 124ms） | 0 / 0 |
| JS heap used / total | 11 / 27MB | 11 / 27MB |
| reduced-motion / weak-network 不变量 | PASS | PASS |

A/B 历史证据（P4-01 reviewNote，非本次 rerun）：delta p50 **0.9 → 0.3ms**，键入 **1.2 → 0.6ms**。本次只把“after”数字作为当前基线；不回填未复测的 before 数字。

**回滚开关**：`localStorage.setItem('hub.perfMemo', '0')` 后刷新，GroupView 不再复用已结算时间线的 memo，回退到每次渲染重算（语义不变，仅性能回退）；删除该键或设为 `1` 恢复。实现 commit `2b44d4e`；`bench:hub-long` 的 rollback 页单次实测 memo-on delta p50 0.3ms / memo-off 0.9ms（另一次 0.2/0.8，run variance），两次 thread HTML hash 均为 `37edea6`（证明开关绕过 memo 且不改变输出）。

### 3.2 手机端

- 390×844 真实 Chromium 四屏（列表/Hub/群/DM）：`scrollWidth=390`，按钮全部 ≥44px，主题/动效/@/草稿/重试/已读 PASS（5.42s）。
- P4-03d E2E ×3 race：21.1s，真配对 + `/api/cmd` + `scope=hub` WS + FileStore；A/B provider 调用 1/1；stop 后不启动 B、不产生 post-stop final。
- 静态手机审计：44px / motion / XSS / @ / draft / read / theme / 390 全 PASS（0.08s）。

### 3.3 成本

- **默认配置 0 增量 LLM 调用**：P4 没有修改 provider/metrics/cost/billing 路径；E2E 断言 A/B provider 调用各 1 次；mobile server 不调用模型。
- 唯一条件成本：用户配置的 Stop hook 返回 `decision=block` 时，内核最多续跑 8 轮（既有 `maxStopHookBlocks=8`）。默认关闭；这是用户显式策略，不是 Hub 核心回推依赖。
- P4 没有产出美元成本数据；本复盘只声明“默认 0 增量调用”并标注 hook 的条件成本，不伪造金额。

## 4. 已知限制与 defer

| 项 | 来源 | 严重度 | 发布阻塞？ |
|---|---|---|---|
| OS `hub://` 打包/签名交付未真机验证（macOS LaunchServices、Windows 注册表、Linux handler） | P4-02 defer | 高（若发布宣传深链） | **是，发布前必须验证** |
| iOS Safari 未测（只有 Chromium/桌面） | P4-03d | 高（若手机 Web 面向 iPhone） | **是，公开移动发布前** |
| P4-04b 搜索/归档未实现 | `cb3ecd7` | 中（功能缺口） | 否，显式 defer |
| P4-02 Run 视图 jump 无 `data-seq`；DM 深链不水合 groups rail；群导出无“部分时间线”提示 | P4-02 defers | 低–中 UX | 否 |
| P4-02 `rendererReady` 刷新复位无自动化覆盖 | P4-02 defers | 低测试缺口 | 否 |
| P4-04a C1 热替换窄窗口（创建中会话可能漏一次 reload） | P4-04a | 低 | 否；下次创建/重开读新配置 |
| P4-04a C2 hook 测试 POSIX-only；Windows 执行路径未测 | P4-04a | 中（Windows 目标） | 否（macOS）；Windows 发布前补 |
| P4-04a C3 `isHubHomePath` 未做 symlink/case 规范化 | P4-04a | 低（纵深防御） | 否 |
| P4-01 `finalizedMessageIds` 无界；DM 无等价 reconcile；bench 未设 CI 阈值 | P4-01 inspection | 低 | 否 |
| P4-03a close 不 detach tap；`isChunkEvent` 仅测试用；`dispatchMu` 注释过期 | P4-03a review | 低 hygiene | 否 |
| P4-03d `/api/events` 只有无 token 负测，无 invalid/revoked token 负测 | P4-03d review | 低测试缺口 | 否 |
| 桌面 Hub 390px 宽度下左栏抽屉无法展开（只能看到 `hub-side` 之外的空态；手机端走 `mobile_web`） | P4-03c note / P4-UI-11 复核 | 低（桌面窄窗） | 否；桌面最小窗口按 980×620 支持，390 档只保证不横向溢出 |
| `npm run lint` 2 条既有 lab 解析错误、58 warnings | 终审 N14 | 低 repo hygiene | 否 |
| CI 不跑 `desktop/bridge` 与 P4 mobile/bench 脚本 | 终审 N16 | 中置信度缺口 | 否，但发布前应补 |

### 4.1 P4-UI-11 已收口的 P2 细节（原 defer 行已移除）

P2 浏览器审计的六项细节 + 两条 P1 复审非阻塞项在本轮修完，**不再是 defer**；
证据全部是可重跑的脚本断言（`npm run check:p4-p2` 需先起 dev server）：

| 审计项 | 修法 | 证据（实测） |
|---|---|---|
| HUB-CTX-01 Context「消息」label 竖排 14×36 | `.hub-context-picker label { flex:0 0 auto; white-space:nowrap }` + select 吃剩余宽度 | 360px inspector：label `23×18`（宽>高、nowrap）、select `304px`；`check:p4-p2 [1]` |
| HUB-NEED-01 Needs 提问卡先铺原始 JSON | 提问类解析出输入框后不再渲染 `HubNeedBody`；解析失败才退回原文 | 提问卡 `0` 个 `.hub-need-body` + 2 个输入框；审批/失败/交接仍各 1 个 body；`check:p4-p2 [5]` + `shots:hub-failures [4]` |
| HUB-A11Y-01 侧栏小节标题对比度 4.30/3.28 | 智能体 / 工作区标题 `--text-3` → `--text-2`（字号/字重/字距不动） | ink `7.14:1`、paper `6.75:1`（均 ≥4.5，AA 正文口径）；`check:p4-p2 [2]` |
| HUB-TARGET-01 640 点击目标 <32px | `@media (max-width:900px)`：@/发送/上下文/更多 32×32、事件触发入口命中高 32（负 margin 不撑行高）、侧栏筛选 chip 与任务状态筛选 ≥32 | 640/760/900 全部 `32–36`；`check:p4-p2 [3]`；规范注释写在 `hub.css` 尾部（桌面 32–36 / 移动 44） |
| HUB-EDITOR-01 资料页 1280 撑满、删除易误触 | `.hub-modal` `width:min(720px,92vw)`；删除从预览卡挪到 footer 左侧次要位 | 1280：`720px` @ `x=280`（居中）；640：`589px`（92vw）；footer 顺序 `删除智能体 → 取消 → 保存`；`check:p4-p2 [4]` |
| HUB-DL-01 深链「复制链接」浮右上 | URL 与按钮包进 `.hub-deeplink-row`，按钮贴 URL 行尾（间距 8px） | url 右缘 1354 / 按钮左缘 1362（同一行）；`check:p4-p2 [1]` |
| P1 复审：行内「重试」sending 时未禁用 | `UserMessage` 增 `retryDisabled`，DM 用 `sending`、群用 `groupSending`（store 本身已拒绝并发） | `test:hub-conv` / `test:hub-idem` 回归通过；气泡按钮 disabled 与横幅 busy 同态 |
| P1 复审：mock `hub_need_resolve` 与真实不同 | 未知交接 resolution fail-closed、重复 resolve 回 `already:true`、提问缺/空 answers 报错（错误文案与 `hub_taskboard.go` 同口径） | `test:hub-idem` 新增 15 项（103 项全通过）；正常 payload（交接 accept / 提问 answers map）浏览器里仍能 resolve |

顺带修掉的既有问题：`check-mobile-390.mjs` 的头像断言停留在 sigil v1（`.agent-stack` / `.sigil` / 3++3），
在 Luna 迁移（`424d6e7`/`eb7ce39`）后一直红。已按当前引擎改成 demo 口径（群行前 4 + `+2`、DM 32px Luna、
`--eye` 令牌随 ink/paper 切换），`npm run test:mobile-390 -- --require-browser` 重新变绿。

390 × 844 真机口径复核（Chromium 151，`--require-browser`）：`scrollWidth=390`、按钮 ≥44px、
reduced-motion 关动画、@ 提及、失败保草稿、重试幂等、已读游标全部通过；system 主题
`setTheme('system')` + `emulateMedia(colorScheme)` 双向切 ink/paper（手机页与桌面 Hub 各测一次）。

## 5. 收口豁免（B2）

P4-05 的验收命令 `npm test && go test ./...` 在**干净 worktree** 不能直接全绿，因此本文件明确豁免而不是静默跳过：

1. `npm test` 因 `desktop/app/e2e/` 被 gitignore 且干净 worktree 不存在该目录而无法启动；P4 使用新增的 `npm run test:p4`（纯逻辑 7 套件）+ `npm run test:p4:browser`（真实 390 浏览器）+ `npm run test:p4:go`（领域与 P4 关键 Go 测试）。发布前若要全量 npm test，需把稳定 e2e fixture 入库或改造 CI-safe 子集。
2. `go test ./...` 的基线红项见 §2.2：真实 provider 不可用、price-table 全局污染、execproc/codemode 预存在沙箱/文案测试；P4 不碰这些包。CI 等效包与 P4 关键行为已单独跑绿。
3. OS 深链与 iOS Safari 是**发布 gate**，不是 P4 代码失败；本文件不把二者标记为已验证。
4. **P4-UI-11 的桌面细节回归需要 dev server**（脚本自带 mock，但要真实 Chromium 渲染）：
   `npx vite --port 5180` + `node scripts/check-p4-p2-details.mjs --url http://localhost:5180/`。
   本轮跑过的组合：`shoot-hub-narrow`（640/760/900 × ink/paper）、`shoot-hub-failures`
   （失败重试/状态栏/fixture/退出恢复/阈值重算）、`check-p4-p2-details`（六项细节 + system 主题）、
   `test:mobile-390 --require-browser`（390×844）。`scripts/shoot-hub-conv.mjs` 的 390 档在
   P4-UI-9 布局下取不到左栏 Hub 列表（桌面 390 无抽屉），是既有脚本/布局限制，本轮未纳入 gate。
5. 目标尺寸口径统一为 **桌面 32–36 / 移动 44**：桌面 Hub 的 `@`/发送/上下文/更多/事件触发/筛选
   在 ≤900 提到 32；390 手机页继续走 `mobile_web/style.css` 的 44px 触控下限，两边不共用一套数字。

## 6. 复盘

### 做得好的

- P4-03d 用真实配对/HTTP/WS/FileStore 做 E2E，抓到了纯单测抓不到的集成问题。
- 独立 delta review 真的拦下了问题：深链所有权/StrictMode、导出转义、缺失 seq、Hub hook 热替换、Stop 与截断时序/边界测试。
- 安全边界有行为级测试：21 命令白名单、Hub/chat WS 隔离、token gate、仅用户级 hook loader、预信任项目命令仍不执行。
- 390×844 真实 Chromium 测量 + production build benchmark 比静态 CSS/DOM 断言可靠。
- P4-01 回滚开关补上后，性能优化不再是“只能回滚 commit”。

### 要改的

- 验收条款必须可执行：“回滚开关”最初只写了性能数据；“全量回归”在干净 worktree 本来跑不起来。以后 done 前逐条核对，缺证据就写 waiver 或删条款。
- 看板 schema 要校验：`review` 必须是 `passed`，`PASS task-…` 会被 UI 当成未 Review（本轮已修）。
- 干净的 repo 不能依赖 gitignored e2e fixture 才跑 `npm test`；要么入库，要么明确 CI-safe 子集。
- 把两个基线红项（provider 探活、price-table 污染）做成显式 waiver/隔离测试，不要靠“全量看起来绿”。
- 性能 before/after 必须统一 provenance；reviewNote/commit/代码注释不能各写一套数字。

### 后续

- P4-04b：搜索范围/保留期产品拍板后实现；`Store.SearchMessages` 已有读路径。
- OS 深链打包真机（macOS/Windows/Linux）与 iOS Safari 真机：发布 gate。
- P4-04a C1/C2/C3：热替换窄窗口、Windows hook 测试、symlink 路径规范化。
- P4-02：Run 视图 jump、DM groups rail 水合、部分导出提示、`rendererReady` 覆盖。
- P4-01：`finalizedMessageIds` 上限、DM reconcile、bench CI 阈值；回滚开关文档化。
- CI 加入 `test:mobile-390` 与 P4 Hub 脚本；补 `/api/events` invalid/revoked token 负测；补任务板/Needs 浏览器 UX 覆盖。

## 7. 看板收尾

- P4-05 完成后：`tasks.json` 中 P4-05 `status=done`、`review=passed`、`reviewNote` 指向本文件与终审任务。
- `python3 docs/agent-hub/sync_progress.py` 重新生成 `PROGRESS.html`。
- P4-04b 保持 `deferred`，不因 P4-05 收口而改状态。
