p = '/Users/admin/workspace/go-code/desktop/bridge/view_reducer.go'
s = open(p, encoding='utf-8').read()

old = "\treturn int64(approxBytesCalib) * blocksApproxBytes(r.blocks)\n"
new = "\traw := blocksApproxBytes(r.blocks)\n\treturn raw * approxBytesCalibNum / approxBytesCalibDen\n"
assert s.count(old) == 1, s.count(old)
s = s.replace(old, new)

old2 = "const approxBytesCalib = 3 / 2\n"
new2 = """const (
\tapproxBytesCalibNum = 3
\tapproxBytesCalibDen = 2
)
"""
assert s.count(old2) == 1, s.count(old2)
s = s.replace(old2, new2)

open(p, 'w', encoding='utf-8').write(s)
print("patched calib v2")
