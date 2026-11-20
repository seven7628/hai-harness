# 右栏终端 —— 模块扫描报告（TERMINAL_MODULE_SWEEP）

> 目标（任务 A）：把 bundle 里所有终端相关模块找全，判定「仓库 `desktop/app/` 里还有哪些缺失」。
> 结论先行：**没有第 17 个独立模块**——终端前端就是已还原的 16 个源文件 + `termSurface.tsx`。
> 但终端功能**还缺一批「寄生在共享文件里的切片」**（store / transport / i18n / types），
> 这些切片在 bundle 里能逐字取到，是下一步集成的关键资产（§3 列出）。

---

## 1. 扫描方法与边界（可复核）

全部在 `/private/tmp`，**禁用无界递归**：

```bash
# 分批：depth 1 → 2 → 3 → 4，每批先 -print0 | xargs -0 grep -l '^// src/'
find . -maxdepth N -type f \( -name '*.cjs' -o -name '*.mjs' -o -name '*.js' \) \
  -not -path '*/node_modules/*' -not -path '*/go/*' -not -path '*/pkg/*' \
  -print0 | xargs -0 grep -l '^// src/'
# 汇总所有模块标记
... | xargs -0 grep -h '^// src/' | sort -u
```

结果：
- depth 1：17 个文件含 `// src/`；
- depth 2：46 个（**与 depth 3/4 完全一致**，说明没有再深一层的 bundle）；
- 模块标记全集 = **133 条**；其中路径含 `term|pty` 的 = **16 条**（正是已还原的 16 个）。
- 另用字符串路径再扫一遍 `src/**/*.{ts,tsx,mjs,css}`：**无新增**终端路径。

## 2. 独立模块判定

| 判据 | 结果 |
|---|---|
| 路径含 `term/pty/Term/Pty` | 16 条，全部已还原 |
| 模块内引用 `ptyCreate/ptyTicket/ptyClose/pty_changed/termEnsure/termClose` | 命中都在 `transport/host.ts`、`store/useAppStore.ts`、`surfaces/builtin/termSurface.tsx` 三个**已存在但被裁剪**的共享文件里 |
| 更大 bundle（24MB `builtin-surfaces.test.cjs`、1.4MB `p8-host-seams-*`） | 模块集合同上，无新增 |

⇒ **无第 17 个文件**。任务 A 的「写入 worktree 对应路径」没有新增对象（新增的是 CSS，见 `terminal.css`）。

---

## 3. 真正缺的：寄生在共享文件里的终端切片（[实证]，可直接集成）

重克隆后的仓库里，下列三个文件是**上游版**（不含终端代码）；而 bundle 里是**删除前的本地版**（含）。
这就是「功能跑不起来」的根因。逐字切片位置（bundle 行号，因文件而异）：

### 3.1 `src/transport/host.ts`（`store-bundle.cjs` ≈ 8090–8115 行）

```ts
// 右栏终端（docs/TERMINAL_DESIGN.md §5.1）：控制面三条。
ptyCreate: async (input) => {
  const r = await d.ptyCreate(input);
  if (!r.ok) return { ok:false, error: r.error ?? "pty_create 失败" };
  const pty = ptyInfoOf(r.data);
  return pty ? { ok:true, pty } : { ok:false, error:"pty_create 返回形状异常" };
},
ptyTicket: async (ptyId) => {
  const r = await d.ptyTicket({ pty_id: ptyId });
  ... url/ticket/expires_in 形状校验 ...
  return { ok:true, url, ticket, expiresIn };
},
ptyClose: async (ptyId) => {
  const r = await d.ptyClose({ pty_id: ptyId });
  return { ok: r.ok, error: r.error };
},
```

### 3.2 `src/store/useAppStore.ts`（`right-tabs-*/store-bundle.cjs` ≈ 28159–28210 行）

```ts
// closeTabNow 里：
if (gone.kind === "term") get7().termClose(gone.id);
// —— 右栏终端（docs/TERMINAL_DESIGN.md §8）——
termEnsure: (tabId) => { /* 无 workspace → failed；否则 status=creating，
   ptyCreate({workspace, session_id, cols:80, rows:24})，失败按 /bridge\s*(启动中|未运行)/ 退避重试 ≤40 次，
   成功写 { ptyId, status:"running" } */ },
termClose: (tabId) => { /* 有 ptyId 则 ptyClose；从 term 表删除该 tab */ },
```

另有：
- `pty_changed` 事件处理（`case "pty_changed":` ≈ 22332 行）：按 `pty_id → tabId` 反查更新 `term` 表。
- 状态字段：`term: Record<tabId, {status:"creating"|"running"|"failed", ptyId?, error?}>`。
- 关闭标签时 `if (gone.kind === "term") termClose(gone.id)`。

### 3.3 `src/transport/{mock,electron,mobile}.ts`

- `mock.ts`：`async ptyCreate(){...}` 桩（≈19978 行）。
- `mobile.ts`：`ptyCreate/ptyTicket/ptyClose` 一律返回 `{ok:false, error:"终端仅在桌面端可用"}`（≈24124 行）。
- `electron.ts`：经 `window.hai.*` 暴露（与 host 同构）。

### 3.4 `src/store/useAppStore.ts` 的类型

- `RightTab` / `TabKind` 需增加 `'term'`；`TabInstance` 需增加 `{kind:'term'}` 成员。
- 当前上游 `RightTab = 'activity' | 'git' | 'file' | 'web'`（`useAppStore.ts:97`）——**缺 `'term'`**。

### 3.5 `src/i18n.ts`

终端文案键（还原自 `TerminalPane/TerminalTab/termSurface` 的 `t(...)` 调用）：

```
rp.tab.term, term.starting, term.replaying, term.exit, term.failed, term.reopen,
term.insert, term.inserted, term.quoteSource, term.truncated,
term.menu.selection, term.menu.screen, term.menu.copy, term.menu.paste,
term.menu.clear, term.menu.reopen
term.fontMissing（源码注释提到，文案未见）
```

### 3.6 `src/surfaces/builtin/index.ts`

上游**未** `import './termSurface'`（已确认）。要启用终端必须加这一行（副作用注册）。

---

## 4. 结论与集成缺口

| 项 | 状态 |
|---|---|
| 16 个终端独立模块 | ✅ 已还原（本分支），并补 `// [recovered]` 来源头 |
| `termSurface.tsx` | ✅ 已还原 |
| `terminal.css` | ⚠️ 原文丢失，已按契约+截图**重建** |
| `transport/host.ts` 三条 pty | ✅ 已落地（见 §5.1 —— 本仓 `host.ts` 是纯注册缝，实现落在 `transport/electron.ts`） |
| `useAppStore.ts` term 切片 | ✅ 已落地（§5.2） |
| `transport/{mock,electron,mobile}` pty | ✅ 已落地（§5.1） |
| `i18n.ts` term.* | ✅ 已落地（§5.3：`rp.tab.term` + 17 条 `term.*`） |
| `index.ts` 注册 termSurface | ✅ 已落地（§5.3） |
| `@xterm/xterm` + `@xterm/addon-fit` | ✅ 已装（`^6.0.0` / `^0.11.0`） |
| Go 侧 pty 桥 | ✅ 已按契约实现（`pty.go`，见 `TERMINAL_BACKEND_RECOVERY.md`） |

> 这些是**下一步集成清单**，不是恢复失败：切片都在 bundle 里，可逐字搬。
> 本次因「不改大文件 / 时间」未落进 `useAppStore.ts` 等，故**前端尚不能直接运行**（Go 侧已可运行）。

---

## 5. 集成落地记录（2026-11，本分支）

§4 的缺口已全部补齐。**逐条对应**（每条都注明"恢复"还是"实现"）：

### 5.1 transport 层（控制面三条）

| 文件 | 改动 | 来源 |
|---|---|---|
| `src/transport/types.ts` | 新增 `PtyInfo` / `PtyCreateInput` / `PtyCreateResult` / `PtyTicketResult` / `PtyCloseResult` + `Transport` 上三个方法 | 恢复（§3 切片推出的形状）+ Go 侧字段名（`pty.go`） |
| `src/transport/electron.ts` | `ptyCreate` / `ptyTicket` / `ptyClose` 三条映射 + `ptyInfoOf` 形状校验（**不抛**） | 逐字恢复 §3.1 |
| `src/transport/mock.ts` | 三条一律 `{ok:false, error:"终端仅在桌面端可用"}` | 逐字恢复 §3.3 |
| `src/mobile/transport/mobile.ts` | 同上（手机端显式给原因，不靠 Proxy 兜 `unsupported:`） | 逐字恢复 §3.3 |
| `electron/main.ts` + `electron/preload.ts` + `src/lib/bridge.ts` | `pty:create` / `pty:ticket` / `pty:close` 三个 IPC，走 `sendBridgeRequest`（主进程直发，能把 `data` 带回渲染层） | **实现**（依据 §3.1 的 `d.ptyCreate` 用法 + 既有 xlsx/docx/pptx 直发口径） |

> **与 §3.1 的偏差（必须说明）**：§3.1 说切片在 `src/transport/host.ts`。本仓的 `host.ts`
> 是 P8-01b 的**纯注册缝**（`setHostTransport` / `getHubTransport`，没有实现体），
> 把三条 pty 塞进去只会得到一段没人调用的死代码。切片真正的宿主是 `electron.ts`
> （`const d = bridge!` 那一层），故实现落在那里 —— 保存缝不变，`Transport` 接口是它分发的类型。

### 5.2 store 层（运行时表 + 两个动作 + 事件分发 + 标签持久化）

`src/store/useAppStore.ts`：

- `RightTab` / `TabKind` 加 `'term'`；`TabInstance` 加 `TermTabInstance = {id, kind:'term'}`。
- `PersistedTab` 的 `{id, kind:'activity'|'git'}` 扩成 `|'term'`，`toPersistedTab` /
  `fromPersistedTab` 两侧都加了 `case 'term'`。**这两处漏一个就是"重启后终端标签消失"**
  （`toPersistedTab` 漏 → 落到 web 分支 → `webUrlSnapshot` 为 undefined → 整条不持久化）。
- 新增 `export interface TermRuntime` 与 `AppState.term: Record<tabId, TermRuntime>`（初始 `{}`）。
- 新增动作 `termEnsure` / `termClose`（逐字恢复 §3.2，含 40 次退避重试与
  `/bridge\s*(启动中|未运行)/` 判定；常量 `BRIDGE_NOT_READY_RE`、`TERM_CREATE_MAX_ATTEMPTS`）。
- `closeTabNow` 里加 `if (gone.kind === 'term') get().termClose(gone.id)`。
- `dispatchEvent` 新增 `case 'pty_changed'`：**只更新 `term` 表**（不建视图、不进 checkpoint、
  不落盘），按 `pty_id → tabId` 反查，查不到（已关标签的迟到事件）直接丢弃。

### 5.3 文案 / 注册 / 入口

- `src/i18n.ts`：`rp.tab.term` + 17 条 `term.*`（中英各一份，文案逐字取自 §3.5 / 设计 §6）。
- `src/surfaces/types.ts`：`BuiltinKind` 加 `'term'`；`src/surfaces/registry.ts`：
  `BUILTIN_KINDS` 加 `'term'`（不加则注册即抛、恢复时标签被当坏数据丢掉）。
- `src/surfaces/builtin/index.ts`：`import './termSurface'`（副作用注册，§3.6）。
- `src/components/RightPanel.tsx`：`ADDABLE_KINDS` 加 `'term'` —— **推断项**（文档未覆盖）：
  终端没有别的入口（file 由点文件产生、web 由内嵌页面同步），不进＋菜单则功能不可达。

### 5.4 组件接线（"能跑起来"的关键）

三个小文件是编译形态且依赖 bundle 内联变量（`registerSurface` / `TerminalTab` / `TermIcon` /
`import_jsx_runtime`），**在源码里必然 ReferenceError**，因此按 `TerminalPane.tsx` 同一口径
回译为 TSX 并补真实 import：`TermIcon.tsx`、`TerminalTab.tsx`、`termSurface.tsx`。
六个纯模块（frame / writer / paneMachine / theme / formatSelection / commandContext）原先
**没有任何 `export`**，已补导出名与类型并移除 `@ts-nocheck`。

### 5.5 验证（本机真实输出）

```
npx tsc --noEmit          → 8 行错误，全部在 src/p8-host-seams-entry.tsx（本次改动前就有，见 §5.6）
npx vite build            → ✓ built in ~6s（xterm 进 main chunk，terminal.css 已 @import）
npm run test:terminal     → 68 tests / 68 pass / 0 fail
go build ./...            → 通过
go test -run TestPty ./desktop/bridge/ → 7 PASS + 1 SKIP（TestPtyLiveEcho 因本机沙箱不允许分配 PTY 而跳过）
```

### 5.6 已知遗留

- `src/p8-host-seams-entry.tsx` 是上一次恢复遗留的 **bundle 输出**（105 行，含 license 尾巴，
  没有任何代码 import 它；`scripts/test-host-seams.mjs` 只是把自己的虚拟入口 *命名为* 这个名字）。
  `tsc --noEmit` 的 8 条错误全在这里，**本次改动前就有**（基线 16 条 = 这 8 条 + 8 条终端相关）。
  按"不删文件"的约定没有动它；清理方式是把它删掉或回译成真正的探针入口。
- 真实 PTY 分配（`forkpty`）在本机沙箱里 `operation not permitted`，`TestPtyLiveEcho` 走
  `t.Skipf` 跳过。因此"起真 shell → 回显"这条链**未在本机验证**；帧编解码、票据一次性/过期、
  未知会话拒绝等 7 条契约有测试覆盖。
