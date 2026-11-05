package agenthub

// memory.go：智能体记忆的目录与路径规则（方案 §6.9 / §9.2，P1-05）。
//
// 每智能体一个独立记忆根：`<hub>/agents/<id>/memory/repos/<id>/`。
// 为什么**不**复用项目记忆的 git 身份解析：智能体 home 不是 git 仓库，Bootstrap 会
// 解析不出身份 → 永远不注入（等价没开记忆）。所以身份固定为 agentID（§6.9.1），
// 由 bridge 用 memory.Config{Directory: MemoryRootFor(...), ProjectDirName: id} 注入。
//
// 为什么放在 agents/<id>/ 而不是可写 home 里：记忆是 Profile 级财产（DM 与所有群共享），
// 不该被 `bash rm` 一条命令带走；浏览/编辑走 bridge 命令（带备份），不走智能体文件工具。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MemoryRootFor 返回智能体记忆的**根目录**（memory.Config.Directory 的值）：
// `<hubRoot>/agents/<agentID>/memory`。实际记忆文件在 `<root>/repos/<agentID>/`。
func MemoryRootFor(hubRoot string, id AgentID) (string, error) {
	if strings.TrimSpace(hubRoot) == "" {
		return "", fmt.Errorf("%w: empty hub root", ErrInvalid)
	}
	if !validID(string(id)) {
		return "", fmt.Errorf("%w: agent id %q", ErrInvalid, id)
	}
	return filepath.Join(hubRoot, "agents", string(id), "memory"), nil
}

// MemoryAgentDir 返回该智能体记忆文件所在目录：`<hubRoot>/agents/<id>/memory/repos/<id>`。
func MemoryAgentDir(hubRoot string, id AgentID) (string, error) {
	root, err := MemoryRootFor(hubRoot, id)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "repos", string(id)), nil
}

// ResolveMemoryPath 把「记忆面板里的相对路径」解析成绝对路径，并挡住逃逸。
//
// 规则（§9.2）：只允许记忆目录内的 `.md` 文件；不接受绝对路径、`..`、符号链接逃逸。
// 返回的 path 一定是 `<agentDir>/<rel>` 的真实路径（resolve 后仍在 agentDir 内）。
func ResolveMemoryPath(hubRoot string, id AgentID, rel string) (string, error) {
	dir, err := MemoryAgentDir(hubRoot, id)
	if err != nil {
		return "", err
	}
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return "", fmt.Errorf("%w: empty path", ErrInvalid)
	}
	// 反斜杠在 macOS 是合法文件名字符，但它也是 Windows 的路径分隔符 —— 面板里
	// 出现 "..\.." 必须同样被拒（换平台不会突然变成穿越）。
	if strings.Contains(rel, `\`) {
		return "", fmt.Errorf("%w: path must not contain backslashes", ErrInvalid)
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: path must be relative", ErrInvalid)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: path escapes memory dir", ErrInvalid)
	}
	if hasMemoryBackupSegment(clean) {
		// .backups 只由 WriteMemoryFile 自己维护（备份 + prune 只认 .bak）。面板往这里
		// 写 .md 会让「备份目录只含备份」的前提失效，也让 prune 的假设变模糊。
		return "", fmt.Errorf("%w: %s is reserved for backups", ErrInvalid, memoryBackupDir)
	}
	if !strings.HasSuffix(strings.ToLower(clean), ".md") {
		return "", fmt.Errorf("%w: only .md files are allowed", ErrInvalid)
	}
	abs := filepath.Join(dir, clean)
	// 符号链接逃逸：目录/文件任一环节指向外部都拒绝（EvalSymlinks 对不存在路径报错，
	// 新建文件时退回父目录判定）。
	checked, err := evalWithin(dir, abs)
	if err != nil {
		return "", err
	}
	return checked, nil
}

// memoryBackupDir 记忆备份目录（相对记忆目录；不参与注入，面板不可写）。
const memoryBackupDir = ".backups"

// hasMemoryBackupSegment 判断路径里是否出现保留段 .backups（任意层级）。
func hasMemoryBackupSegment(clean string) bool {
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if strings.EqualFold(seg, memoryBackupDir) {
			return true
		}
	}
	return false
}

// evalWithin 解析符号链接后仍要求落在 dir 内。
// 目标或中间目录不存在时（面板首次写入 topics/x.md），先解析**最近的真实祖先**再把
// 剩余段拼回去 —— 不能直接把字面路径当真实路径比：macOS 上 /tmp → /private/tmp，
// 字面路径与真实目录永远 Rel 出 ".."（P1-05 实测）。
func evalWithin(dir, abs string) (string, error) {
	dirReal, err := realpathLoose(dir)
	if err != nil {
		return "", err
	}
	real, err := realpathLoose(abs)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(dirReal, real)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: path escapes memory dir (symlink)", ErrInvalid)
	}
	return real, nil
}

// realpathLoose 解析符号链接；路径（或其任意祖先）不存在时，解析最近的真实祖先再
// 拼回剩余段。返回的一定是真实路径（用于与同样解析过的目录做 Rel）。
func realpathLoose(p string) (string, error) {
	var suffix []string
	cur := filepath.Clean(p)
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				real = filepath.Join(real, suffix[i])
			}
			return real, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

// MemoryEntry 记忆文件清单的一项（面板列表用）。
type MemoryEntry struct {
	Name     string    `json:"name"` // 相对记忆目录的路径（`MEMORY.md` / `topic.md`）
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// ListMemory 列出记忆目录下的 `.md` 文件（不含子目录，按名字排序）。
// 目录不存在 = 空列表（新智能体还没写过记忆，不是错误）。
func ListMemory(hubRoot string, id AgentID) ([]MemoryEntry, error) {
	dir, err := MemoryAgentDir(hubRoot, id)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []MemoryEntry{}, nil
		}
		return nil, err
	}
	out := make([]MemoryEntry, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue // 列目录与读信息之间被删：跳过而不是整体失败
		}
		out = append(out, MemoryEntry{Name: e.Name(), Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// MaxMemoryBackups 每个智能体保留的记忆备份数（写入前备份，超出丢最旧）。
const MaxMemoryBackups = 20

// MemoryWriteResult 一次记忆写入的结果（面板回执用）。
type MemoryWriteResult struct {
	Path   string `json:"path"`             // 相对记忆目录（面板显示用）
	Backup string `json:"backup,omitempty"` // 备份文件名（空 = 之前没有这个文件）
	Bytes  int    `json:"bytes"`
}

// ReadMemoryFile 读一个记忆文件。文件不存在返回 ErrNotFound（面板据此展示「新建」）。
func ReadMemoryFile(hubRoot string, id AgentID, rel string) (string, error) {
	path, err := ResolveMemoryPath(hubRoot, id, rel)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrNotFound
		}
		return "", err
	}
	return string(data), nil
}

// WriteMemoryFile 写一个记忆文件：先备份旧内容，再原子覆盖（0600）。
//
// 为什么备份：记忆是 Profile 级财产，面板一次误编辑（或模型写坏）不该不可逆；
// 备份放 `<memory>/.backups/`，不参与注入（ListMemory 只列顶层 .md）。
// 为什么不走 memory 包的写通道：那是给模型工具用的（带 modified 戳等语义），
// 面板编辑是「人直接改文件」，只需要可逆与原子。
func WriteMemoryFile(hubRoot string, id AgentID, rel, content string) (MemoryWriteResult, error) {
	path, err := ResolveMemoryPath(hubRoot, id, rel)
	if err != nil {
		return MemoryWriteResult{}, err
	}
	res := MemoryWriteResult{Path: filepath.ToSlash(filepath.Clean(rel)), Bytes: len(content)}
	if old, rerr := os.ReadFile(path); rerr == nil {
		name, berr := backupMemoryFile(hubRoot, id, filepath.Clean(rel), old)
		if berr != nil {
			return MemoryWriteResult{}, fmt.Errorf("备份旧记忆失败: %w", berr)
		}
		res.Backup = name
	} else if !os.IsNotExist(rerr) {
		return MemoryWriteResult{}, rerr
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return MemoryWriteResult{}, err
	}
	if err := writeFileAtomic0600(path, []byte(content)); err != nil {
		return MemoryWriteResult{}, err
	}
	return res, nil
}

// backupMemoryFile 写一份带时间戳的备份并裁剪到 MaxMemoryBackups。
// 文件名 `<rel 里的 / 换成 __>.<20060102T150405.000000000>.bak`。
func backupMemoryFile(hubRoot string, id AgentID, rel string, body []byte) (string, error) {
	dir, err := MemoryAgentDir(hubRoot, id)
	if err != nil {
		return "", err
	}
	bdir := filepath.Join(dir, memoryBackupDir)
	if err := os.MkdirAll(bdir, 0o700); err != nil {
		return "", err
	}
	safe := strings.NewReplacer("/", "__", "\\", "__").Replace(rel)
	name := fmt.Sprintf("%s.%s.bak", safe, time.Now().UTC().Format("20060102T150405.000000000"))
	if err := writeFileAtomic0600(filepath.Join(bdir, name), body); err != nil {
		return "", err
	}
	pruneMemoryBackups(bdir)
	return name, nil
}

// pruneMemoryBackups 每个被编辑的文件各留最近 MaxMemoryBackups 份。
//
// 两个坑（P1-05 复核实测）：
//   - 备份名是 `<rel 前缀>.<时间戳>.bak`，**前缀先于时间戳**参与排序。按完整文件名
//     排序时，先写 21 次 topics/x.md（前缀 topics__ 排最后），再写两次 MEMORY.md，
//     刚写的 MEMORY 备份会排在前面被当成「最旧」删掉 —— 旧内容已被覆盖，不可逆。
//   - 因此必须按「前缀分组 + 时间戳排序」，而不是全局按名字排。分组也让一个文件的
//     频繁编辑挤不掉另一个文件的历史。
func pruneMemoryBackups(dir string) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	groups := map[string][]string{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".bak") {
			continue
		}
		if _, _, ok := splitBackupName(name); !ok {
			continue // 不是我们写的备份（手工放入）：不动
		}
		groups[backupPrefix(name)] = append(groups[backupPrefix(name)], name)
	}
	for _, names := range groups {
		if len(names) <= MaxMemoryBackups {
			continue
		}
		sort.Slice(names, func(i, j int) bool {
			ti, tj := backupTimeKey(names[i]), backupTimeKey(names[j])
			if ti != tj {
				return ti < tj // 定长时间戳，字典序即时间序
			}
			return names[i] < names[j]
		})
		for _, name := range names[:len(names)-MaxMemoryBackups] {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}

// backupTimeLayout 备份文件名里的时间戳格式（**含一个 `.`**：秒与纳秒之间）。
// 因此不能「按最后一个 `.` 切分」——切出来的是纳秒段，排序与分组都会错。
const backupTimeLayout = "20060102T150405.000000000"

// splitBackupName 把 `<prefix>.<时间戳>.bak` 拆成前缀与时间戳。
// 时间戳定长且以 `.` 结尾与后缀相连；不匹配（手工放进来的文件）返回 ok=false，
// 这类文件不参与 prune（不动用户手放的东西）。
func splitBackupName(name string) (prefix, ts string, ok bool) {
	base := strings.TrimSuffix(name, ".bak")
	if len(base) < len(backupTimeLayout)+1 {
		return "", "", false
	}
	cut := len(base) - len(backupTimeLayout)
	if base[cut-1] != '.' {
		return "", "", false
	}
	return base[:cut-1], base[cut:], true
}

// backupTimeKey 取时间戳（用于排序）。
func backupTimeKey(name string) string {
	_, ts, _ := splitBackupName(name)
	return ts
}

// backupPrefix 取时间戳之前的前缀（同一被编辑文件的备份共享前缀）。
func backupPrefix(name string) string {
	prefix, _, ok := splitBackupName(name)
	if !ok {
		return name
	}
	return prefix
}

// writeFileAtomic0600 tmp+rename（同目录，保证 rename 原子）；目录 0700、文件 0600。
func writeFileAtomic0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-mem-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
