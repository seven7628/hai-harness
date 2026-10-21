package agents

import (
	"testing"
	"time"
)

// 自动摘离阈值的两级解析（RunOptions 单次运行 → Config loop 级默认）。
//
// 这条链路此前是**死代码**：RunOptions.AsyncPromoteAfter 从来没有被宿主赋值，
// 引擎的 promoteAfter 恒为 0 —— 于是「长工具不钉死主 loop」只剩用户手点 promote 一条路。
// 现在宿主读 settings 的 agent.async_promote_after_seconds 接到 Config，
// 判据（谁压谁）必须由本用例钉住，否则装配一变就是静默回到「全关」。
func TestEffectivePromoteAfter(t *testing.T) {
	const (
		off  = time.Duration(0)
		loop = 60 * time.Second
		run  = 5 * time.Second
	)
	cases := []struct {
		name    string
		run, lp time.Duration
		want    time.Duration
		why     string
	}{
		{"都没配 → 关", off, off, off, "默认行为必须逐字节不变"},
		{"只配 loop 级 → 用 loop", off, loop, loop, "宿主只有一个装配点，不该让每处 RunOptions 抄一遍"},
		{"RunOptions 非零 → 压过 loop", run, loop, run, "单次运行可覆盖（测试/子 agent 不同策略）"},
		{"RunOptions 负值 → 显式关闭", -1, loop, off, "0 已表示「没声明」，关闭必须能用负值表达"},
		{"loop 负值 → 关", off, -1, off, "宿主误配负值时按关处理，不 panic 不穿透"},
	}
	for _, c := range cases {
		if got := effectivePromoteAfter(c.run, c.lp); got != c.want {
			t.Errorf("%s: effectivePromoteAfter(%v, %v) = %v, want %v（%s）", c.name, c.run, c.lp, got, c.want, c.why)
		}
	}
}
