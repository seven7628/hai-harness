# 修复方案：堵住 `rm -rf` 防护的缺口（2026-11-19 事故后）

## 事故回顾

2026-11-19 22:12–22:20，本机大量数据被删除。复现出的删除命令模式**全部**通过了
`tools/builtin/bash.go` 的 `checkHighRisk` 静态检查（返回空串 = 放行）：

```
rm -rf /Users/admin/workspace/go-code                        => 放行
rm -rf ~/workspace/go-code                                   => 放行
rm -rf $HOME/workspace/go-code                               => 放行
rm -rf /Users/admin/cybertron-gogroup                        => 放行
rm -rf /Users/admin/workspace/go-code/.go-code/worktrees/*   => 放行
find /Users/admin -maxdepth 2 -name go-code -exec rm -rf {} + => 放行
```

## 缺口一：家目录 / 工作区下的具体子路径完全放行

`isDangerousPath`（bash.go:687）只拦这些"宽泛目标"：

```go
case ".", "./", "..", "../", "/", "~", "~/", "*", ".*", "$HOME", "$HOME/", "~/*", "$HOME/*":
    return true
```

以及 `../` 前缀、`*/` 前缀、含通配的家目录路径。

**不含**通配的家目录具体子路径（`~/workspace/go-code`、`$HOME/workspace/go-code`）
一律放行 —— 这是 2026-09 的用户决策（"家目录子路径放行，与工作区子目录一致"）。

### 但这条决策在 `rm -rf` 上不成立

`rm -rf dist/` 与 `rm -rf ~/workspace/go-code` 的**语义完全不同**：

| 形态 | 语义 | 风险 |
|---|---|---|
| `rm -rf dist/` | 删当前工作区下的一个子目录 | 低 —— 范围小、可重建 |
| `rm -rf ~/workspace/go-code` | 删整个项目仓库（含 .git） | **高 —— 不可重建** |
| `rm -rf $HOME/workspace/go-code` | 同上 | **高** |
| `rm -rf /Users/admin/workspace/go-code` | 同上 | **高** |

`rm -rf` 的危险性与**目标是不是通配**无关，与**目标是不是用户的核心资产**有关。
一个包含 `.git` 的仓库目录、用户 home 下的项目目录 —— 这些删掉就是灾难，
且不可能靠"重新 clone"完全还原（本地未推送提交、���提交改动会永久丢失）。

### 建议的判定口径

不要用"路径形态"判危险，要用**"这条命令在当前上下文里值不值这个风险"**判。

具体三条规则（都保持现有"工作区内相对子目录放行"的语义）：

**规则 A：绝对路径 / 家目录锚定的目标，只要指向"仓库根"或"家目录的直接子目录"就拦。**

判据：目标路径解析后
- 等于当前工作区根 → 拦（`rm -rf /path/to/repo`）
- 等于家目录本身 → 拦（已拦）
- 是家目录的**一级子目录** → 拦（`~/workspace`、`~/Projects` 这类"用户放项目的地方"）
- 命中 git 仓库根（目标下有 `.git`）→ 拦

**规则 B：家目录锚定的目标（`~/` / `$HOME/` 开头）默认改为拦，只对白名单子路径放行。**

放行白名单（明确是缓存/临时性质）：`~/.cache/`、`~/.npm/`、`~/.cargo/`、
`~/Library/Caches/`、`~/go/pkg/mod/cache/`、`/tmp/`。

理由：家目录下的东西**几乎都是用户的**，不像工作区里的 `dist/` 那样可随手重建。
"删家目录下的某个东西"应该是罕见且明确的动作，值得多一次确认。

**规则 C：`find ... -delete` 与 `find ... -exec rm` 纳入检查。**

现状：`dangerousRmTarget` 按 token 流解析 `rm`，遇到 `find` 时 `inRm` 为 false，
于是 `-exec rm -rf {} +` 这段**根本没被当作 rm 调用看待**。这是纯粹的绕过。

判据：命令里出现 `find` 且带 `-delete` 或 `-exec rm`/`-exec unlink`，且搜索根
是家目录 / 工作区根 / 绝对路径的宽范围（`-maxdepth` 大或没有）→ 拦。

## 缺口二：非 bash 工具发起的删除完全无防护

`checkHighRisk` 只保护 bash 工具。其它 agent 工具（run_python / codemode /
MCP 工具 / 插件）发起的删除不在检查范围内。

事故里删除命令的具体来源尚未定位（需 root 取 `fs_usage`），但**防护必须假设
存在第二条路径**。

建议：
1. 给 `run_python` / codemode 的脚本加**同源**的高危静态检查（`run_python_test.go`
   已有 `TestRunPythonHighRiskStaticCheck`，说明那层已经有一套 —— 需确认它是否
   复用了 `checkHighRisk`；若是，缺口自动收敛）
2. 插件 / MCP 工具侧：至少在**审计日志**里记录"谁在什么时候删了哪个路径"，
   出事后能定位

## 缺口三：删除没有"第二道确认"

现在的模型是：静态检查通过 ⇒ 直接执行。对 `rm -rf` 这种不可逆操作，
建议加一层**广度判定**：目标若是目录且递归+强制 ⇒ 记录审计事件（谁、什么路径、
工作区是不是它）。不需要弹窗确认（会打断 agent 流程），但**必须可追溯**。

## 实施顺序

1. 先补**规则 A**（仓库根 / 家目录一级子目录）—— 覆盖本次事故的主要形态，误伤面小
2. 再补**规则 C**（find -delete / find -exec rm）—— 纯规则补充，无语义争议
3. 规则 B（家目录默认拦 + 白名单）涉及 2026-09 的用户决策，需**先与用户确认**
4. 缺口二/三需要跨模块改动，单独排期

## 配套测试

`tools/builtin/builtin_test.go` 的 `TestBashHighRisk` / `TestBashHighRiskDeep`
已有放行/拦截清单。新增规则后要同步补：

```go
// 事故复现：这些曾全部放行
blocked := []string{
    "rm -rf /Users/admin/workspace/go-code",
    "rm -rf ~/workspace/go-code",
    "rm -rf $HOME/workspace/go-code",
    "find /Users/admin -maxdepth 2 -name go-code -exec rm -rf {} +",
}
```

注意：放行清单里 `rm -rf /tmp/foo`、`rm -rf dist/`、`rm -rf .git/` 必须仍然放行
（回归保护）。`rm -rf .git/` 要单独想清楚 —— 删掉 git 元数据是不可逆的，
但它也在工作区内且不涉及源码，倾向**仍然放行**（模型可能在做实验），但要在
审计日志里留痕。
