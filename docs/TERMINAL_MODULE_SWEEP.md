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
| `transport/host.ts` 三条 pty | ❌ 上游缺失，切片已定位（§3.1），**未改**（避免动大文件） |
| `useAppStore.ts` term 切片 | ❌ 上游缺失，切片已定位（§3.2–3.4），**未改** |
| `transport/{mock,electron,mobile}` pty | ❌ 同上（§3.3） |
| `i18n.ts` term.* | ❌ 上游缺失（§3.5） |
| `index.ts` 注册 termSurface | ❌ 上游缺失（§3.6） |
| `@xterm/xterm` + `@xterm/addon-fit` | ❌ 不在 `package.json`，需安装 |
| Go 侧 pty 桥 | ✅ 已按契约实现（`pty.go`，见 `TERMINAL_BACKEND_RECOVERY.md`） |

> 这些是**下一步集成清单**，不是恢复失败：切片都在 bundle 里，可逐字搬。
> 本次因「不改大文件 / 时间」未落进 `useAppStore.ts` 等，故**前端尚不能直接运行**（Go 侧已可运行）。
