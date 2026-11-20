# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
L=open(p).read().split('\n')
def block_bounds(needle, occ=0):
    hits=[i for i,l in enumerate(L) if needle in l]
    assert hits, needle
    idx=hits[occ]; a=idx
    while a>=0 and not L[a].startswith('```'): a-=1
    b=idx
    while b<len(L) and not L[b].startswith('```'): b+=1
    return a,b

# 1) replace §21.10 command constant block with pointer + exact text
a,b=block_bounds("#### E-command_registry.go-1：命令名常量")
# block after the "**定位**" line
h=next(i for i in range(a,a+12) if L[i].startswith('```go'))
g=next(i for i in range(h+1,len(L)) if L[i].startswith('```'))
L[h+1:g]='''	// —— 产物（预览/版本/选区/导出）——
	CmdArtifactOpen      Command = "artifact_open"      // 取某版本的预览 URL
	CmdArtifactList      Command = "artifact_list"      // 工作区产物列表（画布/左栏）
	CmdArtifactPromote   Command = "artifact_promote"   // 用户「设为产物」
	CmdArtifactReject    Command = "artifact_reject"    // 用户「不是产物」
	CmdArtifactVerify    Command = "artifact_verify"    // 客户端自检结果回传
	CmdArtifactExport    Command = "artifact_export"    // 导出（基础 HTML 随 A4；PDF/ZIP 后置）
	CmdArtifactCanvas    Command = "artifact_canvas"    // 画布页（Phase C）
	CmdArtifactSelection Command = "artifact_selection" // 选区任务激活/取消（Phase B）
	CmdHostReply         Command = "host_reply"         // 宿主 RPC 回包（preview.screenshot/inspect）'''.split('\n')

# 2) replace §21.10-7 switch
a,b=block_bounds("#### E-main.go-7：命令分发")
h=next(i for i in range(a,a+12) if L[i].startswith('```go'))
g=next(i for i in range(h+1,len(L)) if L[i].startswith('```'))
L[h+1:g]='''	case CmdArtifactOpen:
		m.handleArtifactOpen(c, ws)
	case CmdArtifactList:
		m.handleArtifactList(c, ws)
	case CmdArtifactPromote:
		m.handleArtifactPromote(c, ws, sid)
	case CmdArtifactReject:
		m.handleArtifactReject(c, ws, sid)
	case CmdArtifactVerify:
		m.handleArtifactVerify(c, ws, sid)
	case CmdArtifactExport:
		m.handleArtifactExport(c, ws, sid)
	case CmdArtifactCanvas:
		m.handleArtifactCanvas(c, ws, sid)
	case CmdArtifactSelection:
		m.handleArtifactSelection(c, ws, sid)
	case CmdHostReply:
		m.handleHostReply(c)'''.split('\n')

s='\n'.join(L)

# 3) §8.7 add selection constant
old="+	CmdHostReply      Command = \"host_reply\"      // 宿主 RPC 回包（preview.screenshot）"
new="+	CmdArtifactSelection Command = \"artifact_selection\" // 选区任务激活/取消（Phase B）\n+	CmdHostReply      Command = \"host_reply\"      // 宿主 RPC 回包（preview.screenshot/inspect）"
assert old in s
s=s.replace(old,new,1)

# 4) §8.7 add case
old2="""+	case CmdHostReply:
+		m.handleHostReply(c)"""
new2="""+	case CmdArtifactSelection:
+		m.handleArtifactSelection(c, ws, sid)
+	case CmdHostReply:
+		m.handleHostReply(c)"""
assert old2 in s
s=s.replace(old2,new2,1)

# 5) §21.1 add artifact_commands.go row
old3="| N14 | `desktop/bridge/artifact_embed.go` | `//go:embed artifact_inspector.js` | ~16 行 | §8.6 |"
new3=old3+"\n| N14b | `desktop/bridge/artifact_commands.go` | `handleArtifactOpen/List/Promote/Reject/Verify/Export/Canvas/Selection` | ~230 行 | §8.7 |"
assert old3 in s
s=s.replace(old3,new3,1)

open(p,'w').write(s)
print('ok')
