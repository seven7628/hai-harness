# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
anchor = '''### 23.3 发送时注入：`<selection>` 是数据，不是指令'''
addition = r'''### 23.2b 命令入口：`handleArtifactSelection`

**定位**：`desktop/bridge/artifact_commands.go`（§8.7 的命令族），由 `main.go` 的
`case CmdArtifactSelection:` 调用。

```go
// handleArtifactSelection 预览页的两种提交：
//   mode=reference → 只存一条引用，返回 refId（前端放 chip）；
//   mode=edit      → 校验空闲 → 激活写守卫 → 立刻 Ask 一条改稿任务。
//
// 为什么两种模式都先落 ref：直接改也要能被 Composer 记住（用户在等模型时可以再引用一次）；
// 引用表是**唯一**的选区事实源，页面/渲染层都不保留副本。
func (m *manager) handleArtifactSelection(c command, ws, sid string) {
	bs := m.get(ws, sid)
	if bs == nil {
		m.respCode(c, false, "session not found", "session_not_found", nil)
		return
	}
	id := str(c.Payload, "artifact_id")
	version := str(c.Payload, "version")
	if id == "" || version == "" {
		m.resp(c, false, "artifact_id and version are required", nil)
		return
	}
	sel, err := json.Marshal(c.Payload["selection"])
	if err != nil || len(sel) == 0 || len(sel) > selectionRefMaxBytes {
		m.resp(c, false, "selection payload missing or too large", nil)
		return
	}
	// 版本必须真实存在：页面是模型生成的，它报的 version 不可信；这也是「过期引用」
	// 唯一能确定性拒绝的地方（版本被清理 → 这里 404，而不是发出去让模型猜）。
	v, err := m.artifacts.Version(ws, id, version)
	if err != nil {
		m.respCode(c, false, "selection target version not found", "selection_ref_stale",
			map[string]any{"artifact_id": id})
		return
	}
	ref := &selectionRef{
		WS: ws, SID: sid, Artifact: id, Version: version, Entry: v.Manifest.Entry,
		Payload: sel, Label: selectionLabel(id, v.Manifest.Entry, sel),
	}
	refID, err := m.selectionRefs.Put(ref)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	mode := str(c.Payload, "mode")
	if mode == "" || mode == "reference" {
		m.resp(c, true, "", map[string]any{
			"ref_id": refID, "label": ref.Label, "artifact_id": id,
			"version": version, "selector": selectorOf(sel),
		})
		return
	}
	if mode != "edit" {
		m.resp(c, false, "unknown mode: "+mode, nil)
		return
	}
	// 直接改：忙就拒绝（scope 是会话级，激活会把正在跑的 run 一起拦掉，见 §24 R18）。
	if bs.s.IsRunning() {
		m.respCode(c, false, "session is busy; wait for the current run to finish",
			"session_busy", nil)
		return
	}
	// 顺序不能换：先 set（守卫生效）→ 再 Ask。反过来会留一个「run 已启动、守卫还没生效」的窗口。
	bs.selectionScope.set(id, v.Manifest.Entry, manifestPaths(v))
	instruction := strings.TrimSpace(str(c.Payload, "instruction"))
	if instruction == "" {
		instruction = "Apply the requested change to the selected element."
	}
	if err := bs.s.Ask(context.Background(), buildSelectionMessage(instruction, []*selectionRef{ref})); err != nil {
		bs.selectionScope.clear()
		m.resp(c, false, err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"ref_id": refID, "started": true})
}

// manifestPaths 版本内全部文件路径（顺序稳定）：写守卫的允许集合。
func manifestPaths(v *artifactVersion) []string {
	out := make([]string, 0, len(v.Manifest.Files))
	for _, f := range v.Manifest.Files {
		out = append(out, f.Path)
	}
	return out
}

// selectorOf / selectionLabel 只从已裁剪的 selection JSON 里取展示字段，不解析 DOM。
func selectorOf(raw json.RawMessage) string {
	var s struct {
		Selector string `json:"selector"`
	}
	_ = json.Unmarshal(raw, &s)
	return s.Selector
}

func selectionLabel(id, entry string, raw json.RawMessage) string {
	var s struct {
		Tag  string `json:"tag"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &s)
	tag := s.Tag
	if tag == "" {
		tag = filepath.Base(entry)
	}
	t := strings.TrimSpace(s.Text)
	if len([]rune(t)) > 24 {
		t = string([]rune(t)[:24]) + "…"
	}
	if t == "" {
		return fmt.Sprintf("%s · %s", id, tag)
	}
	return fmt.Sprintf("%s · %s「%s」", id, tag, t)
}
```

> `selection.changed`（悬停/点选）**不**落 ref：它每次点选都发，落表会把 20 条上限瞬间打满。
> 只有 `selection.commit`（用户按了「引用」或「直接改」）才进表。

'''
assert anchor in s
s=s.replace(anchor, addition+anchor,1)
open(p,'w').write(s)
print('ok')
