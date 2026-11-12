package memory

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 记忆条目的使用计数（2026-11 新增，热点清单的数据源）。
//
// # 为什么需要它
//
// 全量索引注入是每会话 5–7k token 的固定开销，而实测 57% 的请求根本不需要记忆。
// 要决定"哪几条值得在启动时出现"，唯一可用的信号是**过去谁被真的读过**——
// 不是文件名长度、不是修改时间、也不是模型自评的重要性。
//
// 实测背景（本地 72 条记忆）：54/93 条从未被任何会话读过。启动注入把它们全带上，
// 就是在为一个多数时候不发生的未来付固定成本。
//
// # 为什么是宿主自己数，而不是让模型报
//
// 模型无法被要求"顺便统计你读了什么"——那是行为协议，采纳率不可控（上一版 6 条
// 协议实测 12%~62%）。而 postRead 钩子已经在读成功时触发（memory_wiring.go），
// 计数是钩子的副产品，不需要模型配合，也不会因为模型忘了而失真。
//
// # 为什么计数不是"淘汰依据"
//
// 读得少 ≠ 该删：一条"只在发版那天有用"的记忆可能 90 天没人读，但那天必须存在。
// 因此 usage **只用于排序**（热点清单取谁），绝不参与删除、过期与 [CHECK] 判定。
// 这与 entities 的定位同构——排序信号，不是生命周期信号。
//
// # 存储位置与形态
//
//	<memory-dir>/.state/usage.json
//
// 放 `.state/` 而不是索引目录根：它不是记忆（模型不该在记忆清单里看到它），
// 也不该被 IsTopic 认成一条可读记忆。
type usageEntry struct {
	Reads int   `json:"reads"`
	Last  int64 `json:"last"` // Unix 秒；只为人看与排序并列时用，不参与过期判定
}

type usageState struct {
	Version int                   `json:"version"`
	Files   map[string]usageEntry `json:"files"`
}

// usageVersion 形态版本号。将来加字段时靠它区分，而不是靠"缺字段就猜"。
const usageVersion = 1

// stateDirName 记忆目录下的宿主私有状态目录（usage / entities 都落这里）。
const stateDirName = ".state"

// UsageEntry 是 ReadUsage 的公开视图（测试与评估脚本用）。
type UsageEntry struct {
	Reads int
	Last  time.Time
}

// usagePath 返回 usage 文件路径。
func usagePath(d Dir) string {
	return filepath.Join(d.Path, stateDirName, "usage.json")
}

// ReadUsage 读出该记忆目录的使用计数（key 为相对记忆目录的 "/" 分隔路径）。
//
// 任何错误都返回空 map：调用点是启动注入，一条坏掉的统计文件不该让会话起不来，
// 也不该让模型看到一段"统计读不出来"的噪声——退化成"没有热点数据"即可，
// 热点清单会按索引原序兜底（见 BuildHotList）。
func (c Config) ReadUsage(d Dir) map[string]UsageEntry {
	out := map[string]UsageEntry{}
	raw, err := c.withDefaults().ReadFile(usagePath(d))
	if err != nil {
		return out
	}
	var st usageState
	if err := json.Unmarshal(raw, &st); err != nil {
		return out
	}
	for k, v := range st.Files {
		out[k] = UsageEntry{Reads: v.Reads, Last: time.Unix(v.Last, 0)}
	}
	return out
}

// RecordRead 给一条记忆记一次读（postRead 钩子调用）。
//
// **尽力而为**：返回 err 供调用点记日志，但读文件本身绝不因为统计失败而失败——
// 统计是观测，不是功能。这一点与 emitMemoryActivity 一致。
//
// 原子写：复用 atomicWrite（同目录临时文件 + rename）。usage.json 每次读记忆都会
// 被重写，写到一半被读走会让下一次注入看到残缺 JSON，整份热点数据作废。
func (c Config) RecordRead(d Dir, absPath string) error {
	rel, ok := d.RelPath(absPath)
	if !ok {
		return nil
	}
	now := c.withDefaults().Now()
	st := c.readUsageState(d)
	if st.Files == nil {
		st.Files = map[string]usageEntry{}
	}
	e := st.Files[rel]
	e.Reads++
	e.Last = now.Unix()
	st.Files[rel] = e
	st.Version = usageVersion

	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(usagePath(d))
	if err := c.withDefaults().MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return c.withDefaults().atomicWrite(usagePath(d), data)
}

// readUsageState 读原始状态（保留未知字段的语义不重要：本文件由宿主独占写）。
func (c Config) readUsageState(d Dir) usageState {
	raw, err := c.withDefaults().ReadFile(usagePath(d))
	if err != nil {
		return usageState{Version: usageVersion}
	}
	var st usageState
	if err := json.Unmarshal(raw, &st); err != nil {
		// 文件坏了：从空状态重建，而不是让之后每一次读都失败。
		return usageState{Version: usageVersion}
	}
	if st.Files == nil {
		st.Files = map[string]usageEntry{}
	}
	return st
}

// RelPath 把记忆目录内的绝对路径转成 usage 的 key（"/" 分隔、相对目录）。
func (d Dir) RelPath(absPath string) (string, bool) {
	if !d.Owns(absPath) {
		return "", false
	}
	rel, err := filepath.Rel(d.Path, filepath.Clean(absPath))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// SortUsageKeys 按读数降序返回 key（并列时按字典序，保证结果稳定可测）。
// 供评估脚本与调试用；注入路径走 BuildHotList 自己的排序。
func SortUsageKeys(m map[string]UsageEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]].Reads != m[keys[j]].Reads {
			return m[keys[i]].Reads > m[keys[j]].Reads
		}
		return keys[i] < keys[j]
	})
	return keys
}
