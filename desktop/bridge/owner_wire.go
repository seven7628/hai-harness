package main

// owner_wire.go —— 「这条状态属于谁」的宿主侧派生（owner 隔离的接线层）。
//
// 归属编码只有一份（owner 包）：workspace:<path> | agent:<id> | group:<id>。
// 本文件只做**从命令 payload / 会话身份到 owner** 的派生，供 hooks（信任）与 im（启用/路由）
// 两条链共用 —— 两处各写一套派生，迟早出现「设置页显示的归属」与「实际派发用的归属」不一致，
// 而那种不一致的表现是「明明确认过了却不生效」，最难查。

import (
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/owner"
)

// ownerOfSession 会话身份 → 归属（唯一派生入口）。
//
// Hub 会话 id 是身份真相源：hub.<agent>.dm → agent:<agent>；hub.<agent>.g.<group> → group:<group>；
// 其余（项目会话、cron 派生会话）→ workspace:<workspace>。
//
// 为什么要给 sessionID 而不是 workspace 优先：Hub 会话的 workspace 是智能体 home
// （~/.go-code/hub/homes/<id>），把它当项目工作区归属会让「所有智能体共用一个 fake 工作区」。
func ownerOfSession(sessionID, workspace string) string {
	return owner.FromSessionID(sessionID, workspace)
}

// ownerFromPayload 从命令 payload 派生归属。
//
// 优先级（显式 > 会话 > 工作区）：
//  1. payload.owner —— 前端显式给出的归属，必须合法（非法直接报错，不静默回落）；
//  2. payload.session_id —— Hub 会话按 agent/group，其余按 workspace；
//  3. payload.workspace —— workspace:<path>。
//
// 三者都没有 → 返回 ("", nil)：调用方决定是「未绑定」（列表展示）还是「拒绝」（写操作）。
// **绝不**默默使用全局：没有归属的信任/启用写入正是跨 owner 越权的入口。
func ownerFromPayload(c command) (string, error) {
	if raw := strings.TrimSpace(str(c.Payload, "owner")); raw != "" {
		if !owner.Valid(raw) {
			return "", fmt.Errorf("owner %q 非法（必须是 workspace:<路径> / agent:<id> / group:<id>）", raw)
		}
		return raw, nil
	}
	sid := strings.TrimSpace(str(c.Payload, "session_id"))
	ws := strings.TrimSpace(str(c.Payload, "workspace"))
	if sid != "" {
		if o := ownerOfSession(sid, ws); o != "" {
			return o, nil
		}
	}
	if ws != "" {
		return owner.Workspace(ws), nil
	}
	return "", nil
}

// ownerScopeLabel 归属的展示短标签（设置页/日志）：owner 包的 Label，空值给「未绑定」。
func ownerScopeLabel(id string) string {
	if id == "" {
		return "未绑定归属"
	}
	return owner.Label(id)
}
