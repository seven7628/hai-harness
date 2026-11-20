import re

p = '/Users/admin/workspace/go-code/desktop/bridge/view_reducer.go'
s = open(p, encoding='utf-8').read()

# 1) 加 unsafe import
assert '"unsafe"' not in s
old_imp = '\t"strings"\n\t"sync"\n'
assert s.count(old_imp) == 1, s.count(old_imp)
s = s.replace(old_imp, '\t"strings"\n\t"sync"\n\t"unsafe"\n')

open(p, 'w', encoding='utf-8').write(s)
print("patched view_reducer.go import")
