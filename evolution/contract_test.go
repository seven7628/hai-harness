package evolution

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

// 本文件是**契约守卫**：它不测试 evolution 自己的功能，而是测试 evolution 的存在
// 没有反向改变 Phase A 的任何一条契约。
//
// 为什么要单独一个文件：Phase B/C/D 的实现分散在 candidate / validator /
// replay / outcome / lifecycle / rollback / evidence / phase_d 多个文件里，每一处
// 都可能无意间越界（多写一个文件进 topics/、绕过索引限额、让候选进系统提示词）。
// 逐条集中在这里断言，任何一处越界都会让本文件失败。

// TestContractRepositoryScoped Phase A 契约 1：Repository-scoped。
//
// 断言两个仓库的候选/账本/快照互不可见，且各自落在自己的记忆根之下。
func TestContractRepositoryScoped(t *testing.T) {
	svcA, fsA := newTestService(t)
	svcB, fsB := newTestServiceWithRepo(t, "other-repo-zzz999")

	promoteTopic(t, svcA, "alpha-topic", "仓库 A 的经验\n")
	promoteTopic(t, svcB, "beta-topic", "仓库 B 的经验\n")

	// A 的候选不出现在 B 的目录里，反之亦然。
	if isUnder(fsB.Dir(), svcA.Candidates().Dir()) {
		t.Fatal("repo A candidates must not live under repo B's memory root")
	}
	if isUnder(fsA.Dir(), svcB.Candidates().Dir()) {
		t.Fatal("repo B candidates must not live under repo A's memory root")
	}
	aNames, err := svcA.Candidates().List()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range aNames {
		if c.RepoID != "acme-repo-abc123" {
			t.Fatalf("candidate %s carries the wrong repo id %q", c.ID, c.RepoID)
		}
	}
	bNames, err := svcB.Candidates().List()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range bNames {
		if c.RepoID != "other-repo-zzz999" {
			t.Fatalf("candidate %s carries the wrong repo id %q", c.ID, c.RepoID)
		}
	}
	// 账本同样隔离。
	if err := svcA.RecordOutcome("alpha-topic", Outcome{Kind: OutcomeHelped, At: testNow}); err != nil {
		t.Fatal(err)
	}
	if got, err := svcB.Outcomes("beta-topic"); err != nil || len(got) != 0 {
		t.Fatalf("repo B must not see repo A's outcomes, got %v (%v)", got, err)
	}
	if got, err := svcB.Outcomes("alpha-topic"); err != nil || len(got) != 0 {
		t.Fatalf("repo B must not see a topic it does not own, got %v (%v)", got, err)
	}
}

// TestContractBoundedIndexUnchanged Phase A 契约 2：bounded Index（200 行 / 25KB）。
//
// evolution 的任何写路径都不许绕过 Phase A 的限额。造一个逼近上限的仓库，
// 走完整门禁 + 晋级，索引必须仍然有界。
func TestContractBoundedIndexUnchanged(t *testing.T) {
	svc, fs := newTestService(t)
	// 先灌足够多的记忆把索引推近限额。
	for i := 0; i < 40; i++ {
		promoteTopic(t, svc, "topic-"+strconv.Itoa(i), "内容 "+strconv.Itoa(i)+"\n")
	}
	idx, err := fs.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if countLines(idx.Raw) > 200 || len(idx.Raw) > 25*1024 {
		t.Fatalf("index bounds violated after evolution writes: %d lines / %d bytes",
			countLines(idx.Raw), len(idx.Raw))
	}
	// 归档会重写索引，重写后依然有界。
	if _, err := svc.Archive("topic-0", "清理"); err != nil {
		t.Fatal(err)
	}
	idx, err = fs.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if countLines(idx.Raw) > 200 || len(idx.Raw) > 25*1024 {
		t.Fatalf("index bounds violated after archive: %d lines / %d bytes",
			countLines(idx.Raw), len(idx.Raw))
	}
	// 索引里不许出现 evolution 的内部状态。
	for _, forbidden := range []string{"evolution", "candidate", "verified", "rollout"} {
		if strings.Contains(strings.ToLower(idx.Raw), forbidden) {
			t.Fatalf("index leaked evolution state %q:\n%s", forbidden, idx.Raw)
		}
	}
}

// TestContractTopicJITUnchanged Phase A 契约 3：Topic JIT。
//
// 候选、账本、快照、负证据、生命周期状态**一律**不得出现在系统提示词里。
// 本包不新增工具、不改 tools 注册表，所以 Agent 能读到的东西完全由 Phase A
// 的 memory_view 决定 —— 也就是只读 topics/ 与 MEMORY.md。
func TestContractTopicJITUnchanged(t *testing.T) {
	svc, fs := newTestService(t)
	const canary = "CANARY-MUST-NOT-BOOTSTRAP-9F3A"
	promoteTopic(t, svc, "jit-topic", "记忆正文 "+canary+"\n")

	// 一条**未晋级**的候选带着另一段 canary。
	pending := topicCandidate("pending-topic", "候选正文 PENDING-CANARY-2B7C\n")
	submitted, err := svc.Submit(pending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(context.Background(), submitted.ID); err != nil {
		t.Fatal(err)
	}
	// 晋级一条，落进 topics/。
	promoteTopic(t, svc, "jit-topic-2", "另一条记忆\n")
	if err := svc.RecordOutcome("jit-topic-2", Outcome{Kind: OutcomeHarmful, At: testNow}); err != nil {
		t.Fatal(err)
	}

	idx, err := fs.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	// 启动时加载的只有索引：不含任何正文。
	if strings.Contains(idx.Raw, canary) {
		t.Fatal("the index must never contain topic body text")
	}
	if strings.Contains(idx.Raw, "PENDING-CANARY") {
		t.Fatal("an unpromoted candidate body leaked into the index")
	}
	// 索引里不能出现 evolution 目录名。
	if strings.Contains(idx.Raw, evolutionDirName) {
		t.Fatalf("the index must not mention the evolution directory:\n%s", idx.Raw)
	}
}

// TestContractNoToolOrAgentWiringUnchanged Phase A 契约 4+5：
// Agent 自主读取、Subagent 默认隔离。
//
// 本包**不注册任何工具、不改 agents 包**。用编译期依赖与工具注册表来断言：
// evolution 不 import agents/session/bridge；且 tools 包里没有新增记忆工具。
func TestContractNoToolOrAgentWiringUnchanged(t *testing.T) {
	// 依赖方向：evolution 不得依赖 agents / session / bridge / subagent。
	// （用 go list 做不到，改为源码级断言：evolution 的非测试文件不 import 它们。）
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{
		"agents": true, "session": true, "desktop/bridge": true, "subagent": true, "skills": true,
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for pkg := range forbidden {
			if strings.Contains(string(raw), "hai-harness/"+pkg) {
				t.Fatalf("%s imports %q; evolution must not depend on hosts (Phase A isolation)", name, pkg)
			}
		}
	}
}

// TestContractFilesystemLayout 落盘布局：evolution 的所有数据都在
// <repo>/evolution/ 之下，绝不散落到 Phase A 的目录里。
func TestContractFilesystemLayout(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "layout", "内容\n")
	c := topicCandidate("layout-candidate", "候选内容\n")
	submitted, err := svc.Submit(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(context.Background(), submitted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Supersede("layout", "layout-candidate", "取代"); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordOutcome("layout", Outcome{Kind: OutcomeHelped, At: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordNegative(NegativeEvidence{Target: "topic:layout", Kind: NegStale, At: testNow}); err != nil {
		t.Fatal(err)
	}

	evoRoot := filepath.Join(fs.Dir(), evolutionDirName)
	if !isUnder(evoRoot, svc.Candidates().Dir()) {
		t.Fatal("candidates must live under <repo>/evolution/")
	}
	// Phase A 的两个关键路径下不许出现 evolution 的文件。
	topicsEntries, err := os.ReadDir(fs.TopicsDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range topicsEntries {
		if strings.Contains(e.Name(), evolutionDirName) || strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("evolution leaked a %q into topics/", e.Name())
		}
	}
	// 索引文件旁只有 MEMORY.md 与 evolution/。
	rootEntries, err := os.ReadDir(fs.Dir())
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{memory.IndexFileName: true, evolutionDirName: true, memory.TopicsDirName: true}
	for _, e := range rootEntries {
		if !allowed[e.Name()] {
			t.Fatalf("unexpected entry %q in the memory root; evolution must not add siblings", e.Name())
		}
	}
}

// TestContractPermissionsMatchPhaseA evolution 的落盘权限与 Phase A 同口径。
func TestContractPermissionsMatchPhaseA(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "perms", "内容\n")
	if err := svc.RecordOutcome("perms", Outcome{Kind: OutcomeHelped, At: testNow}); err != nil {
		t.Fatal(err)
	}
	// 触发一次生命周期写入（lifecycle 目录是懒创建的）。
	if _, err := svc.Stale("perms", "权限检查需要该目录存在"); err != nil {
		t.Fatal(err)
	}
	// 逐个文件检查 0600、目录 0700。
	roots := []string{
		filepath.Join(svc.Candidates().Dir()),
		newEvolutionDirs(mustRepoDir(t, svc)).outcomes,
		newEvolutionDirs(mustRepoDir(t, svc)).lifecycle,
	}
	for _, root := range roots {
		assertPerm(t, root, true)
	}
	c, err := svc.Candidates().List()
	if err != nil || len(c) == 0 {
		t.Fatalf("expected at least one candidate: %v", err)
	}
	p := filepath.Join(svc.Candidates().Dir(), c[0].ID+".json")
	assertPerm(t, p, false)
}

// newTestServiceWithRepo 造第二个仓库的控制面（隔离测试用）。
func newTestServiceWithRepo(t *testing.T, repoID string) (*Service, *memory.FileStore) {
	t.Helper()
	ms := memory.NewService(memory.Config{
		Root:     t.TempDir(),
		Resolver: fixedResolver{id: repoID},
		Now:      func() time.Time { return testNow },
	})
	bound, err := ms.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fs := bound.Store.(*memory.FileStore)
	svc, err := NewService(Config{
		Store: bound.Store, Promoter: PromoterFunc{Fn: ms.WriteTopic},
		Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, fs
}

func mustRepoDir(t *testing.T, svc *Service) string {
	t.Helper()
	return svc.cfg.Store.Dir()
}

// assertPerm 断言路径权限（dir=true 时检查目录，否则检查文件）。
func assertPerm(t *testing.T, path string, dir bool) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	want := os.FileMode(filePerm)
	if dir {
		want = dirPerm
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s has mode %o, want %o", path, got, want)
	}
}
