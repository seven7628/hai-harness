# 测试有效性 Review（变异验证维度）

> 方法：对生产代码逐处注入语义破坏，观察对应测试是否变红。所有变异均已 `byte-exact` 还原
> （`cmp` 校验 7 个被改文件全部一致，无 `.bakmut` 残留），最终 `git diff` 与开工前存档一致。
> 基线：24 条相关测试**全 PASS、零 SKIP**（`go test ./agents/ ./memory/ ./desktop/bridge/ -short` 全绿）。

---

## 1. 变异验证结果表

### A.1 `desktop/bridge/memory_assembly_test.go`

| # | 测试 | 变异内容 | 结果 | 结论 |
|---|---|---|---|---|
| M1 | `TestProductionEngineStampsMemoryOnWrite` | 删掉 `main.go:3842-3845` 的 `ft.WithPostWrite(...)` 整段 | **红** | ✅ **有效** |
| M1 | `TestProductionEngineRespectsSwitchOff` | 同上 | 绿 | ⚠️ **结构性无效**（见下） |
| M2 | `TestMemoryWiredIntoRealSessionSystemPrompt` | 删掉 `main.go:4114-4118` 的 `WithMemoryInjection` 注入 | **红** | ✅ 有效 |
| M2 | `TestMemoryOffMeansNoInjectionInNewSession` | 同上 | **红** | ✅ 有效（意外的额外收益） |

**M1 独立复核结论：属实。** 该测试走 `newSessionEngine`（`memory_assembly_test.go:37`）真实注册 `write_file`，删钩子后立即 `生产装配未补 modified`。这是本仓库唯一能捕获「生产从未接线」的测试。

**但配对的那条是假通过。** `TestProductionEngineRespectsSwitchOff`（`memory_assembly_test.go:104-150`）的全部断言都是**否定式**（"不该出现 `modified:`"、"不该建 `.lock`"）。钩子被整个删除时这些断言**恒真**——M1 实测它 PASS。判据：它的语义等价于「什么都没发生」，因此对「什么都没接线」完全免疫。**配对关系是假的**：注释（`:102-103`）声称"配对上一条：钩子装上了不等于闸门正确"，但它只钉住了"闸门正确"，没钉住"钩子装上了"。

**A.1 第二问（拆 `composeSystemPrompt` 注入分支）** = M2，答：应红的是 `TestMemoryWiredIntoRealSessionSystemPrompt` 与 `TestMemoryOffMeansNoInjectionInNewSession`，实测两条都红。注意**`agents` 包的 4 条测试全部不受影响**（`WithMemoryInjection` 未调用时它们的断言退化为基线自比较，见 §3.1）。

### A.2 `desktop/bridge/memory_wiring_test.go` —— 三道闸

| # | 测试 | 变异内容 | 结果 | 结论 |
|---|---|---|---|---|
| M5 | 全部 8 条 | 闸①`Disabled()` + 闸②`IsMemoryPath()`/`IsAbs` 改成永远通过 | **全绿** | ❌ **无覆盖** |
| M6 | 全部 8 条 | 闸④`dir.IsTopic()` 改成永远通过 | **全绿** | ❌ **无覆盖** |
| M7 | 全部 8 条 | **三道闸全部删除**（`memory_wiring.go:120-136` 只剩 `resolve` + `StampModified`） | **全绿** | ❌ **无覆盖** |

**这是本次 Review 最严重的发现。** `desktop/bridge/memory_wiring.go:120-136` 的 `postWrite` **整个函数体可以删掉 60%，8 条测试无一变红。**

判据（为什么）：三道闸在 `memory` 包内**都有第二道同样的检查**，`desktop/bridge` 侧的闸是纯冗余的：

- 闸①（开关）→ `memory/stamp.go:282` `if !c.enabled() { return StampedSkipped }`（`StampModified` 内部首行）
- 闸②（`IsMemoryPath`）→ `memory/path.go:48` `IsTopic` 调 `d.Owns(path)` 走 `IsInMemoryDir`（`memory/path.go:23-32`）
- 闸④（`IsTopic`）→ `memory/stamp.go:287` `if !d.IsTopic(path) { return StampedSkipped }`

**行为后果**（这些测试的**真正**价值在别处，但断言没写在那里）：闸①②是**性能护栏**——注释（`memory_wiring.go:113-117`）说"工作区内的绝大多数写入在这里就被排除，**零 git 开销**"。拆掉后，每次 `write_file` 都会触发 `h.resolve()` 跑 git 子进程（`memory_wiring.go:70` 的重试路径），即**模型每写一个文件 fork 一次 git**。`TestPostWriteIgnoresNonMemoryPaths`（`:154-174`）本该抓住这个（它断言工作区内 `.md` 写入后目录仍为空），但因为 `resolve` 在 `memTopicDir` 构造的 fixture 里**已缓存**（`memory_wiring_test.go:43` `h.dir = &dir`），git 路径压根没被走到。**假通过。**

**「关闭时不建 .lock」是否有别的测试兜住？** 有，3 条：`TestPostWriteDisabledIsInert:145-150`、`TestPrefOffDisablesNewSessionAssembly:126-131`、`TestProductionEngineRespectsSwitchOff:144-149`。但如上所述这 3 条**全部是否定式断言**，且 `.lock` 由 `memory/stamp.go:298` 在 `StampModified` **内部**创建（宿主侧根本没建）。所以"关闭时不建 .lock"实际由 `memory/stamp.go:282` 保护，**宿主侧的三处断言全都在测一个自己控制不了的东西**。

### A.3 `desktop/bridge/memory_e2e_test.go`

| # | 测试 | 变异内容 | 结果 | 结论 |
|---|---|---|---|---|
| M8 | `TestMemoryWiredIntoRealSessionSystemPrompt` | `promptFootprint` 求和去掉 `ContextPartMemory`（`:84`） | **红** | ✅ 有效 |
| M8 | `TestMemoryOffMeansNoInjectionInNewSession` | 同上 | 绿 | ⚠️ 部分 |

**回答提问：不能通过，会红。** `memory_e2e_test.go:129` 的 `onChars-baseChars < int64(len(idx))` 断言确实咬住了——`ContextPartMemory` 是 `ContextBreakdown` 里的**独立分区**（`agents/context_breakdown.go:107,137`），把它从求和里去掉，`onChars` 就完全不增长，`:125` 的 `onChars <= baseChars` 先炸。**这条断言有效**，其"同时求和 system+memory"的注释（`:68-71`）是准确的。

### A.4 `agents/memory_injection_test.go`

| # | 测试 | 变异内容 | 结果 | 结论 |
|---|---|---|---|---|
| M3 | `TestMemoryInjectionAppearsAfterWorkingMemory` | 注入位置从「工作记忆之后」挪到「之前」（`agent_loop.go:1517-1533` 调换） | **红** | ✅ 有效 |
| M3 | `TestMemoryInjectionAccountedAsOwnPart` | 同上 | 绿 | ✅ 合理（该测试不测位置） |
| M4 | `TestMemoryInjectionAccountedAsOwnPart` | `memoryChars := int64(0)`（`context_breakdown.go:107`） | **红** | ✅ 有效 |
| M4b | `TestMemoryInjectionAccountedAsOwnPart` | 重复计量（memory 计 0 **且** system 不再减 `memoryChars`，`:107-108`） | **红** | ✅ 有效 |

四条断言全部有效，且 `:121` 的总量检查 `diff < want-1 || diff > want+1` 成功抓住了 M4b 的"2× 重复计量"——这是本组测试写得最好的一处。

### A.5 `desktop/bridge/memory_prefs_test.go`

| # | 测试 | 变异内容 | 结果 | 结论 |
|---|---|---|---|---|
| M9 | `TestMemoryPrefsRoundTrip` | `load()` 永远返回默认（忽略文件） | **红** | ✅ 有效 |
| M9 | `TestMemoryCommandsRoundTrip` | 同上 | **红** | ✅ 有效 |
| M9 | `TestMemoryOffMeansNoInjectionInNewSession` | 同上 | **红** | ✅ 有效 |
| M10 | `TestMemoryPrefsRoundTrip` | `set()` 静默失败（不 rename，`:90-93` 改成 `return nil`） | **红** | ✅ 有效 |
| M10 | `TestMemoryCommandsRoundTrip` | 同上 | **红** | ✅ 有效（`memory_prefs_test.go:200-206` 真的去读盘了） |
| M10 | `TestMemoryOffMeansNoInjectionInNewSession` | 同上 | **红** | ✅ 有效 |

**两个问题的答案相同：各 3 条变红。** 偏好层是本组最扎实的部分——`TestMemoryCommandsRoundTrip` 覆盖了「命令注册名/payload/响应字段 + 真读盘」三层，符合它自己注释（`:162-166`）的声明。

**注意 `TestMemoryPrefsCorruptFallsBackToDefault` 在 M9/M10 下都绿**——这是**设计正确**的：它测的是"文件坏 → 默认开"，而 M9 恰好也返回默认开。变异测试只能证明该测试没测错东西，不能证明它没覆盖；此处归为"语义重合，非缺口"。

---

## 2. 覆盖缺口清单

### 2.1 ⚠️ 最高危：`postWrite` 三道闸 —— 已有测试但**假通过**（见 §1 A.2）

无覆盖的生产路径：`memory_wiring.go:121`（闸①开关）、`:124`（闸②`IsMemoryPath`/`IsAbs`）、`:131`（闸④`IsTopic`）。**唯一真实保护是 `memory` 包的冗余检查**，宿主侧这 3 行是未测代码。

### 2.2 ⚠️ 高危：`baselineMemoryImporters` 扫描范围被 cwd 限制 —— 护栏实际失效

**这是本次 Review 发现的第二个严重问题，且直接命中用户背景里「意外接线被静默吞掉」的担忧。**

| # | 变异 | 结果 | 结论 |
|---|---|---|---|
| M11 | 在 `subagent/registry.go` 加 `_ "…/memory"` import | `TestEmptyStateAssertionSkipsOnlyForWiredPackage` **PASS** | ❌ 假通过 |
| M12 | 在 `agents/agent_loop.go` 加 `_ "…/memory"` import（build 能过） | 同上 **PASS** | ❌ 假通过 |

**根因（`memory_baseline_test.go:148`）**：
```go
cmd := exec.Command("go", "list", "-deps=false", "-f", "...", "./...")
```
`exec.Command` **不设 `Dir`**，继承 `go test` 的工作目录 = **被测包目录 `desktop/bridge/`**。实测：

```
$ cd desktop/bridge && go list -deps=false ... ./...   →  只有 2 个包
  github.com/seven7628/hai-harness/desktop/bridge
  github.com/seven7628/hai-harness/desktop/bridge/cmd/im_send_check

$ go list ... ./...   （从仓库根）  →  3 个 importer
```

即：**这个"防意外接线"的护栏只能看到 `desktop/bridge` 自己和它的一个 cmd 子目录。** 往 `agents/`、`subagent/`、`tools/builtin/` 等任何生产包加 memory 引用，它都看不见。注释（`:102-103`）声称"出现**其他** importer 仍按下文硬失败"——**这个能力在当前实现下不存在**。

注释（`:146-147`）写"go list 比手写目录遍历可靠"是对的，但漏了 cwd 这个前提。**修复**：`cmd.Dir = repoRoot()`（或用 `runtime.Caller` 定位 `../..`）。

### 2.3 `rebuildLoop`（switch_model/persona/provider）下的记忆重注入 —— **零覆盖**

```
$ grep -rn "rebuildLoop" --include=*_test.go .
（无输出）
```

`main.go:4131-4138` `rebuildLoop` → `buildLoop` → `main.go:4114-4118` 重跑注入。`main_test.go:1122/1149/1912` 只断言 `switch_*` 返回 ok 和 persona/model 字段，**没有一条断言重建后的 loop 里记忆还在**。

**风险场景**：若将来有人为省一次 git 调用而把注入结果缓存在 `manager` 上（跨 `rebuildLoop` 复用），现有测试**一条都不会红**，而用户在 `switch_model` 后记忆就静默消失。这与背景里 `WithPostToolNudgeEnabled` 的失效形态**完全同构**（都是"装配期状态被缓存/跳过"）。已有的 `TestMemoryRunningSessionUnaffectedByToggle`（`memory_e2e_test.go:182`）只覆盖 `set` 偏好，**不覆盖 `rebuildLoop`**。

### 2.4 子 agent loop 是否有记忆 —— **零覆盖**（无断言，也无反向钉死）

`buildExploreLoop`（`main.go:3715-3757`）与子 agent loop（`main.go:3685`）都不传 `WithMemoryInjection` —— 与 `memory_wiring.go:19-20` 的声明"只接主会话，子 agent 不注入"一致。**但没有任何测试断言这一点。**

`memory_wiring.go:18-20` 把"本轮明确不实现"写成了注释。注释不是护栏：将来有人给子 agent 接上（或漏接）记忆，**没有任何测试会红**。这是**决策与实现之间缺一条钉死测试**，不是当前的 bug。

### 2.5 `withMemoryInjection` 为空 vs 不传 —— 已覆盖 ✅

`agents/memory_injection_test.go:45-55` `TestNoMemoryLeavesPromptByteIdentical` 同时钉了两种表达。变异验证：M2 删掉宿主侧注入后该测试**仍绿**（因为它比较的是两个都不含记忆的 loop）。它防的是 `WithMemoryInjection("")` 被误实现成"注入占位文本"，**方向正确、职责清晰**。✅

### 2.6 Step 0 评测装置（5 条）—— **产品树上永远 SKIP**

实测全部跳过：

| 测试 | 状态 | 跳过原因 |
|---|---|---|
| `TestBaselineStaleContextThreeArms`（`memory_baseline_test.go:834`） | SKIP | `:120` `t.Skipf`（唯一 importer = desktop/bridge） |
| `TestUnanswerableKnowledgeThreeArms`（`memory_unanswerable_test.go:1107`） | SKIP | 同上 |
| `TestSupplementPointerVsConflictPaired`（`memory_supplement_test.go:440`） | SKIP | 同上 |

**风险评估**：`+70pp` / `95%` / "规则三分支"这些数字**只存在于 Wiki 文本**，代码无法复现。具体后果：

1. **无法证伪**。若将来实现出现回归，**没有任何自动化装置**会红。基线数字成了不可回归的信仰。
2. **判定器自洽率 > 90% 这条纪律（`:28-29`）无法再验证**。上一版 36% 的教训正是"判定器失效导致结论作废"，而现在**判定器本身都没机会跑**。
3. **代码与文档可能已漂移**。装置里的 fixture（`baselineFixtureFiles:195`）、判定逻辑（`:29-33` 的双判 + 候选对调）都在维护中，但**永远不被执行**，编译通过 ≠ 逻辑正确。

**处置建议（推荐第 2 项）**：

- ❌ **删除**：丢失装置 = 丢失协议的可执行记录，将来在新 worktree 重跑的成本远高于维护成本。且 `memory_baseline_test.go` 头部 `:100-101` 明说装置保留"是为了记录测的是什么"。
- ✅ **推荐：移到独立目录 + 加 build tag 隔离**。把 5 条评测测试移到 `desktop/bridge/eval/`（独立包），只靠 `-tags eval` / 独立 `go test` 入口触发；`memory_baseline_test.go` 里**只留** `baselineEnforceEmptyMemoryState` + `baselineMemoryImporters` 两个守卫（修好 §2.2 的 cwd bug 后）。好处：产物树默认编译不受影响，装置在 worktree 上**可运行**，且"产品树上有接线"这个事实由守卫显式断言而不是靠 `t.Skip` 掩盖。
- ⚠️ **保留跳过（现状）**：可接受作短期止血，但必须接受上面三条风险，且**必须在 Wiki 里显式标注"以下数字不可复现、不可回归验证"**。

**同时建议**：无论选哪条，都应把 `HAI_EVAL_BASELINE_FORCE` 的语义从"越过守卫"改成"换树执行"——现在这个开关（`:134-137`）会让人在产品树上强跑出一组"测的是实现"的数字并留在报告里，是个**主动制造误导数据**的入口。

### 2.7 `memory_settings_e2e.mjs` 未进 CI —— **零执行概率**

`desktop/app/package.json:76` `"e2e:memory"` 存在，但 `.github/workflows/` 下**只有 `release.yml`**，其中**没有任何 `go test`，也没有 `e2e:memory`**。唯一一条能抓到「i18n 键不存在」的检查，从未在任何自动化流程中运行过——与背景里"三道检查一道没抓到"的教训直接相关：这次补了第四道，但它同样不跑。

---

## 3. 断言强度评估

### 3.1 `agents/memory_injection_test.go:111` 的 `delta > 1 || delta < -1`

**判据**：变异 M4（memory 计 0）与 M4b（重复计量）均被 `:103` 和 `:121` 抓住，`:111` 的 ±1 容差**不是**这些测试通过的原因。

**是否会放过真实缺陷**：`delta` 允许 ±1 token 的 system 分区变化。真正的缺陷形态是「memory 字节被**部分**重复扣减」——若 `systemChars = len(system) - len(skills) - len(subagent) - memoryChars/2`（半额扣），`delta` 会是 `+want/2`。`mem` 长 31 字符 → `want = 7`（`:102`），`delta = 3`，**在 ±1 之外，仍会被抓**。但若把 `mem` 换成更短的测试串（`want` 落到 1-2），半额扣减就落在容差内。**结论：容差本身合理（吸收 `"\n\n"` 分隔符的折算），但它只在 `want ≥ 3` 时有防护力，而 `want` 由测试自己选的字符串长度决定。** 判据可加强：把 `mem` 换成一个几百字符的长串，让 `want` 足够大，容差的相对意义才成立。

**根因不是容差，是"无记忆基线"退化**：M2 删掉宿主注入后，`agents` 包这 4 条测试**全部通过**。它们只测"给了 `WithMemoryInjection` 之后 `composeSystemPrompt` 怎么处理"，**不测"宿主到底传没传"**。这是职责划分正确的（agents 层本来就看不见宿主），但意味着 **`agents` 层的全绿对「生产从未接线」零信息量**——和背景里第 1 次事故的形态一致。

### 3.2 `desktop/bridge/memory_wiring_test.go` 的 i18n 键正则 —— **在另一个文件**，且覆盖面 5/40

前提更正：该正则**不在** `memory_wiring_test.go`，而在 `desktop/app/e2e/memory_settings_e2e.mjs:180`。逐条实测（`keyRe = /^(?:settings|ctx|common|md|app)\.[a-zA-Z][\w.]*$/`）：

| 输入 | 结果 | 说明 |
|---|---|---|
| `settings.copy` | **CAUGHT** | 原始 bug 形态 ✅ |
| `settings.Copy` | CAUGHT（误报向） | 合法键形态被当成坏键 |
| `记忆目录 settings.copy` | **MISSED** | 与相邻文本在同一叶子 |
| `settings.copy（复制）` | **MISSED** | 键后带中文 |
| `memory.dir` / `nav.settings` / `subagent.memory` | **MISSED** | **前缀不在白名单** |
| `settings`（无点号） | **MISSED** | — |
| `SETTINGS.copy` | MISSED | 大写前缀 |

**最严重的一条**：白名单只有 `settings|ctx|common|md|app` 5 个前缀，而 `desktop/app/src/i18n.ts` 实际有 **40 个命名空间 / 2696 个键**。白名单外的 35 个包括：`approval ask browser compact composer cron embed files hook img mcp narrative permissions plan plugin provider recent sandbox skill task thinking todo tool topbar usage …`

**判据**：若下次出的 bug 是 `t('composer.memoryHint')`（比 `settings.copy` 更自然的位置——记忆注入文案属于 composer/narrative 而非 settings），这条断言**会放行**。文件注释（`:176-181`）花了很大篇幅论证"必须扫叶子节点"（这个论证是对的，第一版确实栽在 textContent 拼接上），却对**前缀白名单**这个更大的漏口没有任何说明。**建议**：改为"键必须存在于 zh 表中"的**否定式**断言——遍历叶子文本，凡是 `^[a-z][\w]*\.[\w.]+$` 形态且**在 `i18n.ts` 的 zh 表里查不到**的就报错。这不需要维护白名单，且自动覆盖全部 40 个命名空间。

**附带风险**：该文件在 CI 里从不执行（§2.7），所以以上弱点目前**没有任何实际影响**——但也意味着它**修好了也不会被验证**。

### 3.3 `desktop/bridge/memory_e2e_test.go:129` 的 `onChars-baseChars < int64(len(idx))`

**判据：有效。** M8 实测——把 `ContextPartMemory` 从 `promptFootprint` 求和里去掉，该测试**立即变红**（`:125` 的 `onChars <= baseChars` 先触发）。这是一个**下界**断言，方向正确（增量至少要覆盖索引正文），不会放过"注入被截断"。且 `:84` 同时求和 `System + Memory` 是必要的（`memory_e2e_test.go:68-71` 的注释已解释原因）。**本组最强的一条断言。**

唯一可提的：`len(idx)` 是不含标题/分隔符的裸索引正文，作为下界略微保守（偏松），但偏松方向是"要求更少的增量"，即更容易通过——**这个方向在实践中无害**，因为 `:125` 已经先兜住了"零增量"。

### 3.4 `desktop/app/e2e/memory_settings_e2e.mjs` 各项断言

| 断言 | 行 | 强度 | 判据 |
|---|---|---|---|
| 「下一个会话」文案 | `:141` | 弱 | 纯 `includes` 子串。`.includes('下一个会话')` 在中文界面成立；切到英文界面（`lang: 'en'`）**立即失败**。E2E 未固定语言 → 隐式依赖用户 `settings.json` 默认值。 |
| 记忆目录往返 | `:145-151` | 强 | `waitFor` + `.catch(() => false)` 显式转成 `bad()`，有 timeout 兜底，不会静默通过 ✅ |
| 未翻译 i18n 键 | `:180-186` | **弱** | 见 §3.2：5/40 命名空间。 |
| 复制按钮存在 | `:190` | 中 | 只查 `.settings-body button.btn` + 文案正则 `/复制\|Copy/`，不验证点击后剪贴板真的写入了内容。 |
| 默认「开」 | `:195` | 中 | `String(on).includes('开')`。英文界面下 `On` 不含「开」→ 会误报 `bad`。同样受语言影响。 |
| 点关 → 落盘 | `:203-207` | 强 | 真读 `~/.go-code/memory.json` 断言 `enabled === false` ✅ 唯一一条同时跨 UI+bridge+磁盘的断言。 |
| 重开持久 / 切 tab 往返 | `:213-232` | 中 | 断言强度依赖 `includes('开')`，同语言耦合问题。 |
| renderer 报错 | `:238-245` | 弱 | `filter(/memory\|Memory\|设置\|settings/i)` —— **带 `settings` 的白名单过宽**，与记忆无关的设置报错也会计入（误报向）；反之拼写不同的记忆报错会被漏掉。 |

**共同问题**：全部文本断言依赖中文默认界面。**没有任何一条把 `lang` 固定下来**。判据：把 `settings.json` 的 `lang` 设成 `en`，`:141` 和 `:195` 都会 `bad()` → **假失败**，而不是被跳过。E2E 应当显式 `t.Setenv`/写 `lang: 'zh'` 固定语言，或把断言改为语言无关的（如查 `data-testid` / 结构类名）。

---

## 4. 本维度的风险排序

| 排名 | 风险 | 判据 | 影响 |
|---|---|---|---|
| **1** | **`postWrite` 三道闸完全无覆盖** —— 函数体删 60%，8 条测试全绿 | M5/M6/M7 实测全绿；`memory/stamp.go:282,287` + `memory/path.go:48` 双重冗余 | 闸①②是**性能契约**（"零 git 开销"，`memory_wiring.go:115`）。回归后模型每次写文件 fork 一次 git，**症状是卡顿不是报错**，无任何测试会红。且 3 条"关闭时不建 .lock"断言都在测 `memory` 包自己控制的东西（`stamp.go:298`），**保护是幻觉**。 |
| **2** | **`baselineMemoryImporters` 只扫 `desktop/bridge`，防意外接线的护栏实际不存在** | M11/M12 实测 PASS；`cd desktop/bridge && go list ./...` 只有 2 个包（根目录有 3 个 importer） | **正好命中用户背景里的担忧**：`memory_baseline_test.go:102-103` 注释承诺"出现其他 importer 仍 t.Fatal"，但往 `agents/`、`subagent/` 加 memory 引用**完全静默**。注释与实现不符。修复只需给 `exec.Command` 加 `cmd.Dir`（1 行）。 |
| **3** | **Step 0 评测装置永久 SKIP，基线数字不可复现** | 3 条主测试实测 SKIP；`+70pp`/`95%`/判定器自洽率纪律均无自动化 | 数字成为不可回归验证的信仰；判定器本身（`:28-33`）从未被执行，编译通过 ≠ 逻辑正确。建议移独立目录 + build tag。 |
| **4** | **`rebuildLoop` 路径的记忆重注入零覆盖** | `grep rebuildLoop --include=*_test.go` 无输出 | 若注入结果被跨 `rebuildLoop` 缓存复用，**用户 switch_model 后记忆静默消失**，无测试会红。**与背景第 1 次事故（`WithPostToolNudgeEnabled`）形态完全同构**。 |
| **5** | **`e2e:memory` 不在 CI，唯一能抓 i18n 缺失的检查从不运行** | `.github/workflows/` 只有 `release.yml`，无 `go test`、无 `e2e:memory` | 补上的"第四道检查"同样不跑。修复 §3.2 的正则前，这一条是前置项。 |
| **6** | **i18n 键正则只覆盖 5/40 命名空间** | 40 个命名空间 / 2696 键；`composer.*`、`narrative.*` 等全在白名单外 | 下次 bug 若在 `composer.memoryHint` 这类位置会被放行。**但因 #5 暂不产生实际影响**。 |
| **7** | **`TestProductionEngineRespectsSwitchOff` 是配对幻觉** | M1 删掉整个钩子后它 PASS | 注释（`:102-103`）声称"配对上一条"，实际只钉闸门不钉接线。给人虚假的安全感。 |
| **8** | **子 agent 无记忆是"注释决策"而非"测试契约"** | `buildExploreLoop`（`main.go:3715-3757`）不传 `WithMemoryInjection`；无任何测试断言 | 当前**不是 bug**（符合 `memory_wiring.go:19-20` 的声明），但将来误接/漏接无测试会红。 |
| **9** | **E2E 文本断言语言耦合** | `:141`/`:195` 的 `includes('开')` | 切英文界面 → **假失败**（不是漏报）。E2E 未固定 `lang`。 |

### 总结

**做对了的**（这些是可信的护栏）：
- `TestProductionEngineStampsMemoryOnWrite` —— 真正能捕获「生产从未接线」，M1 独立复核通过
- `TestMemoryWiredIntoRealSessionSystemPrompt` / `TestMemoryOffMeansNoInjectionInNewSession` —— M2/M8 双重验证，跨 prefs→装配→提示词全链
- `TestMemoryInjectionAccountedAsOwnPart` —— 唯一抓住"重复计量"的一处
- `memory_prefs_test.go` 全部 7 条 —— M9/M10 验证 load/set 两侧都被真实钉住
- `agents` 层 4 条 —— 职责清晰（"给了注入之后怎么拼"），M3/M4/M4b 全部有效

**自欺欺人的**（给人安全感但不设防）：
- `memory_wiring_test.go` 的 8 条中，**三道闸的 0 条**（M5/M6/M7 全绿）
- `TestProductionEngineRespectsSwitchOff` 的"配对"角色（M1 下绿）
- `TestEmptyStateAssertionSkipsOnlyForWiredPackage` 的防意外接线能力（M11/M12 下绿）
- `TestPostWriteDisabledIsInert` 等 3 条的 `.lock` 断言（测的是 `memory` 包内部行为）
- `memory_settings_e2e.mjs` 的 i18n 断言（5/40 命名空间）+ 整条检查在 CI 里不执行
- 5 条评测装置（永久 SKIP，数字只在 Wiki 里）

**方法论建议**：本组测试最有效的三条（`memory_assembly_test.go:51`、`memory_e2e_test.go:95`、`memory_prefs_test.go:167`）都有一个共同特征——**断言的是"生产装配函数的最终产物"**（文件字节、提示词字节、磁盘文件），而不是被测对象的内部调用。最弱的几条共同特征是**否定式断言**（"不该出现 X"）——它们对"功能整个没接线"恒真。**新增测试时优先选肯定式 + 走生产入口。**
