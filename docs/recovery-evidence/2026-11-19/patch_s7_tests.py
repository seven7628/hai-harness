import re

files = [
    'desktop/bridge/codemode_test.go',
    'desktop/bridge/cron_test.go',
    'desktop/bridge/main_test.go',
    'desktop/bridge/plugin_proxy_tool_test.go',
    'desktop/bridge/plugin_runtime_test.go',
]

# 只在 cwd_test.go（本次新增）里定义助手，避免同包重复
helper_owner = 'desktop/bridge/cwd_assembly_test.go'

for p in files:
    s = open(p, encoding='utf-8').read()
    orig = s

    # newSessionEngine(workspace, sid, ...) ⇒ 在 sid 之后插入 wd
    def fix_new_engine(m):
        return 'newSessionEngine(%s, %s, workdir.New(%s), ' % (m.group(1), m.group(2), m.group(1))
    s = re.sub(r'newSessionEngine\(([^,\n]+), ([^,\n]+), ', fix_new_engine, s)

    # buildPluginEngine(ws, sid, ...) ⇒ 在 sid 之后插入 wd
    def fix_plugin_engine(m):
        return 'buildPluginEngine(%s, %s, workdir.New(%s.path), ' % (m.group(1), m.group(2), m.group(1))
    s = re.sub(r'buildPluginEngine\((\w+), ([^,\n]+), ', fix_plugin_engine, s)

    if s != orig:
        if '"github.com/seven7628/hai-harness/workdir"' not in s:
            m = re.search(r'\nimport \([^)]*\)\n', s)
            assert m, p
            s = s[:m.end()] + '\t"github.com/seven7628/hai-harness/workdir"\n' + s[m.end():]
        open(p, 'w', encoding='utf-8').write(s)
        print(p, 'patched')

# 助手定义放进 cwd_assembly_test.go（本次新增文件，同包唯一）
p = helper_owner
s = open(p, encoding='utf-8').read()
if 'func wdFor(' not in s:
    helper = '''
// wdFor 造一个会话级 cwd 状态（装配签名里要传）。
// 生产路径传的是 bridgeSession.wd（跨 rebuildLoop 存活）；测试里按需新建。
func wdFor(root string) *workdir.State { return workdir.New(root) }
'''
    s = s.rstrip('\n') + '\n' + helper
    open(p, 'w', encoding='utf-8').write(s)
    print('helper added to', p)
