# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:80]
    s=s.replace(old,new,1)

rep("""		m.registerOfficeTools(eng, ws.path)
		// 产物工具：与办公工具同一层（loop 建一次、kit/开关不可用就不注册）。
		// 注册失败不阻断：模型只是看不到这些工具，其余能力照常。
		m.registerArtifactTools(eng, ws.path)""",
"""		m.registerOfficeTools(eng, ws.path)
		// 产物工具：与办公工具同一层（每次 buildLoop 注册一次；开关关掉就不注册）。
		// 需要 sid：声明表与指标都要按会话归属（见 artifact_tools.go 的签名）。
		m.registerArtifactTools(eng, ws.path, sid)""")

rep("+	if m.artifactsEnabled(ws.path) {", "+	if m.artifactsEnabled() {")
open(p,'w').write(s)
print('ok')
