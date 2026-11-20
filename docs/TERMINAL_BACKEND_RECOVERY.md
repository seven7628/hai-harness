# 右栏终端 —— Go 侧（PTY 桥）恢复报告（TERMINAL_BACKEND_RECOVERY）

> 任务：从**编译痕迹 / 索引 / 备份**里尽可能恢复被删的 Go 侧 PTY 实现；能恢复的逐字、恢复不到的标注清楚。
> 结论先行：**函数体源码不可恢复**（无任何送达的 Go 源/对象/二进制残留），但**依赖、协议面、外部契约
> 已全部取到实证**；缺的只是「怎么写的」，不是「要写成什么」。
>
> 标注：`【实证】` 磁盘上直接取到；`【推断】` 由实证+约定推出；`【不可知】` 无证据。

---

## 1. 实证清单（逐条、带来源）

| # | 证据 | 值 | 来源（可直接复核） | 级别 |
|---|---|---|---|---|
| 1 | Go 模块被下载 | `github.com/creack/pty v1.1.24`，目录 mtime **2026-11-19 18:27**（功能开发当天、删除 22:12 之前） | `~/go/pkg/mod/github.com/creack/pty@v1.1.24/`；`stat -f '%Sm'` | 【实证】 |
| 2 | 模块校验和存在 | `creack/pty v1.1.24 h1:bJrF4RR...` | `~/go/pkg/mod/cache/download/sumdb/sum.golang.org/lookup/github.com/creack/pty@v1.1.24` | 【实证】 |
| 3 | GoLand 索引记录该 import | `github.com/creack/pty`、`creack/ptyt` | `~/Library/Caches/JetBrains/GoLand2025.3/index/go.import.path.prefix/...keystream:586`、`go.file.imports/go.file.imports_inputs.values:1450` | 【实证】 |
| 4 | GoLand 索引有 PTY 类型定义痕迹 | `Winsize`、`Winsize(`、`pty.x` | `index/stubs/Stubs.storage.values`（`strings -a \| grep -E 'Winsize\|pty\.'`） | 【实证】（弱：可能来自依赖 stub） |
| 5 | 构建缓存**已被重建**，删除前对象不复存在 | 缓存全部文件最早 mtime = **2026-11-19 23:15**（删除之后） | `find ~/Library/Caches/go-build -type f -exec stat ...` → 最早 23:15 | 【实证】 |
| 6 | 缓存里现有 bridge 对象**不含** PTY | 24MB 的 bridge 包对象 mtime 23:20–23:41，`grep 'pty_create\|pty_ticket\|pty_id'` 命中 = 0 | `~/Library/Caches/go-build/{66,9b,fc,99,16}-d` | 【实证】 |
| 7 | 缓存里有一个 creack/pty 对象（**删除后**编译） | `91/911c28…-d`，103KB，mtime 00:20 | 同上 | 【实证】 |
| 8 | `main.go` 最新副本不含 PTY | `/private/tmp/main.go.bak3`（393,971B，22:05）与现 `desktop/bridge/main.go` 的 61 个 `(*manager)` 方法**完全一致**；import 无 `net/http`/`websocket`/`creack/pty`；`grep -i pty` 仅 5 处误命中 | `/private/tmp/main.go.bak3`；`diff <(grep func…)` | 【实证】 |
| 9 | 所有 `.go.bak` / `patch*.py` 均无 PTY | 19 个 `*.go.bak*` + 80 个 `patch*.py`，`grep -E 'pty_create|pty_ticket|ptyClose|creack/pty'` 命中 = 0 | `/private/tmp/*.bak*`、`*patch*.py` | 【实证】 |
| 10 | 唯一 HTTP/WS 服务器是 mobile | `mobile_server.go` 用 `nhooyr.io/websocket`，路由 `/api/*`、`/m/`，**无 `/pty`** | `desktop/bridge/mobile_server.go:268-290` | 【实证】 |
| 11 | 现成二进制不含 PTY | `desktop/app/dist/hai-bridge`（32MB，23:43 构建）`strings \| grep pty` = 0 | 该文件 | 【实证】 |
| 12 | 前端契约（Go 必须实现的那一侧） | 命令名、JSON tag、WS 路径、控制帧、事件字段——见 §2 | 还原的 `transport/host.ts` 切片、`frame.ts`、`paneMachine.ts`、`useAppStore` 切片 | 【实证】 |

> **为什么 Go 源码没留下**：删除发生在 22:12，而判断 `go build` 是否跑过的判据是缓存 mtime。
> 缓存被整体重建发生在 **23:15**（重克隆仓库后的首次构建），因此**删除前的所有包对象都被覆盖清空**；
> 期间唯一与 pty 有关的对象是 00:20 的 creack/pty 库对象，时间在删除之后，来源应是恢复期的一次 `go get`
> （本次工作也用 `go get` 把该依赖补了回来，见 §5）。

---

## 2. 从实证还原出的 Go 侧 API 面（外部契约，逐字可靠）

以下全部来自**前端还原代码**（前端是 Go 侧的调用方，字段名由 bridge 权威决定，因此可反推 Go 的 json tag）：

### 2.1 命令（NDJSON，经 `manager.dispatch`）

```
pty_create { workspace string, session_id? string, cols string→int, rows string→int }
           → data { pty_id string, cwd string, shell string, pid number, cols number, rows number }
pty_ticket { pty_id string }
           → data { url string, ticket string, expires_in number(秒) }
pty_close  { pty_id string } → data {}
```
- 【实证】命令名：`transport/host.ts` 切片 `ptyCreate/ptyTicket/ptyClose` 调用 `d3.ptyCreate(...)`，
  底层命令字面量 `pty_create/pty_ticket/pty_close`（`store-bundle.cjs` 的 host 段）。
- 【实证】字段名/类型：`ptyInfoOf` 严格校验 `typeof pid/cols/rows === "number"`、`pty_id/cwd/shell === "string"`；
  `pty_ticket` 校验 `url/ticket` 为 string、`expires_in` 为 number。
- 【实证】入参：`pty_create` 调用点传 `{workspace, session_id, cols:80, rows:24}`（`useAppStore.termEnsure`）。

### 2.2 数据面（WebSocket）

- 【实证】`new WebSocket(url + "?ticket=" + encodeURIComponent(ticket))`，`binaryType="arraybuffer"`。
- 【实证】控制帧 = `0x00` + UTF-8 JSON；数据帧 = 首字节非 `0x00`（`frame.ts`，`frame-test.ts` 6 条用例）。
- 【实证】控制帧：C→S `resize{cols,rows}`（前端 100ms 防抖）；S→C `hello`、`reset`、`gap`、`exit{code?}`。
- 【实证】未知 `t` 前端静默忽略（`default: break`）⇒ 后端可新增控制帧。

### 2.3 事件（既有 event 通道，非 WS）

- 【实证】`pty_changed { pty_id, status: "running"|"exited"|"failed", exit_code?, error? }`；
  前端以 `pty_id → tabId` 反查，只更新运行时表，不进 checkpoint。

### 2.4 依赖与运行形态

- 【实证】依赖 `github.com/creack/pty v1.1.24`。
- 【推断】PTY 由 `creack/pty.StartWithSize`（或 `Start` + `Setsize`）拉起，因为前端会发 `resize`，
  且库里 `Winsize` 在索引中出现。
- 【推断】`/pty` 端点挂在本进程自起的 `net/http` + `nhooyr.io/websocket` 服务器上（仓库唯一既有 WS 栈是
  nhooyr，且在 `go.mod` 里；mobile_server 是唯一先例）。
- 【推断】第 9 条证据说明服务器**不是** mobile_server 的既有路由（现版无 `/pty`），故是**新增路由/新服务器**。

---

## 3. 与 `docs/TERMINAL_BRIDGE_SPEC.md` 的对照

| 规格条款 | 本次证据判定 |
|---|---|
| §1.1 通道 = WebSocket、ticket 放 query | **证实**（前端 `new WebSocket(url?ticket=)`） |
| §1.2 帧编码 0x00+JSON / 数据帧原样 / 畸形回落 / 超长不解析 | **证实**（`frame-test.ts` 6 条；Go 侧已对拍） |
| §1.3 控制帧 `hello/reset/gap/exit/resize` | **证实**（`paneMachine-test.ts` 逐条） |
| §1.4 IPC 三命令 + snake_case json tag | **证实**（transport 切片 + `ptyInfoOf` 校验） |
| §1.4 `pty_create` 返回 6 字段 | **证实**（校验字段与类型） |
| §1.5 `pty_changed` 事件 + 三约束 | **证实**（事件处理切片） |
| §2 状态机时序 | **证实**（`paneMachine-test.ts` 16 条） |
| `reset` 必须在**历史回放前** | **推断**（来自注释与状态机顺序，后端发帧时机无直接证据） |
| `gap` 的触发条件（回放被截断） | **推断**（仅前端消费，后端无证据） |
| ticket TTL / 端口 / Origin 校验策略 | **不可知**（原实现无残留；现实现取保守值，见 §5） |
| Go 函数名、结构体、文件划分 | **不可知**（无对象/源码残留） |

---

## 4. 明确「不可知」的部分（不要包装成恢复）

1. **函数体逻辑**：PTY 如何 spawn（shell 选择、env、`Setsid/Setctty`）、退出码采集方式、错误文案原文——
   全部无证据。
2. **`/pty` 服务如何启动/绑定**：端口策略（固定 vs 随机）、是否复用 mobile 端口、生命周期——无证据。
3. **ticket 存储形态与 TTL**：内存 map？一次性？有效期多少秒——无证据（前端只用一次）。
4. **回放（reset/gap）的实现**：是否真有 scrollback 回放、截断阈值——前端有 `gap` 消费，但后端实现无证据。
5. **并发模型**：一个 PTY 是否允许多连接、写者/读者锁策略——无证据。
6. **错误码**：`pty_create` 失败时 `error` 字符串原文（如"bridge 启动中/未运行"是前端重试判据，
   说明后端确会返回这类文案，但**原文措辞**只有正则 `/bridge\s*(启动中|未运行)/` 这一约束）。

---

## 5. 本次「据契约实现」的取舍（[implemented]，非恢复）

文件：`desktop/bridge/pty.go`、`pty_test.go`（顶部均标 `// [implemented] 依据 docs/TERMINAL_BRIDGE_SPEC.md …`）。

- 依赖：按证据加回 `github.com/creack/pty v1.1.24`（`go.mod`/`go.sum`）。
- 传输：自起 `127.0.0.1:0` 随机端口 + `nhooyr.io/websocket`，路由 `/pty`；`ticket` **一次性 + 30s TTL**
  （30s 为【推断】，前端拿到即连）。
- Origin：`InsecureSkipVerify: true`（ticket 已是硬门槛；否则 `file://`/vite 端口与回环端口不同源会被拒）——【推断】。
- 命令：`pty_create/pty_ticket/pty_close`，返回字段与 §2.1 逐字对齐；dispatch 前置 `handlePtyCommand`。
- 数据面：`decodeFrame/encodeCtrl` 与 `frame.ts` 同构；`resize` 走 `pty.Setsize`；数据帧原样写/读。
- 事件：`pty_changed`（`exited` 带 `exit_code`）。
- 收尾：`shutdownAll` 杀全部 PTY + 关服务器（不留孤儿 shell）。
- 未做（超出契约、无证据）：scrollback `reset/gap` 回放、多连接、鉴权 cookie。

**验证**：`go build ./...` 通过；`go test ./desktop/bridge -run TestPty` 通过（含真实 PTY 回显冒烟）。
