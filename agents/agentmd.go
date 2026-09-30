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
	// Abs 绝对路径，仅供跨层去重（dedupeAgentMDByPath）。不能靠 RelPath 反推：
	// 工作区层的 RelPath 是**相对发现根**的，"AGENTS.md" 相对进程 CWD 解析出来的
	// 是另一条路径，去重会失效。
	Abs string
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
		files = append(files, agentMDFile{Name: d.Name(), RelPath: rel, Content: string(content), Abs: path})
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

// userAgentMDMaxBytes 用户级指令文件体量上限（64 KiB）。见 discoverUserAgentMD 内的说明。
const userAgentMDMaxBytes = 64 << 10

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
// 渲染的路径是**绝对路径**，不用 "~" 缩写：注入 system 的这段文本是模型**要照着
// 操作的路径**，而文件工具的 resolve（tools/builtin/builtin.go）不展开 "~"——
// "~/.agents/AGENTS.md" 会被当成工作区内的相对路径，write_file 静默在
// {ws}/~/.agents/AGENTS.md 建出一个字面量 "~" 目录还报成功（已复现）。这里不能
// 拿"不外泄用户名"换正确性：用户名本来就经 Current Workspace / 工具回执外泄。
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
		// 读失败**继续**兜底下一个名字（AGENTS.md 不可读但 CLAUDE.md 可读时用后者），
		// 不因 Stat 成功就吞掉整个用户级层。
		//
		// 空/纯空白**继续**兜底，与工作区层 composeWorkingMemory 的"空内容跳过"
		// 是同一个"算不算指令文件"的判定，两层口径必须一致 —— 否则一个只放了空
		// 占位的 ~/.agents/AGENTS.md 会把真实偏好的 CLAUDE.md 遮掉，且无任何报错
		//（用户在全局层配的偏好从此静默失效）。
		if err != nil || strings.TrimSpace(string(content)) == "" {
			continue
		}
		// 体量上限：用户级是**全局**文件 —— 一次手滑写出几百 KB，会在每个工作区的
		// 每一轮请求里反复付费，且没有任何提示。工作区层是逐仓的、影响面小得多。
		// 超限时**明确告知**而非静默截断（静默截断会让模型按"规则只到一半"行事）。
		if len(content) > userAgentMDMaxBytes {
			return []agentMDFile{{
				Name:    name,
				RelPath: path,
				Content: fmt.Sprintf(
					"[%s 超过 %d 字节上限（实际 %d），未注入。请精简该文件后重试。]",
					path, userAgentMDMaxBytes, len(content)),
				Abs: path,
			}}
		}
		return []agentMDFile{{
			Name:    name,
			RelPath: path,
			Content: string(content),
			Abs:     path,
		}}
	}
	return nil
}

// dedupeAgentMDByPath 同一物理文件只保留**首次**出现的那一份（用户级优先，故实际
// 保留的是用户级渲染）。
//
// 为什么需要：用户把 ~/.agents 本身当工作区打开（dotfiles / skills 仓库很常见）时，
// 同一个 AGENTS.md 会被用户级与工作区递归各注入一次，模型看到两份逐字相同、
// 路径标注不同的块。工作区层的 RelPath 是相对路径、用户级是绝对路径，所以只能按
// 绝对路径归一后比较，不能按 RelPath 字符串。
func dedupeAgentMDByPath(files []agentMDFile) []agentMDFile {
	seen := make(map[string]bool, len(files))
	out := files[:0]
	for _, f := range files {
		abs := f.Abs
		if abs == "" { // 兜底：理论上无（两条发现路径都填了），退回 RelPath
			if p, err := filepath.Abs(f.RelPath); err == nil {
				abs = p
			} else {
				abs = f.RelPath
			}
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, f)
	}
	return out
}
