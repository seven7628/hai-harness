import os, re, glob

ROOT = '/Users/admin/workspace/go-code'


def read(p):
    return open(os.path.join(ROOT, p), encoding='utf-8').read()


def write(p, s):
    open(os.path.join(ROOT, p), 'w', encoding='utf-8').write(s)


def resub(path, pat, new, tag):
    s = read(path)
    s2, n = re.subn(pat, new, s)
    if n == 0:
        return 0
    write(path, s2)
    print('OK(%d): %s <- %s' % (n, path, tag))
    return n


# 补上**跨行**的调用点（参数之间有换行）：wd 插在 effort 之后 / spec 之后。
MULTI = [
    # buildPluginEngine(ws, sid, prov, model, effort, wd, reg, ...)
    (r'buildPluginEngine\(([^,\n]+), ([^,\n]+), ([^,\n]+), ([^,\n]+), ((?!nil,)[^,\n]+),(\s*)',
     r'buildPluginEngine(\1, \2, \3, \4, \5, nil,\6', 'buildPluginEngine multiline'),
    # buildHubEngine(ws, sid, spec, wd, reg, ...)
    (r'buildHubEngine\(([^,\n]+), ([^,\n]+), ((?!nil,)[^,\n]+),(\s*)',
     r'buildHubEngine(\1, \2, \3, nil,\4', 'buildHubEngine multiline'),
]

for f in sorted(glob.glob(os.path.join(ROOT, 'desktop/bridge/*_test.go'))):
    rel = os.path.relpath(f, ROOT)
    for pat, new, tag in MULTI:
        resub(rel, pat, new, tag)

print('--- multiline call sites done ---')
