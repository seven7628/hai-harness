import os

p = '/Users/admin/workspace/go-code/desktop/bridge/main.go'
s = open(p, encoding='utf-8').read()

old = """func exploreToolEngine(workspace string, wd *workdir.State, sk *skills.Registry, caps *skills.Capabilities) *tools.Engine {
	allowed := make(map[string]bool, len(exploreAllowedTools))"""

new = """func exploreToolEngine(workspace string, wd *workdir.State, sk *skills.Registry, caps *skills.Capabilities) *tools.Engine {
	// wd 为 nil 时（独立/测试用法）退化为「按本引擎新建」：与 newSessionEngineAt 同款兜底。
	// 不兜底的话，下面的 NewFileToolsWithWorkdir(nil) 会把文件工具锚到**进程 cwd**
	//（而不是本 explore 的工作区）⇒ 相对路径解析到别处。
	if wd == nil {
		wd = workdir.New(workspace)
	}
	allowed := make(map[string]bool, len(exploreAllowedTools))"""

assert old in s
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8').write(s)
print('ok')
