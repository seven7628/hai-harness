import re

p = '/Users/admin/workspace/go-code/tools/builtin/workdir_test.go'
s = open(p, encoding='utf-8').read()

old = '''// wdFileTools 从工具集里取 write_file / read_file。
func wdFileTools(t *testing.T, f *FileTools) (write, read interface {
	Call(ctx context.Context, name, args string) (string, error)
}) {
	t.Helper()
	var w, r interface {
		Call(ctx context.Context, name, args string) (string, error)
	}
	for _, tl := range f.Tools() {
		switch tl.Name() {
		case "write_file":
			w = tl
		case "read_file":
			r = tl
		}
	}
	if w == nil || r == nil {
		t.Fatal("工具集里没有 write_file / read_file")
	}
	return w, r
}'''

new = '''// wdFileTools 从工具集里取 write_file / read_file。
func wdFileTools(t *testing.T, f *FileTools) (write, read tools.Tool) {
	t.Helper()
	for _, tl := range f.Tools() {
		switch tl.Name() {
		case "write_file":
			write = tl
		case "read_file":
			read = tl
		}
	}
	if write == nil || read == nil {
		t.Fatal("工具集里没有 write_file / read_file")
	}
	return write, read
}'''

assert old in s
s = s.replace(old, new, 1)

old_imp = '''	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/workdir"'''
new_imp = '''	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
	"github.com/seven7628/hai-harness/workdir"'''
assert old_imp in s
s = s.replace(old_imp, new_imp, 1)

open(p, 'w', encoding='utf-8').write(s)
print('ok')
