# 右栏内嵌终端（TERMINAL_DESIGN）—— 重建稿

> ## ⚠️ 本文不是原件
>
> 原件 `docs/TERMINAL_DESIGN.md` 在 2026-11-19 22:15 的误删事故中丢失，且**从未提交到
> git**（worktree `terminal-panel` 的全部工作只在本地）。全盘扫描确认：**没有任何正文、
> 目录或片段幸存**，GoLand LocalHistory / fileHistory / Cursor 历史 / Trash / 快照均无。
>
> 本文由两条**实证线索**重建：
>
> 1. **前端源码**：误删前的构建产物 bundle（`/private/tmp/builtin-surfaces.test.cjs`,
>    `right-tabs-*/store-bundle.cjs`, `term-*.test.cjs` 等）保留了 `// src/...` 原始模块
>    标记，据此切出 **9 个终端模块**、并逐字取出契约代码。
> 2. **文档引用点**：bundle 的代码注释里保留了指向原文档的 7 处引用，点名了
>    **§5.1 / §8 / §9** 三节存在，本文按这些锚点还原对应内容。
>
> **丢失且无法重建**：散文、决策理由、与 Claude Code / xterm 的调研对比、验证计划、
> 章节 §0–§4 / §6–§7 / §10+ 的实际内容。下文凡标 `【证据】` 者为逐字取自产物，
> 标 `【推断】` 者为据命名与调用推断，标 `【缺失】` 者为原文档有但无从得知。

---

## §1 架构总览 【证据 + 推断】

终端是**右栏的一条标签类型**（`kind: "term"`），与 `activity` / `git` / `file` / `web` 并列。

分三层，边界由 bundle 里的三处注释直接写明：

| 层 | 职责 | 证据 |
|---|---|---|
| 桌面 Go 桥 | **PTY 进程的唯一宿主**（spawn / 收字节 / 关） | `// 右栏终端（docs/TERMINAL_DESIGN.md §5.1）：PTY 只活在桌面的 Go 桥里` |
| 主进程 / preload | 转发 JSON 命令 + 一条事件流 | `src/transport/electron.ts` 的 `ptyCreate/ptyTicket/ptyClose` |
| 渲染层 | xterm.js 实例、标签状态、菜单与引用 | `src/components/terminal/*` |

**手机端明确不支持** 【证据】：

```ts
// 右栏终端（docs/TERMINAL_DESIGN.md §5.1）：PTY 只活在桌面的 Go 桥里，手机连的是
// /api/cmd 面（没有 pty_* 命令，也没有 /pty 的 WS 端点）。这里显式给失败原因，不靠
// Proxy 兜成 `unsupported: ptyCreate` —— 手机面板要显示一句人话。
```

> 由此可知原 §5.1 的标题是「**控制面**」，且其中明确了两个事实：
> 移动端 `/api/cmd` 面无 `pty_*` 命令；也没有 `/pty` 的 WS 端点。

---

## §2 控制面契约（三条命令）【证据】

原 §5.1 的"控制面三条"，逐字取自 `src/transport/electron.ts`：

### 2.1 `ptyCreate(input) -> { ok, pty }`

```ts
ptyCreate: async (input) => {
  const r4 = await d3.ptyCreate(input);
  if (!r4.ok) return { ok: false, error: r4.error ?? "pty_create 失败" };
  const pty = ptyInfoOf(r4.data);
  return pty ? { ok: true, pty } : { ok: false, error: "pty_create 返回形状异常" };
},
```

请求入参 【证据，取自调用点】：`{ workspace, session_id, cols: 80, rows: 24 }`

返回体形状校验 【证据】：

```ts
function ptyInfoOf(data6) {
  if (!data6) return null;
  if (typeof data6.pty_id !== "string" || data6.pty_id === "") return null;
  if (typeof data6.cwd !== "string" || typeof data6.shell !== "string") return null;
  if (typeof data6.pid !== "number" || typeof data6.cols !== "number" || typeof data6.rows !== "number") return null;
  return { pty_id: data6.pty_id, cwd: data6.cwd, shell: data6.shell, pid: data6.pid, cols: data6.cols, rows: data6.rows };
}
```

> 契约要点：`pty_id` / `cwd` / `shell` 是 **string**，`pid` / `cols` / `rows` 是 **number**；
> 任一不符即判为"形状异常"，**不抛异常**（见 §2.4）。

### 2.2 `ptyTicket(ptyId) -> { ok, url, ticket, expiresIn }`

```ts
ptyTicket: async (ptyId) => {
  const r4 = await d3.ptyTicket({ pty_id: ptyId });
  if (!r4.ok) return { ok: false, error: r4.error ?? "pty_ticket 失败" };
  const data6 = r4.data;
  if (!data6 || typeof data6.url !== "string" || typeof data6.ticket !== "string" || typeof data6.expires_in !== "number") {
    return { ok: false, error: "pty_ticket 返回形状异常" };
  }
  return { ok: true, url: data6.url, ticket: data6.ticket, expiresIn: data6.expires_in };
},
```

> **这是原方案里最关键的一条**：数据面不走 IPC，而是走 **WebSocket**，凭一次性 ticket
> 鉴权后才能连。`expires_in` 单位为秒。原 §5.x 应有对应的一节讲 ticket 生命周期与
> WS 端点协议 —— 【缺失】。

### 2.3 `ptyClose(ptyId) -> { ok, error }`

```ts
ptyClose: async (ptyId) => {
  const r4 = await d3.ptyClose({ pty_id: ptyId });
  return { ok: r4.ok, error: r4.error };
},
```

### 2.4 错误处理原则 【证据】

```ts
// 右栏终端（docs/TERMINAL_DESIGN.md §5.1）：控制面三条。这里只做「桥 data → 结构化
// 结果」的映射（data 是 JSON 直通的 Record，字段名由 bridge 决定）；**不抛**——形状
// 不对与业务失败对面板来说是同一件事：显示一句错误，别把整个视图炸掉。
```

> 原 §5.1 的三条设计约束可据此还原：① 只做形状映射，不解释业务；② 失败统一返回
> `{ok:false,error}`；③ **永不抛出**，面板只显示一句话。

---

## §3 事件面：`pty_changed` 【证据】

原 §5.1 后半（进程状态）逐字：

```ts
// 终端进程状态（右栏终端，docs/TERMINAL_DESIGN.md §5.1）：只更新运行时表 ——
// 不建会话视图、不进 checkpoint、不落盘。pty_id 反查 tabId（一个 PTY 只属于一条标签）。
case "pty_changed": {
  const ptyId = typeof raw2.pty_id === "string" ? raw2.pty_id : "";
  if (!ptyId) break;
  const status = raw2.status === "exited" ? "exited" : raw2.status === "failed" ? "failed" : "running";
  const exitCode = typeof raw2.exit_code === "number" ? raw2.exit_code : void 0;
  const error3 = typeof raw2.error === "string" && raw2.error ? raw2.error : void 0;
  setState((s3) => {
    const hit = Object.entries(s3.term).find(([, v3]) => v3.ptyId === ptyId);
    if (!hit) return {};
    const [tabId, prev2] = hit;
    return { term: { ...s3.term, [tabId]: { ...prev2, status, ...exitCode !== void 0 ? { exitCode } : {}, ...error3 ? { error: error3 } : {} } } };
  });
  break;
}
```

事件 payload：`{ pty_id, status: "running"|"exited"|"failed", exit_code?: number, error?: string }`

三条硬约束 【证据】：
1. **只更新运行时表** —— 不建会话视图
2. **不进 checkpoint、不落盘**
3. `pty_id → tabId` 反查（**一个 PTY 只属于一条标签**）

---

## §4 前端状态模型 【证据】

```ts
term[tabId] = {
  status: "creating" | "running" | "exited" | "failed",
  ptyId?: string,
  exitCode?: number,
  error?: string,
}
```

`termEnsure(tabId)`（原 §8 内容，逐字）：

```ts
// —— 右栏终端（docs/TERMINAL_DESIGN.md §8）——
termEnsure: (tabId) => {
  const s3 = get7();
  const cur = s3.term[tabId];
  if (cur && (cur.status === "creating" || cur.status === "running")) return;
  const ws = sessionWorkspaceOf(s3, s3.activeSessionId ?? "") || s3.workspace || "";
  if (!ws) {
    set5({ term: { ...s3.term, [tabId]: { status: "failed", error: "没有工作区，无法确定终端目录" } } });
    return;
  }
  set5({ term: { ...s3.term, [tabId]: { ...cur, status: "creating", error: void 0 } } });
  let attempt = 0;
  const create7 = () => {
    void getTransport().ptyCreate({ workspace: ws, session_id: s3.activeSessionId, cols: 80, rows: 24 }).then((r4) => {
      const cur2 = get7().term[tabId];
      if (!cur2) return;
      if (!r4.ok || !r4.pty) {
        if (/bridge\s*(启动中|未运行)/.test(r4.error ?? "") && attempt < 40) {
          attempt += 1;
          setTimeout(create7, Math.min(1e3 + attempt * 250, 3e3));
          return;
        }
        set5((st) => ({ term: { ...st.term, [tabId]: { ...cur2, status: "failed", error: r4.error ?? "终端创建失败" } } }));
        return;
      }
      set5((st) => ({ term: { ...st.term, [tabId]: { ...cur2, ptyId: r4.pty.pty_id, status: "running" } } }));
    });
  };
  create7();
},
termClose: (tabId) => {
  const cur = get7().term[tabId];
  if (cur?.ptyId) void getTransport().ptyClose(cur.ptyId);
  set5((st) => {
    const next5 = { ...st.term };
    delete next5[tabId];
    return { term: next5 };
  });
},
```

**可还原的设计点**：
- **幂等**：`creating`/`running` 直接返回，重复挂载不会重复建 PTY
- **cwd 来源** = 会话所属工作区（`sessionWorkspaceOf`），无工作区则失败并给出人话
- **桥未就绪要重试**：匹配 `/bridge\s*(启动中|未运行)/`，最多 40 次，退避
  `min(1000 + attempt*250, 3000)ms`（即 1.0s→3.0s 封顶，总时长约 ≤100s）
- **关闭**：先 `ptyClose`（有 ptyId 时），再删表项

---

## §5 标签注册与持久化 【证据】

```ts
"git",
"file",
"web",
// term：右栏终端（docs/TERMINAL_DESIGN.md）。加在这里就等于同时放开了
// 注册校验与持久化恢复 —— 漏了它，症状是「重启后终端标签消失」。
"term"
```

序列化/反序列化分支：

```ts
switch (t4.kind) {
  case "activity":
  case "git":
  case "term":
    return { id: t4.id, kind: t4.kind };
```

> `term` 标签**只持久化 `id` + `kind`**，不持久化 `ptyId` —— 与 §3 的"不落盘"一致，
> 重启后标签还在但 PTY 需要重建。原 §7（标签/生命周期）应有一节讲这个取舍 —— 【缺失】。

---

## §6 i18n 文案表（原 §9，逐字全量 17 条）【证据】

| key | 文案 |
|---|---|
| `term.starting` | 正在启动 shell… |
| `term.replaying` | 恢复中… |
| `term.exit` | 进程已退出（code {code}） |
| `term.reopen` | 重新打开 |
| `term.failed` | 终端启动失败：{error} |
| `term.truncated` | 更早的输出已截断 |
| `term.insert` | 引用到对话 |
| `term.inserted` | 已引用 |
| `term.quoteSource` | 终端 |
| `term.menu.selection` | 引用选中内容 |
| `term.menu.screen` | 引用当前屏幕 |
| `term.menu.copy` | 复制 |
| `term.menu.paste` | 粘贴 |
| `term.menu.clear` | 清屏 |
| `term.menu.reopen` | 重新打开 shell |
| `term.limit` | 终端数量已达上限（{n}），先关掉不用的标签 |
| `term.fontMissing` | 未找到等宽 Nerd 字体，图标可能显示为方框 |

**由文案可还原的功能点**：
- **引用到对话**：可从终端选区/当前屏幕把内容作为引用插入 composer，`
  quoteSource = 终端`
- **回放**：`term.replaying` + `term.truncated` ⇒ 有**输出回放/scrollback 恢复**机制，
  且更早输出会被截断
- **上限**：`term.limit` ⇒ 终端标签数量有上限（与右栏标签的 LRU 同源？【缺失】）
- **Nerd 字体探测**：图标可能显示为方框 ⇒ 有等宽 Nerd 字体检测逻辑

---

## §7 组件分层（9 个文件，已从 bundle 还原）【证据】

| 文件 | 职责（按内容概括） |
|---|---|
| `src/surfaces/builtin/termSurface.tsx` | 右栏 surface 入口，注册 `term` 标签 |
| `src/components/terminal/TerminalPane.tsx` | 258 行，xterm 实例装配、菜单、选区引用、粘贴 |
| `src/components/terminal/TerminalTab.tsx` | 标签头（标题/状态点） |
| `src/components/terminal/TermIcon.tsx` | 图标（Nerd 字体） |
| `src/components/terminal/frame.ts` | 二进制帧编解码 |
| `src/components/terminal/writer.ts` | 写入节流/批处理 |
| `src/components/terminal/paneMachine.ts` | 状态机（creating/running/exited/failed） |
| `src/components/terminal/theme.ts` | 配色（对齐设计 token） |
| `src/components/terminal/formatSelection.ts` | 选区格式化（引用到对话的文本形状） |

> 这些文件是 **esbuild 编译后的 JS**（`jsxDEV(...)` 形式），逻辑与注释完整，需回译成 TSX。

---

## §8 原文档章节地图（已知部分）

| 节 | 主题 | 可信度 |
|---|---|---|
| §5.1 | 控制面三条 + 进程状态事件；PTY 只在桌面桥；移动端无 pty_*/无 /pty WS | 【证据】 |
| §7? | 标签注册、持久化只存 id+kind | 【推断】 |
| §8 | 前端 store 动作（`termEnsure` / `termClose`） | 【证据】 |
| §9 | 文案与 UI 交互（引用到对话、菜单、上限、字体） | 【证据】 |
| §0–§4 | 背景、目标、调研、总体方案 | 【缺失】 |
| §6 | 疑为数据面（WS + ticket + 帧协议） | 【缺失】 |
| §10+ | 验证计划、分期 | 【缺失】 |

---

## §9 恢复缺口清单

| # | 缺口 | 影响 |
|---|---|---|
| 1 | **Go 侧 PTY 桥源码**（spawn zsh、读字节、resize、关闭、ticket 签发、WS 端点 `/pty`） | 全部丢失，需按 §2/§3 契约重写 |
| 2 | 数据面 WS 协议（帧格式、背压、重连、ticket 校验） | `frame.ts`/`writer.ts` 只剩前端一半，后端语义需反推 |
| 3 | 输出回放策略（scrollback 落哪、截断阈值） | 只有 `term.replaying` / `term.truncated` 两条文案 |
| 4 | 终端数量上限与 LRU 关系 | 只有 `term.limit` 一条文案 |
| 5 | 原文档全部散文与决策理由 | 不可恢复 |
| 6 | 9 个还原模块的 TSX 原始形态 | 现在是编译后 JS |

---

_生成时间：2026-11-19。证据文件：`/private/tmp/terminal-panel-recovered/EXTRACT-REPORT.md`。_
