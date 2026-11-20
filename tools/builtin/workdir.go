package builtin

import (
	"context"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/workdir"
)

// 本文件是**会话级 cwd 状态**（workdir.State）与工具层的接合处。
//
// # 为什么工具层要接会话级 cwd
//
// 改造前，"当前目录"有三份：bash 记自己的 cd，文件工具恒定锚工作区根，
// run_python 恒定锚工作区根 ⇒ 模型 `cd sub` 之后三个工具给出三个答案
// （详见 workdir 包的包注释）。
//
// 接上之后：**bash / 文件工具 / run_python 共用同一个 workdir.State**
// ⇒ 三者的「当前目录」永远是同一个值，不会各走各的。
//
// # 分区：谁在改 cwd
//
// 状态按**分区**记录（分区键 = 调用方身份）：
//
//   - 主 agent 直调 → workdir.MainKey；
//   - 子 agent（spawn / explore）→ 它的 task id。
//
// 于是 explore 子 agent 的 `cd` 落在**自己的分区**，不影响主 agent ——
// 这正是"与主会话共用同一个 workdir.State"的意义：共用的是**状态容器**
// （一份真相），不是**目录**（各走各的分区）。
//
// # 分区键从哪来
//
// 两个来源，必须给同一个值：
//
//   - workdir 的 ctx key（workdir.WithKey）—— **工具描述**那条路用
//     （描述在「拼 schema」时算，那时还没有 ToolContext）；
//   - ToolContext.TaskId —— **工具调用**那条路用。
//
// 优先读前者、后者兜底 ⇒ 两条路永远一致（agent loop 在两处注入的是同一个值）。

// CwdKey 调用方的 cwd 记忆分区键（**导出**：宿主层的工具也要按它取当前目录，
// 例如 codemode 的脚本工作目录）。
func CwdKey(ctx context.Context) string {
	if ctx == nil {
		return workdir.MainKey // nil ctx（直调工具的极端用法）⇒ 主分区
	}
	if k := workdir.KeyFrom(ctx); k != workdir.MainKey {
		return k
	}
	if tc := events.ToolContextFrom(ctx); tc != nil && tc.TaskId != "" {
		return tc.TaskId
	}
	return workdir.MainKey
}

// cwdKey 调用方的 cwd 记忆分区键（工具层内部用；导出版见 CwdKey）。
//
// 为什么保留这个短名字：bash 工具内部的读写点很多（currentCwd / setCurrentCwd），
// 用短名保持原样，注释里说清它与 CwdKey 的关系即可。
func cwdKey(ctx context.Context) string { return CwdKey(ctx) }
