// Package remote 实现远程技能下载（exec git 拉取 + 校验 + 装入技能目录）。
//
// 支持来源（GitHub 链接为主）：
//   - 整仓库即一个技能：github.com/owner/repo（仓库根有 SKILL.md）
//   - 仓库内子目录技能：github.com/owner/repo/tree/<ref>/<dir>
//   - 仓库内按名找技能：github.com/owner/repo#skill-name（skills/ 或 .claude/skills/）
//   - 市场仓库：github.com/owner/repo（根含 .claude-plugin/marketplace.json → 装全部插件技能）
//
// 双作用域：global（~/.agents/skills/<name>，所有工作区可见；同名已存在于
// ~/.go-code/skills 时装旧路径）/ workspace（{ws}/.agents/skills/<name>，仅该工作区；
// 同名已存在于 {ws}/.go-code/skills 时装旧路径）。
// 来源清单 ~/.go-code/remote-skills.json（含 scope/target，支撑 update/uninstall）。
// 安装只复制文件、绝不执行；SKILL.md 经 skills.ParseSkill 校验。
package remote

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Source 解析后的远程技能来源。
type Source struct {
	Kind   string // "repo"（仓库根技能）| "subdir"（tree/<ref>/<dir>）| "name"（#name）
	URL    string // 克隆 URL（normalized，含 .git 后缀）
	Ref    string // 分支/标签（tree/<ref>；空 = 默认分支）
	Subdir string // 仓库内技能子目录（subdir）
	Name   string // #name 指定的技能名（name）
}

var (
	// github.com/owner/repo/tree/<ref>/<dir...>
	treeRe = regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+/[^/]+?)(?:\.git)?/tree/([^/]+)(?:/(.+))?$`)
	// github.com/owner/repo
	repoRe = regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+/[^/]+?)(?:\.git)?/?$`)
	// git@github.com:owner/repo
	sshRe = regexp.MustCompile(`^git@github\.com:([^/]+/[^/]+?)(?:\.git)?$`)
)

// ParseURL 解析用户输入的远程技能链接 → Source。
func ParseURL(raw string) (Source, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Source{}, errors.New("URL 为空")
	}
	var name string
	if i := strings.Index(raw, "#"); i >= 0 {
		name = strings.TrimSpace(raw[i+1:])
		raw = strings.TrimSpace(raw[:i])
		if name == "" {
			return Source{}, errors.New("URL 的 #name 为空")
		}
	}
	raw = strings.TrimSuffix(raw, "/")

	if m := treeRe.FindStringSubmatch(raw); m != nil {
		return Source{
			Kind:   "subdir",
			URL:    "https://github.com/" + m[1] + ".git",
			Ref:    m[2],
			Subdir: strings.Trim(m[3], "/"),
			Name:   name,
		}, nil
	}
	if m := repoRe.FindStringSubmatch(raw); m != nil {
		src := Source{Kind: "repo", URL: "https://github.com/" + m[1] + ".git"}
		if name != "" {
			src.Kind = "name"
			src.Name = name
		}
		return src, nil
	}
	if m := sshRe.FindStringSubmatch(raw); m != nil {
		src := Source{Kind: "repo", URL: "git@github.com:" + m[1] + ".git"}
		if name != "" {
			src.Kind = "name"
			src.Name = name
		}
		return src, nil
	}
	// 泛型 git URL（自托管 https / git@host:owner/repo）：仓库根技能；#name 在 skills/ 里找
	if strings.Contains(raw, "://") || strings.HasPrefix(raw, "git@") {
		src := Source{Kind: "repo", URL: raw}
		if name != "" {
			src.Kind = "name"
			src.Name = name
		}
		return src, nil
	}
	return Source{}, fmt.Errorf("无法解析的链接 %q（支持 github.com/owner/repo[/tree/<ref>/<dir>][#skill-name]）", raw)
}

// validSkillName 校验技能名可安全用作目录名：单路径段、无路径穿越、非空。
func validSkillName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return false
	}
	if len(name) > 64 {
		return false
	}
	return true
}
