package memory

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// clearEnv 清掉三个开关环境变量，避免调用者的 shell 配置影响断言。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvAutoMemoryEnabled, EnvAutoMemoryDirectory, EnvProjectDirName} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// 默认开启（官方 auto memory 默认开）。
func TestConfig_DefaultEnabled(t *testing.T) {
	clearEnv(t)
	if !(Config{}).enabled() {
		t.Fatal("默认应开启（官方 auto memory 默认开）")
	}
	if (Config{}).Disabled() {
		t.Fatal("Disabled() 应为 false")
	}
}

// 用户在设置面板关掉 → 关闭；不重启应用即生效（每次读 Config 重新求值）。
func TestConfig_UserToggle(t *testing.T) {
	clearEnv(t)
	off := Config{Enabled: Bool(false)}
	if off.enabled() || !off.Disabled() {
		t.Fatal("用户关闭后应解析为关闭")
	}
	// 再打开（模拟用户在设置面板切回来，同一个 Config 结构被重建）。
	on := Config{Enabled: Bool(true)}
	if !on.enabled() || on.Disabled() {
		t.Fatal("用户打开后应解析为开启")
	}
	// 三态：nil = 用户没设置过 = 跟随环境变量/默认。
	if !(Config{}).enabled() {
		t.Fatal("nil（未设置）应解析为开启")
	}
}

// 环境变量对齐官方三个开关，且**覆盖**用户偏好（CI 要能固定住空状态）。
func TestConfig_EnvOverrides(t *testing.T) {
	clearEnv(t)
	cases := []struct {
		val  string
		want bool
	}{
		{"1", true}, {"true", true}, {"TRUE", true}, {"yes", true}, {"on", true},
		{"0", false}, {"false", false}, {"FALSE", false}, {"no", false}, {"off", false},
		{" true ", true}, {"false ", false},
	}
	for _, c := range cases {
		t.Setenv(EnvAutoMemoryEnabled, c.val)
		got := (Config{}).enabled()
		if got != c.want {
			t.Fatalf("autoMemoryEnabled=%q → %v, want %v", c.val, got, c.want)
		}
		// 环境变量盖过用户偏好。
		if (Config{Enabled: Bool(true)}).enabled() != c.want {
			t.Fatalf("autoMemoryEnabled=%q 未盖过用户偏好", c.val)
		}
		if (Config{Enabled: Bool(false)}).enabled() != c.want {
			t.Fatalf("autoMemoryEnabled=%q 未盖过用户偏好", c.val)
		}
	}
	// 值不认识按"未设"处理（不硬失败：拼错一个值不该让应用起不来）。
	t.Setenv(EnvAutoMemoryEnabled, "yes-please")
	if !(Config{}).enabled() {
		t.Fatal("无法识别的值应按未设处理（回到默认开启）")
	}
	// 但用户偏好仍然是可用的。
	if (Config{Enabled: Bool(false)}).enabled() {
		t.Fatal("无法识别的环境变量值不应盖过用户偏好")
	}
}

// autoMemoryDirectory 改**根**目录，且不改变"哪份记忆属于哪个仓库"（只改落在哪）。
func TestConfig_DirectoryOverride(t *testing.T) {
	clearEnv(t)
	alt := t.TempDir()

	t.Run("Config", func(t *testing.T) {
		c := Config{Directory: alt}
		root, err := c.root()
		if err != nil {
			t.Fatal(err)
		}
		if root != alt {
			t.Fatalf("root = %q, want %q", root, alt)
		}
	})
	t.Run("Env", func(t *testing.T) {
		t.Setenv(EnvAutoMemoryDirectory, alt)
		root, err := (Config{}).root()
		if err != nil {
			t.Fatal(err)
		}
		if root != alt {
			t.Fatalf("root = %q, want %q", root, alt)
		}
		// 相对路径被规范成绝对路径（配置里存相对路径会让记忆跟着 cwd 跑）。
		t.Setenv(EnvAutoMemoryDirectory, "rel/mem")
		root, err = (Config{}).root()
		if err != nil {
			t.Fatal(err)
		}
		if !filepath.IsAbs(root) {
			t.Fatalf("相对目录未被规范成绝对路径：%q", root)
		}
	})
	t.Run("ConfigBeatsEnv", func(t *testing.T) {
		t.Setenv(EnvAutoMemoryDirectory, filepath.Join(alt, "from-env"))
		root, err := (Config{Directory: filepath.Join(alt, "from-cfg")}).root()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(root, "from-env") {
			t.Fatalf("Config.Directory 应优先于环境变量：%q", root)
		}
	})
}

// autoMemoryDirectory + ProjectDirName 组合时，索引限额等仍以该目录为准。
func TestConfig_IsMemoryPath(t *testing.T) {
	clearEnv(t)
	root := t.TempDir()
	cfg := Config{Directory: root}
	inside := filepath.Join(root, "repos", "id", "user_role.md")
	if !cfg.IsMemoryPath(inside) {
		t.Fatalf("%s 应识别为记忆路径", inside)
	}
	for _, p := range []string{
		filepath.Join(t.TempDir(), "notes.md"),
		"relative/x.md",
		root + "2/repos/id/x.md", // 前缀相同的兄弟目录
	} {
		if cfg.IsMemoryPath(p) {
			t.Fatalf("%s 不应识别为记忆路径", p)
		}
	}
}

// IsMemoryPath 在开关关闭时**仍可单独用于筛路径**，但宿主不该调用它——
// 关闭时 Step 4 整个不跑。这个测试只固定"它不依赖开关"这一实现事实。
func TestConfig_IsMemoryPathIgnoresToggle(t *testing.T) {
	clearEnv(t)
	root := t.TempDir()
	cfg := Config{Directory: root, Enabled: Bool(false)}
	if !cfg.IsMemoryPath(filepath.Join(root, "repos", "id", "x.md")) {
		t.Fatal("IsMemoryPath 不应受开关影响（它只是路径范围识别）")
	}
}

// withDefaults 填好 I/O 出口，且不覆盖调用方显式注入的出口。
func TestConfig_WithDefaultsKeepsInjectedIO(t *testing.T) {
	clearEnv(t)
	var reads int32
	c := Config{ReadFile: func(string) ([]byte, error) {
		atomic.AddInt32(&reads, 1)
		return nil, nil
	}}.withDefaults()
	if _, err := c.ReadFile("x"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&reads) != 1 {
		t.Fatal("注入的 ReadFile 被 withDefaults 覆盖了")
	}
	// 未注入的出口被填成真实实现。
	if c.Now == nil || c.WriteFile == nil || c.Rename == nil || c.Remove == nil || c.Resolve == nil {
		t.Fatal("withDefaults 未填齐默认出口")
	}
	// 原 Config 不被修改（值传递）。
	base := Config{}
	_ = base.withDefaults()
	if base.Now != nil {
		t.Fatal("withDefaults 改了调用方的 Config")
	}
}
