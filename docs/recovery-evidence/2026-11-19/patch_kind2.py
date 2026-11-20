# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

# 1) buildSelectionMessage: kind attribute
rep('''	for _, r := range refs {
		b.WriteString("\\n\\n<selection artifact=\\"")
		b.WriteString(r.Artifact)
		b.WriteString("\\" version=\\"")
		b.WriteString(r.Version)
		b.WriteString("\\" entry=\\"")
		b.WriteString(r.Entry)
		b.WriteString("\\">\\n")
		b.Write(r.Payload)
		b.WriteString("\\n</selection>")
	}''','''	for _, r := range refs {
		b.WriteString("\\n\\n<selection kind=\\"")
		b.WriteString(r.Kind)
		b.WriteString("\\" artifact=\\"")
		b.WriteString(r.Artifact)
		b.WriteString("\\" version=\\"")
		b.WriteString(r.Version)
		b.WriteString("\\" entry=\\"")
		b.WriteString(r.Entry)
		b.WriteString("\\">\\n")
		b.Write(r.Payload)
		b.WriteString("\\n</selection>")
	}''')

# 2) handler: kind parsing + per-kind requirement
rep('''	id := str(c.Payload, "artifact_id")
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
	}''','''	kind := str(c.Payload, "kind")
	if kind == "" {
		kind = selElement // 兼容旧 inspector（只发元素）
	}
	if !validSelectionKind(kind) {
		m.resp(c, false, "unknown selection kind: "+kind, nil)
		return
	}
	id := str(c.Payload, "artifact_id")
	version := str(c.Payload, "version")
	// file 可以脱离 artifact（源码面板引用工作区文件）；其余类型必须有版本 ——
	// 没有版本的选区无法回答「你说的是哪一版」，source_get 也无从定位。
	if kind == selFile {
		if str(c.Payload, "selection", "path") == "" {
			m.resp(c, false, "file selection requires selection.path", nil)
			return
		}
	} else if id == "" || version == "" {
		m.resp(c, false, "artifact_id and version are required for this kind", nil)
		return
	}
	sel, err := json.Marshal(c.Payload["selection"])
	if err != nil || len(sel) == 0 || len(sel) > selectionRefMaxBytes {
		m.resp(c, false, "selection payload missing or too large", nil)
		return
	}
	// 版本必须真实存在：页面是模型生成的，它报的 version 不可信；这也是「过期引用」
	// 唯一能确定性拒绝的地方（版本被清理 → 这里 404，而不是发出去让模型猜）。
	var entry string
	if id != "" && version != "" {
		v, verr := m.artifacts.Version(ws, id, version)
		if verr != nil {
			m.respCode(c, false, "selection target version not found", "selection_ref_stale",
				map[string]any{"artifact_id": id})
			return
		}
		entry = v.Manifest.Entry
	}
	ref := &selectionRef{
		WS: ws, SID: sid, Kind: kind, Artifact: id, Version: version, Entry: entry,
		Payload: sel, Label: selectionLabel(kind, id, entry, sel),
	}''')

# 3) selectionLabel signature
rep('''func selectionLabel(id, entry string, raw json.RawMessage) string {
	var s struct {
		Tag  string `json:"tag"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &s)''','''func selectionLabel(kind, id, entry string, raw json.RawMessage) string {
	var s struct {
		Tag  string `json:"tag"`
		Text string `json:"text"`
		Path string `json:"path"`
	}
	_ = json.Unmarshal(raw, &s)
	if kind == selFile && s.Path != "" {
		return s.Path // 文件引用：chip 上直接显示路径
	}
	if kind == selPage || kind == selArtifact {
		return fmt.Sprintf("%s · %s", id, filepath.Base(entry))
	}''')
open(p,'w').write(s)
print('ok')
