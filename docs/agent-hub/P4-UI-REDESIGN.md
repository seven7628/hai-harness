# Hub UI/UX 重设计（2026-11-09）

用户反馈（必须逐条解决）：

1. 当前整体设计差，智能体流程过度复杂。
2. Provider / Model 不是下拉框；必须继承设置里的 Providers / Models。Effort 必须归一化为
   `None / Minimal / Low / Medium / High / XHigh / Max`（外加“跟随模型默认”）。
3. 进入智能体后无法明显回到工作区模式。
4. 不要独立全屏 Hub 页面；智能体 + 群组入口放在左侧栏“工作区管理”的**上方**。
5. 参考图 1–3：左侧会话/智能体列表，中央对话，右侧智能体详情；图 1 是列表型左栏，
   图 2/3 是头像 rail + 中央聊天 + 右侧详情卡。

## 目标信息架构

同一个 App shell：

```
┌────────────────────────┬──────────────────────────────┬─────────────────────┐
│ 左栏                    │ 中栏                          │ 右栏                 │
│ 智能体 / 群组  （新增）  │ 当前智能体或群组对话           │ 智能体/群组详情       │
│ 搜索 / 列表 / 新建       │ 标题 / 消息 / composer         │ 概览 / 任务 / Needs   │
│ ─────────────────────  │                              │ Context / 会话       │
│ 工作区 / 会话（现有）    │                              │                     │
│ Skills / 定时 / 设置     │                              │                     │
└────────────────────────┴──────────────────────────────┴─────────────────────┘
```

- 左栏永远可见；智能体/群组列表是“工作区”上方的独立 section，不是第二层页面。
- 点智能体/群组：Hub 进入中央栏，右栏显示 Hub 详情；左栏 workspace 区仍在。
- 点任意 workspace/会话或“工作区”按钮：立即退出 Hub，中央/右栏恢复工作区内容。
- Hub 打开时 workspace 中央内容**只隐藏不卸载**（草稿/附件/本地 state 必须保留）。
- 不再渲染全屏 `HubSurface` 覆盖层；删除 `.main-surface.hidden` 的整页切换。

## 左栏（新增 Hub section）

顺序：品牌 → Persona → **智能体/群组 section** → 工作区标题/新建/列表 → 底部入口。

Hub section：

- Header 一行：`智能体` 标题；右侧小按钮 `工作区`（退出 Hub）、`＋`（新建智能体）。
- 搜索框：沿用完整历史搜索（输入 debounce 250ms；scope chips 全部/群聊/私聊），
  搜索结果在 section 内滚动；点命中打开会话。
- Tabs：`智能体` / `群组`，带数量；当前项 underline 或 segmented。
- 列表行：
  - 44px Luna 头像（demo 的 12 色职责色，不读 profile.accent）；运行中/待审批状态表情；未读 badge 用 accent。
  - 名称 13px；第二行 preview 11.5px 单行省略；右侧相对时间。
  - 群组行用 2–3 个重叠成员圆点做“成员堆叠”，预览优先任务结论/最后消息。
  - active 行用轻微背景 + 左侧 2px accent 指示，不做过重边框。
- 空态：`还没有智能体` / `还没有群组` + 新建动作。
- Section 最大高度约 38vh，内部滚动；不能把工作区列表挤没。
- 左栏折叠成 44px 图标条时：在顶部加一个 Bot/智能体入口（点击展开左栏并打开 Hub），
  不要求显示完整列表。

## 中栏（对话）

- 使用现有 Hub DM / GroupView 逻辑，抽成可嵌入中央栏的 `HubSurface embedded`（或不含 rail
  的 `HubConversation`），不再自带左侧 rail。
- Header：头像 + 名称 + role/provider/model 一行 muted；右侧 Chat/Run、Context、链接、编辑、
  更多（任务/Needs 可移到右栏 tabs）。
- 消息区：保留现有滚动/深链/highlight/流式：
  - DM 智能体消息左侧、无重泡泡；时间/名字低对比；
  - 人类消息右侧浅色圆角气泡；
  - 群聊中**每个智能体是独立消息卡**：头像 + 名字 + 职责 + 时间 + 自己的气泡，头像用作者的职责色，名字沿用 demo 的 --text；
    不同作者绝不共用一个外框；accent 不参与头像；
  - 同轮消息只收紧间距（“紧凑”），不画“同轮”表头或外框（“分开”恢复常规间距）；
  - 路由/交接/系统动作是气泡外的低对比动作行；触发上下文收进消息头的「N 人触发」，
    不单独占一行（触发者名单与 seq 在 title 里，点击跳到触发消息）；
  - 任务卡沿用卡面，但执行者身份置顶；产物用统一的 Artifacts 行（文件徽标 + 名称 + 路径 +
    大小/行数），不再退化成纯文本列表。
- Composer：底部居中的圆角 pill；左侧 `+` 附件/引用入口（若现有 Hub 没有附件能力，
  只保留一个明确的占位/上下文按钮，不伪造文件上传）；右侧发送按钮；placeholder
  `Message {name}` 或中文等价。
- 中央栏无需再显示“工作区”大按钮；但 header 最左侧/左栏 section 必须有明显返回工作区路径。

## 右栏（Hub 详情）

- Hub 打开时右栏默认显示 `HubDetail`（不是空白），参考图 2/3：
  - 顶部：大头像/emoji、名称、role、provider · model · effort badge、运行状态。
  - 群组：成员列表 + policy/成员数。
  - `正在做` / `任务`：真实任务列表（可复用 HubInspector 的 TaskBoard）。
  - `Needs you`、`Context`、`会话` tabs 保留。
  - 底部 `编辑智能体` 按钮。
- 右栏可收起；收起后中央栏变宽，类似现有右栏快捷键。
- 不要把现有 `HubInspector` 的 inspector 开关当成“默认详情”；Hub 打开时应默认 overview。

## Agent 编辑器（重点收敛）

- 保留现有 AgentEditor 能力，但默认只展示“身份 + 模型”两块，其余进入“高级设置”。
- Provider：`<select>`，选项来自 `useAppStore().settings.provider`（排除 `active`/
  `deleted`/非对象项）。显示 `cfg.label || providerId`；空 = 请先到设置配置。
- Model：`<select>`，选项来自该 provider 的 `models` keys ∪ `fetchedModels[provider]`；
  按名称排序；若当前 agent.model 不在列表，保留一个 disabled/当前项，避免静默丢失。
- Provider 切换：若当前 model 不在新 provider 列表，自动选第一个可用 model。
- Effort：`<select>`，选项：
  - 空值：`跟随模型默认`
  - `None / Minimal / Low / Medium / High / XHigh / Max`（value 小写，后端既有值）
- 高级设置（折叠）：压缩阈值、日预算、单任务上限、记忆开关；不要默认铺满第一屏。
- 保持“基本/人格/工具与权限/记忆/版本”页签或改为“基本/人格/高级”，但不能丢现有能力。
- 删除/保存行为不变；保存后刷新列表并选中。

## 视觉令牌

- 只用现有主题令牌（`--paper/--paper-2/--surface/--ink/--text-3/--border/--accent-solid/...`），
  不新造一组硬编码颜色；ink/paper/system 三主题都要可读。
- 行 hover/active 用 `color-mix(in srgb, var(--ink) 5/9%, transparent)`，沿用现有风格。
- 圆角使用现有 `--radius`；列表行 8px，详情卡 10–12px，composer pill 999px。
- 字号：列表名 13px、preview 11.5px、时间 10.5–11px；消息正文保持现有 Markdown 字号。

## 实现约束

- 不要改 Hub 协议、store 数据语义、搜索/归档后端；这是前端布局/表单重构。
- 不改 mobile_web 手机端（本轮只桌面 shell）。
- 保留现有 `hub://` 深链、搜索、群/DM 消息、任务卡、Run 视图、Context 面板。
- 不能卸载 workspace 中央组件；只能 CSS 隐藏。
- 所有新增交互有 aria-label/键盘可达；不引入新依赖。
- 变更后必须 `npx tsc --noEmit`、`npm run build`，并跑 P4/Hub 相关纯逻辑测试。
- 使用 5176 预览验证（5173/5174/5175 不碰），截图检查左栏/中栏/右栏与返回工作区。

## 验收清单

1. 左栏“智能体/群组”section 位于工作区管理上方；可滚动、可搜索、可切 tabs。
2. 点智能体/群组 → 中央对话 + 右详情；点工作区/会话/“工作区” → 立即回到工作区。
3. Hub 不再全屏覆盖；workspace 草稿不丢。
4. 折叠左栏仍有智能体入口。
5. Agent 编辑器 Provider/Model 是真下拉，选项来自设置；Effort 8 项齐全。
6. 视觉与参考图同层级：左列表、中央对话、右详情，不做重型卡片/多余边框。
7. tsc/build/Hub 测试全绿，5176 真实浏览器截图通过。
