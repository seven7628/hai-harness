package memory

import (
	"testing"
	"time"
)

// "每天最多一次"是无人值守下唯一的频率闸：手动可以随时跑，自动不行。
func TestAutoDreamDailyCap(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	d := memDir(t)
	cfg := Config{Now: func() time.Time { return now }}
	t.Setenv(EnvDreamEvalCommand, "true")

	if why := cfg.AutoDreamBlocker(d); why != "" {
		t.Fatalf("首次应放行，实际被挡：%s", why)
	}
	if err := cfg.MarkAutoDream(d, 1); err != nil {
		t.Fatal(err)
	}
	if why := cfg.AutoDreamBlocker(d); why == "" {
		t.Fatal("刚跑过，应当被挡")
	}

	// 过了 24 小时再放行。
	cfg2 := Config{Now: func() time.Time { return now.Add(25 * time.Hour) }}
	if why := cfg2.AutoDreamBlocker(d); why != "" {
		t.Fatalf("超过一天应放行，实际：%s", why)
	}
}

// 没配回归命令 → 不允许自动归档。这条是安全底线：不验证的无人值守归档
// 等于把"有没有弄坏东西"交给运气。
func TestAutoDreamRequiresEvalCommand(t *testing.T) {
	d := memDir(t)
	cfg := Config{}
	t.Setenv(EnvDreamEvalCommand, "")

	why := cfg.AutoDreamBlocker(d)
	if why == "" {
		t.Fatal("没有回归命令时必须拦住")
	}
	if !contains(why, EnvDreamEvalCommand) {
		t.Fatalf("理由里要说明缺的是哪个配置：%s", why)
	}
}

// 记忆关掉时什么都不做。
func TestAutoDreamRespectsDisabled(t *testing.T) {
	off := false
	d := memDir(t)
	cfg := Config{Enabled: &off}
	t.Setenv(EnvDreamEvalCommand, "true")
	if why := cfg.AutoDreamBlocker(d); why == "" {
		t.Fatal("记忆关闭时不该放行")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
