package main

import (
	"testing"
	"time"
)

// settings 的 agent.async_promote_after_seconds → 生效阈值（0 = 关）。
//
// 钳制理由写在该常量块：下界 10s（低于它与常态无异，摘离只会制造占位往返）、
// 上界 1h（再长等于关闭，不如写 0）。越界不报错而是钳制 —— 这条设置错了只会让
// 「什么时候摘离」偏一点，不值得让会话起不来；但**必须钳**，否则 0.5 秒的误配会把
// 每次 grep 都推进后台。
func TestAsyncPromoteAfterOf(t *testing.T) {
	i := func(v int) *int { return &v }
	cases := []struct {
		name string
		in   *int
		want time.Duration
	}{
		{"未配置 → 关", nil, 0},
		{"显式 0 → 关", i(0), 0},
		{"负值 → 关", i(-5), 0},
		{"正常值原样生效", i(120), 120 * time.Second},
		{"低于下界 → 钳到 10s", i(1), 10 * time.Second},
		{"高于上界 → 钳到 1h", i(99999), time.Hour},
	}
	for _, c := range cases {
		if got := asyncPromoteAfterOf(c.in); got != c.want {
			t.Errorf("%s: asyncPromoteAfterOf = %v, want %v", c.name, got, c.want)
		}
	}
}
