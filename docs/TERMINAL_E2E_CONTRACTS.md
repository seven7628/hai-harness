# 右栏终端 —— E2E/单元契约清单（TERMINAL_E2E_CONTRACTS）

> **来源**：`/private/tmp/term-{pane,format,frame,context,writer}.test.cjs`（删除前 21:10–22:05 落盘，
> 是当时**真实跑过**的测试打包产物；每个文件内含被测模块源码 + `node:test` 用例）。
> 这是比文档更可靠的事实来源：用例名与断言逐字取自产物，未改写语义。
>
> 标注：`[逐字]` = 用例标题/断言来自产物；`[解读]` = 我对其行为的归纳；`[后端要求]` = 反推出的 Go/WS 侧义务。

---

## 1. `frame-test.ts` —— 帧编解码（控制帧 vs 数据帧）

模块：`src/components/terminal/frame.ts`。核心：`encodeCtrl`、`decodeFrame`。
后端对应：`desktop/bridge/pty.go` 的 `encodeCtrl`/`decodeFrame`（已按此实现并加 Go 侧对拍测试）。

| # | 用例名 [逐字] | 验证什么 [解读] | 关键断言 | 后端要求 |
|---|---|---|---|---|
| 1 | 控制帧：0x00 开头 + JSON，往返一致 | `encodeCtrl({t:"resize",cols:120,rows:40})` 首字节为 0 且 JSON 往返 | `frame[0]===0`；`data===undefined`；`ctrl` 深等于原对象 | 服务端同一编码 |
| 2 | 数据帧：非 0x00 开头原样返回（含中间的 NUL 也算数据） | `[104,0,105]` 不被误判 | `ctrl===undefined`；`data` 与原数组全等 | PTY 原始字节直通，**中间 NUL 不能当分隔** |
| 3 | 畸形控制帧按数据帧处理（不判死连接） | `[0]` / `[0,123]` / `[0,'x','y']` / `'\0{"t":""}'` 全部回落 | 每种 `ctrl===undefined` 且 `data===原值` | **解析失败不得关连接**，必须回落成数据 |
| 4 | 超长输出不被 JSON 解析（只认首字节） | 256KB `'a'` 全量透传 | `data.length===256<<10` | 大帧不预解析、不设 64KB 上限 |
| 5 | UTF-8 多字节字符原样透传（终端输出会切在字符中间） | `"中文日志"` 字节不变 | `decode(data)==="中文日志"` | 按字节转发，不在服务端做 UTF-8 边界处理 |
| 6 | ArrayBuffer 与 Uint8Array 两种入参等价 | 同一控制帧两种入参解码一致 | `deepEqual(fromBuf.ctrl, fromU8.ctrl)` | WS `binaryType=arraybuffer`，两种都要接受 |

**Go 侧对拍**：`desktop/bridge/pty_test.go::TestPtyCtrlFrameRoundTrip / TestPtyDataFramePassthroughKeepsMiddleNUL / TestPtyMalformedCtrlFallsBackToData / TestPtyOversizedFrameNotJSONParsed / TestPtyUTF8Passthrough`（已通过）。

---

## 2. `formatSelection-test.ts` —— 选区 → 引用文本

模块：`src/components/terminal/formatSelection.ts`。核心：`formatSelection(text)`，上限 200 行 / 20000 字节。

| # | 用例名 [逐字] | 验证什么 [解读] | 关键断言 |
|---|---|---|---|
| 1 | 单行、不含反引号 → 一对反引号包起来 | 行内代码 | `"git status"` → `` `git status` `` |
| 2 | 单行但含反引号 → 原文返回，不包 | 避免破坏内联代码 | `` `echo \`date\`` `` 原样 |
| 3 | 单行：前后的空白与行尾换行被 trim，结果不含首尾换行 | 清洗 | `"  ls -la  \n"` → `` `ls -la` `` |
| 4 | 多行 → 三反引号围栏，围栏各自单独成行 | 代码块 | `"a\nb"` → ```` ```\na\nb\n``` ```` |
| 5 | 多行正文保留首行缩进（缩进就是代码的语义） | 不破坏缩进 | `"  a\n    b\n"` 正文原样 |
| 6 | 多行且本身含三反引号 → 围栏改用三波浪线 | 嵌套围栏 | 用 `~~~` |
| 7 | 空串或全空白 → 空串 | 空选区不产出 | `""`/`"   "`/`"\n\n \t\n"` → `""` |
| 8 | 恰好 200 行不截断 | 边界含 | 无「已截断」，行数 = 202（围栏 2 + 正文 200） |
| 9 | 超过 200 行 → 截到 200 行，并注明原文行数 | 截断标注 | 末行 `… （已截断，共 201 行）` |
| 10 | 超过 20000 字节 → 截断（超长单行从中间断） | 字节上限 | 正文 20000 字节 + 标注行 |
| 11 | 恰好 20000 字节不截断（边界是「超过」） | 边界不含 | 无「已截断」 |
| 12 | 中文按 UTF-8 字节截断，不留半个字（U+FFFD） | 字节安全 | `"中"×10000` → 正好 6666 个汉字（20000/3） |
| 13 | 结果形状：单行结果首尾没有多余换行，多行结果首尾各恰好一个围栏行 | 形状契约 | 首行是围栏、尾行同种围栏、围栏间至少 1 行 |

后端义务：无（纯前端格式化）。但「引用到对话」最终走 `emitUI("composer_quote")`。

---

## 3. `paneMachine-test.ts` —— 终端连接状态机

模块：`src/components/terminal/paneMachine.ts`。核心：`next(state, ev) → {state, fx}`（纯函数）。
状态：`idle → creating → connecting → replaying → live → exited`，任意态可 `error→failed` / `close→idle`。
副作用 `fx`：`create-pty` / `issue-ticket` / `open-socket` / `close-socket` / `close-pty` / `reset-term` / `show` / `toast`。

| # | 用例名 [逐字] | 验证什么 [解读] | 期望 |
|---|---|---|---|
| 1 | initialPaneState 是 idle：挂载即可渲染，不会凭空建 PTY | 初始态 | `idle`，无 fx |
| 2 | idle + open → creating，并请求建 PTY | 打开 | fx=`create-pty` |
| 3 | creating + created → connecting（带 ptyId），并请求票据 | 建好 | 带 `ptyId`，fx=`issue-ticket` |
| 4 | creating + timeout(create) → failed，只 toast（还没有 socket 可关） | 建终端超时 | error=「创建终端超时」，fx 仅 `toast` |
| 5 | connecting + ticket → connecting 带上票据，并开 socket | 取票 | 带 `ticket`，fx=`open-socket` |
| 6 | connecting + ws-open / hello → 状态与 fx 都不动 | 握手无副作用 | 原样返回，fx=[] |
| 7 | connecting + reset → replaying，并重置终端 | 回放前清屏 | fx=`reset-term` |
| 8 | replaying + output → live，且 show 只出现一次 | 首帧输出转 live | fx=`show`（仅此一次） |
| 9 | connecting + timeout(connect) → failed，关 socket 并 toast | 连接超时 | error=「连接终端超时」，fx=`close-socket`+`toast` |
| 10 | 任意状态 + error → failed，关 socket 并把原文递给 toast | 错误透传 | `error` 原文进 toast |
| 11 | live + exit → exited（带退出码），并关 socket | 进程退出 | 带 `code`，fx=`close-socket` |
| 12 | close：有 ptyId 时连 PTY 一起收 | 关闭 | fx=`close-socket`+`close-pty` |
| 13 | close：没有 ptyId 时只关 socket | 关闭 | fx 仅 `close-socket` |
| 14 | 未列出的组合原样返回，fx 为空 | 非法转移忽略 | state 引用不变，fx=[] |
| 15 | 超时落在错误的阶段时不算数（迟到的 timeout 不该毁掉一个已连上的终端） | 阶段校验 | 非对应阶段忽略 |
| 16 | 纯函数：任何转移都不改入参 | 不可变性 | 入参 state 不被修改 |

**后端要求**：控制帧 `hello`（无副作用）、`reset`（回放前）、`gap`（截断提示）、`exit{code}`；建连超时由前端计时，后端只需在 ticket 有效期内尽快 accept。

---

## 4. `writer-test.ts` —— 输出写入调度（不变式 1–6）

模块：`src/components/terminal/writer.ts`。核心：`terminalWriter(write)`，把相邻输出块合并成一次 `write`，保证「同时最多一个 in-flight」。

| # | 用例名 [逐字] | 不变式 | 关键断言 |
|---|---|---|---|
| 1 | 不变式 1：done 之前不写下一块，同时最多一个 in-flight | 串行 | 每次 settle 才推进一格；`peak===1` |
| 2 | 不变式 2：紧邻的输出块合并成一次 write | 合并 | push a,b,c → `["a","bc"]` |
| 3 | 不变式 2：步骤是合并的分界，跨不过去 | 步骤隔离 | a,b,step,c,d → `["a","b","cd"]` |
| 4 | 不变式 3：输出 A → 步骤 → 输出 B，调用顺序与合并情况 | 顺序 | 步骤等前面写完，且不被后面输出抢占 |
| 5 | 不变式 3：空闲时 push 的步骤立刻执行，输出跟在它后面 | 空闲步骤 | `["step","write:a"]` |
| 6 | 不变式 4：clear 丢弃排队内容，后续 push 全被忽略 | 丢弃 | clear 后一个字都不再写 |
| 7 | 不变式 4：clear 之后迟到的 done 不会执行队列里的步骤 | 丢弃 | 队列步骤 `ran===0` |
| 8 | 不变式 5：空 Uint8Array 不产生 write | 空块跳过 | 不占一格 |
| 9 | 不变式 6：步骤抛异常只丢它自己，队列继续 | 容错 | 不中断后续输出 |
| 10 | 同一个 done 被回调两次不会让两个 write 同时在飞 | 幂等 | 第二次 done 不推进；`peak===1` |

**后端要求**：无。但「先 `reset` 再回放、首帧再转 live」的顺序依赖后端把 `reset` 发在历史回放**之前**。

---

## 5. `commandContext-test.ts` —— 命令行追踪与引用头

模块：`src/components/terminal/commandContext.ts`。核心：`trackCommandLine`、`withCommandHeader`、`describeCwd`、`findCommandLine`。

| # | 用例名 [逐字] | 验证什么 [解读] |
|---|---|---|
| 1 | 逐字符输入 + 回车 → 提交去掉首尾空白的命令 | 正常提交 |
| 2 | 退格（DEL 0x7f）只删一个字符 | 退格 |
| 3 | 空回车不提交（提示符上直接回车不是命令） | 空行 |
| 4 | ⌃U 清行后重新输入 → 提交的是新命令 | 清行 |
| 5 | Tab 补全 ⇒ 这一行 dirty，回车不提交命令 | Tab 污染→不提交 |
| 6 | 方向键（ESC 序列）同样 dirty | 历史浏览→不提交 |
| 7 | dirty 之后按退格不会「洗白」：仍然不提交 | dirty 粘滞 |
| 8 | ⌃C 中断后再输入的命令是干净的 | 中断重置 |
| 9 | 多行粘贴（含 \r）按多次提交处理 | 粘贴 |
| 10 | 正文里没有这条命令 → 补一行 `$ cmd` 并空一行 | 引用加头 |
| 11 | 正文已经含这条命令（引用整屏）→ 不加头 | 去重 |
| 12 | 没有命令 / 空正文 → 原样返回 | 退化 |
| 13 | 命令前后空白被 trim（终端里常带尾部空格） | 清洗 |
| 14 | 从后往前找：同名命令出现两次时命中最后一条 | findCommandLine 反向 |
| 15 | 提示符与命令同一行也能命中 | 同行命中 |
| 16 | 没找到 / 没命令 → -1（调用方退回整屏） | 未命中 |
| 17 | 目录为空 / 未给 → 与原来一致（不写括号） | 头部目录可选 |
| 18 | 正文已含命令 → 即使给了目录也不重复 | 去重 |
| 19 | describeCwd：与工作区相同（inside 且 rel 空）→ 空串（不制造噪音） | cwd 描述 |
| 20 | describeCwd：工作区之内 → 相对路径 | cwd 描述 |
| 21 | describeCwd：工作区之外 → 绝对路径（相对路径会误导） | cwd 描述 |
| 22 | describeCwd：拿不到 / undefined → 空串 | cwd 描述 |

后端要求：`pty_create` 回 `cwd`（describeCwd 的输入），且 `exit_code` 供 `term.exit` 文案。

---

## 附：相邻但非 `term-*` 的回归面（供集成时一并跑）

- `builtin-surfaces.test.cjs`：断言内置 surface 注册表包含 `term`（`registerSurface(termSurface)` 的顺序/形状）。
- `surface-registry.test.cjs`：surface 注册/查找契约。
- `rightpanel-select.test.cjs`：右栏标签选择逻辑（`kind` 取值含 `term` 时仍能选中/切走）。
- `view-budget.test.cjs`：视图预算（终端 `weight:"heavy"` 的回收门槛）。
- `layout.test.cjs` / `input-history.test.cjs`：布局与输入历史（与本功能相邻、共享 composer）。

> 这 5 个 `.cjs` 在 `/private/tmp` 里有产物，但对应源码模块属其它子系统；本清单只列「与本功能共享断言面」的部分，未逐字展开。
