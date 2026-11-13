package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// hooks_commands_test.go —— 钩子面板三件套（契约 §5.3/§5.4/§5.5、§8）。

// hooksBody 造一个 hooks 段 payload（面板保存形态）。
func hooksBody(events map[string]any, extra map[string]any) map[string]any {
	body := map[string]any{"events": events}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// hookGroup 造一个 matcher 组 payload（handler 命令 = command）。
func hookGroup(matcher, command string, extra map[string]any) map[string]any {
	h := map[string]any{"type": "command", "command": command}
	for k, v := range extra {
		h[k] = v
	}
	g := map[string]any{"hooks": []any{h}}
	if matcher != "" {
		g["matcher"] = matcher
	}
	return g
}

// hooksList 发 hooks_list 并返回 data（workspaces 走 JSON 形态 []any）。
func hooksList(t *testing.T, m *manager, buf *syncBuf, id int, workspaces ...string) map[string]any {
	t.Helper()
	paths := make([]any, 0, len(workspaces))
	for _, w := range workspaces {
		paths = append(paths, w)
	}
	m.dispatch(cmd(id, "hooks_list", map[string]any{"workspaces": paths}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("hooks_list 响应异常: %v", resp)
	}
	data, _ := resp["data"].(map[string]any)
	if data == nil || data["user"] == nil {
		t.Fatalf("hooks_list data 缺 user: %v", resp["data"])
	}
	return data
}

// hookTrustOf data.trust 里的命令状态（缺省 = untrusted，与前端读取一致）。
func hookTrustOf(data map[string]any, cmd string) string {
	trust, _ := data["trust"].(map[string]any)
	if v, ok := trust[cmd].(string); ok {
		return v
	}
	return "untrusted"
}

// hooksProjectOf 取 data.projects 里某个 workspace 的视图。
func hooksProjectOf(t *testing.T, data map[string]any, ws string) map[string]any {
	t.Helper()
	list, _ := data["projects"].([]any)
	for _, p := range list {
		pm, _ := p.(map[string]any)
		if w, _ := pm["workspace"].(string); w == ws {
			return pm
		}
	}
	t.Fatalf("projects 缺 workspace=%s: %v", ws, data["projects"])
	return nil
}

// eventsOf body.events 里某事件的组数组。
func eventsOf(body map[string]any, ev string) []any {
	evs, _ := body["events"].(map[string]any)
	groups, _ := evs[ev].([]any)
	return groups
}

// TestHooksListSources：三类来源（用户 / Claude 兼容 / 项目）+ 合并视角 + 信任 + 告警，
// 且**不创建运行态**。
func TestHooksListSources(t *testing.T) {
	m, buf := testManager(t)
	home := os.Getenv("HOME")
	wsA, wsB := t.TempDir(), t.TempDir()
	userCmd, claudeCmd, projCmd, claudeProjCmd := "/hooks/user.sh", "/hooks/claude.sh", "/hooks/proj.sh", "/hooks/claude-proj.sh"

	// 用户级：settings.json 的 hooks 段（另有 provider 段，不该被当成未知键）；
	// 含一个非法 matcher（运行期告警 → warnings）与一个未知事件（不进模型 → unknownKeys）
	if err := os.MkdirAll(filepath.Join(home, ".go-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	userSettings := map[string]any{
		"provider": map[string]any{"active": "deepseek"},
		"hooks": map[string]any{
			"disableAllHooks": false,
			"events": map[string]any{
				"PreToolUse": []any{
					hookGroup("bash", userCmd, nil),
					hookGroup("[unclosed", "/hooks/bad-regex.sh", nil), // 非法正则 → Load 告警
				},
				"Notification": []any{hookGroup("", "/hooks/future.sh", nil)}, // 未知事件
			},
			"thirdPartyKey": 1,
		},
	}
	b, _ := json.Marshal(userSettings)
	if err := os.WriteFile(filepath.Join(home, ".go-code", "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// 兼容层：~/.claude/settings.json（只读）
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	claudeSettings := map[string]any{"hooks": map[string]any{"events": map[string]any{"Stop": []any{hookGroup("", claudeCmd, nil)}}}}
	b, _ = json.Marshal(claudeSettings)
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	// 项目级：wsA 有自己的 hooks.json + .claude 兼容层；wsB 什么都没配
	if err := os.MkdirAll(filepath.Join(wsA, ".go-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	projBody := map[string]any{"allowProjectHooks": true, "events": map[string]any{"PreToolUse": []any{hookGroup("bash|write_file", projCmd, nil)}}}
	b, _ = json.Marshal(projBody)
	if err := os.WriteFile(filepath.Join(wsA, ".go-code", "hooks.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wsA, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(map[string]any{"hooks": map[string]any{"events": map[string]any{"PostToolUse": []any{hookGroup("write_file", claudeProjCmd, nil)}}}})
	if err := os.WriteFile(filepath.Join(wsA, ".claude", "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	wsGone := filepath.Join(t.TempDir(), "deleted")

	data := hooksList(t, m, buf, 1, wsA, wsB, wsGone)

	// 不创建运行态（打开设置页不得建会话/连 MCP/起 watcher）
	for _, p := range []string{wsA, wsB, wsGone} {
		if rt := m.peekRuntime(p); rt != nil {
			t.Fatalf("hooks_list 不得创建运行态: %s", p)
		}
	}

	user, _ := data["user"].(map[string]any)
	if user["id"] != "user" || user["kind"] != "user" || user["title"] != "用户配置" || user["editable"] != true {
		t.Fatalf("user 视图基础字段错: %v", user)
	}
	if user["present"] != true || user["ok"] != true {
		t.Fatalf("user 应 present+ok: %v", user)
	}
	// 未知顶层键（provider 是 settings.json 的兄弟键，不算 hooks 段的未识别键）
	if keys, _ := user["unknownKeys"].([]any); len(keys) != 2 {
		t.Fatalf("unknownKeys 应含 thirdPartyKey 与未知事件名: %v", user["unknownKeys"])
	}
	body, _ := user["body"].(map[string]any)
	if len(eventsOf(body, "PreToolUse")) != 2 {
		t.Fatalf("user body 事件未回读: %v", body)
	}
	if evs, _ := body["events"].(map[string]any); evs["Notification"] != nil {
		t.Fatalf("未知事件不该进可编辑模型: %v", evs)
	}

	claude, _ := data["claudeUser"].(map[string]any)
	if claude == nil || claude["editable"] != false || claude["present"] != true || claude["kind"] != "claude-user" {
		t.Fatalf("claudeUser 视图错: %v", claude)
	}

	pa := hooksProjectOf(t, data, wsA)
	if pa["id"] != "project:"+wsA || pa["kind"] != "project" || pa["title"] != filepath.Base(wsA) ||
		pa["exists"] != true || pa["present"] != true || pa["editable"] != true {
		t.Fatalf("项目视图基础字段错: %v", pa)
	}
	if pa["path"] != filepath.Join(wsA, ".go-code", "hooks.json") {
		t.Fatalf("项目 hooks 路径错: %v", pa["path"])
	}
	if cl := pa["claude"].([]any); len(cl) != 1 {
		t.Fatalf("项目 .claude 兼容层应只列存在的文件: %v", pa["claude"])
	} else if cm, _ := cl[0].(map[string]any); cm["path"] != filepath.Join(wsA, ".claude", "settings.json") {
		t.Fatalf("项目 claude 条目的 path 错: %v", cm)
	}
	pb := hooksProjectOf(t, data, wsB)
	if pb["present"] != false || pb["ok"] != true {
		t.Fatalf("未配置项目应 present=false + ok=true: %v", pb)
	}
	if keys, _ := pb["unknownKeys"].([]any); len(keys) != 0 {
		t.Fatalf("未配置项目不该有 unknownKeys: %v", pb["unknownKeys"])
	}
	pg := hooksProjectOf(t, data, wsGone)
	if pg["exists"] != false {
		t.Fatalf("目录被删的项目应 exists=false（前端过滤）: %v", pg)
	}

	// effective：合并视角（用户层 + 兼容层；项目层各自判定）
	eff, _ := data["effective"].(map[string]any)
	if eff["readClaudeSettings"] != true {
		t.Fatalf("readClaudeSettings 缺省应为 true: %v", eff)
	}
	sources, _ := eff["sources"].([]any)
	if len(sources) != 2 {
		t.Fatalf("sources 应列出实际读到的两个文件: %v", sources)
	}

	// warnings：非法 matcher 的告警可见（"配了却不跑"的第一现场）
	warns, _ := data["warnings"].([]any)
	joined := ""
	for _, w := range warns {
		joined += w.(string) + "\n"
	}
	if !strings.Contains(joined, "invalid matcher") {
		t.Fatalf("应包含非法 matcher 告警: %v", warns)
	}

	// trust：用户级默认可信（会执行）；项目级/兼容层项目默认未信任（安全默认）
	if got := hookTrustOf(data, userCmd); got != "trusted" {
		t.Fatalf("用户级命令应默认可信，实为 %s", got)
	}
	if got := hookTrustOf(data, claudeCmd); got != "trusted" {
		t.Fatalf("claude-user 命令应默认可信，实为 %s", got)
	}
	if got := hookTrustOf(data, projCmd); got != "untrusted" {
		t.Fatalf("项目级命令应默认未信任，实为 %s", got)
	}
	if got := hookTrustOf(data, claudeProjCmd); got != "untrusted" {
		t.Fatalf("项目 .claude 命令应默认未信任，实为 %s", got)
	}

	// claudeUser 文件不存在 → null（前端据此隐藏卡片）
	if err := os.Remove(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	data = hooksList(t, m, buf, 2, wsA)
	if data["claudeUser"] != nil {
		t.Fatalf("兼容层文件不存在应返回 null: %v", data["claudeUser"])
	}
}

// TestHooksSetUserAndProject：两种 scope 的写盘位置/内容 + 严格校验拒绝。
func TestHooksSetUserAndProject(t *testing.T) {
	m, buf := testManager(t)
	home := os.Getenv("HOME")
	ws := t.TempDir()

	// 用户级：settings.json 里的其他段必须保留
	if err := os.MkdirAll(filepath.Join(home, ".go-code"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".go-code", "settings.json"),
		[]byte(`{"provider":{"active":"deepseek"},"mesh":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.dispatch(cmd(1, "hooks_set", map[string]any{
		"scope": "user",
		"body": hooksBody(map[string]any{
			"PreToolUse": []any{hookGroup("bash", "/hooks/user.sh", map[string]any{"timeout": 10, "statusMessage": "linting"})},
		}, map[string]any{"disableAllHooks": true, "disabled": []any{"noisy"}}),
	}))
	resp, _ := lastResp(t, buf)
	if resp["ok"] != true {
		t.Fatalf("user scope 保存应通过: %v", resp)
	}
	if d, _ := resp["data"].(map[string]any); d["changed"] != true {
		t.Fatalf("响应 data.changed 应为 true: %v", resp["data"])
	}
	file := jsonFile(t, filepath.Join(home, ".go-code", "settings.json"))
	if _, keep := file["provider"]; !keep {
		t.Fatalf("settings.json 其他段被抹掉: %v", file)
	}
	if _, keep := file["mesh"]; !keep {
		t.Fatalf("settings.json 其他段被抹掉: %v", file)
	}
	hooksSec, _ := file["hooks"].(map[string]any)
	if hooksSec == nil || hooksSec["disableAllHooks"] != true {
		t.Fatalf("hooks 段未写入: %v", file["hooks"])
	}
	groups := eventsOf(hooksSec, "PreToolUse")
	if len(groups) != 1 || groups[0].(map[string]any)["matcher"] != "bash" {
		t.Fatalf("hooks 段内容错: %v", hooksSec)
	}
	// 回读一致（面板保存后以磁盘为准回拉）
	data := hooksList(t, m, buf, 2, ws)
	userBody, _ := data["user"].(map[string]any)["body"].(map[string]any)
	if userBody["disableAllHooks"] != true || len(eventsOf(userBody, "PreToolUse")) != 1 {
		t.Fatalf("回读不一致: %v", userBody)
	}

	// 项目级：整文件 = hooks 段（{ws}/.go-code/hooks.json）
	m.dispatch(cmd(3, "hooks_set", map[string]any{
		"scope": "project", "workspace": ws,
		"body": hooksBody(map[string]any{"Stop": []any{hookGroup("", "/hooks/stop.sh", nil)}}, map[string]any{"allowProjectHooks": true}),
	}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("project scope 保存应通过: %v", resp)
	}
	pfile := jsonFile(t, filepath.Join(ws, ".go-code", "hooks.json"))
	if pfile["allowProjectHooks"] != true {
		t.Fatalf("项目文件内容错: %v", pfile)
	}
	if len(eventsOf(pfile, "Stop")) != 1 {
		t.Fatalf("项目文件事件错: %v", pfile)
	}
	// 整文件形态：不该有 events 包裹之外的外层键（不是 settings.json 形态）
	if _, hasWrapper := pfile["hooks"]; hasWrapper {
		t.Fatalf("项目文件应是整文件 hooks 段（不该再套一层 hooks）: %v", pfile)
	}
	data = hooksList(t, m, buf, 4, ws)
	if p := hooksProjectOf(t, data, ws); p["present"] != true {
		t.Fatalf("项目来源应 present: %v", p)
	}

	// 严格校验（拒绝式）：未知事件 / 非 command 类型 / 负超时 / 非法 matcher 正则 / 空命令
	before, _ := os.ReadFile(filepath.Join(ws, ".go-code", "hooks.json"))
	for i, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"未知事件", hooksBody(map[string]any{"Nope": []any{hookGroup("", "/x/a.sh", nil)}}, nil), "未知事件 Nope"},
		{"handler 类型", hooksBody(map[string]any{"Stop": []any{hookGroup("", "/x/a.sh", map[string]any{"type": "http"})}}, nil), "暂不支持的 handler 类型"},
		{"负超时", hooksBody(map[string]any{"Stop": []any{hookGroup("", "/x/a.sh", map[string]any{"timeout": -1})}}, nil), "超时不能为负数"},
		{"非法 matcher", hooksBody(map[string]any{"PreToolUse": []any{hookGroup("[unclosed", "/x/a.sh", nil)}}, nil), "matcher 非法正则"},
		{"空命令", hooksBody(map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command"}}}}}, nil), "命令不能为空"},
	} {
		m.dispatch(cmd(10+i, "hooks_set", map[string]any{
			"scope": "project", "workspace": ws, "body": tc.body,
		}))
		resp, _ := lastResp(t, buf)
		if resp["ok"] != false || !strings.Contains(resp["error"].(string), tc.want) {
			t.Fatalf("%s 应拒绝并给出可读原因（含 %q）: %v", tc.name, tc.want, resp)
		}
		after, _ := os.ReadFile(filepath.Join(ws, ".go-code", "hooks.json"))
		if string(after) != string(before) {
			t.Fatalf("%s 校验失败却写了盘", tc.name)
		}
	}

	// scope / workspace 参数校验
	m.dispatch(cmd(30, "hooks_set", map[string]any{"scope": "galaxy", "body": hooksBody(nil, nil)}))
	if resp, _ := lastResp(t, buf); resp["ok"] != false || !strings.Contains(resp["error"].(string), "未知 scope") {
		t.Fatalf("未知 scope 应拒绝: %v", resp)
	}
	m.dispatch(cmd(31, "hooks_set", map[string]any{"scope": "project", "body": hooksBody(nil, nil)}))
	if resp, _ := lastResp(t, buf); resp["ok"] != false || !strings.Contains(resp["error"].(string), "workspace 为空") {
		t.Fatalf("项目 scope 缺 workspace 应拒绝: %v", resp)
	}
	m.dispatch(cmd(32, "hooks_set", map[string]any{"scope": "project", "workspace": filepath.Join(t.TempDir(), "gone"), "body": hooksBody(nil, nil)}))
	if resp, _ := lastResp(t, buf); resp["ok"] != false {
		t.Fatalf("目录不存在的项目应拒绝: %v", resp)
	}

	// 空 body（用户级）= 清空 hooks 事件（段仍在，其他键保留）
	m.dispatch(cmd(33, "hooks_set", map[string]any{"scope": "user", "body": map[string]any{"events": map[string]any{}}}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("清空应通过: %v", resp)
	}
	file = jsonFile(t, filepath.Join(home, ".go-code", "settings.json"))
	if _, keep := file["provider"]; !keep {
		t.Fatalf("清空 hooks 不该动其他段: %v", file)
	}
	if groups := eventsOf(file["hooks"].(map[string]any), "PreToolUse"); len(groups) != 0 {
		t.Fatalf("清空后不该有事件: %v", file["hooks"])
	}
}

// TestHooksTrustImmediateEffectAndHotApply：hooks_set 热替换到会话 + 信任即刻生效
// （信任判定在每次派发时查共享信任库 → 不需要重建会话）。
func TestHooksTrustImmediateEffectAndHotApply(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran.txt")
	// 命令把 "ran" 写进标记文件（stdout 保持空：PreToolUse 的 stdout 会被当结果 JSON 解析）
	cmdStr := "printf ran >> " + marker

	bs, err := m.create(ws, "s-hooks-hot", "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if h := bs.s.Hooks(); h != nil {
		t.Fatalf("未配置时不该有 hooks: %+v", h)
	}

	// 项目级 hooks：allowProjectHooks 由项目文件自举（peekAllowProject）
	m.dispatch(cmd(1, "hooks_set", map[string]any{
		"scope": "project", "workspace": ws,
		"body": hooksBody(map[string]any{"PreToolUse": []any{hookGroup("bash", cmdStr, map[string]any{"timeout": 5})}},
			map[string]any{"allowProjectHooks": true}),
	}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("hooks_set 应通过: %v", resp)
	}

	// 会话已热替换（下一次 Run 生效）：拿到的就是新配置
	h := bs.s.Hooks()
	if h == nil || h.PreToolUse == nil {
		t.Fatalf("hooks_set 应把新配置推给已存在的会话: %+v", h)
	}
	// 未信任 → 不执行（项目层安全默认），但派发本身有告警可见
	call := events.PreToolUseContext{Call: core.ToolCall{Name: "bash", Arguments: "{}"}}
	_ = h.PreToolUse(context.Background(), call)
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("未信任的项目级 hook 不该执行")
	}

	// hooks_trust → 即刻生效（同一份 hooks 对象，查库发生在派发时）
	m.dispatch(cmd(2, "hooks_trust", map[string]any{"commands": []any{cmdStr}, "scope": "project", "reason": "测试确认", "workspace": ws}))
	resp, _ := lastResp(t, buf)
	if resp["ok"] != true {
		t.Fatalf("hooks_trust 应通过: %v", resp)
	}
	if d, _ := resp["data"].(map[string]any); d["trusted"] != float64(1) {
		t.Fatalf("trusted 计数错: %v", resp["data"])
	}
	_ = bs.s.Hooks().PreToolUse(context.Background(), call)
	ran, err := os.ReadFile(marker)
	if err != nil || string(ran) != "ran" {
		t.Fatalf("信任后应即刻执行（无需重建会话）: %v / %s", err, ran)
	}
	// 信任状态在 hooks_list 里同步可见
	data := hooksList(t, m, buf, 3, ws)
	// 信任按来源 owner 存：项目卡读自己的 trust，顶层 trust 只代表「当前 owner」视图。
	project := hooksProjectOf(t, data, ws)
	if got := hookTrustOf(project, cmdStr); got != "trusted" {
		t.Fatalf("信任后 hooks_list 应显示 trusted，实为 %s", got)
	}
	// 信任库落在 ~/.go-code/hooks-state.json（与 settings.json 分开，避免互相覆盖）
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".go-code", "hooks-state.json")); err != nil {
		t.Fatalf("信任库文件缺失: %v", err)
	}

	// 空命令跳过、空列表不报错（前端"信任此项目的钩子"在无钩子时是 no-op）
	m.dispatch(cmd(4, "hooks_trust", map[string]any{"commands": []any{"  "}, "scope": "project", "workspace": ws}))
	if d, _ := lastResp(t, buf); d["ok"] != true || d["data"].(map[string]any)["trusted"] != float64(0) {
		t.Fatalf("空命令应跳过且计数为 0: %v", d)
	}

	// 项目 hooks 再改命令 → 内容哈希不匹配 → 自动回到未信任（modified 需重新确认）
	newCmd := "printf ran2 >> " + marker
	m.dispatch(cmd(5, "hooks_set", map[string]any{
		"scope": "project", "workspace": ws,
		"body": hooksBody(map[string]any{"PreToolUse": []any{hookGroup("bash", newCmd, nil)}},
			map[string]any{"allowProjectHooks": true}),
	}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("改命令应通过: %v", resp)
	}

	// 清空项目 hooks → 派发器消失（零开销路径），会话拿到 nil
	m.dispatch(cmd(6, "hooks_set", map[string]any{
		"scope": "project", "workspace": ws,
		"body": hooksBody(map[string]any{}, map[string]any{"allowProjectHooks": true}),
	}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("清空应通过: %v", resp)
	}
	if h := bs.s.Hooks(); h != nil {
		t.Fatalf("配置清空后会话应回到零开销路径（hooks = nil）: %+v", h)
	}
}

// TestHooksSetUserAppliesToAllWorkspaces：user scope 的热应用覆盖全部已打开的工作区。
func TestHooksSetUserAppliesToAllWorkspaces(t *testing.T) {
	m, buf := testManager(t)
	wsA, wsB := t.TempDir(), t.TempDir()
	bsA, err := m.create(wsA, "s-a", "")
	if err != nil {
		t.Fatal(err)
	}
	bsB, err := m.create(wsB, "s-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if bsA.s.Hooks() != nil || bsB.s.Hooks() != nil {
		t.Fatalf("未配置时不该有 hooks")
	}
	m.dispatch(cmd(1, "hooks_set", map[string]any{
		"scope": "user",
		"body":  hooksBody(map[string]any{"Stop": []any{hookGroup("", "/hooks/stop.sh", nil)}}, nil),
	}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("user scope 保存应通过: %v", resp)
	}
	if bsA.s.Hooks() == nil || bsA.s.Hooks().Stop == nil {
		t.Fatalf("user 层变更应推给 wsA 的会话")
	}
	if bsB.s.Hooks() == nil || bsB.s.Hooks().Stop == nil {
		t.Fatalf("user 层变更应推给 wsB 的会话（跨工作区）")
	}
	// 无运行态的工作区不受影响、也不被创建
	wsCold := t.TempDir()
	m.dispatch(cmd(2, "hooks_set", map[string]any{
		"scope": "user",
		"body":  hooksBody(map[string]any{"PreToolUse": []any{hookGroup("bash", "/hooks/x.sh", nil)}}, nil),
	}))
	if resp, _ := lastResp(t, buf); resp["ok"] != true {
		t.Fatalf("再次保存应通过: %v", resp)
	}
	if m.peekRuntime(wsCold) != nil {
		t.Fatalf("hooks_set 不得创建运行态")
	}
}
