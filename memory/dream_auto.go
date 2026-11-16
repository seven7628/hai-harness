package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 自动归档的准入判定（"每天最多一次"落地在这里）。
//
// 只有一道门：**频率**。同一记忆库每天最多自动跑一次——手动可以随时跑，自动不行，
// 无人值守的连续触发坏了也没人拦。
//
// 2026-11 去掉的那道门：曾要求"必须先配置回归命令（GOCODE_DREAM_EVAL_CMD）才允许
// 自动归档"。那是把**评测**和**产品**绑在了一起——评测回答的是"记忆系统能力如何"，
// 那件事已经做完了。产品要的是"记忆不随时间腐化"，保障手段是快照 + 可恢复 +
// 有日志，而不是每次归档都跑一遍 97 题。

const (
	// EnvDreamEvalCommand 归档后要跑的回归命令（shell）。未配置 = 不允许自动归档。
	EnvDreamEvalCommand = "GOCODE_DREAM_EVAL_CMD"
	// dreamAutoCap 自动归档的最短间隔。
	dreamAutoCap = 24 * time.Hour
	// dreamAutoState 自动运行的记账文件（相对记忆目录）。
	dreamAutoState = "dream-auto.json"
)

// autoDreamState 记账内容。只记"上次自动归档是什么时候"，不记结论——
// 结论在 dream-report.json 里。
type autoDreamState struct {
	LastAt       time.Time `json:"last_at"`
	LastArchived int       `json:"last_archived"`
}

func autoDreamPath(d Dir) string { return filepath.Join(d.Path, stateDirName, dreamAutoState) }

// DreamEvalCommand 取回归命令（去空白；空 = 没配）。
func DreamEvalCommand() string {
	return strings.TrimSpace(os.Getenv(EnvDreamEvalCommand))
}

// AutoDreamBlocker 返回"现在不能自动归档"的原因；空串 = 可以。
//
// 单独返回原因是刻意的：无人值守路径失败时最需要的就是一句能写进日志的理由，
// 而不是一个布尔值。
func (c Config) AutoDreamBlocker(d Dir) string {
	c = c.withDefaults()
	if c.enabled() == false {
		return "记忆已关闭"
	}
	if st, ok := c.readAutoDream(d); ok {
		if age := c.Now().Sub(st.LastAt); age >= 0 && age < dreamAutoCap {
			return "今天已经自动归档过（" + humanAge(age) + "）"
		}
	}
	return ""
}

// MarkAutoDream 记一次自动归档（best-effort：记账失败不该让已执行的归档回滚）。
func (c Config) MarkAutoDream(d Dir, archived int) error {
	c = c.withDefaults()
	st := autoDreamState{LastAt: c.Now().UTC(), LastArchived: archived}
	raw, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return err
	}
	if err := c.MkdirAll(filepath.Join(d.Path, stateDirName), 0o755); err != nil {
		return err
	}
	return c.atomicWrite(autoDreamPath(d), append(raw, '\n'))
}

func (c Config) readAutoDream(d Dir) (autoDreamState, bool) {
	raw, err := c.ReadFile(autoDreamPath(d))
	if err != nil {
		return autoDreamState{}, false
	}
	var st autoDreamState
	if err := json.Unmarshal(raw, &st); err != nil || st.LastAt.IsZero() {
		return autoDreamState{}, false
	}
	return st, true
}
