# 手机 Web 复用桌面 UI（P8）

> 背景：手机端 `desktop/bridge/mobile_web` 是独立无构建 PWA（app.js 3657 行 + hub_model.js 957 行 + tokens.css 副本），
> 已与桌面 runtime V2 / Hub 组件漂移。用户要求：**不再单独实现一套，复用桌面现有能力，只做手机布局**。
> 审计结论见本文 §1；进度只认 [`PROGRESS.html`](./PROGRESS.html)。

## 0. 决策（主 Session 拍板，可被用户否决）

1. **复用方式**：在现有 `desktop/app` Vite 里加 mobile 入口，复用 `useHubStore` + Hub 组件；不新建独立工程。
2. **会话页**：复用 `useAppStore` 的只读切片（blocks/turns/approvals/questions/loadOlder），不另写精简 store；`Narrative` 通过 host seam 接。
3. **Hub 皮肤**：手机 Hub 收敛到桌面 `ink/paper`（通过移动 CSS 覆盖 `.hub-demo` 调色板）；桌面 Hub 视觉本轮不动。
4. **退役节奏**：P8-01/02 新页与旧页并存（路径/环境变量分流）；P8-03 通过同一批 e2e 后删除旧 `app.js`/`hub_model.js`。
5. **主题**：手机固定 `data-skin='runtime'`，主题 `ink/paper/system` 与桌面同 key（`hai.theme`）。

## 1. 审计结论（2026-11-13）

- `useHubStore` 可直接复用，零 `useAppStore` 值依赖；Hub 展示组件（RoundGroup/TaskResultCard/ConversationBar/MentionPicker/ConversationMessage/AgentSigil/AgentStack）是纯 props。
- `GroupView`/`HubSurface` 仅 2 处 `useAppStore`（rightExpanded/setRightExpanded）+ 少量 showToast/openProjectSession 需要 host seam。
- `Narrative`/`NarrativeBlocks`/`TaskCard` 有 20+ 个 `useAppStore` 选择器，但多数 runtime 组件只有 `import type`；会话页可复用，代价高于 Hub。
- 主题 token 是纯 DOM 观察者（theme/useTheme/useSkin）；`tokens.css` 是 `index.css` 的逐字副本，已靠 `tokens_sync_test.go` 守。
- 四个真正阻塞：
  1. `getTransport()` 浏览器静默走 mock → 移动入口需分流到 `mobileTransport`。
  2. 手机 WS 不投 `command_response` → 移动 transport 必须在 HTTP `/api/cmd` 应答后合成事件。
  3. `Transport` 60+ 方法 → 移动实现只做核心方法，其余降级 `unsupported`。
  4. `i18n.ts` 绑 `useAppStore` → P8-01b 已抽 `host/lang.tsx`（`useT/useLang/t` 只读 host lang），手机包不再拉桌面 store。
- 第五个边界（P8-01b 实测）：`components/markdown.tsx` / `lib/localAsset` 也静态依赖 `useAppStore`/`getTransport`；消息一渲染就把整个桌面 store + mock 拖进手机包。已改走 `HostWorkspaceContext` + `getHostTransport()`。
- CSS：不要 import 整个 `index.css`；mobile 入口只引 tokens + `hub.css` + `runtime-skin.css`，移动布局单独 `mobile.css`。

## 2. 分期

| ID | 内容 | 依赖 | 验收 |
|---|---|---|---|
| P8-01 | Hub 竖切：mobile 入口 + mobile transport + host seam + Hub 列表/群/DM 复用 + 移动布局 CSS + 构建产物 | — | 390/430：列表→群/DM→发消息/流式/任务卡/Needs；桌面 e2e 不回归；新页可通过环境变量托管 |
| P8-02 | 会话页复用：`Narrative` host seam + `Composer` 移动变体 + 审批/提问底部停靠 | P8-01 | 390/430：打开会话→流式→工具折叠→审批/提问→子智能体卡；与桌面同一份块渲染 |
| P8-03 | 主题/布局收口 + 旧页退役：Hub 收敛 ink/paper、`visualViewport`、safe-area、测试改写、删旧 app.js/hub_model.js | P8-02 | 三主题 390/430/640；`test:mobile-*`/`check-mobile-390` 全绿；`mobile_web` 只剩构建产物/字体 |

## 3. P8-01 具体交付（本期）

新增：

```
desktop/app/mobile.html
desktop/app/src/mobile/main.tsx
desktop/app/src/mobile/MobileApp.tsx
desktop/app/src/mobile/transport/mobile.ts
desktop/app/src/mobile/screens/HubListScreen.tsx
desktop/app/src/mobile/screens/ConversationScreen.tsx
desktop/app/src/mobile/screens/InspectorDrawer.tsx
desktop/app/src/mobile/mobile.css
desktop/app/src/host/panels.ts
desktop/app/src/host/lang.tsx
desktop/app/src/transport/host.ts
desktop/app/scripts/build-mobile.mjs（或 npm script）
```

改动：

- `transport/index.ts`：`window.__HAI_MOBILE__` → `mobileTransport()`。
- `transport/types.ts`：如需要，给 mobile 的 `send/onEvent` 最小接口（不要改桌面语义）。
- `hub/GroupView.tsx`、`hub/HubSurface.tsx`、`hub/HubInspector.tsx`、`hub/HubInlineNeeds.tsx`、`hub/HubAttachments.tsx`、`hub/MessageBubble.tsx`：`useAppStore` 的 6 处改为 `host/panels.ts`（桌面实现不变）。
- `vite.config.ts`：多入口 `main` + `mobile`；构建输出到 `dist-mobile` 或 `desktop/bridge/mobile_web_next`。
- `mobile_server.go`：P8-01 可先通过 `HAI_MOBILE_WEB_DIR` 指向新产物验证；不改 `/m/` 默认 embed，避免旧页回归。
- `package.json`：`mobile:build` / `mobile:dev`。

手机 transport 要求：

- `send({id,type,payload})` → `POST /api/cmd {cmd:type,payload}`，带 `Authorization: Bearer <token>`。
- 应答后**必须**合成 `command_response` 事件 `{event_type:'command_response', id, ok, data, error}` 再分发给 `onEvent` 订阅者；否则 `hubCommand` 20s 超时。
- `onEvent` 多订阅；打开 chat/hub 两条 WS（`/api/events?scope=chat|hub&token=...`），断线重连；同一帧去重。
- 未实现方法返回 `{ok:false,error:'unsupported'}` 或合理默认；不能抛未捕获异常。
- 配对/token 读取沿用 `localStorage` 的 `hai.mobile.token`（或现有 key），P8-01 可先复用旧页配对屏或做一个最小配对屏。

Hub 屏幕要求：

- 列表：群组/私聊/Needs 入口，复用 `useHubStore` 的 `groups/agents` 与 `AgentSigil/AgentStack`，点击设置 `activeGroupId`/`activeId`。
- 群：渲染 `GroupView`；DM：渲染 `HubSurface`；外层移动 header（返回、标题、更多/主题）。
- 布局：单列、`100dvh`、safe-area、底部 composer 不被遮挡、所有可点 ≥44px、无横向溢出。
- 主题：`applyTheme` + `data-skin='runtime'`；移动 CSS 覆盖 `.hub-demo` 的色板到 ink/paper（先保证深色一致，浅色跟随）。

### P8-01b 落地记录（2026-11-13，`926c94d` / merge `ab5a01f`）

- 新增 `HostCapabilities`：桌面全开；手机 `groupAdmin/agentEditor/artifactPreview/promptPreview = false`，allowlist 之外的入口不渲染、也不发请求（不留会 403 的按钮）。
- 列表 = `<HubSidebar variant="mobile" />`；会话 = `<HubSurface />`（群内部 `GroupView`）；详情 = `<HubInspector />` 装进右侧抽屉。
- 三个 seam：`transport/host.ts`（Hub store/命令/附件）、`host/lang.tsx`（i18n）、`HostWorkspaceContext` + `getHostTransport()`（markdown/localAsset）。
- 手机包边界：`grep -c "useAppStore|mock/proj-auth|electronTransport|proj-auth" dist-mobile/assets/*.js` 全 0；首屏 218KB，会话 chunk 按需加载。

### P8-02 落地记录（2026-11-13，`a91b35c` / merge `ab1a380`）

- 会话列表数据来自 `workspace_list` + `list`（不读桌面 localStorage 项目树）；打开会话走 `setActiveSession` + `connectChat`，事件由 store 的 `wire()` 消费。
- 会话页直接渲染 `<CenterPanel variant="mobile" />`：`Narrative` / `ApprovalPanel` / `AskUserQuestionPanel` / `Composer` 原样复用；只隐藏 `RecentBar` 与手机不支持的模型/模式/Effort/附件入口。
- `ask_batch` 加入手机 allowlist（与 `ask` 同一能力：用户消息进会话 inbox），发送走桌面 Composer 的暂存队列整队推送。
- 首屏 220KB；点「会话」额外 5.8KB；会话页 chunk 649KB 按需加载，仍含桌面 store/mock（复用代价，首屏边界保持 0）。
- 已知既有缺陷：`Narrative` 一轮内「交卷边界后又来工具」时折叠键会弹回；桌面/手机同一份代码，建议单独 follow-up。

## 4. 测试

- Node：mobile transport 单测（HTTP 应答 → command_response、WS 事件分发、401/断线）；`test:host-seams` 锁 transport/lang/markdown 三个 seam 的桌面等价与手机降级。
- 浏览器：390×844/430×932 打开 mobile 入口（mock 或测试 bridge），列表→群/DM，断言无横向溢出、44px、composer 可见；`check:mobile-hub` 已覆盖 38 条。
- 桌面回归：`npx tsc --noEmit`、`test:css`、`test:hub-conv`、`test:hub-p3b`、`test:right-tabs`、`check-hub-*` 关键脚本。
- Go：`go test ./desktop/bridge -run Mobile`（若改 embed/白名单）。
- 不占 5173/5180；用 52xx。

## 5. 子智能体契约

1. 只在自己的 worktree 改；不碰主工作树、不 push、不改本文件/看板。
2. 每个任务独立 commit（`feat(hub-p8-01): ...`）；不 amend。
3. 报告改动、测试命令/exit code、反证、`git status --short`、残余风险。
4. 若发现需要改桌面共享文件，必须跑桌面 Hub e2e 并说明行为零变化。
