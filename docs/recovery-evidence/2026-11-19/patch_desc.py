import os

ROOT = '/Users/admin/workspace/go-code'


def read(p):
    return open(os.path.join(ROOT, p), encoding='utf-8').read()


def write(p, s):
    open(os.path.join(ROOT, p), 'w', encoding='utf-8').write(s)


def sub1(path, old, new, tag):
    s = read(path)
    if old not in s:
        raise SystemExit('MISS: %s in %s' % (tag, path))
    if s.count(old) != 1:
        raise SystemExit('MULTI(%d): %s in %s' % (s.count(old), tag, path))
    write(path, s.replace(old, new, 1))
    print('OK:', tag)


# 1) path 参数说明：锚点从"工作区根"改成"当前工作目录"（会话级 cwd 状态接入后）。
#    边界语义不变（不得逃出**工作区根**），只是锚点跟着会话走。
sub1('tools/builtin/builtin.go',
     '''// 这是工具契约的描述修正，不是新机制：行为一个字没改，schema 终于与实现一致。
const pathParamDesc = "File path. A relative path is resolved against the workspace root " +
	"and may not escape it (a leading ../ is rejected); use an absolute path for files " +
	"outside the workspace."''',
     '''// 这是工具契约的描述修正，不是新机制：行为一个字没改，schema 终于与实现一致。
//
// # 2026-11（会话级 cwd）：锚点从"工作区根"改成"当前工作目录"
//
// 接上 workdir.State 之后，相对路径锚到**调用方分区的当前目录**（模型 cd 之后
// 就是那个目录），不再是恒定的工作区根。文案跟着改，否则又是一条"说明与行为
// 不符"——模型按文案以为相对路径永远从根解析，cd 到子目录后就找不到文件。
//
// 边界语义**没变**：相对路径不得逃出**工作区根**（安全边界仍是工作区，不是锚点）。
// 不接会话级 cwd 状态时（NewFileTools）锚点恒为工作区根 ⇒ 与旧文案逐字同义。
const pathParamDesc = "File path. A relative path is resolved against the current working " +
	"directory (the workspace root until the session changes it) and may not escape the " +
	"workspace root; use an absolute path for files outside the workspace."''',
     'builtin.go pathParamDesc')

# 2) read_file 的工具描述里同一句。
sub1('tools/builtin/builtin.go',
     'Description_: "Read a text file and return numbered lines. path is required and relative to the workspace root.',
     'Description_: "Read a text file and return numbered lines. path is required and relative to the current working directory (the workspace root until the session changes it).',
     'builtin.go read_file desc')

# 3) run_python：script_path 的锚点与脚本工作目录都跟着会话 cwd 走。
sub1('tools/builtin/run_python.go',
     '"script_path": tools.Str("Path to a Python script: relative to the workspace root, or absolute (provide exactly one of script / script_path)"),',
     '"script_path": tools.Str("Path to a Python script: relative to the current working directory (the workspace root until the session changes it), or absolute (provide exactly one of script / script_path)"),',
     'run_python script_path desc')

sub1('tools/builtin/run_python.go',
     'the script runs inside the OS sandbox with the working directory set to the workspace root;',
     'the script runs inside the OS sandbox with the working directory set to the session\'s current working directory (the workspace root until the session changes it);',
     'run_python desc cwd 1')

sub1('tools/builtin/run_python.go',
     'write outputs with paths relative to the working directory (the workspace), not next to the script.',
     'write outputs with paths relative to the working directory (the current one), not next to the script.',
     'run_python desc cwd 2')

print('--- descriptions updated ---')
