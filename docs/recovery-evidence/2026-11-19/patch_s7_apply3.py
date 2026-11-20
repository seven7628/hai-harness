import os

ROOT = '/Users/admin/workspace/go-code'
TOK = 'cod' + 'emode'


def read(p):
    return open(os.path.join(ROOT, p), encoding='utf-8').read()


def write(p, s):
    open(os.path.join(ROOT, p), 'w', encoding='utf-8').write(s)


def sub1(path, old, new, tag):
    s = read(path)
    if old not in s:
        if new in s:
            print('SKIP already-applied:', tag)
            return
        raise SystemExit('MISS: %s in %s' % (tag, path))
    if s.count(old) != 1:
        raise SystemExit('MULTI(%d): %s in %s' % (s.count(old), tag, path))
    write(path, s.replace(old, new, 1))
    print('OK:', tag)


# main.go：registerCodemode 带上会话级 cwd 状态（s7b 的 codemode 脚本工作目录）。
sub1('desktop/bridge/main.go',
     '\tregisterCodemode(eng, workspace, sessionsRoot, sid, sbx, loadCodemodeSettingsQuiet())',
     '\tregisterCodemode(eng, workspace, sessionsRoot, sid, sbx, wd, loadCodemodeSettingsQuiet())',
     'main.go registerCodemode wd')

print('--- main.go done ---')
