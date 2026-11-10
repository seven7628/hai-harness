package memory

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// 自动盘点（Dreaming 第一步）：会话开始时若上次盘点已过时，后台跑一次**只读**盘点，
// 结果落成一行摘要；下次注入时把它如实说出来。
//
// 边界（刻意划死）：
//   - 只读。这里不调用任何写记忆的出口，唯一写的文件是 .state/dream-report.json。
//   - 不裁决、不归档。归档要模型给裁决、宿主过护栏、归档后跑回归——那是另一条路径
//     （lifecycle.go），自动触发它等于半夜替用户挪记忆。
//   - 说话只说事实（"6 条代码锚点失效"），不说"建议清理"——与 guidance.go 同一约束。

const (
	// dreamNoteFile 报告落盘位置（相对记忆目录）。
	dreamNoteFile = "dream-report.json"
	// dreamNoteTTL 摘要的有效期。过了就当作没说过：一份两周前的"6 条失效"比不说更糟，
	// 因为用户会以为它说的是现在。
	dreamNoteTTL = 14 * 24 * time.Hour
	// dreamRefreshAfter 再次触发盘点的最短间隔。盘点要扫会话转录，不便宜；
	// 每次开会话都跑等于把成本摊到用户每一次等待上。
	dreamRefreshAfter = 12 * time.Hour
)

// DreamNote 一次盘点的摘要（只存数字，不存全文——全文由 memory_dream 现算）。
type DreamNote struct {
	At         time.Time `json:"at"`
	Topics     int       `json:"topics"`
	NotIndexed int       `json:"not_indexed"`
	Stale      int       `json:"stale"`
	MissingTyp int       `json:"missing_type"`
	Duplicates int       `json:"duplicates"`
	// DeadAnchors 代码锚点失效条数；Unbound=true 时这一项**没测**，不是"没问题"。
	DeadAnchors int  `json:"dead_anchors"`
	Unbound     bool `json:"unbound"`
	// Contradicted 冲突层里"被后续会话推翻"的条数（模型判的，不是宿主猜的）。
	Contradicted int `json:"contradicted"`
}

// HasIssues 有没有值得说一句的东西。
func (n DreamNote) HasIssues() bool {
	return n.DeadAnchors > 0 || n.NotIndexed > 0 || n.Stale > 0 ||
		n.MissingTyp > 0 || n.Duplicates > 0 || n.Contradicted > 0
}

// noteFromReport 把完整报告压成摘要。
func noteFromReport(r DreamReport, now time.Time) DreamNote {
	n := DreamNote{
		At:          now.UTC(),
		Topics:      r.Topics,
		NotIndexed:  len(r.NotIndexed),
		Stale:       len(r.Stale),
		MissingTyp:  len(r.MissingType),
		Duplicates:  len(r.Duplicates),
		DeadAnchors: len(r.Anchors.Dead),
		Unbound:     r.Anchors.Unbound,
	}
	for _, v := range r.Conflicts {
		if v.Verdict == ConflictContradicted {
			n.Contradicted++
		}
	}
	return n
}

func dreamNotePath(d Dir) string { return filepath.Join(d.Path, stateDirName, dreamNoteFile) }

// SaveDreamNote 落盘（best-effort 由调用方决定，这里如实返回错误）。
func (c Config) SaveDreamNote(d Dir, n DreamNote) error {
	c = c.withDefaults()
	raw, err := json.MarshalIndent(n, "", " ")
	if err != nil {
		return err
	}
	if err := c.MkdirAll(filepath.Join(d.Path, stateDirName), 0o755); err != nil {
		return err
	}
	return c.atomicWrite(dreamNotePath(d), append(raw, '\n'))
}

// LoadDreamNote 读摘要；不存在/损坏都当作"没有"——它只是提示语的数据源。
func (c Config) LoadDreamNote(d Dir) (DreamNote, bool) {
	c = c.withDefaults()
	raw, err := c.ReadFile(dreamNotePath(d))
	if err != nil {
		return DreamNote{}, false
	}
	var n DreamNote
	if err := json.Unmarshal(raw, &n); err != nil || n.At.IsZero() {
		return DreamNote{}, false
	}
	return n, true
}

// RunDreamAndSave 跑一次只读盘点并落摘要。返回摘要供调用方记日志。
func (c Config) RunDreamAndSave(d Dir) (DreamNote, error) {
	c = c.withDefaults() // 零值 Config 必须可用：Now/ReadFile 等出口都在这里补默认
	rep, err := c.Dream(d)
	if err != nil {
		return DreamNote{}, err
	}
	n := noteFromReport(rep, c.Now())
	if err := c.SaveDreamNote(d, n); err != nil {
		return n, err
	}
	return n, nil
}

// DreamDue 判断该不该现在跑一次盘点（过时 + 从未跑过都算）。
func (c Config) DreamDue(d Dir) bool {
	c = c.withDefaults()
	n, ok := c.LoadDreamNote(d)
	if !ok {
		return true
	}
	return c.Now().Sub(n.At) >= dreamRefreshAfter
}

// dreamNotice 把摘要拼成注入末尾的一句话。空串 = 什么都不说。
//
// 措辞约束：只报**已经量到的数字**与**去哪看全文**。不说"建议"、不说"过期记忆"——
// "锚点失效"是可验证的事实，"过期"是判断，判断留给模型。
func dreamNotice(n DreamNote, now time.Time) string {
	if !n.HasIssues() {
		return ""
	}
	age := now.Sub(n.At)
	if age < 0 || age > dreamNoteTTL {
		return ""
	}
	var parts []string
	add := func(kind string, cnt int) {
		if cnt > 0 {
			parts = append(parts, fmt.Sprintf("%d 条%s", cnt, kind))
		}
	}
	add("记忆没进索引", n.NotIndexed)
	add("代码锚点失效", n.DeadAnchors)
	add("缺少 type", n.MissingTyp)
	add("被后续会话推翻", n.Contradicted)
	add("重复", n.Duplicates)
	add("长期没被读过", n.Stale)
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("上次盘点（%s）：%s。完整报告用 `memory_dream` 看。",
		humanAge(age), strings.Join(parts, "、"))
}

// humanAge 把时间差说成人话（只到天）。
func humanAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return "刚刚"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天前", int(d.Hours()/24))
	}
}
