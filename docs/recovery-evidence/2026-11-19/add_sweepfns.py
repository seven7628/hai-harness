# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:80]
    s=s.replace(old,new,1)

rep('''func (h *artifactPreviewHost) lookupRef(k, id, version string) artifactPageRef {
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	return h.refs[refKey(k, id, version)]
}''','''func (h *artifactPreviewHost) lookupRef(k, id, version string) artifactPageRef {
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	return h.refs[refKey(k, id, version)]
}

// SweepRefs 清掉指向**已不存在版本**的预览索引（周期清扫的一部分）。
// 不清的话：用户删了版本/清了工作区，旧 ref 还在，URL 仍能生成，但 serveFile 每次 404 ——
// 表现是「预览白屏」，排查成本很高。
func (h *artifactPreviewHost) SweepRefs(_ time.Time) {
	if h == nil || h.svc == nil {
		return
	}
	h.refsMu.Lock()
	defer h.refsMu.Unlock()
	for key, ref := range h.refs {
		if _, err := h.svc.Version(ref.Workspace, ref.ArtifactID, ref.Version); err != nil {
			delete(h.refs, key)
		}
	}
}''')

rep('''func (s *artifactMetricsStore) Record(ev map[string]any) {''','''// RotateIfLarge 账本 > 8 MiB 时轮转成 metrics.jsonl.1（只保留 1 份）。
// 这是追加型账本，读路径只做统计分析；留全部历史没有意义，还会拖慢清扫时的目录遍历。
func (s *artifactMetricsStore) RotateIfLarge() {
	if s == nil || s.path == "" {
		return
	}
	st, err := os.Stat(s.path)
	if err != nil || st.Size() < 8<<20 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = os.Rename(s.path, s.path+".1")
}

func (s *artifactMetricsStore) Record(ev map[string]any) {''')
open(p,'w').write(s)
print('ok')
