package conversion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/internal/atomicfile"
)

// cacheKeyVersion 产物口径版本。**改转换脚本/产物结构时必须 bump 它**（R08a 注释）。
//
// 为什么必须有这一位：三个转换脚本是编译期常量，随 app 版本一起变；缓存键里若不体现
// 「是哪一版脚本产出的」，升级后用户仍会命中上一版脚本的 HTML —— 表现为「更新了却什么都没变」，
// 且没有任何报错（最坏的一种故障）。bump 的代价只是升级后一次性重转。
const cacheKeyVersion = "r08a-v1"

// keyBuildStamp 二进制构建标识（vcs revision / 模块版本），也是缓存键的一位。
//
// 为什么把「哪个构建产出的」也放进键，而不是只靠人工 bump cacheKeyVersion：转换脚本是
// 编译进二进制的常量，**改了脚本却忘了 bump** 是最容易犯的错，后果是升级后仍命中旧产物、
// 且完全静默（用户只会觉得「更新没生效」）。带上构建标识后，换一个构建 = 换一套键，
// 正确性不再依赖记性。代价：每次重新构建都会让旧条目自然过期（缓存本来就是可丢的）。
var keyBuildStamp = buildStamp()

// buildStamp 取构建标识（尽量短、稳定）：优先 vcs revision，其次模块版本。
// 取不到（未启用 buildvcs / 本地 go run）就返回空串 —— 退化成「只靠 cacheKeyVersion」。
func buildStamp() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			return s.Value[:12]
		}
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return ""
}

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
}

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
	_ = os.Chtimes(c.entryDir(key), now, now)
	return p, true
}

// Put 把 r（转换产物）写进条目并返回产物路径。失败不留下可用条目。
func (c *Cache) Put(key string, r io.Reader, meta Meta) (string, error) {
	dir := c.entryDir(key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	// 顺序有意义：meta 是诊断信息（可缺），index.html 的落地 = 条目可用（不可缺）。
	if b, err := json.Marshal(meta); err == nil {
		_ = atomicfile.Replace(filepath.Join(dir, metaName), string(b))
	}
	p := filepath.Join(dir, indexName)
	if err := atomicfile.ReplaceReader(p, r); err != nil {
		return "", err
	}
	now := c.now()
	_ = os.Chtimes(dir, now, now)
	return p, nil
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
	fmt.Fprintf(h, "%s|build=%s|kind=%s|name=%s|params=%s|",
		cacheKeyVersion, keyBuildStamp, kind, name, paramsFingerprint(params))
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
