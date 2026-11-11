# Demo 源码移植记录（会话区 + 头像）

用户提供的唯一视觉源：

- `hai-super-assistant/docs/demo/index.html`（内嵌 CSS/JS）
- `hai-super-assistant/docs/demo/luna-avatar-preview/mascot-bots.js`
- `hai-super-assistant/docs/demo/luna-avatar-preview/luna-face.js`

## 直接搬过来的部分

| demo 源 | 仓库落点 |
|---|---|
| `index.html` 的 `<style>` | `desktop/app/src/hub/demo.css`（选择器统一加 `.hub-demo` 作用域，声明未改） |
| `.chat-header` / `.messages` / `.messages-inner` / `.msg-row` / `.agent-head` / `.agent-text` / `.user-bubble` / `.owner-avatar` / `.event-card` / `.composer` DOM | `ConversationMessage.tsx`、`MessageBubble.tsx`、`TaskResultCard.tsx`、`TaskArtifacts.tsx`、`GroupView.tsx`、`HubSurface.tsx` |
| `mascot-bots.js` | `desktop/app/src/hub/vendor/mascot-bots.js` 与 `desktop/bridge/mobile_web/mascot-bots.js` |
| `luna-face.js` | `desktop/app/src/hub/vendor/luna-face.js` 与 `desktop/bridge/mobile_web/luna-face.js` |
| `avatar()` / `groupAvatar()` / 配色规则 / 状态表情 | `lunaAvatar.ts`（桌面）与 `mobile_web/app.js` 的 `hubSigil()`（手机） |

`mascot-bots.js` 唯一的改动：末尾在 CommonJS 导出之外始终写 `globalThis.LUNA_BOTS`，保证 Vite/Rollup 打包后 `luna-face.js` 仍能拿到几何表；几何/眼形/动效未改。

## 只替换为真实数据的部分

demo 是静态 `stateConversations`；仓库里保留同样的 DOM/类名，只把数据源换成 Hub store / bridge 事件：

- `msg-row`：`GroupBubble` / `HubBubble` 读 `HubMessage`，流式仍走 `StreamingMessage`。
- `event-card`：任务卡读 `task_result` 快照；handoff/approval 读真实 message kind；不造假按钮。
- `composer`：发送/停止/@ mention/派任务仍走 `sendGroup` / `sendGroupAssign` / `stopGroupConversation`。
- 头像状态：`running / pending / deleted` → `working / needs_attention / offline`，与桌面、手机同一套 `stateResolver`。
- 群叠放：`planStack` 按 demo 的 4 个 + `+N` 口径；桌面/手机同一份纯逻辑。
- owner-only、observer、seq 高亮、日期分隔、滚动跟随、Context/深链/导出仍由原逻辑提供。

## 本次边界

本次只移植**会话主区 + 头像**，即用户截图中与 demo 差异最大的部分。左栏/右栏仍保留现有 Hub 导航结构（智能体/群组/工作区/详情），未整体替换成 demo 的 demo 侧栏与 Computer 面板；如果要求整页 1:1，需要另开任务搬 `sidebar` / `right-panel` 并重新接真实数据。

## 验收

- `npx tsc --noEmit`
- `npm run test:p4`
- `npm run test:css`
- `npm run test:p4:go`
- `npm run build`
- `node scripts/shoot-hub-conv.mjs`（1440/1280/640/390，ink/paper，群/DM）
