import re, sys

SITES = [
    ('desktop/bridge/trace.go', '\tf, err := os.Open(evPath)\n', '\tf, err := eventslog.OpenReader(evPath)\n'),
    ('desktop/bridge/snapshot.go', '\tf, err := os.Open(evPath)\n', '\tf, err := eventslog.OpenReader(evPath)\n'),
]

IMP = '\t"github.com/seven7628/hai-harness/internal/eventslog"\n'

for path, old, new in SITES:
    s = open(path, encoding='utf-8').read()
    if old not in s:
        sys.exit('锚点不匹配: %s' % path)
    s = s.replace(old, new, 1)
    if IMP not in s:
        m = re.search(r'\nimport \(\n', s)
        if not m:
            sys.exit('找不到 import 块: %s' % path)
        # 插到 import 块第一行之后（gofmt 会重排）
        ins = m.end()
        s = s[:ins] + IMP + s[ins:]
    open(path, 'w', encoding='utf-8').write(s)
    print('OK  %s' % path)
