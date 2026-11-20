import re, sys

FIELD = '\t// AgentTaskId 子智能体身份（`task-xxx`）；空 = 主会话自己的事件。语义与理由见 events/agentlabel.go。\n' \
        '\tAgentTaskId string `json:"agent_task_id,omitempty"`\n'

TARGETS = [
    ('events/llm.go', 'LLMStart',
     '\tRunId     string    `json:"run_id,omitempty"` // 所属运行（事件自包含，多运行区分归属）\n'),
    ('events/llm.go', 'LLMEnd',
     '\tRunId     string          `json:"run_id,omitempty"` // 所属运行（事件自包含）\n'),
    ('events/llm.go', 'LLMError',
     '\tRunId string `json:"run_id"`          // 所属运行（事件自包含）\n'),
    ('events/tool.go', 'ToolStart',
     '\tRunId     string    `json:"run_id,omitempty"` // 所属运行（事件自包含：主/子 agent 并发工具归属）\n'),
    ('events/tool.go', 'ToolResponse',
     '\tRunId     string `json:"run_id,omitempty"` // 所属运行（事件自包含：主/子 agent 并发工具归属）\n'),
    ('events/content.go', 'ContentChunk',
     '\tRunId     string `json:"run_id,omitempty"`     // 所属运行（事件自包含，多并发 agent 区分归属）\n'),
    ('events/content.go', 'ReasoningChunk',
     '\tRunId     string `json:"run_id,omitempty"`     // 所属运行（事件自包含，多并发 agent 区分归属）\n'),
    ('events/compress.go', 'CompressStart',
     '\tRunId  string `json:"run_id,omitempty"` // 所属运行；旧事件缺失时前端回退主 Agent\n'),
    ('events/compress.go', 'CompressEnd',
     '\tRunId  string `json:"run_id,omitempty"` // 所属运行；旧事件缺失时前端回退主 Agent\n'),
]

for path, struct, anchor in TARGETS:
    s = open(path, encoding='utf-8').read()
    m = re.search(r'type %s struct \{\n' % re.escape(struct), s)
    if not m:
        sys.exit('找不到结构体 %s in %s' % (struct, path))
    end = s.index('\n}\n', m.end())
    body = s[m.end():end]
    if 'AgentTaskId' in body:
        print('SKIP（该结构体已加）%-14s %s' % (struct, path))
        continue
    if body.count(anchor) != 1:
        sys.exit('锚点不唯一 %s/%s: %d' % (path, struct, body.count(anchor)))
    newbody = body.replace(anchor, anchor + FIELD)
    s = s[:m.end()] + newbody + s[end:]
    open(path, 'w', encoding='utf-8').write(s)
    print('OK   %-14s %s' % (struct, path))
