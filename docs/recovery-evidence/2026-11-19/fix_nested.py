# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
old='''	// file 可以脱离 artifact（源码面板引用工作区文件）；其余类型必须有版本 ——
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
	}'''
new='''	sel, err := json.Marshal(c.Payload["selection"])
	if err != nil || len(sel) == 0 || len(sel) > selectionRefMaxBytes {
		m.resp(c, false, "selection payload missing or too large", nil)
		return
	}
	// 版本要求按类型分档：file 可以脱离 artifact（源码面板引用工作区文件），
	// 其余类型必须有版本 —— 没有版本无法回答「你说的是哪一版」，source_get 也无从定位。
	if kind == selFile {
		var f struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(sel, &f); err != nil || strings.TrimSpace(f.Path) == "" {
			m.resp(c, false, "file selection requires selection.path", nil)
			return
		}
	} else if id == "" || version == "" {
		m.resp(c, false, "artifact_id and version are required for this kind", nil)
		return
	}'''
assert old in s
s=s.replace(old,new,1)
open(p,'w').write(s)
print('ok')
