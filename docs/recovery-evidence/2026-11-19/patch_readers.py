import re, sys

# 三处 bridge 读点：os.Open(evPath) → eventslog.OpenReader(evPath)
SITES = [
    ('desktop/bridge/checkpoint.go', '\tpath := filepath.Join(bs.eventsDir, bs.id+".jsonl")\n\tf, err := os.Open(path)\n'),
    ('desktop/bridge/trace.go', '\tf, err := os.Open(evPath)\n'),
    ('desktop/bridge/snapshot.go', '\tf, err := os.Open(evPath)\n'),
]

for path, old in SITES:
    s = open(path, encoding='utf-8').read()
    if old not in s:
        sys.exit('锚点不匹配: %s\n%r' % (path, old))
    if path.endswith('trace.go') or path.endswith('snapshot.go'):
        new = '\tf, err := eventslog.OpenReader(evPath)\n'
    else:
        new = '\tpath := filepath.Join(bs.eventsDir, bs.id+".jsonl")\n\tf, err := eventslog.OpenReader(path)\n'
    s = s.replace(old, new, 1)
    # 加 import
    if '"github.com/seven7628/hai-harness/internal/eventslog"' not in s:
        # 插到 import 块里第一个 hai-harness 之前
        m = re.search(r'\n\t"github\.com/seven7628/hai-harness/', s)
        if not m:
            sys.exit('找不到 import 插入点: %s' % path)
        s = s[:m.start() + 1] + '\t"github.com/seven7628/hai-harness/internal/eventslog"\n' + s[m.start() + 1:]
    open(path, 'w', encoding='utf-8').write(s)
    print('OK  %s' % path)
