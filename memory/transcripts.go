package memory

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// 会话转录里的记忆引用轨迹（Dream v2）。
//
// # 它补的是什么
//
// usage.json 从 postRead 钩子接线那天才开始计数，之前的历史是空的；而 events/*.jsonl
// 里躺着**全部**会话，包括钩子还没有的年代。所以：
//
//	usage     = 现在还在被读吗（实时、精确到次）
//	transcripts = 历史上被读过吗、最后一次是什么时候（长跨度、带日期）
//
// 后者才是"这条记忆还活着吗"能用的证据：一条三年没被碰过的记忆，和一条昨天刚被
// 纠正过的记忆，不该在生命周期里被同样对待。
//
// # 它不能证明什么
//
// 读过一次 ≠ 结论仍然成立。转录只能回答"有没有人用过它"，不能回答"它说的还对吗"。
// 所以这层证据只进"排序与提示"，永远不作为自动归档的依据——与 usage 同一条界线。
//
// # 为什么要按 workspace 找目录
//
// events 按**工作区**分目录（`Users_admin_cybertron-gogroup-0cd9bf5d`），记忆按**仓库**
// 分目录（`git.…-agent-server-g`）。两者不是一一对应：一个工作区下可以有好几个仓库，
// 一个仓库也能在好几个工作区里被打开。因此绑定文件里记的 workspace 是唯一的桥。

// ReadTrace 是一条记忆的引用轨迹。
type ReadTrace struct {
	File     string
	Reads    int       // 转录里出现的读取次数
	Sessions int       // 涉及多少个会话
	Last     time.Time // 最后一次（零值 = 从未）
}

// TranscriptReport 是一个记忆库的转录统计。
type TranscriptReport struct {

	// Files 扫描过的 jsonl 文件数。
	Files int
	// Traces 按文件名索引的轨迹（只含被读过的）。
	Traces map[string]ReadTrace
	// Unbound 找不到 events 目录 → 没扫。
	Unbound bool
	// Since 统计窗口起点。
	Since time.Time
}

// 与 eval_real_memory.py 同一口径：哪些工具会把记忆正文读进上下文。
var readToolNames = map[string]bool{"bash": true, "read_file": true, "grep": true, "glob": true, "exec": true}

// 事件里出现的记忆文件路径。只认 `~/.go-code/memory/repos/<repo>/<file>.md`。
var memPathInEvent = regexp.MustCompile(`\.go-code/memory/repos/([^/\s"'\\]+)/([^\s"'\\]+\.md)`)

// ScanTranscripts 扫**全部**会话转录，统计这个记忆库里每条记忆被读的轨迹。
//
// # 为什么按记忆路径过滤，而不是按工作区
//
// 读某条记忆的会话，未必在"拥有这条记忆的那个仓库"里工作。实测：agent-server-g 的
// 记忆有 208 次读取散在 6 个工作区里（agent-server-g / go-code / wiki / hai-studio…），
// 按工作区筛会漏掉一大半。
//
// 而记忆路径本身就带着仓库身份（`…/memory/repos/<repo-id>/<file>.md`），
// 直接按它过滤既准确又简单——不需要 workspace → repo 的任何映射。
func (c Config) ScanTranscripts(d Dir, window time.Duration) (TranscriptReport, error) {
	c = c.withDefaults()
	rep := TranscriptReport{Traces: map[string]ReadTrace{}}
	if window > 0 {
		rep.Since = c.Now().Add(-window)
	}
	root := c.eventsRoot()
	if root == "" {
		rep.Unbound = true
		return rep, nil
	}
	dirs, err := c.ReadDir(root)
	if err != nil {
		return rep, err
	}
	for _, de := range dirs {
		if !de.IsDir() {
			continue
		}
		dir := filepath.Join(root, de.Name())
		files, _ := c.ReadDir(dir)
		for _, fe := range files {
			if fe.IsDir() || !isEventLogName(fe.Name()) {
				continue
			}
			p := filepath.Join(dir, fe.Name())
			if window > 0 {
				if info, err := os.Stat(p); err == nil && info.ModTime().Before(rep.Since) {
					continue
				}
			}
			rep.Files++
			c.scanOneEventFile(p, d.ID, &rep)
		}
	}
	return rep, nil
}

// scanOneEventFile 扫一个会话文件里的读取事件。
func (c Config) scanOneEventFile(path, repoID string, rep *TranscriptReport) {
	raw, err := c.ReadFile(path)
	if err != nil || !bytes.Contains(raw, []byte(".go-code/memory/repos/")) {
		return
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(line) == 0 || !bytes.Contains(line, []byte("tool_run_start")) ||
			!bytes.Contains(line, []byte(".go-code/memory/repos/")) {
			continue
		}
		var ev struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Timestamp string          `json:"timestamp"`
		}
		if err := json.Unmarshal(line, &ev); err != nil || !readToolNames[ev.Name] {
			continue
		}
		ts, _ := time.Parse(time.RFC3339Nano, ev.Timestamp)
		if !rep.Since.IsZero() && ts.Before(rep.Since) {
			continue
		}
		for _, m := range memPathInEvent.FindAllSubmatch(ev.Arguments, -1) {
			if string(m[1]) != repoID {
				continue // 不是这个记忆库的文件
			}
			name := string(m[2])
			if strings.EqualFold(name, IndexFileName) {
				continue // 索引不是一条记忆
			}
			t := rep.Traces[name]
			t.File = name
			t.Reads++
			t.Sessions++ // 同一文件在同一会话里多次出现会被高估，但这里只用来排序
			if ts.After(t.Last) {
				t.Last = ts
			}
			rep.Traces[name] = t
		}
	}
}

// SilencedFor 返回某条记忆距最后一次被读过了多久（从未被读 = -1）。
func (r TranscriptReport) SilencedFor(name string, now time.Time) time.Duration {
	t, ok := r.Traces[name]
	if !ok || t.Last.IsZero() {
		return -1
	}
	return now.Sub(t.Last)
}

// LongSilent 返回"超过 d 未被读过"的文件名，按沉寂时长降序。
func (r TranscriptReport) LongSilent(names []string, now time.Time, d time.Duration) []string {
	type row struct {
		name string
		ago  time.Duration
	}
	var rows []row
	for _, n := range names {
		ago := r.SilencedFor(n, now)
		if ago < 0 || ago > d {
			rows = append(rows, row{n, ago})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ago > rows[j].ago })
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.name)
	}
	return out
}

// eventsRoot 解析转录根目录。
func (c Config) eventsRoot() string {
	if c.EventsRoot != "" {
		return c.EventsRoot
	}
	r, err := c.root()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(r), "events")
}
