import re, sys

FIELD = '\t// AgentTaskId 子智能体身份（`task-xxx`）；空 = 主会话自己的事件。语义与理由见 events/agentlabel.go。\n' \
        '\tAgentTaskId string `json:"agent_task_id,omitempty"`\n'

TARGETS = [
    ('events/agent.go', 'GoalAlignmentReminder'),
    ('events/agent.go', 'RunSilentEnd'),
    ('events/ask.go', 'AskUserQuestion'),
    ('events/command.go', 'CommandResult'),
    ('events/hooks.go', 'ToolHookWarning'),
    ('events/memory_activity.go', 'MemoryActivity'),
    ('events/refs_loaded.go', 'RefsLoaded'),
    ('events/tool.go', 'ToolApprovalRequested'),
    ('events/tool.go', 'ToolOutput'),
]

for path, struct in TARGETS:
    s = open(path, encoding='utf-8').read()
    m = re.search(r'type %s struct \{\n' % re.escape(struct), s)
    if not m:
        sys.exit('找不到结构体 %s in %s' % (struct, path))
    end = s.index('\n}\n', m.end())
    body = s[m.end():end]
    if 'AgentTaskId' in body:
        print('SKIP %-24s %s' % (struct, path))
        continue
    # 锚点 = body 里第一行含 json:"run_id 的行；没有就用第一行
    lines = body.split('\n')
    idx = None
    for i, ln in enumerate(lines):
        if 'json:"run_id' in ln:
            idx = i
            break
    if idx is None:
        idx = 0
    lines.insert(idx + 1, FIELD.rstrip('\n'))
    newbody = '\n'.join(lines)
    s = s[:m.end()] + newbody + s[end:]
    open(path, 'w', encoding='utf-8').write(s)
    print('OK   %-24s %s' % (struct, path))
