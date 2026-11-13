package conversion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/internal/atomicfile"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// cacheKeyVersion 产物口径版本。**改转换脚本/产物结构时必须 bump 它**（R08a 注释）。
//
// 为什么必须有这一位：三个转换脚本是编译期常量，随 app 版本一起变；缓存键里若不体现
// 「是哪一版脚本产出的」，升级后用户仍会命中上一版脚本的 HTML —— 表现为「更新了却什么都没变」，
// 且没有任何报错（最坏的一种故障）。bump 的代价只是升级后一次性重转。
const cacheKeyVersion = "r08a-v1"

// keyScriptFingerprint 三个转换脚本（编译期常量）的内容指纹，是缓存键里
// 「转换器版本」这一位。
//
// 为什么是**脚本内容哈希**而不是构建标识（vcs revision / 版本号）：后者既过度又不足。
//   - 不足：`go run` / `buildvcs=false` / 改了脚本还没提交时 revision 不变 —— 正好漏掉
//     它声称要防的场景（改了脚本、静默命中旧产物，用户只看到「更新没生效」）；
//   - 过度：改一行无关的 Go 代码就让整份缓存作废。
//
// 脚本是常量，算一次 sha256 成本为零，语义精确：**产物生成规则变了才换键**。
var keyScriptFingerprint = builtin.PreviewScriptFingerprint()

// indexName 条目里的产物文件名。它存在 = 条目可用（写入顺序见 Cache.Put）。
const indexName = "index.html"

// metaName 条目的元信息（诊断/LRU 排查用，读缓存时不依赖它）。
const metaName = "meta.json"

// Meta 缓存条目的元信息。
type Meta struct {
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
}

// Cache 内容寻址的产物目录：<dir>/<key>/index.html。
//
// 为什么按内容寻址（而不是按路径）：同一份文件可能在多个工作区/多个路径下被打开
// （复制一份、换目录、从产物卡和文件树各点一次）—— 路径键会各转一次，内容键只转一次。
// 代价是每次都要把文件读一遍算 sha256（100 MiB 约 0.2s），换来的是「重复预览零转换」。
//
// 目录形状是**扁平 key**（没有按 session 分子目录，偏离 01-design §4.7 的
// `<session>/<uuid>/`）：session 不在键里，正是为了让跨会话/跨工作区命中；再套一层
// 会话目录反而会把同一份内容复制 N 份，让 512 MiB 的 LRU 预算提前耗尽。
type Cache struct {
	dir      string
	maxBytes int64
	maxAge   time.Duration
	now      func() time.Time

	// mu 串行化 Put 与 Sweep（两者都会删/建条目目录）。
	//
	// 为什么需要（评审指出的窄窗口）：Sweep 按**目录 mtime** 判过期，而 Put 在写完
	// 内容之后才刷 mtime —— 若那个 key 恰好有一条 24h 前的残条目，启动清扫可能在
	// 「刚写完 index.html、还没刷 mtime」的瞬间把整个目录删掉，用户拿到一个已被删除的
	// 产物路径（面板白屏）。
	//
	// 两道防线，各自能盖住的部分不同：
	//   - 这把锁：Sweep 与 Put **不交错**（前提是两者用同一个 Cache 实例 ——
	//     bridge 的启动清扫必须走 Service.cache，见 (*manager).sweepPreviewCache）；
	//   - Put 一开始就刷 mtime：把窗口**收窄到 Chtimes 之前**（MkdirAll 到 Chtimes
	//     之间仍有一个极短的瞬间目录还是旧时间；能删的前提是并发 Sweep 恰在此刻
	//     ReadDir，且此后再无 Chtimes —— 概率极低但不为零，别写成「归零」）。
	// Lookup 的 Chtimes 不受这把锁保护（命中路径不该排队），那点竞争只影响 LRU 顺序。
	mu sync.Mutex
}

// cacheFileMode 产物文件权限：0600。
//
// 里面是**用户文档的内容**（表格数值、会议纪要、合同条款），与 memory 子树同口径：
// 同机其它用户不该能直读。目录仍是 0700（MkdirAll），纵深防御。
const cacheFileMode = 0o600

// NewCache 构造缓存目录句柄（不建目录：首次写入时才建）。
func NewCache(dir string, maxBytes int64, maxAge time.Duration) *Cache {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxCacheBytes
	}
	if maxAge <= 0 {
		maxAge = DefaultMaxCacheAge
	}
	return &Cache{dir: dir, maxBytes: maxBytes, maxAge: maxAge, now: time.Now}
}

// Dir 缓存根目录（诊断/测试用）。
func (c *Cache) Dir() string { return c.dir }

// Entry 条目目录。
func (c *Cache) entryDir(key string) string { return filepath.Join(c.dir, key) }

// index 条目产物路径。
func (c *Cache) indexPath(key string) string { return filepath.Join(c.entryDir(key), indexName) }

// Lookup 命中则返回产物绝对路径，并把条目按「最近使用」刷新（LRU 依据是目录 mtime）。
//
// 判据是 index.html 可读：它是 Put 的**最后一步**（原子替换），故「文件在」与
// 「内容完整」是同一件事 —— 半成品条目（进程在转换中途被杀）永远不会被当成命中。
func (c *Cache) Lookup(key string) (string, bool) {
	p := c.indexPath(key)
	if st, err := os.Stat(p); err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return "", false
	}
	now := c.now()
	if err := os.Chtimes(c.entryDir(key), now, now); err != nil {
		// 刷 mtime 失败 = LRU 会把这个条目当成更旧的（极端情况下刚命中就被回收）。
		// 不影响本次命中（产物还在），故只记一行诊断，不失败。
		// 只打 key（十六进制哈希），不打路径 —— 路径里有用户名段（日志口径 §1.8）。
		fmt.Fprintf(os.Stderr, "[conversion] touch cache entry failed key=%s err=%v\n", key, err)
	}
	return p, true
}

// Put 把 r（转换产物）写进条目并返回产物路径。失败不留下可用条目。
func (c *Cache) Put(key string, r io.Reader, meta Meta) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dir := c.entryDir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// 先刷 mtime 再写内容：写完（可能几十 ms）之前若被并发 Sweep 看到旧 mtime，
	// 整个目录会被当成过期条目删掉。见 Cache.mu 的注释。
	now := c.now()
	_ = os.Chtimes(dir, now, now)
	// 顺序有意义：meta 是诊断信息（可缺），index.html 的落地 = 条目可用（不可缺）。
	// meta 同样 0600：它记的是**源文件名**（用户文档名字本身就是信息）。
	if b, err := json.Marshal(meta); err == nil {
		_ = atomicfile.ReplaceReader(filepath.Join(dir, metaName), bytes.NewReader(b), cacheFileMode)
	}
	p := filepath.Join(dir, indexName)
	if err := atomicfile.ReplaceReader(p, r, cacheFileMode); err != nil {
		return "", err
	}
	_ = os.Chtimes(dir, c.now(), c.now())
	return p, nil
}

// EntryDir 条目目录（诊断/测试用）。
func (c *Cache) EntryDir(key string) string { return c.entryDir(key) }

// FileAt 条目里的某个具名产物的绝对路径（存在且非空才算有）。
//
// 与 Lookup 的分工：Lookup 只认 index.html（HTML 预览的「条目可用」判据）；
// PDF/PNG 这类**非 HTML 产物**用 FileAt 逐名查（R08d：office_convert 的 PDF、
// office_render 的逐页 PNG 都写在同一套内容寻址目录里，共享 LRU 与限额）。
func (c *Cache) FileAt(key, name string) (string, bool) {
	if !validArtifactName(name) {
		return "", false
	}
	p := filepath.Join(c.entryDir(key), name)
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
		return "", false
	}
	// 命中也要刷 mtime：LRU 依据是目录 mtime，不刷会让热条目被当成最旧的。
	now := c.now()
	_ = os.Chtimes(c.entryDir(key), now, now)
	return p, true
}

// PutFile 把 r 写成条目里的具名产物（<key>/<name>），返回绝对路径。
//
// 为什么需要「具名」（而不是复用 Put）：Put 固定写 index.html —— 对 HTML 预览是对的
// （浏览器按扩展名决定渲染器），对 PDF 却是致命的：一份 PDF 字节被命名成 index.html，
// Chromium 会**按 HTML 解析**，用户看到的是白屏或半页乱码。故产物名必须由调用方给定。
func (c *Cache) PutFile(key, name string, r io.Reader, meta Meta) (string, error) {
	if !validArtifactName(name) {
		return "", fmt.Errorf("conversion: 非法的产物名 %q（只接受单层文件名）", name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	dir := c.entryDir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	now := c.now()
	_ = os.Chtimes(dir, now, now)
	if b, err := json.Marshal(meta); err == nil {
		_ = atomicfile.ReplaceReader(filepath.Join(dir, metaName), bytes.NewReader(b), cacheFileMode)
	}
	p := filepath.Join(dir, name)
	if err := atomicfile.ReplaceReader(p, r, cacheFileMode); err != nil {
		return "", err
	}
	_ = os.Chtimes(dir, c.now(), c.now())
	return p, nil
}

// validArtifactName 产物名必须是单层文件名。
//
// 这不是洁癖：key 由内容哈希算出，而 name 来自参数/调用点；一旦允许 "a/b" 或 ".."，
// 缓存写入就成了「按模型可算出的 key 往任意相对路径写文件」的通道。
func validArtifactName(name string) bool {
	return name != "" && name != "." && name != ".." && filepath.Base(name) == name
}

// Stats 一次清扫的结果（启动日志/测试断言用）。
type Stats struct {
	Entries int   // 清扫后现存条目数
	Bytes   int64 // 清扫后占用字节
	Removed int   // 删掉的条目数
	Freed   int64 // 释放的字节
}

// Sweep 过期回收 + 总量 LRU：启动时跑一次（01-design §4.7）。
//
// 两条规则各自解决一个问题：
//   - 过期（maxAge，默认 24h）：进程被杀留下的残条目、改了文件的旧产物，不会永久占坑；
//   - 总量（maxBytes，默认 512 MiB）：只靠过期的话，一小时内连开 200 个大表就会顶满磁盘。
//
// 只删缓存根目录下的**子目录**，绝不顺着 key 之外的位置递归 —— 这是「清理」，
// 不是「清理工作区」（硬约束：不加任何删除工作区文件的逻辑）。
func (c *Cache) Sweep() (Stats, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ents, err := os.ReadDir(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		return Stats{}, nil // 还没用过缓存：无事可做，不是错误
	}
	if err != nil {
		return Stats{}, err
	}
	type item struct {
		path string
		mod  time.Time
		size int64
	}
	var (
		live []item
		st   Stats
	)
	now := c.now()
	for _, e := range ents {
		if !e.IsDir() {
			continue // 顶层只放 key 目录；散文件（如手工产物）不碰
		}
		p := filepath.Join(c.dir, e.Name())
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		size := dirSize(p)
		if now.Sub(info.ModTime()) > c.maxAge {
			if os.RemoveAll(p) == nil {
				st.Removed++
				st.Freed += size
			}
			continue
		}
		live = append(live, item{path: p, mod: info.ModTime(), size: size})
	}
	// LRU：从最久未用的开始删，直到落回预算内。
	sort.Slice(live, func(i, j int) bool { return live[i].mod.Before(live[j].mod) })
	for _, it := range live {
		st.Bytes += it.size
	}
	dropped := 0
	for _, it := range live {
		if st.Bytes <= c.maxBytes {
			break
		}
		if os.RemoveAll(it.path) == nil {
			st.Removed++
			dropped++
			st.Freed += it.size
			st.Bytes -= it.size
		}
	}
	st.Entries = len(live) - dropped
	return st, nil
}

// dirSize 目录下常规文件的总字节（不跟随软链接）。
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// CacheKey 内容寻址键：sha256(版本 | kind | 文件名 | 参数指纹 | 文件字节)。
//
// 文件名为什么在键里（只看字节不够）：三个转换脚本都把 `os.path.basename(src)` 写进
// 产物（<title> 与页面大标题）—— 同一份内容换个名字，产物**不同**。只按内容做键，
// 用户把「周报.xlsx」另存为「月报.xlsx」会看到标题还写着周报（实测过这类"看着像缓存没刷"
// 的误报）。代价：改名 = 重转一次，可接受。
//
// 为什么不把 workspace 放进键：产物只由「字节 + 文件名 + kind + 参数」决定，沙箱根
// 只影响进程权限，不影响 HTML。放进去会让同一份文件在不同工作区各转一次。
func CacheKey(kind, name string, params map[string]string, src string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	fmt.Fprintf(h, "%s|script=%s|kind=%s|name=%s|params=%s|",
		cacheKeyVersion, keyScriptFingerprint, kind, name, paramsFingerprint(params))
	// 1 MiB 缓冲：100 MiB 的输入约 100 次 syscall，不让哈希成为新的瓶颈。
	buf := make([]byte, 1<<20)
	if _, err := io.CopyBuffer(h, f, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// paramsFingerprint 参数指纹：键有序拼接（map 迭代无序，不排序会让同参数算出不同键）。
// 当前三条通路都没有参数（脚本是编译期常量），字段留着是为了以后加参数（dpi 等）时
// 不必再改键的口径 —— 少一个字段就是「静默命中旧产物」的坑。
func paramsFingerprint(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	return b.String()
}
