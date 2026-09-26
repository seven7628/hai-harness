package evolution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

// promoteTopic 走完门禁把一条记忆放进可召回索引（Phase C 测试的前置）。
func promoteTopic(t *testing.T, svc *Service, name, body string) {
	t.Helper()
	c := topicCandidate(name, body)
	submitted, err := svc.Submit(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(context.Background(), submitted.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.Candidates().Get(submitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Promote(stored); err != nil {
		t.Fatal(err)
	}
}

// --- Outcome 与 Confidence ----------------------------------------------

// TestConfidenceIgnoresNeutralReads 置信度只由有效证据驱动：
// 大量"读了但中性"的观测不应把置信度压低（否则会惩罚"没造成危害"）。
func TestConfidenceIgnoresNeutralReads(t *testing.T) {
	base := []Outcome{
		{Kind: OutcomeHelped, At: testNow},
		{Kind: OutcomeRead, At: testNow},
		{Kind: OutcomeRead, At: testNow},
		{Kind: OutcomeRead, At: testNow},
	}
	conf := DefaultConfidence(base)
	if conf.Count != 1 {
		t.Fatalf("neutral reads must not count as evidence, got count=%d", conf.Count)
	}
	if conf.Value < 0.5 {
		t.Fatalf("one helped observation should not yield low confidence, got %.3f", conf.Value)
	}
	// 中性观测再多也不改变结论。
	for i := 0; i < 50; i++ {
		base = append(base, Outcome{Kind: OutcomeRead, At: testNow})
	}
	again := DefaultConfidence(base)
	if again.Count != conf.Count || again.Value != conf.Value {
		t.Fatalf("neutral reads changed the verdict: %+v -> %+v", conf, again)
	}
}

// TestConfidenceFallsWithHarmfulObservations 负证据必须拉低置信度。
func TestConfidenceFallsWithHarmfulObservations(t *testing.T) {
	good := DefaultConfidence([]Outcome{
		{Kind: OutcomeHelped, At: testNow}, {Kind: OutcomeHelped, At: testNow},
		{Kind: OutcomeHelped, At: testNow}, {Kind: OutcomeHelped, At: testNow},
	})
	bad := DefaultConfidence([]Outcome{
		{Kind: OutcomeHelped, At: testNow}, {Kind: OutcomeHarmful, At: testNow},
		{Kind: OutcomeHarmful, At: testNow}, {Kind: OutcomeHarmful, At: testNow},
	})
	if bad.Value >= good.Value {
		t.Fatalf("harmful evidence must lower confidence: good=%.3f bad=%.3f", good.Value, bad.Value)
	}
	if bad.HarmfulRatio < 0.7 {
		t.Fatalf("unexpected harmful ratio %.3f", bad.HarmfulRatio)
	}
}

// TestConfidenceWithNoEvidenceIsPrior 无证据时落在先验 0.5，而不是 0 或 1。
func TestConfidenceWithNoEvidenceIsPrior(t *testing.T) {
	conf := DefaultConfidence(nil)
	if conf.Value != 0.5 {
		t.Fatalf("expected the 0.5 prior, got %.3f", conf.Value)
	}
	if conf.Count != 0 {
		t.Fatalf("expected zero observations, got %d", conf.Count)
	}
}

// TestRecordOutcomePersistsAndComputes 结果账本落盘 + 置信度可复算。
func TestRecordOutcomePersistsAndComputes(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "build-and-verify", "构建必须用项目规定的命令。\n")

	if err := svc.RecordOutcome("build-and-verify", Outcome{Kind: OutcomeHelped, Note: "省了 3 次探查", At: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordOutcome("build-and-verify", Outcome{Kind: OutcomeHelped, At: testNow.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	conf, err := svc.ConfidenceOf("build-and-verify")
	if err != nil {
		t.Fatal(err)
	}
	if conf.Count != 2 || conf.Value < 0.7 {
		t.Fatalf("unexpected confidence %+v", conf)
	}

	// 重开一个 Service（模拟新会话）后账本仍在。
	svc2 := reopenService(t, svc)
	conf2, err := svc2.ConfidenceOf("build-and-verify")
	if err != nil {
		t.Fatal(err)
	}
	if conf2.Count != conf.Count {
		t.Fatalf("outcome ledger did not survive a reopen: %+v vs %+v", conf, conf2)
	}
}

// TestHarmfulOutcomeAlsoRecordsNegativeEvidence 有害观测自动落一条负证据
// （不依赖调用点记得写两处）。
func TestHarmfulOutcomeAlsoRecordsNegativeEvidence(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "risky", "这条记忆其实是错的。\n")
	if err := svc.RecordOutcome("risky", Outcome{Kind: OutcomeHarmful, Note: "让 Agent 跑了一个不存在的命令", At: testNow}); err != nil {
		t.Fatal(err)
	}
	ev, err := svc.NegativeEvidenceOf("topic:risky")
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 {
		t.Fatalf("expected 1 negative evidence entry, got %d", len(ev))
	}
	if ev[0].Kind != NegHarmful {
		t.Fatalf("unexpected kind %q", ev[0].Kind)
	}
	sum, err := svc.SummarizeEvidence("topic:risky")
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Blocking {
		t.Fatal("one harmful observation must be blocking")
	}
}

// TestNeutralOutcomeRecordsNoNegativeEvidence 中性观测不产生负证据。
func TestNeutralOutcomeRecordsNoNegativeEvidence(t *testing.T) {
	svc, _ := newTestService(t)
	if err := svc.RecordOutcome("neutral", Outcome{Kind: OutcomeRead, At: testNow}); err != nil {
		t.Fatal(err)
	}
	ev, err := svc.NegativeEvidenceOf("topic:neutral")
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 0 {
		t.Fatalf("neutral observations must not create negative evidence, got %d", len(ev))
	}
}

// TestNegativeEvidenceWeighting 负证据权重口径：一次 harmful 强过三条 stale。
func TestNegativeEvidenceWeighting(t *testing.T) {
	svc, _ := newTestService(t)
	for i := 0; i < 3; i++ {
		if err := svc.RecordNegative(NegativeEvidence{
			Target: "topic:soft", Kind: NegStale, Detail: "很久没用了", At: testNow,
		}); err != nil {
			t.Fatal(err)
		}
	}
	soft, err := svc.SummarizeEvidence("topic:soft")
	if err != nil {
		t.Fatal(err)
	}
	if !soft.Blocking {
		t.Fatalf("three stale entries should be blocking, got %+v", soft)
	}
	if err := svc.RecordNegative(NegativeEvidence{
		Target: "topic:hard", Kind: NegHarmful, Detail: "直接导致失败", At: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	hard, err := svc.SummarizeEvidence("topic:hard")
	if err != nil {
		t.Fatal(err)
	}
	if !hard.Blocking {
		t.Fatalf("one harmful entry should be blocking, got %+v", hard)
	}
	// 单条 harmful（权重 3）应当至少抵得上三条 stale（权重 1×3）——这是权重的设计
	// 意图"一次就足以证明有害 > 反复脱节"的可观测形式。
	if hard.TotalWeight < soft.TotalWeight {
		t.Fatalf("harmful weight %.1f should not be weaker than cumulative stale weight %.1f", hard.TotalWeight, soft.TotalWeight)
	}
	// 单条 harmful 必须比单条 stale 更重（这是权重的核心主张：一次实际危害
	// 的严重程度高于一次"看起来过时"）。
	for _, k := range []NegativeKind{NegContradicted, NegStale, NegUnhelpful} {
		if defaultWeight(NegHarmful) <= defaultWeight(k) {
			t.Fatalf("harmful weight %v must exceed %s weight %v", defaultWeight(NegHarmful), k, defaultWeight(k))
		}
	}
}

// TestUnhelpfulAloneIsNotBlocking 单纯的"没用上"不该阻止晋级。
func TestUnhelpfulAloneIsNotBlocking(t *testing.T) {
	svc, _ := newTestService(t)
	if err := svc.RecordNegative(NegativeEvidence{
		Target: "topic:niche", Kind: NegUnhelpful, Detail: "这次任务不相关", At: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	sum, err := svc.SummarizeEvidence("topic:niche")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Blocking {
		t.Fatalf("a single unhelpful note must not block promotion: %+v", sum)
	}
}

// TestEvidenceTargetNormalization 证据目标的三种写法归一。
func TestEvidenceTargetNormalization(t *testing.T) {
	svc, _ := newTestService(t)
	for _, form := range []string{"foo", "topic:foo", "topics/foo.md"} {
		if err := svc.RecordNegative(NegativeEvidence{Target: form, Kind: NegStale, Detail: "x", At: testNow}); err != nil {
			t.Fatalf("%s: %v", form, err)
		}
	}
	ev, err := svc.NegativeEvidenceOf("topic:foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 3 {
		t.Fatalf("expected 3 normalized into one ledger, got %d", len(ev))
	}
}

// TestEvidenceTargetPathTraversalRejected 证据目标的路径边界。
func TestEvidenceTargetPathTraversalRejected(t *testing.T) {
	svc, _ := newTestService(t)
	if err := svc.RecordNegative(NegativeEvidence{Target: "../../etc/passwd", Kind: NegStale, At: testNow}); err == nil {
		t.Fatal("path traversal in an evidence target must be rejected")
	}
}

// --- 生命周期 ------------------------------------------------------------

// TestLifecycleRequiresReason 无依据的状态变更被拒（可审计性底线）。
func TestLifecycleRequiresReason(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.Stale("some-topic", "  "); !errors.Is(err, ErrNoReason) {
		t.Fatalf("expected ErrNoReason, got %v", err)
	}
}

// TestStaleIsSoftMark 关键不变式：Stale 不删内容、不移出索引。
//
// 这条直接来自评估协议："低频"不等于"无效"。只打标记，宿主才有空间区分
// "暂时没用"与"确实有害"。
func TestStaleIsSoftMark(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "quarterly", "只在季度任务里用到的经验。\n")

	lc, err := svc.Stale("quarterly", "90 天没有观测")
	if err != nil {
		t.Fatal(err)
	}
	if lc.State != LifeStale {
		t.Fatalf("expected stale, got %q", lc.State)
	}
	// 内容仍在，索引仍在。
	if _, err := fs.ReadTopic("quarterly"); err != nil {
		t.Fatalf("stale must not delete the topic: %v", err)
	}
	idx, err := fs.ReadIndex()
	if err != nil || !strings.Contains(idx.Raw, "quarterly") {
		t.Fatalf("stale must not remove the index entry: %v\n%s", err, idx.Raw)
	}
	// 历史留痕。
	if len(lc.History) != 1 || lc.History[0].To != LifeStale {
		t.Fatalf("expected a history entry, got %+v", lc.History)
	}
}

// TestSupersedeRecordsRelation Supersede 记录取代关系但不删旧主题。
func TestSupersedeRecordsRelation(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "old-build", "旧构建命令 make bootstrap。\n")
	promoteTopic(t, svc, "new-build", "新构建命令 ./scripts/ci.sh。\n")

	lc, err := svc.Supersede("old-build", "new-build", "构建流程已迁移到 scripts/ci.sh")
	if err != nil {
		t.Fatal(err)
	}
	if lc.State != LifeSuperseded || lc.SupersededBy != "new-build" {
		t.Fatalf("unexpected lifecycle %+v", lc)
	}
	if _, err := fs.ReadTopic("old-build"); err != nil {
		t.Fatalf("supersede must not delete the old topic: %v", err)
	}
	revs, err := svc.Revisions("old-build")
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 1 {
		t.Fatalf("supersede must snapshot the old content, got %d snapshots", len(revs))
	}
	if !strings.Contains(revs[0].Content, "make bootstrap") {
		t.Fatalf("snapshot does not contain the superseded body:\n%s", revs[0].Content)
	}
}

// TestSupersedeRequiresTarget Supersede 必须说明被谁取代。
func TestSupersedeRequiresTarget(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "x", "内容\n")
	if _, err := svc.Supersede("x", "", "理由"); err == nil {
		t.Fatal("supersede without a target must be rejected")
	}
}

// TestExpireIsSoftMark Expire 同样只是软标记。
func TestExpireIsSoftMark(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "old-ttl", "有过期时间的内容。\n")
	lc, err := svc.Expire("old-ttl", "TTL 2026-01-01 已到")
	if err != nil {
		t.Fatal(err)
	}
	if lc.State != LifeExpired {
		t.Fatalf("expected expired, got %q", lc.State)
	}
	if _, err := fs.ReadTopic("old-ttl"); err != nil {
		t.Fatalf("expire must not delete the topic: %v", err)
	}
}

// TestArchiveRemovesFromRecallableIndex 归档必须把文件移出 topics/ 且清掉索引条目。
//
// 状态与可召回性脱节是这类系统最典型的自欺：Agent 继续被"已归档"记忆影响。
func TestArchiveRemovesFromRecallableIndex(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "obsolete", "已经过时的做法。\n")

	lc, err := svc.Archive("obsolete", "与当前实现冲突")
	if err != nil {
		t.Fatal(err)
	}
	if lc.State != LifeArchived {
		t.Fatalf("expected archived, got %q", lc.State)
	}
	if _, err := fs.ReadTopic("obsolete"); err == nil {
		t.Fatal("archived topic must no longer be readable from topics/")
	}
	idx, err := fs.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(idx.Raw, "obsolete") {
		t.Fatalf("archived topic must leave the index:\n%s", idx.Raw)
	}
	// 归档副本保留。
	archivePath := filepath.Join(newEvolutionDirs(fs.Dir()).archive, "obsolete.md")
	if _, err := os.Stat(archivePath); err != nil {
		t.Fatalf("archive copy is missing: %v", err)
	}
}

// TestArchiveThenRestoreRoundTrip 归档后可取回（内容不丢）。
func TestArchiveThenRestoreRoundTrip(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "round-trip", "原始内容 UNIQUE-MARKER。\n")
	before, err := fs.ReadTopic("round-trip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Archive("round-trip", "临时下架"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore("round-trip", "确认还需要"); err != nil {
		t.Fatal(err)
	}
	after, err := fs.ReadTopic("round-trip")
	if err != nil {
		t.Fatalf("restored topic must be readable again: %v", err)
	}
	if after.Body != before.Body {
		t.Fatalf("restore lost content:\nbefore %q\nafter  %q", before.Body, after.Body)
	}
	idx, _ := fs.ReadIndex()
	if !strings.Contains(idx.Raw, "round-trip") {
		t.Fatalf("restore must bring the index entry back:\n%s", idx.Raw)
	}
}

// TestIllegalLifecycleTransitionRejected 非法迁移被拒。
func TestIllegalLifecycleTransitionRejected(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "seq", "内容\n")
	if _, err := svc.Archive("seq", "先归档"); err != nil {
		t.Fatal(err)
	}
	// archived -> superseded 不在白名单里。
	if _, err := svc.Supersede("seq", "other", "非法"); !errors.Is(err, ErrIllegalTransition) {
		t.Fatalf("expected ErrIllegalTransition, got %v", err)
	}
}

// TestUnmarkRestoresActive 误判纠正入口。
func TestUnmarkRestoresActive(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "revived", "内容\n")
	if _, err := svc.Stale("revived", "误判"); err != nil {
		t.Fatal(err)
	}
	lc, err := svc.Unmark("revived", "季度任务确实会用")
	if err != nil {
		t.Fatal(err)
	}
	if lc.State != LifeActive {
		t.Fatalf("expected active, got %q", lc.State)
	}
}

// TestRevalidateUpdatesConfidenceOnly 允许刷新置信度而不改状态。
func TestRevalidateUpdatesConfidenceOnly(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "refresh", "内容\n")
	if _, err := svc.Stale("refresh", "暂时没用"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := svc.RecordOutcome("refresh", Outcome{Kind: OutcomeHelped, At: testNow.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	lc, err := svc.Revalidate("refresh")
	if err != nil {
		t.Fatal(err)
	}
	if lc.State != LifeStale {
		t.Fatalf("revalidate must not change state, got %q", lc.State)
	}
	if lc.Confidence.Count != 4 || lc.Confidence.Value < 0.7 {
		t.Fatalf("revalidate must refresh the confidence snapshot, got %+v", lc.Confidence)
	}
}

// --- Rollback ------------------------------------------------------------

// TestRollbackRestoresParentVersion 核心负向测试：一个**坏版本**通过晋级覆盖
// 好版本后，回滚必须真的把好版本恢复回来。
//
// 场景按真实链路构造：好记忆先落盘 → 一条内容错误的候选走完门禁并覆盖它 →
// Rollback。坏版本的唯一真实入口就是晋级覆盖（内存里的破坏不会被本包观测到，
// 那是 Phase A 内存态的问题，不是本包的责任）。
func TestRollbackRestoresParentVersion(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "build-steps", "正确内容：构建用 ./scripts/ci.sh。\n")
	good, err := fs.ReadTopic("build-steps")
	if err != nil {
		t.Fatal(err)
	}

	// 坏版本经门禁晋级（覆盖好版本）。
	promoteTopic(t, svc, "build-steps", "错误内容：构建用 make legacy。\n")
	bad, _ := fs.ReadTopic("build-steps")
	if bad.Body == good.Body {
		t.Fatal("test setup: the bad version should differ")
	}

	res, err := svc.Rollback("build-steps", "新内容是错的，回滚")
	if err != nil {
		t.Fatal(err)
	}
	if res.RestoredVersion != 1 {
		t.Fatalf("expected to restore v1, got %d", res.RestoredVersion)
	}
	after, err := fs.ReadTopic("build-steps")
	if err != nil {
		t.Fatal(err)
	}
	if after.Body != good.Body {
		t.Fatalf("rollback did not restore the body:\nwant %q\ngot  %q", good.Body, after.Body)
	}
	lc, err := svc.LifecycleOf("build-steps")
	if err != nil {
		t.Fatal(err)
	}
	if lc.State != LifeRolledBack {
		t.Fatalf("expected rolled_back, got %q", lc.State)
	}
}

// TestRollbackRestoresIndexEntry 回滚后索引条目必须同步回来。
func TestRollbackRestoresIndexEntry(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "indexed", "V1 内容。\n")
	promoteTopic(t, svc, "indexed", "V2 内容。\n")
	if _, err := svc.Rollback("indexed", "回滚"); err != nil {
		t.Fatal(err)
	}
	idx, err := fs.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idx.Raw, "indexed") {
		t.Fatalf("rollback must leave the topic in the index:\n%s", idx.Raw)
	}
}

// TestRollbackIsReversible 回滚本身也可逆（回滚再回滚能回到坏版本）。
//
// 这一条是回滚能被信任的前提：如果回滚是单向的，误回滚就等于不可恢复的破坏。
func TestRollbackIsReversible(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "two-way", "A 版本内容。\n")
	promoteTopic(t, svc, "two-way", "B 版本内容。\n")

	if _, err := svc.Rollback("two-way", "回到 A"); err != nil {
		t.Fatal(err)
	}
	a, _ := fs.ReadTopic("two-way")
	if !strings.Contains(a.Body, "A 版本") {
		t.Fatalf("first rollback failed: %q", a.Body)
	}
	// 第二次回滚应恢复 B（第一次回滚前把 B 存成了 v2）。
	if _, err := svc.Rollback("two-way", "再回到 B"); err != nil {
		t.Fatal(err)
	}
	b, _ := fs.ReadTopic("two-way")
	if !strings.Contains(b.Body, "B 版本") {
		t.Fatalf("rollback is not reversible: %q", b.Body)
	}
}

// TestRollbackWithoutSnapshotFails 没有快照就明确报错，不静默成功。
func TestRollbackWithoutSnapshotFails(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "no-snap", "内容\n")
	if _, err := svc.Rollback("no-snap", "试试"); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("expected ErrNoRevision, got %v", err)
	}
}

// TestRollbackRequiresReason 回滚也要依据。
func TestRollbackRequiresReason(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "r2", "内容\n")
	if _, err := svc.Rollback("r2", ""); !errors.Is(err, ErrNoReason) {
		t.Fatalf("expected ErrNoReason, got %v", err)
	}
}

// TestRestoreFromSpecificVersion 恢复到指定版本，越界明确报错。
func TestRestoreFromSpecificVersion(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "v3", "V1 内容。\n")
	promoteTopic(t, svc, "v3", "V2 内容。\n")
	promoteTopic(t, svc, "v3", "V3 内容。\n")
	revs, err := svc.Revisions("v3")
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 {
		t.Fatalf("expected 2 snapshots (pre-promote v2, pre-promote v3), got %d", len(revs))
	}

	res, err := svc.RestoreFrom("v3", 1, "回到最初版本")
	if err != nil {
		t.Fatal(err)
	}
	if res.RestoredVersion != 1 {
		t.Fatalf("expected v1, got %d", res.RestoredVersion)
	}
	got, _ := fs.ReadTopic("v3")
	if !strings.Contains(got.Body, "V1") {
		t.Fatalf("expected V1 content, got %q", got.Body)
	}
	if _, err := svc.RestoreFrom("v3", 99, "不存在的版本"); !errors.Is(err, ErrNoRevision) {
		t.Fatalf("expected ErrNoRevision for a missing version, got %v", err)
	}
	// 越界调用不能悄悄恢复成最接近的版本。
	after, _ := fs.ReadTopic("v3")
	if !strings.Contains(after.Body, "V1") {
		t.Fatalf("a failed restore must not modify the topic: %q", after.Body)
	}
}

// --- 过期建议 ------------------------------------------------------------

// TestExpireCandidatesSuggestsWithoutActing 建议接口**只读**。
func TestExpireCandidatesSuggestsWithoutActing(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "mostly-bad", "内容\n")
	promoteTopic(t, svc, "fine", "内容\n")
	if err := svc.RecordOutcome("mostly-bad", Outcome{Kind: OutcomeHarmful, At: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordOutcome("mostly-bad", Outcome{Kind: OutcomeHarmful, At: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordOutcome("fine", Outcome{Kind: OutcomeHelped, At: testNow}); err != nil {
		t.Fatal(err)
	}

	sw, err := svc.ExpireCandidates(ExpireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sw) != 1 || sw[0].Topic != "mostly-bad" || sw[0].Target != LifeArchived {
		t.Fatalf("unexpected suggestions %+v", sw)
	}
	// 建议之后什么都没发生。
	if _, err := fs.ReadTopic("mostly-bad"); err != nil {
		t.Fatalf("suggestions must not delete anything: %v", err)
	}
	if _, err := svc.LifecycleOf("mostly-bad"); !errors.Is(err, ErrNoLifecycle) {
		t.Fatalf("suggestions must not create lifecycle state, got %v", err)
	}
}

// TestExpireCandidatesIgnoresTopicsWithoutOutcomes 没有观测的主题不产生建议。
func TestExpireCandidatesIgnoresTopicsWithoutOutcomes(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "no-data", "内容\n")
	sw, err := svc.ExpireCandidates(ExpireOptions{StaleAfter: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if len(sw) != 0 {
		t.Fatalf("a topic without observations must not get a suggestion, got %+v", sw)
	}
}

// TestSingleHarmfulObservationDoesNotSuggestArchive 一次 harmful 不足以建议归档。
func TestSingleHarmfulObservationDoesNotSuggestArchive(t *testing.T) {
	svc, _ := newTestService(t)
	promoteTopic(t, svc, "one-slip", "内容\n")
	if err := svc.RecordOutcome("one-slip", Outcome{Kind: OutcomeHarmful, At: testNow}); err != nil {
		t.Fatal(err)
	}
	sw, err := svc.ExpireCandidates(ExpireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sw) != 0 {
		t.Fatalf("a single harmful observation must not trigger an archive suggestion, got %+v", sw)
	}
}

// TestOutcomeLedgerRejectsOverflow 观测条数超限报错而不是静默丢最旧的。
func TestOutcomeLedgerRejectsOverflow(t *testing.T) {
	svc, _ := newTestService(t)
	for i := 0; i < maxOutcomesPerTopic; i++ {
		if err := svc.RecordOutcome("busy", Outcome{Kind: OutcomeHelped, At: testNow.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	err := svc.RecordOutcome("busy", Outcome{Kind: OutcomeHelped, At: testNow})
	if !errors.Is(err, ErrTooManyOutcomes) {
		t.Fatalf("expected ErrTooManyOutcomes, got %v", err)
	}
	// 既有记录完好。
	got, err := svc.Outcomes("busy")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxOutcomesPerTopic {
		t.Fatalf("overflow must not drop records, got %d", len(got))
	}
}

// TestResetOutcomesIsExplicit 重置必须显式调用。
func TestResetOutcomesIsExplicit(t *testing.T) {
	svc, _ := newTestService(t)
	if err := svc.RecordOutcome("resettable", Outcome{Kind: OutcomeHelped, At: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ResetOutcomes("resettable"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Outcomes("resettable")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected an empty ledger, got %d", len(got))
	}
}

// reopenService 用同一个 store 造一个新 Service（模拟跨会话复用）。
func reopenService(t *testing.T, old *Service) *Service {
	t.Helper()
	svc, err := NewService(Config{
		Store:    old.cfg.Store,
		Promoter: old.cfg.Promoter,
		Now:      func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestBlockingEvidenceStopsFurtherPromotionOfSameTopic 负证据阻止同名主题
// 再次晋级：一条已被证明有害的主题，不该被新候选覆盖回去。
//
// 这条是"负证据必须影响行为"而非"只是记一笔"的检验。实现方式是宿主显式
// 查询 verdict 后拒绝 —— 本包不自动阻断（自动阻断需要判断"证据是否过期"，
// 那是宿主评估的职责），但判定口径必须可用且稳定。
func TestBlockingEvidenceStopsFurtherPromotionOfSameTopic(t *testing.T) {
	svc, fs := newTestService(t)
	promoteTopic(t, svc, "risky", "第一条版本\n")
	if err := svc.RecordOutcome("risky", Outcome{Kind: OutcomeHarmful, Note: "直接导致任务失败", At: testNow}); err != nil {
		t.Fatal(err)
	}
	verdict, err := svc.SummarizeEvidence("topic:risky")
	if err != nil {
		t.Fatal(err)
	}
	if !verdict.Blocking {
		t.Fatalf("expected blocking evidence, got %+v", verdict)
	}
	// 宿主据此拒绝晋级（这是宿主决策，本包提供口径）。
	before, _ := fs.ReadTopic("risky")
	c := topicCandidate("risky", "第二条版本\n")
	submitted, err := svc.Submit(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(context.Background(), submitted.ID); err != nil {
		t.Fatal(err)
	}
	if verdict.Blocking {
		// 宿主不调用 Promote。
		_ = submitted
	}
	after, _ := fs.ReadTopic("risky")
	if after.Body != before.Body {
		t.Fatal("the host declining to promote must leave the topic unchanged")
	}
}

// TestPhaseCDoesNotAutoRunAnything 确认控制面没有后台推进。
//
// 测的是"没有副作用"而不是"行为"：把时间推后很久、写入有害观测，主题不应
// 自己被降级或删除。自动淘汰必须由宿主显式调用。
func TestPhaseCDoesNotAutoRunAnything(t *testing.T) {
	root := t.TempDir()
	clock := testNow
	ms := memory.NewService(memory.Config{
		Root: root, Resolver: fixedResolver{id: "acme-repo-abc123"},
		Now: func() time.Time { return clock },
	})
	bound, err := ms.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(Config{
		Store: bound.Store, Promoter: PromoterFunc{Fn: ms.WriteTopic},
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	promoteTopic(t, svc, "quiet", "内容\n")
	for i := 0; i < 5; i++ {
		if err := svc.RecordOutcome("quiet", Outcome{Kind: OutcomeHarmful, At: clock}); err != nil {
			t.Fatal(err)
		}
	}
	// 把时钟推后一年：不该有任何东西自己动了。
	clock = clock.Add(365 * 24 * time.Hour)
	if _, err := bound.Store.ReadTopic("quiet"); err != nil {
		t.Fatalf("nothing may auto-archive a topic: %v", err)
	}
	if _, err := svc.LifecycleOf("quiet"); !errors.Is(err, ErrNoLifecycle) {
		t.Fatalf("no lifecycle record may appear on its own, got %v", err)
	}
	// 宿主显式查询时建议仍然正确。
	sw, err := svc.ExpireCandidates(ExpireOptions{StaleAfter: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(sw) != 1 || sw[0].Topic != "quiet" {
		t.Fatalf("expected a suggestion when asked explicitly, got %+v", sw)
	}
}
