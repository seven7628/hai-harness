package hooks

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// UnreferencedScripts 列出 dir 下**没有任何配置引用**的脚本文件名。
//
// 这是最常见的"hooks 不生效"现场：用户在 ~/.go-code/hooks/ 下写好了脚本，配置里
// 没有对应的 handler，于是它一次都不会执行——而失败是无声的（没有进程、没有日志、
// 没有事件）。实测遇到过一模一样的现场，排查花了半天。
//
// 只做识别，不做自动注册：把脚本写进用户的配置文件是替用户做决定，而且那个文件里
// 同时放着 provider 密钥，为了这点便利去改写它不值得。
//
// 匹配用"命令串里是否出现脚本文件名"，因为脚本可能以 `bash ~/x.sh`、
// `~/.go-code/hooks/x.sh`、`python3 .../x.py` 等多种形态被引用。宁可漏报，
// 不误报成"没人用"——误报会让人去检查一个其实正常的配置。
func UnreferencedScripts(cfg *Config, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	commands := collectCommands(cfg)

	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		used := false
		for _, cmd := range commands {
			if strings.Contains(cmd, name) {
				used = true
				break
			}
		}
		if !used {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// collectCommands 收集全部配置里声明的命令串。
func collectCommands(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, groups := range cfg.Events {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Command != "" {
					out = append(out, h.Command)
				}
			}
		}
	}
	return out
}

// HooksDir 默认的脚本目录（~/.go-code/hooks）。空串表示取不到 home。
func HooksDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".go-code", "hooks")
}
