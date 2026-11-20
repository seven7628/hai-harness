p = 'session/sweep_compact.go'
s = open(p, encoding='utf-8').read()

s = s.replace(
    'func SweepBlockStoreDeadBytes(root string, pause time.Duration, ix *SweepIndex) (SweepCompactResult, error) {',
    'func SweepBlockStoreDeadBytes(root string, minBytes int64, pause time.Duration, ix *SweepIndex) (SweepCompactResult, error) {')
s = s.replace(
    '\t\tif fi.Size() < sweepCompactMinBytes {',
    '\t\tif fi.Size() < minBytes {')
s = s.replace(
    '''// sweepCompactMinBytes 是「值得回收」的体积下限。
//
// 复用 block store 自己的门槛（32MB）：`needsCompactAt` 本来就要求文件 ≥ 它，
// 小于它的 sidecar 连打开都不必（打开可能要全量扫一遍重建索引）。
const sweepCompactMinBytes = 32 << 20''',
    '''// SweepCompactMinBytesDefault 是「值得回收」的体积下限（默认 32MB）。
//
// 复用 block store 自己的门槛（`compactMinBytes`）：`needsCompactAt` 本来就要求
// 文件 ≥ 它，小于它的 sidecar 连打开都不必（打开可能要全量扫一遍重建索引）。
const SweepCompactMinBytesDefault = 32 << 20''')
s = s.replace(
    '''//   - root：会话根目录（~/.go-code/sessions）
//   - pause：每个文件之间的间隔（限速，避免与正常使用抢 IO）''',
    '''//   - root：会话根目录（~/.go-code/sessions）
//   - minBytes：体积下限（小于它的不碰：打开可能要全量扫一遍重建索引，不划算）
//   - pause：每个文件之间的间隔（限速，避免与正常使用抢 IO）''')

open(p, 'w', encoding='utf-8').write(s)
print('updated signature')
