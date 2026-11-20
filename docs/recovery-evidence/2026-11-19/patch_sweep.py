# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:80]
    s=s.replace(old,new,1)

rep('''func (m *manager) sweepArtifacts() {
	if m == nil || m.artifacts == nil {
		return
	}
	root := m.artifacts.root
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			_ = m.artifacts.sweepWorkspace(filepath.Join(root, e.Name()))
		}
	}
}''','''func (m *manager) sweepArtifacts() {
	if m == nil || m.artifacts == nil {
		return
	}
	root := m.artifacts.root
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			_ = m.artifacts.sweepWorkspace(filepath.Join(root, e.Name()))
		}
	}
	// 同一个 tick 顺带清三张**内存表**：它们的键都含版本/会话，磁盘清扫不会替它们收尸。
	now := time.Now()
	if m.selectionRefs != nil {
		m.selectionRefs.Sweep(now) // 过期引用（30 分钟）
	}
	if m.artifactPreview != nil {
		m.artifactPreview.SweepRefs(now) // 预览索引：指向已删版本的 ref 留着就会 404
	}
	if m.artifactMetrics != nil {
		m.artifactMetrics.RotateIfLarge() // jsonl > 8MiB 时轮转（保留 1 份）
	}
}''')
open(p,'w').write(s)
print('ok')
