# -*- coding: utf-8 -*-
p='docs/ARTIFACT_SYSTEM_TECH_PLAN_2026-11-19.md'
s=open(p).read()
def rep(old,new):
    global s
    assert old in s, old[:90]
    s=s.replace(old,new,1)

rep('''	case "bash", "run_python":
		return blockSelection("bash/run_python is disabled during a selection edit; use artifact_edit or write_file inside "+s.dir)''','''	case "bash", "run_python":
		return blockSelection("bash/run_python is disabled during a selection edit; use artifact_edit or write_file inside " + s.dir)
	case "codemode", "agent_spawn", "agent_resume", "subagent", "explore":
		// 三条**守卫覆盖不到**的编排路径，必须在入口封死（不是「顺便」）：
		//   - codemode：脚本内 tools.x(...) 走 Engine.ExecuteOne，**不经过 PreToolUse**
		//     （tools/nested.go 的负向测试钉住了这条降级），bash/写文件全都能绕；
		//   - agent_spawn/subagent/explore：新 Run 的 RunOptions 由工具自己组装，
		//     只继承 ToolContext.Hooks，不继承 Session 的 ToolGuard。
		// 不加这一条，选区守卫等于一块写着「禁止入内」的布帘。
		return blockSelection("orchestration tools (codemode / subagents) are disabled during a selection edit: " +
			"their nested calls and runs do not pass through the selection guard")''')

rep('''	if s.files[p] {
		return true
	}
	if s.dir == "" {
		return true // 入口在根：选区任务允许写工作区根（保守但可解释）
	}
	return strings.HasPrefix(p, s.dir+"/")''','''	if s.files[p] {
		return true
	}
	if s.dir == "" {
		// 入口在工作区根时，「入口目录」= 整个工作区。按目录放行等于允许改任意文件
		// （v1 稿这里 return true，是守卫最严重的一个洞）。只允许 manifest 已有文件
		//（上面已判），新增文件一律拒绝：根目录的新文件与普通工作区文件无法区分。
		return false
	}
	return strings.HasPrefix(p, s.dir+"/")''')
open(p,'w').write(s)
print('ok')
