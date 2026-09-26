package evolution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/memory"
)

// newSkillService 造一个带假 SkillInstaller 的控制面。
//
// 假 installer 把技能落成 skills/ 下的 SKILL.md 文本并记录安装集合，
// 这样测试能同时验证"技能确实被安装"与"撤销确实移除了"，
// 而不依赖宿主真实的 skills.Registry 装配。
func newSkillService(t *testing.T) (*Service, *fakeInstaller) {
	t.Helper()
	svc, _ := newTestService(t)
	inst := &fakeInstaller{}
	svc.cfg.SkillInstaller = inst
	return svc, inst
}

type fakeInstaller struct {
	installed map[string]SkillCandidate
	removed   []string
	failOn    string
}

func (f *fakeInstaller) Install(c *SkillCandidate) (string, error) {
	if f.failOn == c.Name {
		return "", errString("install refused by host")
	}
	if f.installed == nil {
		f.installed = map[string]SkillCandidate{}
	}
	f.installed[c.Name] = *c
	return "<skills>/" + c.Name, nil
}

func (f *fakeInstaller) Remove(name string) error {
	if _, ok := f.installed[name]; !ok {
		return nil
	}
	delete(f.installed, name)
	f.removed = append(f.removed, name)
	return nil
}

func goodSkill(name string, refs ...string) *SkillCandidate {
	return &SkillCandidate{
		Name:         name,
		Description:  "Recover from a failed build",
		Version:      "1.0.0",
		Instructions: "# Recover\n\nRun ./scripts/ci.sh --recover before retrying.",
		MemoryRefs:   refs,
	}
}

// verifyCandidate 提交 + 验证一条技能候选，返回已验证的候选。
func verifyCandidate(t *testing.T, svc *Service, c *SkillCandidate) *Candidate {
	t.Helper()
	submitted, err := svc.SubmitSkill(c, "从失败的构建里总结出来的")
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
	return stored
}

// newTestServiceWithInstaller 造一个带假 installer 的控制面并返回其 Phase A store。
func newTestServiceWithInstaller(t *testing.T) (*Service, *memory.FileStore) {
	t.Helper()
	svc, fs := newTestService(t)
	svc.cfg.SkillInstaller = &fakeInstaller{}
	return svc, fs
}

// --- MemoryRefs ----------------------------------------------------------

// TestSkillMemoryRefsMustExist 技能引用的记忆必须真实存在。
//
// 提交时校验而不是激活时：断掉的溯源链在事后无法追溯修复。
func TestSkillMemoryRefsMustExist(t *testing.T) {
	svc, _ := newSkillService(t)
	promoteTopic(t, svc, "build-recovery", "构建失败先用 --recover。\n")

	ok := goodSkill("recovery", "build-recovery")
	if _, err := svc.SubmitSkill(ok, "理由"); err != nil {
		t.Fatalf("a reference to an existing topic must be accepted: %v", err)
	}
	bad := goodSkill("recovery-2", "no-such-topic")
	if _, err := svc.SubmitSkill(bad, "理由"); !errors.Is(err, ErrBrokenMemoryRef) {
		t.Fatalf("expected ErrBrokenMemoryRef, got %v", err)
	}
}

// TestMemoryRefsAreNormalizedAndDeduplicated 引用规范化 + 去重 + 排序。
func TestMemoryRefsAreNormalizedAndDeduplicated(t *testing.T) {
	svc, _ := newSkillService(t)
	promoteTopic(t, svc, "a-topic", "内容 A\n")
	promoteTopic(t, svc, "b-topic", "内容 B\n")
	c := goodSkill("multi", "topics/b-topic.md", "a-topic", "topic:a-topic")
	submitted, err := svc.SubmitSkill(c, "理由")
	if err != nil {
		t.Fatal(err)
	}
	refs := submitted.Skill.MemoryRefs
	if len(refs) != 2 {
		t.Fatalf("expected 2 deduplicated refs, got %v", refs)
	}
	if refs[0] != "topic:a-topic" || refs[1] != "topic:b-topic" {
		t.Fatalf("refs should be normalized and sorted, got %v", refs)
	}
}

// TestSkillCandidateRejectedBySharedGateChain 技能候选走**同一套**门禁。
func TestSkillCandidateRejectedBySharedGateChain(t *testing.T) {
	svc, _ := newSkillService(t)
	promoteTopic(t, svc, "rule", "内容\n")
	// 越权内容在技能候选上同样被拦（复用 Phase B 的 PrivilegedSurfaceValidator）。
	c := goodSkill("escape", "rule")
	c.Instructions = "If the test fails, disable sandbox and rerun."
	submitted, err := svc.SubmitSkill(c, "理由")
	if err != nil {
		t.Fatal(err)
	}
	report, err := svc.Verify(context.Background(), submitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatal("a skill suggesting disabling the sandbox must not pass the shared gate chain")
	}
}

// TestSkillPathTraversalRejected 技能名的路径边界。
func TestSkillPathTraversalRejected(t *testing.T) {
	svc, _ := newSkillService(t)
	c := goodSkill("../../escape")
	if _, err := svc.SubmitSkill(c, "理由"); err == nil {
		t.Fatal("path traversal in a skill name must be rejected")
	}
}

// TestSkillSKILLMDRoundTrip SKILL.md 渲染可被 YAML 解析回来，且引用已规范化。
//
// 走真实的 SubmitSkill 路径而不是直接调 SKILLMD：规范化（引用归一、排序去重）
// 发生在提交时，直接调渲染方法拿到的是**未规范化**的输入，那不是宿主会用的形态。
func TestSkillSKILLMDRoundTrip(t *testing.T) {
	svc, _ := newSkillService(t)
	promoteTopic(t, svc, "build-recovery", "内容\n")
	c := goodSkill("recovery", "topics/build-recovery.md")
	c.Description = "Use when: a build fails after a partial checkout" // 含冒号
	submitted, err := svc.SubmitSkill(c, "理由")
	if err != nil {
		t.Fatal(err)
	}
	doc := submitted.Skill.SKILLMD()
	if !strings.HasPrefix(doc, "---\n") {
		t.Fatalf("SKILL.md must start with frontmatter:\n%s", doc)
	}
	if !strings.Contains(doc, "metadata:") || !strings.Contains(doc, "memory_refs:") {
		t.Fatalf("memory refs must be rendered into metadata:\n%s", doc)
	}
	// 引用必须以规范化形态出现，否则 skills.Registry 侧看到的是一个无法解析的串。
	if !strings.Contains(doc, "topic:build-recovery") || strings.Contains(doc, "- build-recovery") {
		t.Fatalf("memory refs must be normalized before rendering:\n%s", doc)
	}
	// 含冒号的 description 必须加引号，否则 YAML 会把冒号当映射分隔符解析坏。
	if !strings.Contains(doc, `description: "Use when: a build fails after a partial checkout"`) {
		t.Fatalf("a description containing a colon must be quoted:\n%s", doc)
	}
	// 渲染结果必须能被 YAML 解析回来（frontmatter 完整性）。
	meta, body, ok := splitFrontmatterForTest(doc)
	if !ok {
		t.Fatalf("frontmatter is not parseable:\n%s", doc)
	}
	if meta["name"] != "recovery" {
		t.Fatalf("unexpected name %q", meta["name"])
	}
	if body == "" {
		t.Fatal("the skill body must survive rendering")
	}
}

// --- 技能候选的晋级与撤销 -------------------------------------------------

// TestSkillActivationRequiresGate 通过门禁的技能才能激活。
func TestSkillActivationRequiresGate(t *testing.T) {
	svc, inst := newSkillService(t)
	promoteTopic(t, svc, "build-recovery", "内容\n")

	// 未验证：不能激活。
	pending, err := svc.SubmitSkill(goodSkill("recovery", "build-recovery"), "理由")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateSkill(pending); !errors.Is(err, ErrSkillNotVerified) {
		t.Fatalf("expected ErrSkillNotVerified, got %v", err)
	}
	if len(inst.installed) != 0 {
		t.Fatal("nothing may be installed before the gate passes")
	}

	verified := verifyCandidate(t, svc, goodSkill("recovery", "build-recovery"))
	dir, err := svc.ActivateSkill(verified)
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		t.Fatal("expected an install directory")
	}
	if _, ok := inst.installed["recovery"]; !ok {
		t.Fatal("the skill should have been installed")
	}
	rec, err := svc.SkillRecord("recovery")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != SkillActive {
		t.Fatalf("expected active, got %q", rec.Status)
	}
}

// TestActivateWithoutInstallerFails 没有 installer 时明确报错，不猜命名空间。
func TestActivateWithoutInstallerFails(t *testing.T) {
	svc, _ := newTestService(t) // 没有 SkillInstaller
	promoteTopic(t, svc, "build-recovery", "内容\n")
	verified := verifyCandidate(t, svc, goodSkill("recovery", "build-recovery"))
	if _, err := svc.ActivateSkill(verified); !errors.Is(err, ErrNoSkillInstaller) {
		t.Fatalf("expected ErrNoSkillInstaller, got %v", err)
	}
}

// TestActivateRechecksMemoryRefs 激活时重验引用（记忆可能已被归档）。
func TestActivateRechecksMemoryRefs(t *testing.T) {
	svc, _ := newSkillService(t)
	promoteTopic(t, svc, "transient", "临时记忆\n")
	verified := verifyCandidate(t, svc, goodSkill("uses-transient", "transient"))
	if _, err := svc.Archive("transient", "确认不再需要"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivateSkill(verified); !errors.Is(err, ErrBrokenMemoryRef) {
		t.Fatalf("activation must recheck memory refs, got %v", err)
	}
}

// TestRevokeSkillRemovesInstallation 负迁移后的撤销入口。
func TestRevokeSkillRemovesInstallation(t *testing.T) {
	svc, inst := newSkillService(t)
	promoteTopic(t, svc, "build-recovery", "内容\n")
	verified := verifyCandidate(t, svc, goodSkill("recovery", "build-recovery"))
	if _, err := svc.ActivateSkill(verified); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeSkill("recovery", "评估显示它让结果变差"); err != nil {
		t.Fatal(err)
	}
	if _, ok := inst.installed["recovery"]; ok {
		t.Fatal("revoke must remove the installed skill")
	}
	if len(inst.removed) != 1 || inst.removed[0] != "recovery" {
		t.Fatalf("unexpected removals %v", inst.removed)
	}
	rec, _ := svc.SkillRecord("recovery")
	if rec.Status != SkillRevoked {
		t.Fatalf("expected revoked, got %q", rec.Status)
	}
}

// TestRevokeRequiresReason 撤销也要依据。
func TestRevokeRequiresReason(t *testing.T) {
	svc, _ := newSkillService(t)
	if err := svc.RevokeSkill("nothing", ""); !errors.Is(err, ErrNoReason) {
		t.Fatalf("expected ErrNoReason, got %v", err)
	}
}

// TestSkillInstallFailureIsNotRecordedAsActive 安装失败不得留下 active 记录。
func TestSkillInstallFailureIsNotRecordedAsActive(t *testing.T) {
	svc, inst := newSkillService(t)
	promoteTopic(t, svc, "build-recovery", "内容\n")
	inst.failOn = "recovery"
	verified := verifyCandidate(t, svc, goodSkill("recovery", "build-recovery"))
	if _, err := svc.ActivateSkill(verified); err == nil {
		t.Fatal("expected the install error to propagate")
	}
	rec, err := svc.SkillRecord("recovery")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status == SkillActive {
		t.Fatal("a failed install must not be recorded as active")
	}
}

// --- 联合 Replay：2×2 对照 ------------------------------------------------

// armsFor 造一组典型的四臂结果。
func armsFor(none, mem, skill, both float64) map[JointArm]JointArmResult {
	return map[JointArm]JointArmResult{
		ArmNone:   {Arm: ArmNone, Score: none},
		ArmMemory: {Arm: ArmMemory, Score: mem},
		ArmSkill:  {Arm: ArmSkill, Score: skill},
		ArmBoth:   {Arm: ArmBoth, Score: both},
	}
}

// splitFrontmatterForTest 极简 frontmatter 解析（测试用）。
func splitFrontmatterForTest(doc string) (map[string]string, string, bool) {
	if !strings.HasPrefix(doc, "---\n") {
		return nil, "", false
	}
	rest := doc[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return nil, "", false
	}
	meta := map[string]string{}
	for _, line := range strings.Split(rest[:end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		meta[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	return meta, rest[end+5:], true
}

func balancedRequest() JointRequest {
	b := time.Minute
	return JointRequest{
		TaskID: "t1", Budget: b,
		Configs: []JointConfig{
			{Arm: ArmNone, Budget: b}, {Arm: ArmMemory, Budget: b},
			{Arm: ArmSkill, Budget: b}, {Arm: ArmBoth, Budget: b},
		},
	}
}

// TestJointEffectiveOnlyWhenNotExplainedByOneSide 核心不变式：
// 联合收益能被单侧完全解释时，**不得**宣称联合有效。
func TestJointEffectiveOnlyWhenNotExplainedByOneSide(t *testing.T) {
	// 记忆 0.8、技能 0.5、联合 0.8 —— 联合的收益就是记忆的收益，没有额外东西。
	v := JointEvaluate(balancedRequest(), armsFor(0.0, 0.8, 0.5, 0.8), JointOptions{MinGain: 0.1, MinResidual: 0.05})
	if v.JointEffective {
		t.Fatalf("joint must not be claimed when the gain is fully explained by memory alone: %+v", v)
	}
	if v.Residual >= 0.05 {
		t.Fatalf("expected a near-zero residual, got %.3f", v.Residual)
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "explained by a single side") {
		t.Fatalf("reasons should explain the single-side explanation, got %v", v.Reasons)
	}
}

// TestJointEffectiveWithRealResidual 联合确实带来额外收益时可以宣称。
func TestJointEffectiveWithRealResidual(t *testing.T) {
	// 记忆 0.6、技能 0.5、联合 0.9 —— 超出最好单侧的 0.3 是真交互。
	v := JointEvaluate(balancedRequest(), armsFor(0.0, 0.6, 0.5, 0.9), JointOptions{MinGain: 0.1, MinResidual: 0.05})
	if !v.JointEffective {
		t.Fatalf("expected joint effective, got %+v", v)
	}
	if v.Residual < 0.05 {
		t.Fatalf("expected a positive residual, got %.3f", v.Residual)
	}
	if v.NegativeMigration {
		t.Fatal("no negative migration here")
	}
}

// TestJointInconclusiveBelowNoiseFloor 提升低于噪声地板时结论是 inconclusive。
func TestJointInconclusiveBelowNoiseFloor(t *testing.T) {
	// 残差 0.10 高于 MinResidual，唯一该拦它的是噪声地板 0.50。
	v := JointEvaluate(balancedRequest(), armsFor(0.0, 0.10, 0.05, 0.20), JointOptions{MinGain: 0.5, MinResidual: 0.05})
	if v.JointEffective {
		t.Fatal("a gain below the noise floor must not be claimed as effective")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "noise floor") {
		t.Fatalf("reasons should mention the noise floor, got %v", v.Reasons)
	}
}

// TestNegativeMigrationBlocksJointClaim 核心负向测试：
// 联合比单侧都差时**必须**检出负迁移并拒绝宣称。
func TestNegativeMigrationBlocksJointClaim(t *testing.T) {
	// 记忆 0.9、技能 0.85、联合 0.4 —— 联合把两个都有效的单侧弄坏了。
	v := JointEvaluate(balancedRequest(), armsFor(0.0, 0.9, 0.85, 0.4), JointOptions{MinGain: 0.1, MinResidual: 0.05})
	if !v.NegativeMigration {
		t.Fatalf("negative migration must be detected: %+v", v)
	}
	if v.JointEffective {
		t.Fatal("joint effectiveness must not be claimed under negative migration")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "negative migration") {
		t.Fatalf("reasons should mention negative migration, got %v", v.Reasons)
	}
}

// TestNoGainIsNotNegativeMigration "没收益"与"负迁移"是两件事。
func TestNoGainIsNotNegativeMigration(t *testing.T) {
	// 联合等于基线：既没收益也没负迁移（联合不比单侧差）。
	v := JointEvaluate(balancedRequest(), armsFor(0.0, 0.0, 0.0, 0.0), JointOptions{MinGain: 0.1, MinResidual: 0.05})
	if v.NegativeMigration {
		t.Fatalf("no gain is not negative migration: %+v", v)
	}
	if v.JointEffective {
		t.Fatal("no gain cannot be claimed as effective")
	}
}

// TestIncompleteComparisonIsNotClaimed 缺臂时不得宣称。
func TestIncompleteComparisonIsNotClaimed(t *testing.T) {
	partial := map[JointArm]JointArmResult{
		ArmNone: {Arm: ArmNone, Score: 0}, ArmBoth: {Arm: ArmBoth, Score: 1},
	}
	v := JointEvaluate(balancedRequest(), partial, JointOptions{MinGain: 0.1, MinResidual: 0.05})
	if v.JointEffective {
		t.Fatal("an incomplete 2x2 cannot support a joint claim")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "all four arms") {
		t.Fatalf("reasons should mention the missing arms, got %v", v.Reasons)
	}
}

// TestBuildJointRequestDescribesVisibleSets 2×2 的自变量是"各臂能看到什么"。
func TestBuildJointRequestDescribesVisibleSets(t *testing.T) {
	svc, _ := newSkillService(t)
	promoteTopic(t, svc, "build-recovery", "内容\n")
	verified := verifyCandidate(t, svc, goodSkill("recovery", "build-recovery"))
	if _, err := svc.ActivateSkill(verified); err != nil {
		t.Fatal(err)
	}
	req, err := svc.BuildJointRequest("t1", []string{"build-recovery"}, []string{"recovery"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Configs) != 4 {
		t.Fatalf("expected 4 arms, got %d", len(req.Configs))
	}
	byArm := map[JointArm]JointConfig{}
	for _, c := range req.Configs {
		byArm[c.Arm] = c
	}
	if len(byArm[ArmNone].MemoryRefs) != 0 || len(byArm[ArmNone].SkillNames) != 0 {
		t.Fatalf("the baseline arm must see nothing, got %+v", byArm[ArmNone])
	}
	if len(byArm[ArmMemory].MemoryRefs) != 1 || len(byArm[ArmMemory].SkillNames) != 0 {
		t.Fatalf("memory-only arm is wrong: %+v", byArm[ArmMemory])
	}
	if len(byArm[ArmSkill].SkillNames) != 1 || len(byArm[ArmSkill].MemoryRefs) != 0 {
		t.Fatalf("skill-only arm is wrong: %+v", byArm[ArmSkill])
	}
	both := byArm[ArmBoth]
	if len(both.MemoryRefs) != 1 || len(both.SkillNames) != 1 {
		t.Fatalf("joint arm is wrong: %+v", both)
	}
	// 四臂预算必须相同，否则对照无效。
	for _, c := range req.Configs {
		if c.Budget != req.Budget {
			t.Fatalf("arm %s has budget %s, want %s", c.Arm, c.Budget, req.Budget)
		}
	}
}

// TestBuildJointRequestRequiresActiveSkill 未激活的技能不能进实验臂。
func TestBuildJointRequestRequiresActiveSkill(t *testing.T) {
	svc, _ := newSkillService(t)
	promoteTopic(t, svc, "build-recovery", "内容\n")
	verifyCandidate(t, svc, goodSkill("recovery", "build-recovery")) // 未激活
	if _, err := svc.BuildJointRequest("t1", []string{"build-recovery"}, []string{"recovery"}, time.Minute); !errors.Is(err, ErrSkillNotVerified) {
		t.Fatalf("expected ErrSkillNotVerified, got %v", err)
	}
}

// TestBuildJointRequestRequiresBudget 没有预算 = 没有配对，拒绝。
func TestBuildJointRequestRequiresBudget(t *testing.T) {
	svc, _ := newSkillService(t)
	if _, err := svc.BuildJointRequest("t1", nil, nil, 0); err == nil {
		t.Fatal("a joint request without a budget must be rejected (arms would be unpaired)")
	}
}

// TestRunJointWithoutReplayerClaimsNothing 默认不做真实 LLM 调用：
// 没注入执行器时结论是"实验没跑"，而不是一个看起来跑过的空结论。
func TestRunJointWithoutReplayerClaimsNothing(t *testing.T) {
	svc, _ := newSkillService(t) // 没有 JointReplayer
	v, err := svc.RunJoint(context.Background(), balancedRequest(), JointOptions{})
	if !errors.Is(err, ErrNoReplayer) {
		t.Fatalf("expected ErrNoReplayer, got %v", err)
	}
	if v.JointEffective {
		t.Fatal("nothing may be claimed when the experiment did not run")
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "did not run") {
		t.Fatalf("reasons should say the experiment did not run, got %v", v.Reasons)
	}
}

// TestRunJointRejectsUnpairedBudgets 预算不一致的 2×2 必须被挡住。
func TestRunJointRejectsUnpairedBudgets(t *testing.T) {
	svc, _ := newSkillService(t)
	svc.cfg.JointReplayer = JointReplayerFunc{
		ReplayerName: "host",
		Fn: func(context.Context, JointRequest) (map[JointArm]JointArmResult, error) {
			t.Fatal("the replayer must not be called for an unpaired request")
			return nil, nil
		},
	}
	req := balancedRequest()
	req.Configs[2].Budget = 2 * time.Minute // 破坏配平
	if _, err := svc.RunJoint(context.Background(), req, JointOptions{}); err == nil {
		t.Fatal("unpaired arms must be rejected before spending any budget")
	}
}

// TestRunJointUsesInjectedReplayer 宿主注入后正常跑完并判定。
func TestRunJointUsesInjectedReplayer(t *testing.T) {
	svc, _ := newSkillService(t)
	calls := 0
	svc.cfg.JointReplayer = JointReplayerFunc{
		ReplayerName: "host",
		Fn: func(context.Context, JointRequest) (map[JointArm]JointArmResult, error) {
			calls++
			return armsFor(0.0, 0.6, 0.5, 0.9), nil
		},
	}
	v, err := svc.RunJoint(context.Background(), balancedRequest(), JointOptions{MinGain: 0.1, MinResidual: 0.05})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("the replayer should be called exactly once, got %d", calls)
	}
	if !v.JointEffective {
		t.Fatalf("expected joint effective, got %+v", v)
	}
}

// TestRunJointRejectsIncompleteResult 执行器没返回四臂时报错而不是"部分有效"。
func TestRunJointRejectsIncompleteResult(t *testing.T) {
	svc, _ := newSkillService(t)
	svc.cfg.JointReplayer = JointReplayerFunc{
		ReplayerName: "host",
		Fn: func(context.Context, JointRequest) (map[JointArm]JointArmResult, error) {
			return map[JointArm]JointArmResult{ArmBoth: {Arm: ArmBoth, Score: 1}}, nil
		},
	}
	if _, err := svc.RunJoint(context.Background(), balancedRequest(), JointOptions{}); err == nil {
		t.Fatal("an incomplete result set must be an error, not a partial claim")
	}
}

// --- 技能负迁移评估 ------------------------------------------------------

// TestSkillNegativeTransferDetected 核心负向测试：
// 技能单用挺好、与记忆搭配后变差 → 判为负迁移。
func TestSkillNegativeTransferDetected(t *testing.T) {
	svc, _ := newSkillService(t)
	// 技能单用 0.85（很好），联合 0.4（被记忆带偏）。
	v := JointEvaluate(balancedRequest(), armsFor(0.0, 0.9, 0.85, 0.4), JointOptions{MinGain: 0.1, MinResidual: 0.05})
	sv := svc.EvaluateSkillTransfer(v, "recovery", SkillTransferOptions{MinDelta: 0.0})
	if !sv.NegativeTransfer {
		t.Fatalf("negative transfer must be detected: %+v", sv)
	}
	if sv.Delta >= 0 {
		t.Fatalf("expected a negative delta, got %.3f", sv.Delta)
	}
	if !strings.Contains(sv.Reason, "below skill-only") {
		t.Fatalf("reason should explain the drop, got %q", sv.Reason)
	}
}

// TestSkillHarmfulEvidenceImpliesNegativeTransfer 有害负证据本身足以判负。
//
// 这一条与实测 Delta 独立：一次"明显变差"和一个"反复记录的害处"是两个
// 各自都不足以确信、但任意一个都值得停下来的信号。
func TestSkillHarmfulEvidenceImpliesNegativeTransfer(t *testing.T) {
	svc, _ := newSkillService(t)
	// 实测 Delta 为正（技能与记忆搭配略好），但存在有害负证据。
	v := JointEvaluate(balancedRequest(), armsFor(0.0, 0.6, 0.5, 0.7), JointOptions{MinGain: 0.1, MinResidual: 0.05})
	if err := svc.RecordNegative(NegativeEvidence{
		Target: "skill:recovery", Kind: NegHarmful, Detail: "让 Agent 重复调用了同一个失败工具", At: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	sv := svc.EvaluateSkillTransfer(v, "recovery", SkillTransferOptions{MinDelta: -1.0, RequireNoHarmfulEvidence: true})
	if !sv.NegativeTransfer {
		t.Fatalf("harmful evidence must imply negative transfer: %+v", sv)
	}
	if !strings.Contains(sv.Reason, "harmful") {
		t.Fatalf("reason should mention the harmful evidence, got %q", sv.Reason)
	}
}

// TestSkillNoNegativeTransfer 单侧有效且与记忆无交互不算负迁移。
func TestSkillNoNegativeTransfer(t *testing.T) {
	svc, _ := newSkillService(t)
	// 技能单用 0.5，联合 0.9 —— 记忆帮了技能。
	v := JointEvaluate(balancedRequest(), armsFor(0.0, 0.6, 0.5, 0.9), JointOptions{MinGain: 0.1, MinResidual: 0.05})
	sv := svc.EvaluateSkillTransfer(v, "recovery", SkillTransferOptions{MinDelta: 0.0, RequireNoHarmfulEvidence: true})
	if sv.NegativeTransfer {
		t.Fatalf("no negative transfer expected: %+v", sv)
	}
	if sv.Delta <= 0 {
		t.Fatalf("expected a positive delta, got %.3f", sv.Delta)
	}
}

// TestSkillTransferNeedsCompleteArms 缺臂时不下结论。
func TestSkillTransferNeedsCompleteArms(t *testing.T) {
	svc, _ := newSkillService(t)
	partial := JointVerdict{TaskID: "t1", Results: map[JointArm]JointArmResult{
		ArmSkill: {Arm: ArmSkill, Score: 0.5},
	}}
	sv := svc.EvaluateSkillTransfer(partial, "recovery", SkillTransferOptions{})
	if sv.NegativeTransfer {
		t.Fatal("an incomplete comparison must not produce a negative-transfer verdict")
	}
	if !strings.Contains(sv.Reason, "incomplete") {
		t.Fatalf("reason should flag the incomplete comparison, got %q", sv.Reason)
	}
}

// TestSkillMigrationEvidenceRecordsJointHarm 负迁移证据可被记录并回查。
func TestSkillMigrationEvidenceRecordsJointHarm(t *testing.T) {
	svc, _ := newSkillService(t)
	if err := svc.RecordNegative(NegativeEvidence{
		Target: "skill:recovery", Kind: NegMigration, Detail: "2x2 对照中联合臂得分低于单侧臂", At: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	ev, err := svc.NegativeEvidenceOf("skill:recovery")
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Kind != NegMigration {
		t.Fatalf("unexpected evidence %+v", ev)
	}
	sum, err := svc.SummarizeEvidence("skill:recovery")
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Blocking {
		t.Fatalf("migration evidence should be blocking, got %+v", sum)
	}
}

// TestPhaseDDoesNotChangeMemoryRecallability Phase D 不改变 Phase A 的可召回性。
//
// 技能的激活/撤销不写 MEMORY.md、不动 topics/：技能与记忆是两个独立能力，
// Phase D 引入的关联只是溯源信息。
func TestPhaseDDoesNotChangeMemoryRecallability(t *testing.T) {
	svc, fs := newTestServiceWithInstaller(t)
	promoteTopic(t, svc, "build-recovery", "内容\n")
	before, err := fs.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	verified := verifyCandidate(t, svc, goodSkill("recovery", "build-recovery"))
	if _, err := svc.ActivateSkill(verified); err != nil {
		t.Fatal(err)
	}
	if err := svc.RevokeSkill("recovery", "测试"); err != nil {
		t.Fatal(err)
	}
	after, err := fs.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if before.Raw != after.Raw {
		t.Fatalf("skill lifecycle must not touch MEMORY.md:\nbefore:\n%s\nafter:\n%s", before.Raw, after.Raw)
	}
	if _, err := fs.ReadTopic("build-recovery"); err != nil {
		t.Fatalf("skill lifecycle must not touch topics/: %v", err)
	}
}
