import os, hashlib, datetime
OUT='/private/tmp/goland-recovered'
recs=[]
for line in open(os.path.join(OUT,'_records.tsv')):
    p=line.rstrip('\n').split('\t')
    if len(p)>=5: recs.append(p)
# group by original path
groups={}
for rel,ts,sz,sha,note in recs:
    groups.setdefault(rel,[]).append((ts,int(sz),sha,note))

lines=[]
lines.append('GO-LAND / AGENT RECOVERY INDEX')
lines.append('='*78)
lines.append('Generated: '+datetime.datetime.now().strftime('%Y-%m-%d %H:%M:%S %Z'))
lines.append('Output root: /private/tmp/goland-recovered/')
lines.append('Layout: files/<original repo path>/<timestamp>-<kind>-<filename>')
lines.append('')
lines.append('-'*78)
lines.append('SUMMARY')
lines.append('-'*78)
lines.append(f'Unique original paths recovered : {len(groups)}')
lines.append(f'Total file versions recovered   : {len(recs)}')
lines.append(f'  AUTHENTIC pre-deletion copies  : {sum(1 for r in recs if "AUTHENTIC" in r[4])}')
lines.append(f'  Reconstructed versions         : {sum(1 for r in recs if "RECONSTRUCTED" in r[4] or "reconstructed" in r[4])}')
lines.append('')
lines.append('-'*78)
lines.append('FILES')
lines.append('-'*78)
for rel in sorted(groups):
    lines.append(f'\n{rel}')
    for ts,sz,sha,note in sorted(groups[rel]):
        lines.append(f'    {ts}  {sz:>9,d} bytes  sha256:{sha}')
        lines.append(f'        {note}')
lines.append('')
lines.append('-'*78)
lines.append('PROVENANCE NOTES')
lines.append('-'*78)
lines.append("""
AUTHENTIC  = byte-exact pre-deletion copies that survived the deletion on disk
             (found under /private/tmp as *.bak files made by the original
             work session's patch scripts). These are the highest-fidelity
             artifacts in this recovery.

RECONSTRUCTED = files rebuilt by the recovery session by replaying the original
             work session's Python patch scripts (/private/tmp/patch_s3.py,
             s4, s5, s6, p1, s7, s7b, and the S7 apply-chain) against the
             pristine clone at commit 98028fb. Verified by:
               - `go build ./...`  -> PASS (whole repo)
               - `go vet ./workdir/ ./tools/builtin/` -> PASS
               - `go test ./workdir/ ./tools/builtin/ -run Workdir|Cwd|Partition` -> PASS

KEY FILES (the ones that carried the uncommitted "session-level State" work):
  desktop/bridge/main.go   -- has 3 AUTHENTIC versions; bak3 (22:05:10) is the
                              final pre-deletion state (contains workdir.State,
                              session-level wd field, buildHubEngine w/ wd).
  workdir/workdir.go       -- the new package (session-level cwd state).
  tools/builtin/workdir.go -- tool-layer adapter (CwdKey partition resolution).
  tools/builtin/bash.go    -- bash tool: cwd now resolved via workdir.
  tools/builtin/builtin.go -- file tools: relative paths anchored to cwd.
  tools/builtin/run_python.go, grep.go, glob.go, worktree.go -- same treatment.
""")
open(os.path.join(OUT,'INDEX.txt'),'w').write('\n'.join(lines))
print('INDEX.txt written:', len('\n'.join(lines)), 'bytes')
