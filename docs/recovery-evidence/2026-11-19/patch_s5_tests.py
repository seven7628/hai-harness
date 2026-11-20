import re

edits = {
 'desktop/bridge/assembly_wiring_test.go': [
   ('buildExploreLoop(ws, "c7-explore", pc, "deepseek", c7ProbeModel, "low", nil)',
    'buildExploreLoop(ws, workdir.New(ws), "c7-explore", pc, "deepseek", c7ProbeModel, "low", nil)'),
 ],
 'desktop/bridge/compress_thinking_wiring_test.go': [
   ('buildExploreLoop(t.TempDir(), "s-comp", pc, "opencode", forcedModel, "low", nil)',
    'buildExploreLoop(t.TempDir(), workdir.New(t.TempDir()), "s-comp", pc, "opencode", forcedModel, "low", nil)'),
 ],
 'desktop/bridge/desktop_coordination_test.go': [
   ('buildExploreLoop(t.TempDir(), "s-test", pc, "opencode", model, "low", nil)',
    'buildExploreLoop(t.TempDir(), workdir.New(t.TempDir()), "s-test", pc, "opencode", model, "low", nil)'),
 ],
 'desktop/bridge/main_test.go': [
   ('exploreToolEngine(t.TempDir(), sk, nil)',
    'exploreToolEngine(t.TempDir(), workdir.New(t.TempDir()), sk, nil)'),
 ],
}
for path, subs in edits.items():
    s = open(path, encoding='utf-8').read()
    for old, new in subs:
        assert old in s, (path, old)
        s = s.replace(old, new)
    if '"github.com/seven7628/hai-harness/workdir"' not in s:
        m = re.search(r'\n\t"github.com/seven7628/hai-harness/[^"]+"\n', s)
        assert m, path
        s = s[:m.end()] + '\t"github.com/seven7628/hai-harness/workdir"\n' + s[m.end():]
    open(path, 'w', encoding='utf-8').write(s)
    print(path, 'ok')
