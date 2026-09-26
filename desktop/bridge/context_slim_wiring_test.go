package main

import "testing"

// 三宿主（主会话 / spawn / explore）的 codeAgentTuning 必须都带 contextSlim ——
// 漏一个就意味着该 loop 的上下文不受治理（本文件既有注释正是在警告
// 「看起来有、实际失效」的开关）。
func TestContextSlimWiredForAllHosts(t *testing.T) {
	for _, host := range []codeAgentHost{hostMainLoop, hostSpawnLoop, hostExploreLoop} {
		tn := codeAgentTuningFor(host)
		if !tn.contextSlim.enabled {
			t.Fatalf("%s: contextSlim 未启用", host)
		}
		// 三宿主取同一组 slim 选项（单一事实源）：断言条数与开关值，
		// 避免「某宿主漏传」这种回归静默通过。
		want := contextSlimTuningFor().options()
		got := tn.contextSlim.options()
		if len(got) != len(want) {
			t.Fatalf("%s: slim 选项数 %d，期望 %d", host, len(got), len(want))
		}
		// 既有基线：postToolNudge + compressThreshold 恒有（2 项），
		// goalAlignmentRounds 仅主会话有（子宿主刻意不接，见 codeAgentTuning 注释）。
		base := 2
		if tn.goalAlignmentRounds != nil {
			base++
		}
		if n := len(tn.options()); n != base+len(want) {
			t.Fatalf("%s: options() = %d 项，期望 %d（基线 %d + slim %d）",
				host, n, base+len(want), base, len(want))
		}
	}
	// 逃生舱：enabled=false 时不得产生任何 slim 选项
	var off codeAgentTuning
	off.contextSlim = contextSlimTuning{enabled: false}
	if got := off.contextSlim.options(); got != nil {
		t.Fatalf("关闭时仍产生 %d 个选项", len(got))
	}
}
