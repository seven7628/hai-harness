import re, sys

# ── ① agents 包：ac.Handler(ac.ctx, &events.X{ → ac.emit(&events.X{ ──
AGENT_SITES = [
    ('agents/hooks.go', 'ToolHookWarning'),
    ('agents/agent_loop.go', 'RunSilentEnd'),
    ('agents/agent_loop.go', 'GoalAlignmentReminder'),
    ('agents/fileref.go', 'RefsLoaded'),
    ('agents/commands.go', 'CommandResult'),
]
for path, t in AGENT_SITES:
    p = path
    s = open(p, encoding='utf-8').read()
    old = 'ac.Handler(ac.ctx, &events.%s{' % t
    n = s.count(old)
    if n != 1:
        sys.exit('agents 站点 %s/%s 命中 %d 次（期望 1）' % (p, t, n))
    s = s.replace(old, 'ac.emit(&events.%s{' % t)
    open(p, 'w', encoding='utf-8').write(s)
    print('OK  agents  %-22s %s' % (t, p))

# ── ② tools/bridge：X(ctx, &events.Y{ → X(ctx, events.WithAgentTaskID(ctx, &events.Y{ ──
# 关闭处需要多补一个 ')'：字面量的 '}' 之后的 ')' 变成 '))'
TOOL_SITES = [
    ('tools/engine.go', 'handler(ctx, &events.ToolApprovalRequested{'),
    ('tools/engine.go', 'handler(ctx, &events.ToolStart{'),
    ('tools/engine.go', 'handler(ctx, &events.ToolResponse{'),
    ('tools/question/question.go', 'tc.Handler(ctx, &events.AskUserQuestion{'),
    ('tools/builtin/bash_tail.go', 'h(ctx, &events.ToolOutput{'),
    ('desktop/bridge/memory_wiring.go', 'tc.Handler(ctx, &events.MemoryActivity{'),
]


def match_brace(s, open_idx):
    """open_idx 指向 '{'，返回其匹配的 '}' 下标（跳过字符串/注释里的括号）。"""
    depth = 0
    i = open_idx
    n = len(s)
    while i < n:
        c = s[i]
        if c == '"':          # 跳过字符串字面量（含转义）
            i += 1
            while i < n and s[i] != '"':
                if s[i] == '\\':
                    i += 1
                i += 1
        elif c == '`':
            i += 1
            while i < n and s[i] != '`':
                i += 1
        elif c == '{':
            depth += 1
        elif c == '}':
            depth -= 1
            if depth == 0:
                return i
        i += 1
    sys.exit('括号不匹配')


for path, anchor in TOOL_SITES:
    s = open(path, encoding='utf-8').read()
    idx = s.find(anchor)
    if idx < 0:
        sys.exit('找不到站点 %s :: %s' % (path, anchor))
    if s.find(anchor, idx + 1) >= 0:
        sys.exit('站点不唯一 %s :: %s' % (path, anchor))
    # 找到字面量的 '{'
    brace = s.index('{', idx + len(anchor) - 1)
    close = match_brace(s, brace)
    after = s[close + 1:close + 3]
    if not after.startswith(')'):
        sys.exit('关闭处形状异常 %s :: %s -> %r' % (path, anchor, after))
    # 1) 开头包一层
    s = s[:idx] + anchor.replace('&events.', 'events.WithAgentTaskID(ctx, &events.') + s[idx + len(anchor):]
    # 2) 关闭处补一个 ')'
    close += len(anchor.replace('&events.', 'events.WithAgentTaskID(ctx, &events.')) - len(anchor)
    s = s[:close + 1] + ')' + s[close + 1:]
    open(path, 'w', encoding='utf-8').write(s)
    print('OK  tools   %-22s %s' % (anchor.split('events.')[1].rstrip('{'), path))
