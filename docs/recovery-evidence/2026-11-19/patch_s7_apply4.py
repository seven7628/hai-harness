import os, re, glob

ROOT = '/Users/admin/workspace/go-code'


def read(p):
    return open(os.path.join(ROOT, p), encoding='utf-8').read()


def write(p, s):
    open(os.path.join(ROOT, p), 'w', encoding='utf-8').write(s)


def resub(path, pat, new, tag, expect=None):
    s = read(path)
    s2, n = re.subn(pat, new, s)
    if n == 0:
        return 0
    if expect is not None and n != expect:
        raise SystemExit('COUNT %d != %d: %s in %s' % (n, expect, tag, path))
    write(path, s2)
    print('OK(%d): %s <- %s' % (n, path, tag))
    return n


# S7 把 wd 插进了这些生产签名的固定位置；测试调用点按同样位置补一个 nil
# （nil = "本引擎自建一份"，见 newSessionEngineAt 的兜底分支）。
SIGS = [
    # newSessionEngine(workspace, sid, wd, cs, ...) -- wd 在 sid 之后
    (r'newSessionEngine\(([^,\n]+), ([^,\n]+), ', r'newSessionEngine(\1, \2, nil, ', 'newSessionEngine'),
    # buildPluginEngine(ws, sid, prov, model, effort, wd, reg, ...) -- wd 在 effort 之后
    (r'buildPluginEngine\(([^,\n]+), ([^,\n]+), ([^,\n]+), ([^,\n]+), ([^,\n]+), ',
     r'buildPluginEngine(\1, \2, \3, \4, \5, nil, ', 'buildPluginEngine'),
    # buildHubEngine(ws, sid, spec, wd, reg, ...) -- wd 在 spec 之后
    (r'buildHubEngine\(([^,\n]+), ([^,\n]+), ([^,\n]+), ',
     r'buildHubEngine(\1, \2, \3, nil, ', 'buildHubEngine'),
    # exploreToolEngine(workspace, wd, sk, caps) -- wd 在最前
    (r'exploreToolEngine\(([^,\n]+), ', r'exploreToolEngine(\1, nil, ', 'exploreToolEngine'),
    # buildExploreLoop(workspace, wd, sid, ...) -- wd 在最前
    (r'buildExploreLoop\(([^,\n]+), ', r'buildExploreLoop(\1, nil, ', 'buildExploreLoop'),
]

files = sorted(glob.glob(os.path.join(ROOT, 'desktop/bridge/*_test.go')))
for f in files:
    rel = os.path.relpath(f, ROOT)
    for pat, new, tag in SIGS:
        resub(rel, pat, new, tag)

print('--- test call sites done ---')
