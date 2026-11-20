# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
old='''```go
func (t *artifactPublishTool) Exposure() tools.ToolExposure { return tools.ExposureDirect }
func (t *artifactPublishTool) Annotations() tools.ToolAnnotations {
	return tools.ToolAnnotations{IdempotentHint: true, OpenWorldHint: false}
}
func (t *artifactPublishTool) AdvertiseCodemode() bool { return true }
```'''
new='''```go
func (t *artifactPublishTool) Exposure() tools.ToolExposure { return tools.ExposureDirect }
func (t *artifactPublishTool) Annotations() tools.ToolAnnotations {
	return tools.ToolAnnotations{IdempotentHint: true, OpenWorldHint: false}
}
func (t *artifactPublishTool) AdvertiseCodemode() bool { return true }
func (t *artifactPublishTool) ToolTimeout() time.Duration { return 10 * time.Second }

// 只读工具同时实现 ReadOnlyTool：runTools 的只读判定与 codemode 的串行门都读它。
func (t *artifactSourceGetTool) ReadOnly() bool { return true }
func (t *artifactSourceGetTool) AdvertiseCodemode() bool { return true }
```'''
assert old in s
s=s.replace(old,new,1)
open(p,'w').write(s)
print('ok')
