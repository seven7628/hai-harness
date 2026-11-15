# Creator Mode / Surface Skill 效果评估（2026-11-15）

> 范围：Creator Mode 已被 D-N 取消，能力由「常驻 surface 工具 + skill 路由 + 审批」提供。
> 本报告评估仓内与这条链路强相关的 `skills/hai-surface-dev/SKILL.md` 及其机制；外部
> skills（office/visual/ego 等）不在本轮行为评估内。

## 结论先行

**机制是通的，默认配置下「效果」接近 0。** `hai-surface-dev` 的 `requires.tools` 要求
`ui_inspect + surface_install`，而 `features.surfaceTools` 默认 **false**；没有这两个工具时
技能不会出现在系统提示词里，`load_skill` 也会拒绝加载。结果是：用户不手动改
`settings.json`，模型既看不到「怎么装插件」，也调不到安装工具。

## 实测结果

| 检查 | 证据 | 结果 |
|---|---|---|
| 技能可解析 | `TestSurfaceSkillLoadable` | PASS |
| 清单/加载同源 | `skills.Registry.LoadFor` | 通过；能力不足时返回 `requires tool: ui_inspect, tool: surface_install` |
| 能力门矩阵 | 真实注册表 + 空/单工具/双工具 | 空 → `[]`；仅 `ui_inspect` → `[]`；仅 `surface_install` → `[]`；双工具 → `[hai-surface-dev(enabled=true)]` |
| 默认开关 | `loadSurfaceToolsEnabled` 缺省 false；本机 `settings.json` 无 `features` | 默认不提供 surface 工具 → 技能不可见 |
| 可视化开关 | renderer 检索 `surfaceTools` 仅命中 i18n 文案 | 未发现设置页开关；只能手改 `settings.json` |
| 发现阶段成本 | description 230 字符；正文 20,828 bytes / 390 行 / 11 节 / 11 个代码块 | 清单只注入一句；正文按需加载，渐进披露成立 |
| 模板可安装 | `TestSurfaceTemplateInstalls` | PASS（`applied` + 幂等 `already_installed`） |
| 技能最小示例可安装 | 按 SKILL 的 minimum example 造目录跑真安装器 | `status=applied` |
| 历史使用证据 | 本地 `~/.go-code/events` / `sessions` 检索 `hai-surface-dev` | 未发现加载记录；无法用历史会话做效果回归 |

## 发现

### P0：默认关 + requires.tools = 技能自锁

这是当前最大的效果问题，不是文案问题：

1. `surfaceTools=false` 时引擎不注册 `surface_install`；
2. `requires.tools` 过滤把 `hai-surface-dev` 从清单和 `load_skill` 一起拿掉；
3. 插件页空态只说「模型调用 surface_install」，但没有告诉我怎么把工具打开；
4. 技能正文里虽然写了开关，但正文加载不了 —— 形成鸡生蛋循环。

### P1：D11 文案容易被模型误解

SKILL 同时写：

- Step 1：用 `ui_inspect` 看现状；
- D11：`ui_inspect` 是「已禁用的旧只读命令」。

前者是**宿主工具**，后者是**插件 manifest.commands 里禁止声明**。当前措辞没有把这层
区分说透，模型可能因此不调用 `ui_inspect`，反而绕路猜状态。

### P2：触发描述覆盖不足

description 有「插件 / 面板 / 看板 / 右栏 / right panel / surface / dashboard / add a tab /
装个面板」，但没有「PDF 阅读器」「文档面板」「给应用加功能」等用户常用说法。对
「给应用加个 PDF 阅读器」这类最典型的 Creator 任务，路由可能不加载技能。

### P2：还缺行为级 A/B 证据

本轮验证的是**机制、内容、安装**，不是「有 skill 比没 skill 写出的插件更好」。没有可用
真实 Provider 凭据时，不能伪造这个结论。建议用固定任务集跑有/无 skill 的对照，而不是
只看单测。

### 正向项

- 渐进披露正确：清单只带 name + description，正文按需进入上下文；
- 模板与最小示例都能过真安装器，不是「文档里的死例子」；
- 正文覆盖了 no JSX / host.command / onEvent / D11 / entries / tools / 审批 / 自检命令；
- 技能正文 20KB，只在需要时加载，常驻成本主要是 230 字符 description，方向正确。

## 建议

1. **先解决 P0（二选一）**：
   - 把 `features.surfaceTools` 默认改为 true（接受 600–900 token 常驻 schema）；或
   - 保持默认 false，但在设置页/插件页空态提供可见开关，并加一个只依赖 `ui_inspect`
     的引导提示，告诉用户如何打开 surface 工具。
2. **修 D11 措辞**：明确「`ui_inspect` 仍是宿主工具；只是不能在插件的 `commands` 里声明」。
3. **扩 description 触发词**：加入 PDF 阅读器、文档面板、给应用加功能、sidebar、plugin。
4. **建行为评测**（需要真实 Provider 或本地可用的 OpenAI 兼容端点）：
   - 4 个固定任务：最小面板 / 带 agent.run 的面板 / read_only:false 工具 / requires_panel；
   - 每个任务 3 reps，skill on/off；
   - 指标：安装 `applied` 率、pageerror 为 0 率、首次通过率、修复轮数、工具调用数、耗时；
   - 用 `verify_external_surface_electron.mjs` + P2 T11–T14 作为判据；
   - 报告写 `evals/creator-skills/`，不要把 key 写入任何文件。

## 复跑

```bash
go test ./desktop/bridge -run 'TestSurfaceSkillLoadable|TestSurfaceTemplateInstalls' -count=1 -v
go test ./skills ./agents -run 'Skill' -count=1
# 能力矩阵（临时脚本）：
# 空 / ui_inspect only / surface_install only → 技能均不可见；两者都有 → enabled
```

> 本轮没有运行真实 Provider 行为评测：没有凭据。机制结论可以复跑；行为结论待补 A/B。
