package evolution

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// 权限位与 memory/filestore.go 同口径（目录 0700、文件 0600）。
//
// 为什么不复用 memory 里的原子写：那些函数是 Phase A 的**未导出**实现，
// 而 Phase A 的契约要求"不重构它"。这里复制一份最小实现并保持同样的语义
// （临时文件 + fsync + rename），比为了复用而把 memory 的内部函数导出、
// 或让 evolution 反向成为 memory 的依赖面都要干净。
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// evolution 目录在仓库记忆根下的相对布局。
//
// 关键不变式：candidates/ 与 records/ 都在 topics/ **之外**，且不写 MEMORY.md。
// Phase A 的 memory_view 只读 topics/、BootstrapIndex 只读 MEMORY.md，
// 因此"落在 evolution/ 下"在结构上就等于"不可被召回"——不需要额外的过滤逻辑，
// 也就不会出现"过滤逻辑漏了一条路径"这种召回泄漏。
const (
	// evolutionDirName 自进化数据在仓库记忆根下的目录名。
	evolutionDirName = "evolution"
	// candidatesDirName 待验证候选目录（Phase B 起）。
	candidatesDirName = "candidates"
	// outcomesDirName 主题级结果账本目录（Phase C）。
	outcomesDirName = "outcomes"
	// evidenceDirName 负证据账本目录（Phase C/D）。
	evidenceDirName = "evidence"
	// lifecycleDirName 生命周期状态目录（Phase C）。
	lifecycleDirName = "lifecycle"
	// revisionsDirName 历史版本快照目录（Phase C，供 Rollback）。
	revisionsDirName = "revisions"
	// archiveDirName 归档目录（Phase C）。
	archiveDirName = "archive"
	// skillsDirName Skill Candidate 与 MemoryRefs 目录（Phase D）。
	skillsDirName = "skills"
)

// evolutionDirs 一次性算出全部子目录路径。
type evolutionDirs struct {
	root       string
	candidates string
	outcomes   string
	evidence   string
	lifecycle  string
	revisions  string
	archive    string
	skills     string
}

func newEvolutionDirs(repoDir string) evolutionDirs {
	root := filepath.Join(repoDir, evolutionDirName)
	return evolutionDirs{
		root:       root,
		candidates: filepath.Join(root, candidatesDirName),
		outcomes:   filepath.Join(root, outcomesDirName),
		evidence:   filepath.Join(root, evidenceDirName),
		lifecycle:  filepath.Join(root, lifecycleDirName),
		revisions:  filepath.Join(root, revisionsDirName),
		archive:    filepath.Join(root, archiveDirName),
		skills:     filepath.Join(root, skillsDirName),
	}
}

// errNotExist 文件不存在的归一哨兵（os.IsNotExist 覆盖不到的包装错误）。
var errNotExist = os.ErrNotExist

// removeIfExists 删除文件；不存在不算错误。
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("evolution: remove %s: %w", path, err)
	}
	return nil
}

// safeSegment 校验一个可用于单级路径的名字（ID / 主题名 / 技能名）。
//
// 只允许小写字母数字、连字符、下划线、点，且拒绝 "." / ".." / 含分隔符：
// 这些名字直接来自模型产出，必须在拼接路径**之前**拒绝，而不是拼接后再判断前缀。
func safeSegment(kind, name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("evolution: empty %s name", kind)
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("evolution: %s name %q must be a single safe segment", kind, name)
	}
	if len(name) > 128 {
		return fmt.Errorf("evolution: %s name too long (%d > 128)", kind, len(name))
	}
	for _, r := range name {
		ok := r == '-' || r == '_' || r == '.' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return fmt.Errorf("evolution: %s name %q has unsupported character %q", kind, name, r)
		}
	}
	return nil
}

// joinSegment 在 base 下拼一个已校验的单级名。
func joinSegment(base, kind, name string) (string, error) {
	if err := safeSegment(kind, name); err != nil {
		return "", err
	}
	full := filepath.Join(base, name)
	if !isUnder(base, full) {
		return "", fmt.Errorf("evolution: path %q escapes %q", full, base)
	}
	return full, nil
}

// isUnder child 是否等于或在 base 之下。
func isUnder(base, child string) bool {
	rel, err := filepath.Rel(base, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// countLines 行数（与 memory 包同口径：空串 0 行，否则换行数 +1）。
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// writeFileAtomic 原子写：临时文件 + chmod + fsync + rename。
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("evolution: create dir for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("evolution: create temp file for %s: %w", path, err)
	}
	name := tmp.Name()
	fail := func(err error, verb string) error {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("evolution: %s %s: %w", verb, path, err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		return fail(err, "write")
	}
	if err := tmp.Chmod(filePerm); err != nil {
		return fail(err, "chmod")
	}
	if err := tmp.Sync(); err != nil {
		return fail(err, "sync")
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("evolution: close %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("evolution: rename %s -> %s: %w", name, path, err)
	}
	return nil
}

// writeJSON 原子写 JSON（缩进输出：这些文件是给人审计的，不是热路径）。
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("evolution: encode %s: %w", path, err)
	}
	return writeFileAtomic(path, string(b)+"\n")
}

// readFileString 读整个文件为字符串；不存在时返回空串（调用点自行判定）。
func readFileString(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// readJSON 读 JSON；文件不存在时返回 os.ErrNotExist 包裹的错误。
func readJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRecordBytes))
	if err != nil {
		return fmt.Errorf("evolution: read %s: %w", path, err)
	}
	if len(raw) > maxRecordBytes {
		return fmt.Errorf("evolution: record %s exceeds %d bytes", path, maxRecordBytes)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("evolution: decode %s: %w", path, err)
	}
	return nil
}

// maxRecordBytes 单条记录（候选 / 账本文件）的字节上限。
//
// 边界：账本是审计材料而不是数据仓库；超限说明调用点塞了不该塞的东西
// （比如整个 Topic 正文重复多份），直接报错比静默截断更容易发现问题。
const maxRecordBytes = 256 * 1024

// validUTF8 校验字节是合法 UTF-8（模型产出的候选内容必须过这一关）。
func validUTF8(s string) bool { return utf8.ValidString(s) }
