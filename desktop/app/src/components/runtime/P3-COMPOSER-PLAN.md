# P3 · Composer 三件事（`/` 技能、`@` 文件目录、多图扇形灯箱）

判据只有一个：`design/agent-runtime/v2/` 里的字面值。任何一处「我觉得这样更好看」都不写进代码；
如果设计与我们的宿主结构冲突，写在下面「刻意偏离」里，并说明为什么。

## 0 · 设计源（要搬的东西）

| 部件 | 源 | 行 |
|---|---|---|
| 弹出面板 `.pop` + `@keyframes pop-in` | `ui.components.css` | 694-702 |
| `.pop-label` / `.pop-row`（`.is-active` `.is-on` `.k` `.m` `svg` `kbd`） | 同上 | 703-724 |
| `.pop-foot` | 同上 | 725-731 |
| `.pop-search`（含 input） | 同上 | 732-740 |
| `[data-pop='cmd'|'src']` 窄面板 + lifted sheet 覆写 | 同上 | 2545-2565 |
| `.pop-row.sk` / `.pop-row.dir` / `.chip.src[data-kind='dir']` | 同上 | 2564-2572 |
| 扇形 `.cy-imgs` / `.cy-im`（`--i` `--mid`，`data-n='1'` 大缩略图） | 同上 | 2729-2761 |
| 灯箱 `.cy-lb` / `.cy-lb-x` / `@keyframes cy-lb-in` | 同上 | 2763-2795 |
| 640px 断点（`.cy-im` 52×37、margin -12） | 同上 | 2796-2801 |

## 1 · CSS（两份新文件，皮肤前缀逐字）

- `components/runtime/pop/pop.css`：上面 pop 那七行表里的全部规则，前缀 `html[data-skin='runtime']`。
- `components/runtime/fan/fan.css`：扇形 + 灯箱 + 640 断点，同样前缀。

**刻意偏离（唯一一处）**：`.pop` 的位置声明不照抄。设计里它锚在输入卡里
（`left/right:14px; bottom:54px`），我们的锚点是 `.composer`（`position:relative`），
现有 `.cmd-menu` 用的是 `bottom: calc(100% + 8px); left:0; right:0`。位置属于宿主几何，
照抄会把面板放到错误的地方；视觉部分（padding 5、r-md、`--raised` 底、lifted sheet 阴影、
pop-in 160ms）全部逐字保留。

## 2 · `/` 技能菜单（Composer.tsx，`cmdOpen` 分支）

结构（设计 `ui.js` 的 `[data-pop='cmd']`）：

```
.pop[data-pop='cmd']
  .pop-search        ← input，受控（初值 = 已输入的 /x），空 = 全量
  .pop-label        技能 <b>N</b>
  .pop-row.sk ×n    svg(✦ 用现有 spark 图标) + .k(命令名) + .m(技能 summary) + kbd(1…9)
  .pop-label        内置 <b>M</b>
  .pop-row.sk ×m
  .pop-foot         ↑↓ 选择 · ⏎ 插入 · esc 关闭
```

- 门控沿用现状：首 token 是未完成的命令前缀时才开（`/api-test/x` 不误报）。
- 键位：↑/↓ 移动 `.is-active`（首行默认 active），Enter/Tab 接受，Esc 关闭；
  数字键 1-9 直接接受第 n 行（`kbd` 给出的承诺必须能按，同 A1 的规矩）。
- 插入仍是 `命令 + 空格`，光标落命令之后（现有 `completeCommand` 的行为不变）。
- 类名互斥：运行时 `rt ? 'pop ...' : 'cmd-menu'`，另两档皮肤零变化。

## 3 · `@` 文件目录（FileRefPanel.tsx）

结构：`.pop[data-pop='src']` → `.pop-search` → `.pop-label`（面包屑 + 计数）→
`.pop-row.dir`（文件夹：svg 琥珀色、`.k` 名称、`.m` 子项数）→ `.pop-row`（文件）→
`.pop-foot`（⏎ 引用当前目录 · esc 关闭）。

- 数据源不动：继续走 `fsList`，`filter` / `onPick` 契约不变（只换 DOM 与皮肤 CSS）。
- 键位与 (2) 同一套（抽一个 `usePopKeys` 或就地实现，两处一致）。
- 子项数拿不到就不显示 `.m`（不编数字）。

## 4 · 多图扇形 + 灯箱（Composer 附件区）

- 图片附件从 `.attach-chip` 换成 `.cy-imgs[data-n]` + `.cy-im`：
  `--i` = 索引，`--mid` = (n-1)/2，单图 `data-n='1'` 自动变大缩略图。
- 点击 / Enter / Space → `.cy-lb` 灯箱（同一份 `src`，`figcaption` 写文件名）；
  `.cy-lb-x`、点背景、Esc 关闭；打开时 `body` 滚动锁 + 焦点移到关闭钮。
- 文档引用 chips 保持现状（设计里它们是 `.chip.src`，本次不动）。
- 640px 断点由 `fan.css` 自带。

## 5 · 验证

1. **资源对账**（脚本，与 A1/A7 同法）：把 `pop.css`/`fan.css` 的每条声明与设计逐条比，diff 必须为 0。
2. **行为断言**：扩展 `lab/verify-runtime-skin.mjs`：
   - `/` 键入 → `.pop[data-pop='cmd']` 出现、行列数、↓ 后 active 行变化、Enter 插入文本；
   - `@` 键入 → `.pop[data-pop='src']` 出现、`.pop-row.dir` 点击进入目录（面包屑变化）；
   - 3 张图 → `.cy-im` 数量 3、`data-n='3'`、第 2 张 `--i=1`、点击开 `.cy-lb`、Esc 关闭；
   - 单图 → `.cy-imgs[data-n='1']` 尺寸 128×88。
3. `npx tsc --noEmit`、`npx vite build`、`npm run test:runtime-turns`。

## 5.1 · 落地时定下的两处（设计没写、产品必须有）

1. **删除图片放在灯箱里**：设计的 `.cy-im` 点击 = 放大（64×45 的卡上没有位置放一个可靠的 ×）。
   删除于是移到灯箱里，用同一个 `.cy-lb-x` 形态（`right:34` 让开关闭钮）。
2. **数字键的作用域**：`kbd` 那列必须能按（同 A1 规矩），但正文里打数字不能被吞 ——
   数字键只在弹层打开时生效；弹层关掉后 1-9 回到正文。

## 6 · 不做

- 技能参数补全、文件内容搜索（没有接口，也没有设计）。
- 不改 `fsList` 契约，不新增 Go 字段。
- 不动 `current`/`v3` 两档皮肤的任何规则。
