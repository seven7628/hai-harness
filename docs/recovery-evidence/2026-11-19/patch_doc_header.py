p = 'docs/BRIDGE_DISK_FAMILIES_DESIGN_2026-11-19.md'
s = open(p, encoding='utf-8').read()

old = """> 2026-11-19 · 磁盘那座山（`~/.go-code` ≈ 14.5GB）的第一轮梳理。
> 目标：**先固定职责与影响范围，再动手**。本轮**没有改任何生产代码**。
> 姊妹文档：`BRIDGE_MEMORY_SKELETON_DESIGN_2026-11-19.md`（内存那一侧的骨架级优化，已实施 3 步）。"""

new = """> 2026-11-19 · 磁盘那座山（`~/.go-code` ≈ 14.5GB）的梳理与实施。
> 目标：**先固定职责与影响范围，再动手**。
> 姊妹文档：`BRIDGE_MEMORY_SKELETON_DESIGN_2026-11-19.md`（内存那一侧的骨架级优化，已实施 3 步）。
>
> **实施状态（2026-11-19）**：
>
> | 步 | 状态 | 提交 |
> |---|---|---|
> | ① 事件补 `agent_task_id` 标签 | ✅ 已实施 | `287ff49` |
> | ② 落盘裁 `llm_end.tool_calls` | ✅ 已实施 | `4c18c39` |
> | ③ 冷 sidecar 死字节回收 | ✅ 已实施 | `11d621d` |
> | ④ events gzip 封存（**不删内容**） | ✅ 已实施 | 见 §7.1 第 3 步 |
> | ⑤ 子智能体 events 分文件 | ⏸ **暂不做**（收益已被 ④ 覆盖大半） | — |
>
> 全程**前端代码零改动**；④ 落地后 `~/.go-code` 预计 ≈ **7.7GB（−47%）**。"""

assert old in s, 'header 锚点没匹配'
s = s.replace(old, new)
open(p, 'w', encoding='utf-8').write(s)
print('header updated')
