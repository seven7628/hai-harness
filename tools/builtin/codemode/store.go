package codemode

// store.go：codemode 的会话级键值 store（脚本内 store() / load() 的宿主侧）。
//
// 落点（设计 §16.5）：**独立 jsonl 文件**，不落会话 jsonl —— 会话文件的 fileRecord 是
// 固定四槽结构，加 custom 槽会触发 jsonl 的 O(N²) 膨胀陷阱（实测 2.9 GB）。
//
// 形态：append-only，每行一次「成功脚本的写入批」；读 = **整文件 replay**。
// 本仓没有 fork 形态（memory/subagent.go:21-22），所以不做 pi 那种「按分支过滤」——
// 直接按文件顺序依次 apply 就是权威状态。
//
// 失败语义（本文件最重要的一条契约）：**只有脚本成功才落盘**。写盘这件事只有
// Commit 一条路径，且必须在脚本成功后调用；失败/超时/取消的脚本把 Pending 丢掉即可，
// 「不写盘」是默认行为而不是需要额外判断的分支（pi execute.d.ts:19-20
// 「Writes are kept only if the script succeeds」）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// StoreFileName 会话目录下的 store 文件名。
	StoreFileName = "codemode-store.jsonl"

	// MaxStoreValueBytes 单个值的上限（对齐 pi prelude 的 262144 字符）。
	// 本仓按**字节**计：字节是落盘与内存的真实成本，而 pi 的「字符」在 JS 里是
	// UTF-16 码元，两者对中文值会差 3 倍，按字节更保守也更好解释。
	MaxStoreValueBytes = 256 << 10 // 256 KiB

	// MaxStoreTotalBytes 整个 store 的上限（对齐 pi 的 1048576）。
	// 统计**键 + 值**的字节数：只算值的话，脚本可以用大量长键把 store 撑到任意大
	//（实测 3000 个 1 KiB 的键 ⇒ 3 MB 落盘、此后每次调用都带 3 MB 的 init 载荷），
	// 而键的字节同样是落盘与每次调用传递的真实成本（对抗复核 F6）。
	MaxStoreTotalBytes = 1 << 20 // 1 MiB

	// MaxStoreKeyBytes 单个键的上限。键是要进 init 载荷与 jsonl 的，长键既贵又是
	// 「绕过总量限制」的载体；1 KiB 远超任何正常用法（键名是标识符，不是数据）。
	MaxStoreKeyBytes = 1 << 10 // 1 KiB

	// storeFileMode 文件权限：0600（只有本用户可读写）。store 里会出现工具结果里的
	// 路径、内容片段，同 spill 文件的既定要求（pi 那边是裸 writeFile，是它的缺陷）。
	storeFileMode = 0o600
)

// 限额相关的哨兵错误：Wave 2 可以把它们翻译成脚本可见的报错，宿主测试也可以
// 直接 errors.Is 断言「是限额而不是别的错」。
var (
	ErrStoreValueTooLarge = errors.New("codemode store: 单值超过上限")
	ErrStoreKeyTooLarge   = errors.New("codemode store: 键超过上限")
	ErrStoreTotalTooLarge = errors.New("codemode store: 总量超过上限")
	ErrStoreInvalidJSON   = errors.New("codemode store: 值不是合法 JSON")
)

// StoreRecord 一行 jsonl = 一次成功脚本的写入批（对齐 pi 的 StoreEntry 形状）。
//
// Set 的值是**压紧后的 JSON 原文**（紧凑、无换行，见 compactJSON）：直接把模型给的
// 值内联进 jsonl 会把一行劈成两行，整文件 replay 时两条都读不出来。
// Delete 表达 store(k, undefined)：删除在 set 之后应用，同键同时出现 = 以删除为准。
type StoreRecord struct {
	Set    map[string]json.RawMessage `json:"set,omitempty"`
	Delete []string                   `json:"delete,omitempty"`
}

// Pending 一次脚本执行期间累积的待落盘写入。
//
// 沙箱内 store(k, v) 经桥接写进 Pending；**只有脚本成功结束**，调用方才把它交给
// Store.Commit。本类型刻意不提供任何「自动落盘」路径：失败不写盘由调用方决定，
// 而「没有 commit 就没有写」把它变成默认行为。
//
// 同键的「最后一次操作生效」在 Pending 内部完成（Set 会撤销同键的 Delete，反之亦然），
// 所以一行 jsonl 里不会出现「既 set 又 delete 同一个键」的歧义。
type Pending struct {
	set      map[string]json.RawMessage
	setOrder []string
	del      map[string]bool
	delOrder []string
}

// NewPending 建一个空的待落盘写入批。
func NewPending() *Pending {
	return &Pending{set: map[string]json.RawMessage{}, del: map[string]bool{}}
}

// Set 暂存一次 store(key, value)。校验在**暂存时**就做（单值 JSON 合法性 + 单值上限），
// 这样桥接可以在脚本内就拒绝并让脚本看到错误，而不是等脚本跑完才失败。
//
// 总量上限不在这里查：它要看「apply 之后的整店」，只有 Store（持有当前状态）知道，
// 统一放在 Commit 里，保证「校验不过 = 一个字节都不写」。
func (p *Pending) Set(key string, value json.RawMessage) error {
	if key == "" {
		return fmt.Errorf("codemode store: 键不能为空")
	}
	if len(key) > MaxStoreKeyBytes {
		return fmt.Errorf("%w: 键 %d 字节 > 上限 %d 字节（1 KiB）。键是标识符，别把数据塞进键里",
			ErrStoreKeyTooLarge, len(key), MaxStoreKeyBytes)
	}
	compact, err := compactJSON(value)
	if err != nil {
		return fmt.Errorf("codemode store: 键 %q 的值不是合法 JSON: %w", key, err)
	}
	if len(compact) > MaxStoreValueBytes {
		return fmt.Errorf("%w: 键 %q 的值 %d 字节 > 上限 %d 字节（256 KiB）",
			ErrStoreValueTooLarge, key, len(compact), MaxStoreValueBytes)
	}
	if _, ok := p.set[key]; !ok {
		p.setOrder = append(p.setOrder, key)
	}
	p.set[key] = compact
	if p.del[key] {
		delete(p.del, key)
		p.delOrder = removeString(p.delOrder, key)
	}
	return nil
}

// Delete 暂存一次 store(key, undefined)。
func (p *Pending) Delete(key string) error {
	if key == "" {
		return fmt.Errorf("codemode store: 键不能为空")
	}
	if _, ok := p.set[key]; ok {
		delete(p.set, key)
		p.setOrder = removeString(p.setOrder, key)
	}
	if !p.del[key] {
		p.del[key] = true
		p.delOrder = append(p.delOrder, key)
	}
	return nil
}

// Empty 本批没有任何写入（成功的空脚本不该让 jsonl 长一行）。
func (p *Pending) Empty() bool {
	return p == nil || (len(p.set) == 0 && len(p.delOrder) == 0)
}

// Len 本批的写入条数（set + delete）。
func (p *Pending) Len() int {
	if p == nil {
		return 0
	}
	return len(p.setOrder) + len(p.delOrder)
}

// Record 取出本批对应的记录（写入顺序 = 调用顺序，便于对拍与人工排查）。
func (p *Pending) Record() StoreRecord {
	if p == nil {
		return StoreRecord{}
	}
	rec := StoreRecord{}
	if len(p.setOrder) > 0 {
		rec.Set = make(map[string]json.RawMessage, len(p.setOrder))
		for _, k := range p.setOrder {
			rec.Set[k] = p.set[k]
		}
	}
	rec.Delete = append([]string(nil), p.delOrder...)
	return rec
}

// ReplayStats 整文件 replay 的统计（损坏行不致命，但必须可见）。
type ReplayStats struct {
	Lines   int // 读到的非空行数（含被跳过的）
	Applied int // 成功 apply 的条目数
	Skipped int // 跳过的损坏/不可解析行数
}

// Store 会话级键值 store。并发安全（codemode 的脚本虽由 ExecScope 串行化，但宿主
// 侧可能有并行调用与 UI 读取）。
type Store struct {
	mu    sync.Mutex
	path  string
	vals  map[string]json.RawMessage
	stats ReplayStats
}

// StorePath 会话目录下的 store 路径（宿主装配用）。dir 为空 = 不持久化。
func StorePath(sessionDir string) string {
	if sessionDir == "" {
		return ""
	}
	return filepath.Join(sessionDir, StoreFileName)
}

// OpenStore 打开（或创建）store 文件并整文件 replay。
//
// path == "" → 纯内存 store：写入只在进程内可见（对齐 pi 拿不到 appendEntry 时的降级
// 形态），供「不需要跨调用持久化」的宿主与测试使用。
//
// 目录必须已存在：store 不住在别人的目录所有权里（会话目录由 session 层创建），
// 少了它报错比悄悄 MkdirAll 更容易发现装配错误。
//
// 权限：文件不存在 → 建成 0600；已存在但权限更宽 → 收紧到 0600（沿用下来的文件
// 可能是老版本或别的工具建的）。
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, vals: map[string]json.RawMessage{}}
	if path == "" {
		return s, nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("codemode store: 目录不存在或不可用 %q（store 不负责建目录）", dir)
		}
	}
	if err := os.Chmod(path, storeFileMode); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("codemode store: 收紧权限到 0600 失败: %w", err)
	}
	// 建/触达一次，确保文件存在且为 0600（O_APPEND 追加，永不重写整文件）。
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, storeFileMode)
	if err != nil {
		return nil, fmt.Errorf("codemode store: 打开失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("codemode store: 关闭失败: %w", err)
	}
	if err := s.replay(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path 落盘路径（"" = 纯内存）。
func (s *Store) Path() string { return s.path }

// Stats 本次 replay 的统计。
func (s *Store) Stats() ReplayStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Values 当前 store 的快照（沙箱 init 载荷用）。
// 返回**副本**（值也复制）：调用方改不动 store 内部状态，也不会与后续 Commit 竞态。
func (s *Store) Values() map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]json.RawMessage, len(s.vals))
	for k, v := range s.vals {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

// Commit 校验并**追加**一批写入（append-only，不重写文件）。
//
// 契约（调用方必须遵守，本类型无法强制）：
//   - **只在脚本成功结束时调用**。失败、超时、取消、审批被拒 —— 一律把 Pending
//     丢掉。写盘只有这一条路径，所以「失败不落盘」不需要调用方写任何分支判断，
//     它就是不调用 Commit。
//   - p 为空（脚本没调 store）时本方法不碰文件：否则每次成功调用都让 jsonl 长
//     （哪怕只是 `{}`），replay 成本与文件体积都会白涨。
//
// 原子性：任何校验失败（非法 JSON / 单值超限 / 总量超限）都在**写入之前**返回，
// 文件与内存状态都不变 —— 半批写入是比整批失败更糟的结局。
func (s *Store) Commit(p *Pending) error {
	if p.Empty() {
		return nil
	}
	return s.appendRecord(p.Record())
}

// appendRecord 追加一条记录（先校验，后落盘）。
func (s *Store) appendRecord(rec StoreRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1) 校验 + 压紧，并算出 apply 之后的状态（内存态是唯一真源，文件只是它的日志）。
	norm := StoreRecord{}
	next := make(map[string]json.RawMessage, len(s.vals)+len(rec.Set))
	for k, v := range s.vals {
		next[k] = v
	}
	if len(rec.Set) > 0 {
		norm.Set = make(map[string]json.RawMessage, len(rec.Set))
		for k, v := range rec.Set {
			if k == "" {
				return fmt.Errorf("codemode store: 键不能为空")
			}
			if len(k) > MaxStoreKeyBytes {
				return fmt.Errorf("%w: 键 %d 字节 > 上限 %d 字节（1 KiB）。键是标识符，别把数据塞进键里",
					ErrStoreKeyTooLarge, len(k), MaxStoreKeyBytes)
			}
			compact, err := compactJSON(v)
			if err != nil {
				return fmt.Errorf("codemode store: 键 %q 的值不是合法 JSON: %w", k, err)
			}
			if len(compact) > MaxStoreValueBytes {
				return fmt.Errorf("%w: 键 %q 的值 %d 字节 > 上限 %d 字节（256 KiB）。把值切碎分别存，或只存路径/摘要",
					ErrStoreValueTooLarge, k, len(compact), MaxStoreValueBytes)
			}
			norm.Set[k] = compact
			next[k] = compact
		}
	}
	for _, k := range rec.Delete {
		delete(next, k)
	}
	norm.Delete = append([]string(nil), rec.Delete...)

	total := 0
	for k, v := range next {
		if len(k) > MaxStoreKeyBytes {
			return fmt.Errorf("%w: 键 %d 字节 > 上限 %d 字节（1 KiB）。键是标识符，别把数据塞进键里",
				ErrStoreKeyTooLarge, len(k), MaxStoreKeyBytes)
		}
		total += len(k) + len(v)
	}
	if total > MaxStoreTotalBytes {
		return fmt.Errorf("%w: 写入后总量 %d 字节（键+值）> 上限 %d 字节（1 MiB）。删掉不再需要的键（store(k, undefined)）再存",
			ErrStoreTotalTooLarge, total, MaxStoreTotalBytes)
	}

	// 2) 落盘（内存 store 跳过）。Values 更新放在落盘之后：写失败不留幽灵状态。
	if s.path != "" {
		line, err := json.Marshal(norm)
		if err != nil {
			return fmt.Errorf("codemode store: 序列化记录失败: %w", err)
		}
		if err := s.appendLine(line); err != nil {
			return err
		}
	}
	s.vals = next
	return nil
}

// appendLine 追加一行（含结尾换行）。
//
// 单次 Write + O_APPEND：同一进程内一次 Write 不会被别的写插进中间，所以并发
// Commit 不会互相撕行。文件末尾若不是换行（上次崩溃/被杀留下半行），先补一个换行
// 再写 —— 否则新记录会和那半行粘成一行，replay 时**两条都读不出来**。
//
// 每次 Commit 重新 open（不长期持有 fd）：会话目录被搬动、清理、替换 inode 之后，
// 长持有的 fd 会静默写进旧 inode，而 store 的写入频率（每次成功脚本一次）完全
// 不值得为此担这个风险。
func (s *Store) appendLine(line []byte) error {
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, storeFileMode)
	if err != nil {
		return fmt.Errorf("codemode store: 打开失败: %w", err)
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], st.Size()-1); err == nil && last[0] != '\n' {
			if _, err := f.Write([]byte("\n")); err != nil {
				return fmt.Errorf("codemode store: 修补残缺行失败: %w", err)
			}
		}
	}
	if _, err := f.Write(append(append([]byte(nil), line...), '\n')); err != nil {
		return fmt.Errorf("codemode store: 追加失败: %w", err)
	}
	return nil
}

// replay 整文件 replay（对齐 pi readCodemodeStore：按顺序依次 apply）。
//
// 损坏行（半行 JSON、形状不对、值不是合法 JSON）**跳过并计数**，不报错。理由：
//   - 最可能的成因是上次写盘中途崩溃留下的半行（append-only 没有多行原子性）；
//   - store 是会话级草稿纸而不是用户数据，一条坏行不该让整个会话的 codemode 变成
//     不可用（报错 = 之后每次调用都失败，且用户无从修复）；
//   - 静默跳过是不行的，所以计数留在 ReplayStats 里，Wave 2 可以据此告警。
//
// 补一笔：跳过的半行不会污染后续写入 —— appendLine 落盘前会补换行。
//
// replay **不做**限额校验：历史文件不该因为限额后来调小/调大而读不出来（读不出来
// 比读多了更糟）。限额是写入路径上的闸门。
func (s *Store) replay() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("codemode store: 读取失败: %w", err)
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		s.stats.Lines++
		var rec StoreRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			s.stats.Skipped++
			continue
		}
		if !validRecord(rec) {
			s.stats.Skipped++
			continue
		}
		s.apply(rec)
		s.stats.Applied++
	}
	return nil
}

// validRecord 形状校验：至少有一条写入，且 set 的值必须是非空合法 JSON。
// （json.Unmarshal 已保证 Set/Delete 的类型，但值本身是 RawMessage，不会替我们验；
// `null`、`{}` 这类空行没有语义，按损坏行处理。）
func validRecord(rec StoreRecord) bool {
	if len(rec.Set) == 0 && len(rec.Delete) == 0 {
		return false
	}
	for _, v := range rec.Set {
		if len(v) == 0 || !json.Valid(v) {
			return false
		}
	}
	return true
}

// apply 把一条记录合入内存状态（set 先、delete 后；同键 = 以删除为准）。
// 值统一压紧，保证「store 里的值永远是 compact JSON」这条不变量。
func (s *Store) apply(rec StoreRecord) {
	for k, v := range rec.Set {
		compact, err := compactJSON(v)
		if err != nil {
			continue
		}
		s.vals[k] = compact
	}
	for _, k := range rec.Delete {
		delete(s.vals, k)
	}
}

// compactJSON 校验并压紧一个 JSON 值。
//
// 压紧不只是省字节：缩进/多行的 JSON 一旦内联进 jsonl，会把**一行劈成两行**，
// 整文件 replay 时两条都读不出来（append-only 没有事务可以回滚这种损坏）。
func compactJSON(v []byte) (json.RawMessage, error) {
	if len(v) == 0 {
		return nil, ErrStoreInvalidJSON
	}
	if !json.Valid(v) {
		return nil, ErrStoreInvalidJSON
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, v); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStoreInvalidJSON, err)
	}
	return json.RawMessage(buf.Bytes()), nil
}

// removeString 从切片里摘掉一个元素（保持其余顺序）。
func removeString(xs []string, s string) []string {
	for i, x := range xs {
		if x == s {
			return append(xs[:i:i], xs[i+1:]...)
		}
	}
	return xs
}
