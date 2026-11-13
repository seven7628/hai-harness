// surfaces_command.go —— 插件调用宿主能力的**权威闸门**（PR-14 D；01-design §5.2b/§5.5）。
//
// 为什么闸门在 Go 而不在渲染层：渲染层跑的就是插件自己的进程。那边写的
// `if (!allow.has(name)) return denied` 只是**省一次 IPC** 的礼貌，不是边界 ——
// 任何能改 console 的人也能直接调 `window.desktop.surfaceCommand`。所以放行判定
// 必须在 bridge：它知道这份 index 是真的（flock 读盘），也知道宿主愿意开哪些只读命令。
//
// 两条闸同时成立才放行：
//  1. **插件声明**：名字在该 id 的 `surface.json.commands` 里（声明是给用户看的契约）；
//  2. **宿主允许**：名字在下面这张表里（表里只有只读命令，写类一律不开）。
//
// 判定顺序是刻意的：先 id → index(enabled) → manifest → 声明 → 宿主表。每条失败都
// 单独审计（reason 固定几类），因为「插件调了什么被拒了」是装完插件后唯一的对账材料。
//
// 错误文案是**固定集合**（denied / unknown_id / timeout / error）：插件的面板会把
// reason 显示给用户，把内部 error（git 的原始输出、绝对路径、栈）原样吐出去等于
// 借插件的手泄漏宿主内部结构。细节留在 audit.jsonl（本机、0600）里。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// surfaceCommand_* 允许/拒绝的封闭集合（也是回给插件的 reason 的全部取值）。
const (
	surfaceCmdDenied    = "denied"
	surfaceCmdUnknownID = "unknown_id"
	surfaceCmdTimeout   = "timeout"
	surfaceCmdError     = "error"
)

// surfaceCommandTimeout 单条宿主命令的上限。
//
// 5s：表里最强的 io 是 `git status --porcelain` + numstat（大仓库 ~1-2s）。超过这个数
// 基本是「工作区在别的进程里被锁死」或「挂在网络盘上」，回 timeout 比让面板转圈好。
// 刻意与 bridge 请求的超时（main.ts 的 10s）错开：这里先超时，插件拿到的是
// `timeout`，而不是主进程先放弃后转成的 `denied`（后者会把「慢」误报成「越权」）。
const surfaceCommandTimeout = 5 * time.Second

// surfaceHostCommand 一条宿主只读命令。
//
// read 直接拿到 manager 与 workspace：这些 handler 是**内部调用**（复用 dispatch 里
// 同一批函数），不是再走一遍 dispatch —— 后者会把「命令面」重新放大成任意命令。
// 参数一律从 args 里按需取，缺失即零值（与各命令自己的缺省口径一致）。
type surfaceHostCommand struct {
	// what 一句话说明（只给测试/排障读，不进给插件的错误文案）
	what string
	read func(m *manager, ctx context.Context, ws string, args map[string]any) (any, error)
}

// surfaceHostCommands 宿主**允许插件调用**的只读命令表 —— 唯一白名单。
//
// 只放四类，理由逐条：
//   - bootstrap：git_snapshot —— 面板最常见的内容就是「这个工作区现在怎么样」；
//   - workspace_list / session_list —— 「有哪些工作区/会话」是任何看板类面板的原料，
//     且两者都只读左侧栏已有的元数据，不碰磁盘上的对话正文；
//   - ui_inspect —— 观测自己（快照就在内存里），让插件能回答「我现在是不是被禁用了」。
//
// **没有**写类（file_save / workspace_add / session_delete / git_commit…）、没有
// provider/网络、没有审批/中断：插件写磁盘必须走自己的面板 UI 所依赖的既有通路，
// 不在这张表里开洞。加一条之前先问：这条命令被一个恶意插件连续调用 1000 次，用户
// 会损失什么？答不上来就别加（03-code-plan §5.1 的成本纪律同样适用于权限）。
var surfaceHostCommands = map[string]surfaceHostCommand{
	"git_snapshot": {
		what: "工作区 git 快照（分支/变更文件/± 行数）",
		read: func(m *manager, ctx context.Context, ws string, _ map[string]any) (any, error) {
			if ws == "" {
				return nil, fmt.Errorf("workspace 为空")
			}
			snap, err := gitSnapshot(ctx, ws)
			if err != nil {
				return nil, err
			}
			parseAheadBehind(ctx, ws, &snap)
			if snap.Files == nil {
				snap.Files = []gitFile{} // 干净仓库给空数组（与 git_snapshot 命令同一口径）
			}
			return snap, nil
		},
	},
	"workspace_list": {
		what: "工作区清单",
		read: func(m *manager, _ context.Context, _ string, _ map[string]any) (any, error) {
			return map[string]any{"workspaces": m.workspaceEntries()}, nil
		},
	},
	"session_list": {
		what: "某工作区的会话清单（元数据，不含对话正文）",
		read: func(m *manager, _ context.Context, ws string, args map[string]any) (any, error) {
			target := ws
			if v, ok := args["workspace"].(string); ok && strings.TrimSpace(v) != "" {
				target = strings.TrimSpace(v)
			}
			if target == "" {
				return nil, fmt.Errorf("workspace 为空")
			}
			return map[string]any{"sessions": m.listSessions(target)}, nil
		},
	},
	"ui_inspect": {
		what: "渲染层 UI 快照（≤256 KiB 的内存缓存，可能 stale）",
		read: func(m *manager, _ context.Context, _ string, _ map[string]any) (any, error) {
			raw, age, ok := m.ui.Snapshot()
			if !ok {
				return map[string]any{"ok": false, "reason": "no_snapshot"}, nil
			}
			return map[string]any{
				"ok":    true,
				"ageMs": age.Milliseconds(),
				"stale": age > uiStaleAfter,
				"data":  json.RawMessage(raw),
			}, nil
		},
	},
}

// surfaceHostCommandNames 宿主允许的命令名（码点排序；测试与文档读它）。
func surfaceHostCommandNames() []string {
	out := make([]string, 0, len(surfaceHostCommands))
	for name := range surfaceHostCommands {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// handleSurfaceCommand 处理 `{type:"surface_command", payload:{id,name,args}}`。
//
// 回执形状（渲染层 host.command 的 Promise 直接吃它）：
//
//	允许：command_response{ok:true, data:{result:<handler 结果>}}
//	拒绝：command_response{ok:false, code:<固定 reason>, data:{ok:false, reason}}
//
// **永不 panic、永不把内部 error 原样回给插件**：所有分支都收敛到上面两类。
func (m *manager) handleSurfaceCommand(c command) {
	id := strings.TrimSpace(str(c.Payload, "id"))
	name := strings.TrimSpace(str(c.Payload, "name"))
	args, _ := c.Payload["args"].(map[string]any)
	ws := strings.TrimSpace(str(c.Payload, "workspace"))
	if v, ok := args["workspace"].(string); ok && strings.TrimSpace(v) != "" {
		ws = strings.TrimSpace(v) // 显式 args 优先（看板类面板可能要读别的项目的 git）
	}

	deny := func(reason, auditReason string) {
		m.surfaces.auditCommand(auditID(id), name, false, auditReason)
		m.respCode(c, false, reason, reason, map[string]any{"ok": false, "reason": reason})
	}

	// ① id 本身：不合法的 id 连查表都不该发生（它是路径段，见 hai-surface:// 的同一判据）。
	if !surfaceIDPattern.MatchString(id) {
		deny(surfaceCmdDenied, "id 不合法："+clipText(id, 60))
		return
	}
	// ② 只有 enabled 的插件能调命令：禁用 = 连能力一起收走（不是「灰掉但还能用」）。
	if !m.surfaces.Enabled(id) {
		deny(surfaceCmdUnknownID, "index 中不存在或未启用")
		return
	}
	// ③ manifest 是声明的来源。读不出来 = 磁盘上这份插件已坏 → 拒绝（不猜）。
	mf, err := m.surfaces.Manifest(id)
	if err != nil {
		deny(surfaceCmdDenied, "manifest 不可读")
		return
	}
	// ④ 声明：manifest.commands 是用户装插件时看到的契约。
	if !surfaceDeclares(mf.Commands, name) {
		deny(surfaceCmdDenied, "未在 manifest.commands 中声明")
		return
	}
	// ⑤ 宿主表：声明了也不等于宿主答应。
	hc, ok := surfaceHostCommands[name]
	if !ok {
		deny(surfaceCmdDenied, "不在宿主只读命令白名单")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), surfaceCommandTimeout)
	defer cancel()
	out, err := hc.read(m, ctx, ws, args)
	if ctx.Err() == context.DeadlineExceeded {
		m.surfaces.auditCommand(id, name, false, "timeout")
		m.respCode(c, false, surfaceCmdTimeout, surfaceCmdTimeout, map[string]any{"ok": false, "reason": surfaceCmdTimeout})
		return
	}
	if err != nil {
		// 内部错误只进审计（本机 0600），回给插件的仍是固定文案。
		m.surfaces.auditCommand(id, name, false, surfaceCmdError+": "+err.Error())
		m.respCode(c, false, surfaceCmdError, surfaceCmdError, map[string]any{"ok": false, "reason": surfaceCmdError})
		return
	}
	m.surfaces.auditCommand(id, name, true, "")
	m.resp(c, true, "", map[string]any{"result": out})
}

// surfaceDeclares 名字是否在声明列表里（大小写敏感：命令名是协议字面量）。
func surfaceDeclares(declared []string, name string) bool {
	if name == "" {
		return false
	}
	for _, d := range declared {
		if strings.TrimSpace(d) == name {
			return true
		}
	}
	return false
}

// auditID 审计行的 id 字段：非法/超长 id 截断成可读的一条，绝不写空串。
// 空 id 的审计行等于「有人干了点什么」—— 那比没有审计更容易误导。
func auditID(id string) string {
	if id == "" {
		return "<empty>"
	}
	return clipText(id, 60)
}
