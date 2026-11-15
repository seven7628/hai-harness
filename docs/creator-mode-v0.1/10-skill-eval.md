# Creator Mode / Surface Skill 效果评估（2026-11-15）

> 范围：Creator Mode 已被 D-N 取消，能力由「常驻 surface 工具 + skill 路由 + 审批」提供。
> 本报告评估仓内与这条链路强相关的 `desktop/vendor/create-mode-skills/create-mode/SKILL.md` 及其机制；外部
> skills（office/visual/ego 等）不在本轮行为评估内。

## 结论先行

**机制是通的，默认配置下「效果」接近 0。** `create-mode` 的 `requires.tools` 要求
`ui_inspect + surface_install`，而 `features.surfaceTools` 默认 **false**；没有这两个工具时
技能不会出现在系统提示词里，`load_skill` 也会拒绝加载。结果是：用户不手动改
`settings.json`，模型既看不到「怎么装插件」，也调不到安装工具。

## 实测结果

| 检查 | 证据 | 结果 |
|---|---|---|
| 技能可解析 | `TestCreateModeSkillLoadable` | PASS |
| 清单/加载同源 | `skills.Registry.LoadFor` | 通过；能力不足时返回 `requires tool: ui_inspect, tool: surface_install` |
| 能力门矩阵 | 真实注册表 + 空/单工具/双工具 | 空 → `[]`；仅 `ui_inspect` → `[]`；仅 `surface_install` → `[]`；双工具 → `[create-mode(enabled=true)]` |
| 默认开关 | `loadSurfaceToolsEnabled` 缺省 false；本机 `settings.json` 无 `features` | 默认不提供 surface 工具 → 技能不可见 |
| 可视化开关 | 设置 →「面板插件」→「AI 创建面板」开关 + `settings:set` + `reload_settings` | **已补（2026-11-15）**；默认仍 false，但用户能看到入口 |
| 发现阶段成本 | description 299 字符；正文 7,575 bytes / 135 行（文件 7,575 bytes）；L3 5 refs + 2 scripts + 3 assets | 清单只注入一句；正文按需加载；细节按需读 L3 |
| 模板可安装 | `TestSurfaceTemplateInstalls` | PASS（`applied` + 幂等 `already_installed`） |
| 技能最小示例可安装 | 按 SKILL 的 minimum example 造目录跑真安装器 | `status=applied` |
| 历史使用证据 | 本地 `~/.go-code/events` / `sessions` 检索 `hai-surface-dev`（当时名；现 `create-mode`） | 未发现加载记录；无法用历史会话做效果回归 |

## 发现

### P0（已修一半）：默认关 + requires.tools = 技能自锁

2026-11-15 已补设置页可见开关；默认仍 false（fail-closed）。剩余问题是「开关默认关时，技能仍不可见」，这是**有意的成本/安全选择**，但空态必须把开关指出来：

1. `surfaceTools=false` 时引擎不注册 `surface_install`；
2. `requires.tools` 过滤把 `create-mode` 从清单和 `load_skill` 一起拿掉；
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
- 技能正文 7.4KB，只在需要时加载，常驻成本主要是 299 字符 description，方向正确。

## 建议

1. **P0 已选方案**：保持默认 false（fail-closed），设置页「面板插件 → AI 创建面板」提供
   可见开关。仍需在插件空态加一句「没有插件？先打开 AI 创建面板」的引导。
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
go test ./desktop/bridge -run 'TestCreateModeSkillLoadable|TestSurfaceTemplateInstalls' -count=1 -v
go test ./skills ./agents -run 'Skill' -count=1
# 能力矩阵（临时脚本）：
# 空 / ui_inspect only / surface_install only → 技能均不可见；两者都有 → enabled
```

> 本轮没有运行真实 Provider 行为评测：没有凭据。机制结论可以复跑；行为结论待补 A/B。

## Anthropic Agent Skills 对齐（2026-11-15）

依据 Anthropic 官方 Agent Skills overview / best practices：目录 + `SKILL.md`；frontmatter
必须有 `name`（≤64、lowercase/hyphen、非保留词）与 `description`（≤1024、无 XML）；三层渐进
披露（metadata → 指令正文 → `scripts/` / `references/` / `assets/`）；正文建议 <500 行；引用
文件保持一层深；脚本只执行、不把源码读进上下文。

| 要求 | 我们 | 判定 |
|---|---|---|
| 目录 + SKILL.md + YAML name/description | `desktop/vendor/create-mode-skills/create-mode/SKILL.md` | 通过 |
| name ≤64 / lowercase / hyphen / 非保留词 | `create-mode` 本身合法 | 通过（但解析器不校验长度/字符/保留词） |
| description ≤1024 / 无 XML | 299 字符、无 XML | 通过 |
| Level 1：只注入 name + description | `Registry.ListFor` / `Descriptor` | 通过 |
| Level 2：触发后才加载正文 | `load_skill` → `InstructionsBlock` | 通过 |
| 正文 <500 行 | 135 行 | 通过 |
| 正文体积接近官方 <5k tokens 建议 | 7.4KB（约 1.9k tokens）；细节在 L3 | 通过 |
| Level 3：references/scripts/assets，一层深 | 5 个 `references/` + 2 个 `scripts/` + 3 个 `assets/`；资源在技能目录内 | 通过（模板仍与根 `templates/surface` 双份，测试防漂移） |
| 脚本执行而非读入上下文 | `scripts/validate-plugin.mjs` + `scripts/install-smoke.sh`，SKILL 只给路径 | 通过 |
| 多模型测试 | 无 Haiku/Sonnet/Opus 结果 | **未验证** |

另外，我们的 `requires`（能力面过滤）、`version`、`manifest` 注入是宿主扩展；它们不破坏
Anthropic 格式，但不是标准字段，应该在我们的规范里明确标成「go-code extension」。

## 分层与规范盘点

### 框架分层：良好

- 发现层：`~/.agents/skills` / `~/.go-code/skills` / 工作区 `.agents/skills` / `.go-code/skills`
  四层，同名高层覆盖，symlink 可跟随；`skills/registry.go`。
- 上下文层：metadata（`ListFor`）→ 正文（`load_skill`）→ `Resources` 清单；
  `InstructionsBlock` 与 `/skill` 命令共用同一段字节。
- 能力层：`requires.input` / `requires.tools` + `Capabilities` 过滤，宿主 UI 还能显示「为什么不可用」。
- 版本层：`version` 用于随包技能覆盖用户旧副本（office/visual/ego/create-mode 同步链路）。

### 这个技能的内容分层：已修

- L2 现在 135 行 / 7.4KB，只留导航 + 六步流程 + 硬规则；L3 按需加载。
- `references/`：panel-api、tools、entries、examples、verification 五个文件，均一层深。
- `scripts/`：`validate-plugin.mjs`（静态 preflight）+ `install-smoke.sh`（真安装器）。
- `assets/`：模板三件套（surface.json / panel.js / icon.svg）随技能分发；根
  `templates/surface` 仍是安装器/e2e 的权威入口，`TestCreateModeSkillLoadable` 逐字节防漂移。
- 仍保留的仓路径：`install-smoke.sh` 需要 repo 的 `desktop/bridge`；这是开发环境脚本，
  不是可移植技能的硬依赖（安装器检查仍可由宿主工具完成）。

### CreateMode 规范盘点

| 材料 | 内容 | 问题 |
|---|---|---|
| `01-design.md` §5.1 | D-N：4 常驻工具 + 1 skill + 审批；工具 schema 成本纪律 | 定义清楚，但默认值/工具数量约束与代码有漂移 |
| `03-code-plan.md` §3.7 | 工具注册骨架、skill frontmatter 骨架、模板/插件页 | 2026-11-15 已改为默认 **false** + 设置页开关，与实现/SKILL/测试一致 |
| `desktop/vendor/create-mode-skills/create-mode/SKILL.md` | 操作规范、D11、host API、entries、tools、自检 | 是当前事实上的规范；但只有 1 个技能，无独立 skill spec |
| `templates/surface/*` | 可运行示例，真安装器可过 | 模板在技能目录外，不算技能资产 |
| `surface_skill_content_test.go` | 解析、description 80–300、requires 两向、模板安装、命令白名单 | 守住了静态形状，不守触发/行为/多模型效果 |

**结论**：有「设计说明 + 一个技能 + 一个模板 + 静态守卫」，但此前没有一份独立的
「CreateMode Skill 规范」把 frontmatter、触发词、分层、token 预算、安全边界、验收指标
写成可执行契约。默认值漂移（design true vs code false）就是缺少单一事实源的症状。

> **2026-11-15 落地**：新增 `11-skill-spec.md`（单一规范）；`create-mode` 已拆成
> `SKILL.md`（135 行 / 7.4KB）+ 5 个 `references/` + 2 个 `scripts/` + 3 个 `assets/`；
> 设置页开关已补；默认值四处统一为 false。下面「建议的规范骨架」保留为方法论记录。

## 建议的规范骨架（下一步）

1. `skills/README.md` 或 `docs/creator-mode-v0.1/11-skill-spec.md`：
   - 必填/可选 frontmatter；哪些是 Anthropic 标准、哪些是 go-code 扩展；
   - description 必须写「做什么 + 什么时候用」+ 中英触发词；≤1024；
   - 正文 <500 行、<5k token；L3 资源一层深、必须相对技能目录；
   - 安装/审批/D11/密钥/网络边界；
   - 触发集与四类 golden task 的验收指标。
2. ~~把 `create-mode` 拆出 `references/` 与 `scripts/`，SKILL.md 只留导航 + 六步流程。~~ —— 已完成。
3. ~~统一 `features.surfaceTools` 默认值并提供可视化开关~~ —— 已完成（默认 false；设置页开关；文档同步）。
4. 做有/无 skill 的行为 A/B（真实 Provider 或本地端点），至少 Haiku/Sonnet/Opus 各一轮。

## 独立 builder / evaluator 实测（2026-11-15）

方法：一个独立子智能体**只靠技能文档**写一个 PDF 阅读器插件；另一个独立子智能体在不知道
builder 结论的情况下验收。没有让主会话代写，避免把主会话能力误当成技能效果。

**产物判定：PASS。**

- 真安装器 `TestSurfaceInstallExternal`：`status:"applied"`，1 个入口，sha256 写入 index；
- 真实浏览器探针 `verify_external_surface.mjs`：T1–T9 绿，真 `panel.js` 字节进注册表，＋ 菜单
  标题/键盘/非空渲染/pageerror=0；
- `verify_external_surface_electron.mjs`：首轮 **BLOCKED**（不是插件失败）。用仓库自己的
  `templates/surface` 跑同一命令也在「等待 ＋ 菜单入口」超时。后续定位到根因：失败运行用的是
  **另一个 checkout/worktree 的旧 `dist`/`dist-electron`**，`surface:index` 为空；脚本只判产物
  存在、不判新鲜度，于是空 index 静默表现成 30s 超时。已在 main 修脚本前置（空宿主 index →
  exit 2 + 明确诊断；源码比产物新则重建；清掉继承的 `VITE_DEV_SERVER_URL`）并复跑 **PASS**
  （3 个入口、面板非空、pageerror=0）。

**技能侧判定：documentation FAIL（3 major）**，已在 1.1.1 修：

1. 随包模板 `assets/panel.js` 使用不存在的 `theme.tokens.bg2` / `.muted`，而文档明确只有 8 个
   token。已改为合法 token；`validate-plugin.mjs` 新增 token 名静态检查，防止模板再漂移。
2. `surface.json` 字段集缺失：`name` 必须是 `{"zh","en"}` 对象（裸字符串会让安装器报误导性的
   “不是合法 JSON”）；模板里的 `placement` 是死字段、`api` 未被消费。`references/panel-api.md`
   新增完整字段表，两个模板都删除 `api` / `placement`。
3. 自相矛盾：`entries.md` 把声明入口标题写成 `manifest.title`（应为 `entries[].title`）；工具
   description “≤120” 与 “≤300” 未区分（300 是安装器硬上限，120 是设计预算）；验证文档没说明
   browser 档自带 Vite/5232、不需要 dev server/5173，也没给 Electron control。均已修正。

**同时修正的 minor**：`apply` 返回值被忽略、`onEvent` 由宿主自动回收的 teardown 语义；
`reason` 闭集（含 `panel_not_open` / `timeout` / `plugin_unloaded`）；`composer.quote.source`
的字符串/对象两种形状与 200 字符截断；`composer.quote.text` 按 UTF-8 字节而非字符计 8 KiB；
provider 的 `order` / `icon` / `dedupeKey`；验证脚本的技能相对路径；`validate-plugin.mjs`
的边界（只做静态 preflight，不是安装器）以及 name/entries/tools/token 检查。

**仍未做**：有/无 skill 的真实模型行为 A/B（需要 provider，且要固定同一 prompt 与评分）。
Electron 真机门已在 main 产物上复跑通过；首红是旧 checkout 的 dist + 脚本静默，不是宿主构建缺陷。
机制验收已通过，不能把机制绿当成模型效果结论——这一点仍按规范第 6 节标注。

**复核（独立子智能体，2026-11-15）**：documentation **PASS**。3 major 逐条按行为复核——模板
token 0 个非法、两份模板 md5 一致；manifest 字段表 + name 对象 + 模板无 api/placement；验证文档
自带 Vite/5232、control 语义、exit 1 宿主/插件区分。命令：validate templates/example 0；
install-smoke 0（`applied`，entries 3）；浏览器 T1–T9 32 ✅ / 0 ❌；`TestSurfaceTemplateContracts`
等 Go 用例 0。仍只余 minor：示例 fixture 保留 api/placement（只读，警告即可）。Electron 门
首红已定位并修复为脚本前置（旧 checkout 的 dist），main 上复跑 PASS。报告：
`/tmp/creator-skill-eval/recheck-report.md`。

## 真实 Provider 行为 A/B（2026-11-15，n=1）

环境：`opencode4 · deepseek-v4.1-flash`，临时 HOME + safeStorage 解密的真实 key（harness
只 symlink keys.json，不读/不打印 key），`features.surfaceTools=true`，同一条 prompt，
两条件各自独立工作区；产物/报告在 `/tmp/creator-skill-eval/ab-20261115-021836/`。

| | ON（技能在册） | OFF（技能不在册） |
|---|---|---|
| 收敛 | ✅ 99.6s | ❌ 撞 8min 上限仍在跑 |
| 首动作 | `load_skill hai-surface-dev`（当时名；现 `create-mode`）→ 完整 SKILL.md | 仓库考古（含直接读磁盘技能） |
| 插件 | `hello-panel/`，validate 0 | `hello-plugin/`，validate 0 |
| 安装 | `surface_install` → **applied** | **0 次** surface_install；`surfaces/` 空 |
| 工具/审批 | 18 / 5 | 27 / 16 |
| pageerror | 0 | 0 |

**怎么读**：这是 n=1 的真实模型观察，不是统计结论。ON 明确走了「技能 → 骨架 → 安装 →
复核」；OFF **并不干净**——模型 `cd` 进仓库后直接 `read_file skills/hai-surface-dev/SKILL.md`（当时的仓库路径），
等于“技能在磁盘、不在注册表”。所以差异更像「在册 → 走流程」vs「不在册 → 先环境考古」，
不能归因成「不会写代码」。OFF 的 8min 是人为上限，且卡在 install-smoke 的 Go module 联网；
两侧都产出了合法 validate 产物，但只有 ON 完成真实安装。§6 的「行为未验证」现在改为
「单模型单 rep 已验证，样本与隔离仍不足」，不能用它代替多模型结论。
