import os, re, glob

ROOT = '/Users/admin/workspace/go-code'

# S7 在这些生产签名的固定位置插了一个 wd *workdir.State 参数；
# 测试调用点按**同一位置**补一个 nil（nil = "本引擎自建一份"，
# 见 newSessionEngineAt 的兜底分支：if wd == nil { wd = workdir.New(workspace) }）。
#
# 插入位置（0 基下标 = 新参数落在第几个实参）：
INSERTS = {
    'newSessionEngine': 2,    # (workspace, sid, wd, cs, ...)
    'buildPluginEngine': 5,   # (ws, sid, prov, model, effort, wd, reg, ...)
    'buildHubEngine': 3,      # (ws, sid, spec, wd, reg, ...)
    'exploreToolEngine': 1,   # (workspace, wd, sk, caps)
    'buildExploreLoop': 1,    # (workspace, wd, sid, pc, ...)
}


def split_args(s, start):
    """从 '(' 之后开始切分实参（按顶层逗号），返回 (args, close_idx)。"""
    depth = 1
    args = []
    cur = start
    i = start
    while i < len(s):
        c = s[i]
        if c == '(' or c == '[' or c == '{':
            depth += 1
        elif c == ')' or c == ']' or c == '}':
            depth -= 1
            if depth == 0:
                args.append(s[cur:i])
                return args, i
        elif c == ',' and depth == 1:
            args.append(s[cur:i])
            cur = i + 1
        elif c == '"':
            i += 1
            while i < len(s) and s[i] != '"':
                if s[i] == '\\':
                    i += 1
                i += 1
        i += 1
    raise SystemExit('unbalanced call')


def insert_nil(s, name, pos):
    out = []
    i = 0
    key = name + '('
    while True:
        j = s.find(key, i)
        if j < 0:
            out.append(s[i:])
            break
        # 跳过函数定义（前面有 "func "）
        prev = s[max(0, j - 12):j]
        if prev.rstrip().endswith('func'):
            out.append(s[i:j + len(key)])
            i = j + len(key)
            continue
        args, close = split_args(s, j + len(key))
        args = args[:pos] + [' nil'] + args[pos:]
        out.append(s[i:j])
        out.append(key + ','.join(args))
        i = close
    return ''.join(out)


for f in sorted(glob.glob(os.path.join(ROOT, 'desktop/bridge/*_test.go'))):
    rel = os.path.relpath(f, ROOT)
    s = open(f, encoding='utf-8').read()
    orig = s
    for name, pos in INSERTS.items():
        s = insert_nil(s, name, pos)
    if s != orig:
        open(f, 'w', encoding='utf-8').write(s)
        print('OK:', rel)

print('--- test call sites done (arg-aware) ---')
