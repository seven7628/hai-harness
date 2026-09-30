package agents

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// agentMDFile 一个发现的项目指令文件（工作记忆层来源）。
type agentMDFile struct {
	Name    string // 文件名（AGENTS.md / CLAUDE.md）
	RelPath string // 相对发现根的路径（文件头定位用）
	Content string
}

// agentMDNames 协议支持的文件名：Codex 的 AGENTS.md 规范 + Claude Code 的 CLAUDE.md。
// 精确匹配（规范全大写）；大小写变体（agents.md）不识别 —— 与生态惯例一致。
// 同级共存规则：同一目录同时存在时 AGENTS.md 优先（见 discoverAgentMD）。
var agentMDNames = []string{"AGENTS.md", "CLAUDE.md"}

// isAgentMDName 文件名是否协议支持的指令文件（精确匹配）。
func isAgentMDName(name string) bool {
	for _, n := range agentMDNames {
		if name == n {
			return true
		}
	}
	return false
}

// discoverAgentMD 递归发现 dir 下所有 AGENTS.md / CLAUDE.md。
// 跳过以 "." 开头的目录（.git/.idea 等）；不可读目录/文件静默跳过（与 skills 损坏跳过同风格）。
// 同级共存去重：同一目录同时存在 AGENTS.md 与 CLAUDE.md 时只保留 AGENTS.md
// （AGENTS.md 是 Codex 规范、优先；CLAUDE.md 仅当该目录无 AGENTS.md 时兜底，
//
//	避免项目同时被两类工具使用时指令冲突）。
//
// 排序：根优先（depth 升序）→ 同级路径字典序（同目录天然 AGENTS.md 先于 CLAUDE.md）。
func discoverAgentMD(dir string) []agentMDFile {
	var files []agentMDFile
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// 文件名精确匹配先筛（不为每个文件多做一次 Stat）；命中后**跟随符号链接**
		// 确认是文件而非目录 —— `CLAUDE.md → AGENTS.md`、dotfiles 链接都是常见形态，
		// DirEntry.Type() 对 symlink 不按目标算，旧口径会整条漏掉。
		if !isAgentMDName(d.Name()) {
			return nil
		}
		fi, err := os.Stat(path)
		if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
			return nil // 目录/特殊文件（fifo 等）不是指令文件；断链跳过
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			rel = path
		}
		files = append(files, agentMDFile{Name: d.Name(), RelPath: rel, Content: string(content)})
		return nil
	})

	// 同级共存去重：同目录有 AGENTS.md 时剔除同目录 CLAUDE.md（父路径判定）
	preferAgents := make(map[string]bool) // 含 AGENTS.md 的目录（相对根）
	for _, f := range files {
		if f.Name == "AGENTS.md" {
			preferAgents[filepath.Dir(f.RelPath)] = true
		}
	}
	if len(preferAgents) > 0 {
		kept := files[:0]
		for _, f := range files {
			if f.Name == "CLAUDE.md" && preferAgents[filepath.Dir(f.RelPath)] {
				continue
			}
			kept = append(kept, f)
		}
		files = kept
	}

	sort.SliceStable(files, func(i, j int) bool {
		di, dj := pathDepth(files[i].RelPath), pathDepth(files[j].RelPath)
		if di != dj {
			return di < dj
		}
		return files[i].RelPath < files[j].RelPath
	})
	return files
}

func pathDepth(rel string) int {
	return strings.Count(rel, string(filepath.Separator)) + 1
}

// composeWorkingMemory 渲染工作记忆层：每文件一块
//
//	# <Name> (<RelPath>)
//	<content>
//
// 块间空行分隔；空文件跳过；无有效文件返回空串。
func composeWorkingMemory(files []agentMDFile) string {
	var b strings.Builder
	first := true
	for _, f := range files {
		content := strings.TrimSpace(f.Content)
		if content == "" {
			continue
		}
		if !first {
			b.WriteString("\n\n")
		}
		first = false
		fmt.Fprintf(&b, "# %s (%s)\n\n%s", f.Name, f.RelPath, content)
	}
	return b.String()
}

// discoverUserAgentMD 用户级指令文件（~/.agents/AGENTS.md 等）：**单文件、非递归**。
//
// 为什么非递归：用户级目录（~/.agents）同时是 skills / subagents 的家，其子目录里
// 天然可能各有自己的 AGENTS.md（如某个 skill 自带的说明）；递归会把它们全捞进来当
// 个人全局指令 —— 语义错位且体积不可控。用户级只认「本目录的那一份」。
//
// 为什么单文件而非复用工作区的去重规则：工作区层是「一个 root 递归出一组文件」，
// 用户级是「一个文件」，形状本就不同；同名共存时按 agentMDNames 顺序取第一个
// （AGENTS.md 优先于 CLAUDE.md），与工作区的同级共存规则同向。
//
// 路径经 displayAgentMDPath 缩成 ~/.agents/AGENTS.md：注入 system 的是**个人 home
// 路径**，展示缩写既更短、也不外泄用户名。
func discoverUserAgentMD(dir string) []agentMDFile {
	if dir == "" {
		return nil
	}
	for _, name := range agentMDNames {
		path := filepath.Join(dir, name)
		// Stat 而非 Lstat：跟随符号链接（~/.agents/AGENTS.md → dotfiles 仓库是常见形态，
		// 与 discoverAgentMD 同口径）；目录 / 断链 / 特殊文件跳过。
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return []agentMDFile{{
			Name:    name,
			RelPath: displayAgentMDPath(path),
			Content: string(content),
		}}
	}
	return nil
}

// displayAgentMDPath 把用户 home 前缀缩成 ~（仅用于展示；非 home 子树原样返回）。
// 不复用 artifacts.go 的 (*artifactCollector).displayPath：那是「工作区内绝对路径 →
// 相对路径」的另一套语义（此处要的是 home 缩写），方法集不共享、语义也不同。
func displayAgentMDPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path // 非 home 子树（或无法判定）：原样
	}
	return filepath.Join("~", rel)
}
