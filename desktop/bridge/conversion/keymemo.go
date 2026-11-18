package conversion

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sync"
)

// 内容键的**两级化**（第 4 轮爬山）。
//
// 现状：CacheKey 是纯内容寻址（sha256(口径版本 | 脚本指纹 | kind | 文件名 | 参数 | 文件字节)），
// 算它要**把整个源文件读一遍**（100 MiB ≈ 0.2 s CPU + 全量读盘）。而预览请求
// **每次**都要算键 —— 哪怕命中缓存（键是查表的前提）⇒ 命中一次缓存也要付一次
// 全文件读盘 + 哈希，还占着哈希闸门（并发 2）把别的请求堵在后面。
//
// 改：两级键。
//
//	L1 = StatKey：路径 + 大小 + mtime + **ctime** + inode（一次 stat）
//	L2 = 内容 sha256（现在的 CacheKey）
//
// L1 命中 ⇒ 直接复用记住的 L2 ⇒ 免掉全文件读盘 + 哈希。
//
// 为什么 L1 敢用（ctime 是关键）：mtime 可以被 `cp -p`、解压等**保留** ⇒ 只看
// (size, mtime) 会在「内容变了、时间戳被还原」时命中旧产物（给用户看**错内容**，
// 比慢更糟）。ctime（inode 变更时间）用户改不了：任何写入都更新它，换文件
// （换 inode）也更新它 ⇒ 加上 ctime + inode 后，「L1 未变 ⇒ 内容未变」成立。
//
// 边界：仍有理论漏网（同 inode + 同 ctime + 同 size 的原地改写，且把 mtime 一起
// 还原）—— 那要求同时具备「改文件」与「改 ctime」的权限，实际不可达。

// StatKey 文件的便宜凭据（一次 stat）。
func StatKey(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%d", path, fi.Size(), fi.ModTime().UnixNano())
	if id, ok := fileIdentity(fi); ok {
		io.WriteString(h, "|"+id)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// l1Key L1 键：除文件内容以外、决定内容键的全部要素 + 文件的 stat 凭据。
func l1Key(kind, name string, params map[string]string, src string) (string, error) {
	sk, err := StatKey(src)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|", cacheKeyVersion, keyScriptFingerprint, kind, name, paramsFingerprint(params))
	io.WriteString(h, sk)
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// keyMemo 进程内的 L1 → L2 备忘（命中缓存时免掉全文件读盘）。
//
// 为什么放内存而不是落盘：备忘只省「一次 stat vs 一次全文件读」，重建代价极低
// （进程重启 ⇒ 第一次请求读一次文件，之后又回到快路径）；落盘反而要处理「索引与
// 缓存目录不一致」的清理问题。
//
// 容量有界（默认 256 条）：预览的活跃文件集合很小，超出的按插入序淘汰最旧的。
type keyMemo struct {
	mu    sync.Mutex
	m     map[string]string
	order []string
	max   int
	// hook 测试钩子：每次 ContentKey 报一次（hit=true 表示走了备忘、没读文件）。
	hook func(hit bool)
}

func newKeyMemo(max int) *keyMemo {
	if max <= 0 {
		max = 256
	}
	return &keyMemo{m: make(map[string]string), max: max}
}

// get 查备忘（L1 → L2）。
func (km *keyMemo) get(l1 string) (string, bool) {
	km.mu.Lock()
	defer km.mu.Unlock()
	k, ok := km.m[l1]
	return k, ok
}

// put 记一条（L1 → L2），超容量时淘汰最旧的。
func (km *keyMemo) put(l1, l2 string) {
	km.mu.Lock()
	defer km.mu.Unlock()
	if _, exists := km.m[l1]; exists {
		km.m[l1] = l2
		return
	}
	km.m[l1] = l2
	km.order = append(km.order, l1)
	for len(km.order) > km.max {
		oldest := km.order[0]
		km.order = km.order[1:]
		delete(km.m, oldest)
	}
}

// ContentKey 返回源文件的内容键；命中进程内备忘时**不读文件**。
//
// 参数与 CacheKey 完全一致（kind / 文件名 / 参数 / 源文件路径）⇒ 返回的键与
// CacheKey 逐字相同 ⇒ 缓存目录结构与命中语义完全不变。
func (km *keyMemo) ContentKey(kind, name string, params map[string]string, src string) (string, error) {
	l1, err := l1Key(kind, name, params, src)
	if err != nil {
		return "", err
	}
	if l2, ok := km.get(l1); ok {
		if km.hook != nil {
			km.hook(true)
		}
		return l2, nil
	}
	if km.hook != nil {
		km.hook(false)
	}
	l2, err := CacheKey(kind, name, params, src)
	if err != nil {
		return "", err
	}
	km.put(l1, l2)
	return l2, nil
}
