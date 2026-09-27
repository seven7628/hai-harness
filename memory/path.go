package memory

import (
	"path/filepath"
	"strings"
)

// IsInMemoryDir 判断某绝对路径是否属于「某个仓库的记忆目录」，即
// ~/.go-code/memory/repos/<id>/ 之下的文件或目录。
//
// 供两处使用（技术方案 §5.4）：
//
//	① write_file 钩子识别记忆文件并补 `modified`（§5.3，Step 4）；
//	② session 保留期清理排除记忆文件——官方 memory#storage-location：
//	   "excludes the memory files in the memory directory from that retention sweep"。
//
// **只做范围识别，不做路径穿越防护。** 官方机制里记忆路径由系统解析后交给模型，
// 不接受模型构造的任意路径；为一个不存在的攻击面写 safeJoin + 词法检查是自创的。
// 这里的语义因此是纯词法的：按 `filepath.Rel` 判定，不 EvalSymlinks、不 open/O_NOFOLLOW。
//
// 入参非法（非绝对路径 / 清理失败 / 逃出根）一律返回 false——判 false 只是「不当作记忆路径」，
// 调用点据此走普通文件流程，不会因此拒绝写入。
func IsInMemoryDir(root, path string) bool {
	if root == "" || path == "" || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Owns 判断 path 是否落在**这个** Dir 指向的记忆目录内（含目录自身）。
func (d Dir) Owns(path string) bool { return IsInMemoryDir(d.Path, path) }

// IsTopic 判断 path 是否是「一条记忆」的正文文件，即 <memory-dir>/ 下的 .md 且非 MEMORY.md。
//
// 官方 memory#how-it-works 的 `modified` 规则针对 memory file：只要文件**已有** frontmatter，
// 下次被写入时就补 `modified`；官方从不为无 frontmatter 的文件添加 frontmatter。
// 是否真有 frontmatter 由 Step 4 读文件判断，这里只给出「这是不是一条记忆文件」的前置条件。
//
// MEMORY.md 是索引而不是记忆条目（官方："MEMORY.md # Index, one line per memory"），
// 故排除。官方**没有**规定 topic 文件必须用某个后缀或必须扁平存放，这里不做更多约束：
// 官方示例是平铺的 <topic>.md，但 "…and ... # Any other topic files Claude creates"
// 说明子目录里的文件同样可以存在。
func (d Dir) IsTopic(path string) bool {
	if !d.Owns(path) || path == "" {
		return false
	}
	base := filepath.Base(filepath.Clean(path))
	if !strings.EqualFold(base, "MEMORY.md") {
		return strings.EqualFold(filepath.Ext(base), ".md")
	}
	return false
}

// IsIndex 判断 path 是否是索引文件 MEMORY.md。
//
// §5.2 的 200 行 / 25KB 限额与 §4.3 的「超限仍写入、返回错误」都只针对索引，
// 且官方明确 "This limit applies only to MEMORY.md"——所以判定索引必须精确到文件名。
func (d Dir) IsIndex(path string) bool {
	return d.Owns(path) && strings.EqualFold(filepath.Base(filepath.Clean(path)), IndexFileName)
}

// IndexFileName 索引文件名 MEMORY.md（启动时注入前 200 行 / 25KB 的那一份）。
const IndexFileName = "MEMORY.md"
