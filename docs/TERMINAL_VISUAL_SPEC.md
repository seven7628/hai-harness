# 右栏终端 —— 视觉规格（TERMINAL_VISUAL_SPEC）

> **来源口径（硬要求）**：
> - `【图】` = 从 `/private/tmp/term-*.png` 截图里**直接看到**的（可信）。
> - `【源码】` = 从还原的 `TerminalPane.tsx` / `TerminalTab.tsx` / `termSurface.tsx` 读到的结构（可信，
>   但那是**行为/结构**，不是像素；颜色/间距仍需图或推断）。
> - `【推断】` = 图和源码都没有、由我按同类组件与设计令牌推断（**不可当事实**）。
> - `【缺失】** = 找不到任何证据的部分。
>
> 截图落盘时间 19:43–22:05（删除前），均为深色主题、macOS、1440×896 逻辑分辨率（2880×1792 @2x）。

---

## 0. 面板在整页里的位置【图】

- 三栏：左栏（智能体/工作区）、中栏（对话/composer）、**右栏（标签式面板）**。
- 右栏宽度约占整页 **44%**（1280 视口下右栏 ≈ 560px；见 term-shot/term-chip）。
- 右栏顶部是标签条：`进程`（inactive）与 `终端`（active），`终端`标签左侧有终端图标、右侧有 `×`；
  标签条最右有三个图标：`＋`（新建标签）、`⤢`（最大化/全屏）、`▥`（分栏/停靠）。
- 底部整宽状态条（idle · run · 回合 · in/out · total · 缓存 · ctx · cost · 分支）。

---

## 1. 面板本体（`.term-pane` / `.term-host`）

- 【图】终端区域**满幅**：没有右栏其他面板那种 10px 内边距的"卡片感"，也没有第二层滚动条；
  文字从距左约 10px、距顶约 8px 开始（term-shot：提示符起点）。
- 【图】底色比右栏面板底**略深**，接近代码块底；文字为暖白（非纯白）。截图里未见选中态以外的强调色。
- 【图】等宽字体，字号偏小（约 12px），行高紧凑（powerline 图标上下无缝 → line-height 1）。
- 【源码】`termSurface.fullBleed = true`、`weight:"heavy"`、`scope:"session"`、`singleton:true`、`order:15`
  （排在 git=20 之前）。这解释了"满幅、无内边距"。
- 【源码】字体链：`'MesloLGL Nerd Font Mono','Meslo LG L','JetBrains Mono','IBM Plex Mono',monospace`，
  `fontSize:12`、`lineHeight:1`、`scrollback:5000`、`cursorBlink:true`。
- 【源码】主题色读 CSS 变量：背景 `--code-block-bg/--paper-3/--paper`，前景 `--ink`，
  光标 `--cinnabar/--cinnabar-2`，选区 `--accent-weak`（兜底 `rgba(217,122,102,0.28)`，偏朱砂）。
- 【推断】`.term-host` 内边距约 `8px 10px`（截图目测）。

---

## 2. 状态覆盖层（`.term-overlay` 及 `.loading/.exited/.failed`）

- 【源码】四种呈现由 `TerminalPane` 的 `overlay` 计算：
  - `connecting/creating` → `loading`，文案 `term.starting`（"启动中…"）；
  - `replaying` → `loading`，文案 `term.replaying`（"恢复中…"）；
  - `exited` → `exited`，文案 `term.exit` 带退出码，**带"重新打开"按钮**；
  - `failed` → `failed`，文案 `term.failed` 带 `{error}` 原文，**带"重新打开"按钮**；
  - `live` → 无覆盖层。
- 【源码】覆盖层是 `role="status"`、居中；`failed/exited` 多一个 `.btn.btn-xs` 的"重新打开"。
- 【图】**未捕捉到**任何覆盖层截图（6 张都是 live 态）。因此**覆盖层的具体样式（背景透明度、
  模糊、按钮配色）属【缺失】**，`terminal.css` 里的实现是【推断】+ 设计令牌推出来的。
- 【推断】覆盖层半透明遮住已有输出 + 居中文字，`loading` 用弱化灰、`failed` 用错误红（`--crimson`）、
  `exited` 用中性灰（`--ink-2`）。

---

## 3. 选区引用按钮（`.term-insert`）

- 【图】`term-insert-btn.png`：当终端里有**选区**时，右栏右下角浮出一枚圆角按钮，文案"引用到对话" +
  快捷键提示（`⌘⇧↵` 的组合键胶囊）。按钮为深底、细描边、hover 偏朱砂。
- 【源码】只在 `selection.trim() !== "" && state === "live"` 时渲染；点击调用 `quoteSelection()`；
  点击后 800ms 内文案切到 `term.inserted`（"已引用"）。
- 【图】`crop-btn.png` 是同一区域的放大：选区高亮为浅色底、深色字（反色块），下方按钮为**蓝描边圆角**。
- 【推断】按钮绝对定位于右栏右下（`right/bottom ≈ 12px`），`position:absolute`（在 `.term-pane` 内）。

---

## 4. 右键菜单（`.term-menu` / `.term-menu-sep`）

- 【图】`term-menu.png`：在终端上右键，浮出竖向菜单，**项序如下**（自上而下）：
  1. `引用选中内容`  右侧 `⌘⇧↵`
  2. `引用当前屏幕`
  3. —— 分隔线 ——
  4. `复制`（**置灰/禁用**；截图里当前虽有选区高亮，但该项呈灰）
  5. `粘贴`
  6. `清屏`
  7. `重新打开 shell`
- 【源码】顺序与禁用逻辑**逐字吻合**：`term.menu.selection`（`disabled: !selection.trim()`）→
  `term.menu.screen` → `term-menu-sep` → `term.menu.copy`（同样受选区禁用）→ `term.menu.paste` →
  `term.menu.clear` → `term.menu.reopen`。截图里"复制"灰而"引用选中内容"不灰，说明当时
  **selection 非空但 copy 被单独禁用**——【推断】可能是复制走异步剪贴板、在截图瞬间被禁用，或该行本来
  就与选区状态解耦；**这一点不确定，不臆断**。
- 【图】菜单为深色浮层卡片、圆角、投影；项高约 30px，左侧文字、右侧快捷键弱化。
- 【源码】`position:fixed`，坐标取鼠标 `clientX/clientY`；点外部 / 滚轮 / Esc 关闭。

---

## 5. "引用到对话" 的 composer 芯片（跨面板，非终端面板本身）

- 【图】`term-chip.png` / `term-quote-header.png` / `crop-composer.png`：composer 顶部出现一枚
  深色芯片，头部是 `终端` 文案 + `×`（移除），正文是被引用的终端内容。
- 【图】`term-quote-header.png` 的关键证据：芯片正文**第一行是 `$ npm test`，下一行空行，再是输出**。
  这正是 `withCommandHeader(body, command, dir)` 的结果（`$ cmd` + 空行 + 正文）。
- 【图】`crop-composer.png`：正文里命令/输出以等宽字体呈现，长行不换行处被裁；芯片整体圆角、细边。
- 【源码】引用动作 `emitUI("composer_quote", { text, source:{ id:`term:${tabId}`, title } })`，
  `text = formatSelection(selection).slice(0, 8<<10)`；`title` 取 `term.quoteSource`。
- 【推断】芯片样式属 composer 子系统（不在终端 CSS 内），本文件不改它；截图为深底浅字、与 composer 同色系。

---

## 6. 配色观感（综合）

- 【图】整体深色、低饱和暖灰底；终端区比面板底更深一档。
- 【源码】强调/光标用**朱砂** `--cinnabar`(≈#e68c74) / `--cinnabar-2`(≈#d97a66)；选区兜底为
  `rgba(217,122,102,0.28)`。
- 【源码】ANSI 16 色映射到语义令牌：red=`--crimson`、green=`--bamboo`、yellow=`--ochre`、
  blue=`--indigo`、magenta=`--cat-write`、cyan=`--cat-cmd`（终端/沙箱专属青）。
- 【图】截图里 npm 报错文字呈**红色**（对应 `--crimson` / ANSI red），成功/路径多为中性暖白。
- 【缺失】**精确 hex、圆角半径、阴影参数、菜单宽高、状态层透明度**——原 CSS 丢失，均无逐字证据。

---

## 7. 明确缺失清单（不要当成已还原）

1. `.term-overlay` 四种态的**截图**：没有 → 样式全为推断。
2. `.term-menu` 的精确尺寸/内边距/字号：仅目测。
3. `.term-insert` 的精确定位与 hover 过渡曲线：仅目测。
4. 字体缺失时的降级提示（源码提到 i18n 有 `term.fontMissing` 提示位）：**未见截图、未见文案**。
5. 终端在浅色主题下的观感：**6 张全是深色**，浅色分支无证据。
6. "复制"为何在有空选区时仍禁用：**图与源码冲突点，标注为不确定**。
