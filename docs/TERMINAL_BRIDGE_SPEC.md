# 右栏终端 —— Go 桥实现规格（TERMINAL_BRIDGE_SPEC）

> 本文**不是恢复出来的文档**，而是根据已还原的前端实现与单元测试**反推**出的后端实现规格。
> 每一条都标注了证据来源，可直接据此重写丢失的 Go 侧 PTY 桥。
>
> 证据来源（均在 worktree `terminal-panel` 内，从构建产物还原）：
> `components/terminal/{frame,writer,paneMachine,commandContext,formatSelection}.ts`
> 及其 `-test.ts`、`TerminalPane.tsx`（259 行）、`surfaces/builtin/termSurface.tsx`（195 行）

---

## §1 协议总表

### 1.1 传输

| 项 | 值 | 证据 |
|---|---|---|
| 通道 | **WebSocket**（不走 IPC 传数据） | `new WebSocket(url + "?ticket=…")` |
| 鉴权 | 一次性 ticket 放在 query string | 同上 |
| `binaryType` | `arraybuffer` | 同上 |
| 帧类型 | 二进制单帧，**用首字节区分** | `frame.ts` |

### 1.2 帧编码（`frame.ts` 逐字）

```ts
var CTRL_BYTE = 0;                    // 控制帧：首字节 0x00

function encodeCtrl(c2) {             // 控制帧 = 0x00 + UTF-8 JSON
  const json3 = encoder.encode(JSON.stringify(c2));
  const out = new Uint8Array(json3.length + 1);
  out[0] = CTRL_BYTE;
  out.set(json3, 1);
  return out;
}

function decodeFrame(buf) {           // 非 0x00 开头 ⇒ 原样当数据
  const bytes = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  if (bytes.length < 2 || bytes[0] !== CTRL_BYTE) return { data: bytes };
  try {
    const c2 = JSON.parse(decoder2.decode(bytes.subarray(1)));
    if (!c2 || typeof c2.t !== "string" || !c2.t) return { data: bytes };
    return { ctrl: c2 };
  } catch {
    return { data: bytes };
  }
}
```

**四条硬约束**（`frame-test.ts` 逐条断言）：

| # | 约束 | 测试 |
|---|---|---|
| 1 | 控制帧 = `0x00` + JSON，往返一致 | 「控制帧：0x00 开头 + JSON」 |
| 2 | 非 `0x00` 开头**原样**返回，**中间出现的 NUL 也算数据** | 「数据帧：…含中间的 NUL 也算数据」（`[104,0,105]`） |
| 3 | 畸形控制帧按**数据帧**处理，**不断连**（`[0]` / `[0,'{']` / `[0,"xy"]` / `t:""`） | 「畸形控制帧按数据帧处理（不判死连接）」 |
| 4 | 超长帧只看首字节，不做 JSON 解析（256KB 全 `a`） | 「超长输出不被 JSON 解析」 |

> **Go 侧对应实现**：读 WS 消息后判断 `if len(b) >= 2 && b[0] == 0 { if json.Unmarshal(b[1:], &ctrl) == nil && ctrl.T != "" { 控制帧 } }`，
> 否则原样写进 PTY。**解析失败必须回落成数据，不能关连接**——这是设计的显式选择。

### 1.3 控制帧清单

| 方向 | `t` | 载荷 | 语义 | 证据 |
|---|---|---|---|---|
| S→C | `hello` | — | 握手确认；前端**不改状态、无副作用** | `paneMachine.ts` `case "hello"` |
| S→C | `reset` | — | **回放前**让前端 `term.reset()` | `{k:"reset-term"}` → `termRef.current?.reset()` |
| S→C | `gap` | — | 回放被截断，前端写一行 `term.truncated` 暗色提示 | `case "gap"` |
| S→C | `exit` | `{code?: number}` | 进程退出；前端关 socket | `case "exit"` |
| C→S | `resize` | `{cols, rows}` | **100ms 防抖**，仅在 `readyState===OPEN` 时发 | `RESIZE_DEBOUNCE_MS = 100` |

前端对未知 `t` **静默忽略**（`default: break`）⇒ 后端可安全新增控制帧。

### 1.4 IPC 命令（三条）

```ts
// pty_create：入参 ← termEnsure 调用点逐字
{ workspace: string, session_id?: string, cols: 80, rows: 24 }
// 返回 data 必须满足（ptyInfoOf 逐字段校验）：
{ pty_id: string(非空), cwd: string, shell: string, pid: number, cols: number, rows: number }

// pty_ticket
{ pty_id: string } → data: { url: string, ticket: string, expires_in: number(秒) }

// pty_close
{ pty_id: string } → { ok }
```

> 校验是**严格类型**的：`pid/cols/rows` 用 `typeof === "number"`，`pty_id/cwd/shell` 用 `string`。
> Go 侧 `json` tag 必须是 snake_case：`pty_id` / `exit_code` / `expires_in`。

### 1.5 事件（S→C，走既有事件通道，不是 WS）

```ts
{ pty_id: string, status: "running" | "exited" | "failed",
  exit_code?: number, error?: string }
```

三条设计约束（逐字注释）：
- **只更新运行时表** —— 不建会话视图
- **不进 checkpoint、不落盘**
- `pty_id → tabId` 反查，**一个 PTY 只属于一条标签**

---

## §2 生命周期时序（`paneMachine.ts` 全量还原）

```
idle ──open──▶ creating ──created(ptyId)──▶ connecting ──ticket(tk)──▶ connecting(+ticket)
                                                        │
                                                        ├─ ws-open / hello ──▶（状态不变，无 fx）
                                                        │
                                                        ├─ reset ──▶ replaying ──▶ fx: reset-term
                                                        │                  │
                                                        │                  └─ output ──▶ live  fx: show（只一次）
                                                        │
                                                        └─ timeout(connect) ──▶ failed  fx: close-socket + toast

live ──exit(code)──▶ exited  fx: close-socket
任意 ──error(msg)──▶ failed  fx: close-socket + toast(原文)
任意 ──close──▶ idle         有 ptyId: fx: close-socket + close-pty
                             无 ptyId: fx: close-socket
```

### 2.1 关键时序契约

1. **`hello` 必须先于 `reset`**：前端收到 `hello` 不做事，但 `reset` 会触发 `term.reset()`。
   若后端先发 `reset` 再发 `hello` 也合法（两者互不依赖），但 `reset` 必须在**第一个数据帧之前**。
2. **回放顺序固定**：`hello → (gap)? → reset → 历史输出… → 实时输出`
   - `gap` 表示更早的输出被丢弃（滚动缓冲上限），前端据此显示 `term.truncated`
3. **`exited` 后不再发 `pty_changed`**，前端收到 `exit` 即关 socket 并停止
4. **超时只在其阶段有效**（测试逐条断言）：
   - `phase=create` 只在 `creating` 生效
   - `phase=connect` 只在 `connecting` 生效
   - **迟到的 timeout 必须被忽略**（不能毁掉已连上的终端）
5. **`ws-open`/`hello` 不能改状态**（测试用 `assert.equal(r.state, connecting)` 断言**同一对象**）

### 2.2 状态机是纯函数

测试断言「任何转移都不改入参」（`structuredClone` 前后比对）。Go 侧若要有等价状态机，
务必保持纯函数 / 值语义，避免共享可变状态。

---

## §3 前端侧的连接与回放细节（`TerminalPane.tsx`）

```ts
const r4 = await getTransport().ptyTicket(ptyId);
if (!r4.ok || !r4.url || !r4.ticket) { apply({t:"error", …}); return; }
apply({ t: "ticket", ticket: r4.ticket });
const sock = new WebSocket(`${r4.url}?ticket=${encodeURIComponent(r4.ticket)}`);
sock.binaryType = "arraybuffer";
sock.onmessage = (ev) => { … decodeFrame … };
```

- **URL 由 `pty_ticket` 下发**，前端不拼路径 ⇒ 后端可自由决定挂载点（原实现为 `/pty`）
- ticket 走 `encodeURIComponent` ⇒ 必须是 URL-safe 字符集
- **`onclose` 且状态不是 `exited` ⇒ 视为错误**：`term.failed` 填 `"closed"`
- `onerror` ⇒ `term.failed` 填 `"socket"`
- 卸载时：`writer.clear()` + `sock.close()` + `term.dispose()`

### 3.1 写入泵（`writer.ts`）

前端把 PTY 输出按 **批** 写入 xterm，且同一时刻只允许一次 `write` 在飞：

```ts
function terminalWriter(write) {
  // queue: Array<Uint8Array | (() => void)>
  // 函数项是"屏障"：执行完再继续
  // 数据项尽量合并成一个 Uint8Array 一次 write
  // running=true 时不再并发写；write 的 done 回调里再 pump
  clear(): closed = true; queue = []      // 在飞的 write 取消不了，但它的 done 已无意义
}
```

**含义**：后端可以放心地**高频小帧**推送（比如 1KB 一段的实时输出），前端会合并；
但后端**不应自己做节流**，因为前端已有背压。真正的背压点在 WS 缓冲区。

---

## §4 Go 侧实现规格

### 4.1 建议文件布局

```
desktop/bridge/
  pty_manager.go      # PTY 进程表：Create/Close/Resize/Write，生命周期与回收
  pty_ticket.go       # 一次性 ticket 签发与校验（TTL、单次使用）
  pty_ws.go           # WS 端点：/pty?ticket=…，帧编解码，输入输出泵
  pty_history.go      # 环形回放缓冲（scrollback），负责 reset/gap 语义
  pty_commands.go     # IPC 命令注册：pty_create / pty_ticket / pty_close
```

### 4.2 核心类型

```go
// PTY 实例。一个 tabId 对应一个 PTY，绝不跨标签复用。
type ptyProc struct {
    ID      string  // pty_id，全局唯一（进程内）
    WSKey   string  // 创建时的 workspace，用于审计与目录校验
    Session string
    Cmd     *exec.Cmd
    PTY     *os.File  // master fd
    Shell   string
    Cwd     string
    PID     int
    Cols    int
    Rows    int
    hist    *ringBuffer  // 回放缓冲
    subs    map[*wsConn]struct{}  // 允许多端接入？见 §4.6
    mu      sync.Mutex
    exited  bool
    exitCode int
}
```

### 4.3 命令语义

| 命令 | 后端必须做 | 失败返回 |
|---|---|---|
| `pty_create` | 校验 workspace 存在；`cwd` 取会话工作区；spawn 登录 shell（`$SHELL --login` 或 `-i`）；设置 `TERM=xterm-256color`；写入初始 `cols/rows` | `{ok:false,error}`，前端只显示一句 |
| `pty_ticket` | 校验 pty 存在且未退出；签发 **TTL ≤ 60s、单次可用** 的 ticket；返回完整 WS URL | 同上 |
| `pty_close` | 关闭 master fd、SIGKILL 进程组、回收回放缓冲、广播 `pty_changed{status:"exited"}` | 同上（已退出也算 ok） |

**错误文案契约**：`pty_create` 在桥未就绪时返回的 error 必须能被前端正则匹配到
`/bridge\s*(启动中|未运行)/`，否则前端不会重试（它最多重试 40 次、退避 1.0→3.0s）。
原实现里这条路径对应"bridge 启动中"。

### 4.4 WS 端点流程

```go
// GET /pty?ticket=xxx
// 1) 校验 ticket（存在、未过期、未使用）→ 立即作废
// 2) 升级为 WS，设置 read limit（数据帧可大，故限 1MB），
//    写超时、ping/pong（30s 空闲保活）
// 3) 发 hello
// 4) 若回放缓冲有内容：若被截断先发 gap，再发 reset，然后按序发送历史字节
// 5) 进入实时：PTY → WS 直通；WS → PTY 直通
// 6) PTY EOF：发 exit{code}，关闭 WS
```

**`reset` 的确切语义**：前端收到后执行 `term.reset()`（清屏 + 清滚动缓冲）。
后端必须在发送历史**之前**发 `reset`，否则前端会重复渲染。

### 4.5 回放缓冲

| 项 | 建议值 | 依据 |
|---|---|---|
| 保留量 | 与前端 `scrollback: 5000` 同量级（按字节设上限，如 2MB） | `TerminalPane.tsx` 的 xterm 配置 |
| 截断时 | 置 `truncated` 标志，连接时**先发 `gap`** | `case "gap"` → `term.truncated` |
| 落盘 | **不落盘** | `// 不进 checkpoint、不落盘` |

### 4.6 多端接入

`pty_changed` 的注释说"一个 PTY 只属于一条标签"，但**未限制单端连接**。
手机端明确不支持（无 `pty_*` 命令、无 `/pty` 端点）。建议：**桌面允许多端**（标签重挂载时
旧 socket 可能尚未关闭），但 `pty_close` 是终态。

### 4.7 安全

- ticket 必须**一次性 + 短 TTL**，且与 `pty_id` 绑定（换 ticket 不能访问别的 PTY）
- WS 端点必须校验 `Origin`（仅允许桌面 webview）
- `pty_create` 的 `cwd` 必须落在 workspace 内（复用既有 `artifactSafeJoin` 一类的越界校验）
- 命令名固定 `pty_create / pty_ticket / pty_close`，全部经既有 `command_registry` 注册

---

## §5 验收测试清单（对照已还原的前端测试）

| # | 前端已有测试 | Go 侧对应验收 |
|---|---|---|
| 1 | `frame-test`：0x00+JSON 往返 | `encodeCtrl/decodeCtrl` 的 Go 单测 |
| 2 | `frame-test`：非 0x00 原样、含中间 NUL | 二进制安全：`[]byte{104,0,105}` 原样写进 PTY |
| 3 | `frame-test`：畸形控制帧当数据、不断连 | 畸形 JSON 不 panic、不关连接 |
| 4 | `frame-test`：256KB 大帧 | 读限 ≥ 1MB，大输出不分片丢失 |
| 5 | `frame-test`：UTF-8 切在字符中间 | 逐字节直通，**禁止**在 Go 侧做 rune 解码 |
| 6 | `paneMachine-test`：13 条合法转移 | 后端事件顺序（hello→reset→历史→实时） |
| 7 | `paneMachine-test`：迟到 timeout 忽略 | 网关超时必须带 ctx 取消，不能误杀 |
| 8 | `writer-test`：批处理与屏障 | 高频率小帧不丢字节、不乱序 |
| 9 | `commandContext-test`：命令追踪 | 前端负责，后端只需保证字节原样 |
| 10 | `termEnsure` 重试契约 | 桥未就绪时 error 匹配 `/bridge\s*(启动中|未运行)/` |

---

## §6 与既有代码的接合点

| 接合点 | 说明 |
|---|---|
| `command_registry.go` | 注册 `pty_create` / `pty_ticket` / `pty_close` 三条命令 |
| `desktop/bridge/main.go` | 会话删除时清理该会话名下所有 PTY（对应 `E-main.go-11` 的同类要求） |
| 事件通道 | `pty_changed` 走既有事件总线，**不进 checkpoint** |
| `desktop/app/src/lib/bridge.ts` | 前端已有 `ptyCreate/ptyTicket/ptyClose` 三个封装，Go 侧字段名必须 snake_case 对上 |
| 标签持久化 | 只持久化 `{id, kind:"term"}`；重启后 PTY 由 `termEnsure` 重建 |

---

## §7 仍需你确认的设计决策（原文档里应该有，但不可恢复）

1. **回放缓冲上限**：字节数还是行数？2MB / 5000 行？
2. **shell 选择**：固定 `$SHELL -l`，还是允许用户在设置里选？
3. **终端数量上限**（`term.limit` 提到"已达上限（{n}）"）：n 是多少？与右栏标签 LRU 是否同一套？
4. **`pty_ticket` TTL**：60s？是否单次使用？
5. **多端接入**：桌面多窗口是否允许同时连同一个 PTY？
