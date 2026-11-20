# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

rep('''	start := time.Now()
	out := m.artifacts.PublishRun(bs.workspace, bs.id, ae.RunId, ae.ArtifactBundles)
	if m.artifactMetrics != nil {''','''	start := time.Now()
	out := m.artifacts.PublishRun(bs.workspace, bs.id, ae.RunId, ae.ArtifactBundles)
	// **必须写回事件**：AgentEnd 紧接着进 reducer → checkpoint → 事件日志，
	// 不写回的话三处都拿不到 Version（卡片有入口却打不开）。
	// 这是 v1 稿的真实缺陷：函数返回了带版本的副本，调用方却丢弃了返回值。
	ae.ArtifactBundles = out
	// 已打开的预览页要跟着刷新。常规路径是「write_file → AgentEnd 归档」，
	// artifact_edit 自己那次 Notify 只覆盖工具改稿这一条支路。
	// 版本没变（内容寻址去重）就不推，避免同一页无意义 reload 闪一下。
	if m.artifactPreview != nil {
		for _, b := range out {
			if b.Version == "" {
				continue
			}
			key := refKey(wsKey(bs.workspace), b.Id)
			if prev, ok := m.artifactNotified.Load(key); !ok || prev != b.Version {
				m.artifactNotified.Store(key, b.Version)
				m.artifactPreview.Notify(bs.workspace, bs.id, b.Id, b.Version, b.Entry)
			}
		}
	}
	if m.artifactMetrics != nil {''')

# manager field
rep('''	// artifactSweeper 版本库周期清扫（启动 + 每小时；见 artifact_sweep.go）。
	artifactSweeper *previewCacheSweeper''','''	// artifactSweeper 版本库周期清扫（启动 + 每小时；见 artifact_sweep.go）。
	artifactSweeper *previewCacheSweeper
	// artifactNotified 最近一次推给预览页的版本（key = refKey(wsKey,id)）：
	// 内容寻址去重后版本不变时不重复 reload（避免每次都闪一下）。
	artifactNotified sync.Map // map[string]string''')
open(p,'w').write(s)
print('ok')
