import re, sys

p = 'agents/agent_loop.go'
s = open(p, encoding='utf-8').read()

# 只切这 8 类（Agent*/Task* 已有 task_id，不重复盖）
TYPES = ['LLMError', 'LLMStart', 'ContentChunk', 'ReasoningChunk', 'LLMEnd', 'CompressStart', 'CompressEnd']
total = 0
for t in TYPES:
    old = 'ac.Handler(ac.ctx, &events.%s{' % t
    n = s.count(old)
    if n == 0:
        print('!! 没找到 %s 的发射点' % t)
        sys.exit(1)
    s = s.replace(old, 'ac.emit(&events.%s{' % t)
    total += n
    print('  %-16s %d 处' % (t, n))

open(p, 'w', encoding='utf-8').write(s)
print('共改 %d 处' % total)

# 复查：还有没有残留的 ac.Handler(ac.ctx
left = re.findall(r'ac\.Handler\(ac\.ctx, &events\.(\w+)\{', s)
print('残留 ac.Handler(ac.ctx) 发射点：', left)
