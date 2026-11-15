# CreateMode Skill 规范（v1，2026-11-15）

> CreatorMode 代码模式已取消（D-N）。创造能力 = 常驻 surface 工具 + skill 路由 + 审批。
> 本文是这条链路里 **skill 的单一规范**：定义、frontmatter、分层、token 预算、安全边界和
> 验收方式。`skills/hai-surface-dev/SKILL.md` 是本文的第一个实现。

## 1. 定义

一个 CreateMode skill 是一个目录：

```text
<skill-name>/
  SKILL.md                 # 必需：frontmatter + L2 指令
  references/*.md          # 可选：按需读取的长契约
  scripts/*                # 可选：确定性检查（可执行，不把源码塞进上下文）
  assets/*                 # 可选：模板/图标等原始资产
```

模型看到的是 **name + description**；命中后才加载 `SKILL.md` 正文；正文里的链接再按需读取
`references/` 或执行 `scripts/`。禁止把所有细节放进 L1/L2。

## 2. Frontmatter

```yaml
---
name: hai-surface-dev
version: 1.1.3
description: 'Builds or debugs a panel ... Use for 插件/面板/PDF 阅读器/文档面板.'
requires:
  tools: [ui_inspect, surface_install]
---
```

| 字段 | 必需 | 约束 | 归属 |
|---|---|---|---|
| `name` | 是 | `^[a-z][a-z0-9-]{0,63}$`；不用保留词 | Anthropic 标准 |
| `description` | 是 | 80–300 字符（本项目；Anthropic 上限 1024）；无 XML；写「做什么 + 何时用」；含中英触发词 | Anthropic 标准 + 本项目约束 |
| `version` | 是（本项目） | semver；随包技能版本升级时覆盖用户旧副本 | go-code 扩展 |
| `requires` | 否 | `input` / `tools`；能力面过滤，缺一即不可见/不可加载 | go-code 扩展 |
| `allowed-tools` | 否 | 与 Anthropic 兼容的保留字段；本项目的装载器不据此放宽权限 | Anthropic 标准 |
| `license` | 否 | 原样保留 | Anthropic 标准 |

`requires.tools` 必须列**真实工具名**；当前 surface skill 的最低集合是
`ui_inspect + surface_install`。`ui_inspect` 是宿主工具，D11 只禁止它在插件的
`surface.json.commands` 里声明，不要在 skill 里写成「不能用 ui_inspect」。

## 3. 分层与预算

| 层 | 内容 | 预算 | 要求 |
|---|---|---|---|
| L1 metadata | name + description | ≤300 字符 | 每次请求都在上下文里，必须短、具体、含触发词 |
| L2 `SKILL.md` | 导航 + 六步流程 + 硬规则 | ≤200 行 / ≤12 KiB | 只放「现在要做什么」；详细 API/示例/排错放 L3 |
| L3 `references/` | 长契约、示例、验证说明 | 单文件 ≤16 KiB | 一层深；从 SKILL.md 用相对链接可达；不嵌套目录 |
| L3 `scripts/` | 静态校验/安装冒烟/确定性检查 | 单脚本 ≤16 KiB | 可执行、无新依赖、失败给出可读错误；不要把脚本全文读进上下文 |
| L3 `assets/` | 模板、图标等 | 单文件 ≤256 KiB | 技能目录内相对路径；不能指向仓库根的外部路径 |

**渐进披露的验收**：删除 `references/` 后 L2 仍能完成最小面板；删除 L2 的导航则模型必须
知道该读哪个文件。当前 `hai-surface-dev` 的 L2 为 133 行 / 7.1 KiB，L3 为 5 个 reference +
2 个 script + 3 个 asset。

## 4. 内容要求

1. **触发**：description 必须覆盖用户真实说法，至少包含：插件、面板、右栏、看板、
   PDF 阅读器、文档面板、right panel、plugin、surface。不要只写内部术语。
2. **六步流程**：先看现状（`ui_inspect`）→ 问清歧义 → 写 manifest/panel.js → 安装
   （`surface_install`，审批）→ 真机验证 → 回报限制。每一步都要有失败时的可读原因。
3. **硬规则**：no JSX / no bundler；`kind = plugin:<id>`；只注册自己的 kind；只用
   `host.command` + `host.onEvent`；声明 commands/data；D11 四条不可作为插件命令声明；
   `read_only:false` 必须走审批；不得伪造数据。
4. **示例**：必须有一个最小可装示例和一个带 `agent.run` / `composer.quote` 的示例；
   示例必须能被真安装器接受（`TestSurfaceInstallExternal`）。
5. **自检**：L2 必须指向 `scripts/validate-plugin.mjs` 与 `scripts/install-smoke.sh`；
   完整 Electron 检查放在 `references/verification.md`。

## 5. 安全边界

- **密钥**：skill、脚本、示例、日志一律不写 key；真实 Provider key 只从环境/密码管理器进入，
  不写入 `settings.json`、不写命令行、不提交仓库。
- **安装**：`surface_install` 必须有审批；`read_only:false` 工具使用 `MandatoryApproval`，
  Auto/Full Access 也不能静默放行；无审批器 fail-closed。
- **能力**：skill 不能扩大工具集；`requires.tools` 只做减法；D11 四条旧命令既不能当插件
  command，也不能被 skill 教成可用。
- **开关**：`settings.features.surfaceTools` 默认 **false**（fail-closed）；设置 →
  面板插件 →「AI 创建面板」有可见开关。开启后才注册 surface 工具并让本 skill 进入清单；
  修改后发 `reload_settings` 重建工具面。关闭时 skill 必须从清单和 `load_skill` 一起消失。
- **网络/沙箱**：skill 不自行下载依赖；脚本只用 Node/Go 标准能力；安装/运行遵守宿主沙箱。

## 6. 验收

### 静态/机制（每次改 skill 必跑）

- `TestSurfaceSkillLoadable`：frontmatter、description 80–300、能力面双向过滤、L2 预算、
  L3 资源存在且进入 `Registry.Resources`。
- `TestSurfaceTemplateInstalls`：根模板和 `assets/` 模板一致，真安装器 `applied`。
- `npm run test:settings-store`：`features.surfaceTools` 写侧白名单。
- `npm run e2e:surfaces-page`：设置页可见开关 + `settings:set` + `reload_settings`。

### 行为（需要真实 Provider 或本地 OpenAI 兼容端点）

固定 4 类任务 × skill on/off × 3 reps：

| 任务 | 判据 |
|---|---|
| 最小面板 | `applied`、无 pageerror、标题正确 |
| 带 `agent.run` 的面板 | 事件 delta/done、jsonl 落盘、无 pageerror |
| `read_only:false` 工具 | 审批卡必现、批准后 tool.call/tool.result |
| `requires_panel:true` 工具 | 关标签后 `panel_not_open`，插件未收到回调 |

指标：首次安装通过率、pageerror 为 0 率、修复轮数、工具调用数、耗时、token 成本。
真实模型至少覆盖 Haiku / Sonnet / Opus 各一轮；没有凭据时只跑机制测试，并在报告里标注
「行为未验证」，不得用单测代替模型效果结论。2026-11-15 的独立 builder/evaluator 机制实测
（PDF 示例：真安装器 applied + 真浏览器渲染 PASS；Electron 门首红后定位为旧 checkout 的 dist，main 复跑 PASS）见
`10-skill-eval.md` 的「独立 builder / evaluator 实测」。

## 7. 版本与发布

- `version` 变化 = 随包技能覆盖用户旧副本；只改错别字也建议 patch 版本。
- 删除/重命名 skill 时保留一个版本的 tombstone 说明，避免用户 @ 到不存在的能力。
- 任何 skill 改动必须同步 `10-skill-eval.md` 的结论（机制/行为分开写）。
