import re, sys

# ① 记忆包：加 .gz 感知的读事件助手
p = 'memory/config.go'
s = open(p, encoding='utf-8').read()
if 'func (c Config) readEventFile' not in s:
    s += '''
// readEventFile 读事件日志（明文或 **gzip 封存**）。
//
// 事件日志会被后台封存成 `<sid>.jsonl.gz`（见 internal/eventslog）⇒
// 记忆层的三处扫描（transcripts / conflicts / anchors）都要能读封存件，
// 否则**老会话的转录会静默消失**（记忆层少料，用户看不出来但 AI 会变笨）。
//
// 明文走 c.ReadFile（测试注入点保持不变）；封存件走 eventslog（它要解压）。
func (c Config) readEventFile(path string) ([]byte, error) {
	if strings.HasSuffix(path, eventslog.GzSuffix) {
		rc, err := eventslog.OpenReader(strings.TrimSuffix(path, eventslog.GzSuffix))
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	return c.ReadFile(path)
}

// isEventLogName 报告文件名是不是事件日志（明文或封存）。
func isEventLogName(name string) bool {
	return strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl"+eventslog.GzSuffix)
}
'''
    if '"github.com/seven7628/hai-harness/internal/eventslog"' not in s:
        m = re.search(r'\nimport \(\n', s)
        s = s[:m.end()] + '\t"github.com/seven7628/hai-harness/internal/eventslog"\n' + s[m.end():]
    open(p, 'w', encoding='utf-8').write(s)
    print('OK  memory/config.go: readEventFile + isEventLogName')

# ② 三处扫描：过滤条件 + 读取
SCAN = [
    ('memory/transcripts.go', '\t\t\tif fe.IsDir() || !strings.HasSuffix(fe.Name(), ".jsonl") {\n'),
    ('memory/conflicts.go', '\t\tif fe.IsDir() || !strings.HasSuffix(fe.Name(), ".jsonl") {\n'),
    ('memory/anchors.go', '\t\tif fe.IsDir() || !strings.HasSuffix(fe.Name(), ".jsonl") {\n'),
]
for path, filt in SCAN:
    s = open(path, encoding='utf-8').read()
    if filt in s:
        s = s.replace(filt, filt.replace('!strings.HasSuffix(fe.Name(), ".jsonl")', '!isEventLogName(fe.Name())'), 1)
        open(path, 'w', encoding='utf-8').write(s)
        print('OK  %s: 过滤条件' % path)
    else:
        print('!! %s: 过滤锚点没匹配' % path)

# ③ 读取改成 readEventFile
READS = [
    ('memory/transcripts.go', '\t\t\tc.scanOneEventFile(p, d.ID, &rep)\n', None),
    ('memory/conflicts.go', '\t\traw, err := c.ReadFile(path)\n', '\t\traw, err := c.readEventFile(path)\n'),
    ('memory/anchors.go', '\t\traw, err := c.ReadFile(filepath.Join(dir, fe.Name()))\n', '\t\traw, err := c.readEventFile(filepath.Join(dir, fe.Name()))\n'),
]
for path, old, new in READS:
    if new is None:
        continue
    s = open(path, encoding='utf-8').read()
    if old in s:
        s = s.replace(old, new, 1)
        open(path, 'w', encoding='utf-8').write(s)
        print('OK  %s: 读取改走 readEventFile' % path)
    else:
        print('!! %s: 读取锚点没匹配' % path)
