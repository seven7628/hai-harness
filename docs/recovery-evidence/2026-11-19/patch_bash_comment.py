import os

p = '/Users/admin/workspace/go-code/tools/builtin/bash.go'
s = open(p, encoding='utf-8').read()

old = """// cwdKey 调用方的 cwd 记忆分区键（实现见同包的 workdir.go）。
//
// **为什么分区**：工具实例在注册表里共享 —— 主 agent 与后台子 agent 共用同一个
// ToolEngine（desktop/bridge）⇒ cwd 若只有一份，子 agent 的 `cd` 会改掉主 agent
// 后续命令的工作目录（两个 agent 互相踩）。分区键 = 子 agent 的 task id
// （跨轮稳定 ✓），主 agent = workdir.MainKey（跨轮稳定 ✓）。

"""

new = """// cwdKey 调用方的 cwd 记忆分区键（实现与理由见同包的 workdir.go）：
// 分区键 = 子 agent 的 task id（跨轮稳定 ✓），主 agent = workdir.MainKey（跨轮稳定 ✓）。

"""

assert old in s
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('ok')
