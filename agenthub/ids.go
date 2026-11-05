package agenthub

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// ID 前缀。日志、事件、mesh 寻址都靠前缀判断实体类型。
const (
	PrefixAgent    = "ag"
	PrefixGroup    = "grp"
	PrefixTask     = "task"
	PrefixMessage  = "msg"
	PrefixRound    = "rnd"
	PrefixNeed     = "need"
	PrefixArtifact = "art"
)

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// IDGenerator 生成“前缀_随机串”的 ID。
//
// Rand 可注入，测试用 bytes.Reader 得到稳定结果；零值使用 crypto/rand。
// 随机失败时退回时间+计数器，保证 New 不返回空串（调用方不需要处理 error）。
type IDGenerator struct {
	Rand io.Reader
	seq  atomic.Uint64
}

// NewIDGenerator 用给定随机源构造生成器；传 nil 等价于 crypto/rand。
func NewIDGenerator(r io.Reader) *IDGenerator {
	return &IDGenerator{Rand: r}
}

var defaultGenerator = NewIDGenerator(nil)

// NewID 用默认（加密安全）生成器生成 ID。
func NewID(prefix string) string {
	return defaultGenerator.New(prefix)
}

// New 生成一个 ID。prefix 会转小写并去掉首尾下划线；空 prefix 用 "id"。
func (g *IDGenerator) New(prefix string) string {
	prefix = strings.Trim(strings.ToLower(prefix), "_")
	if prefix == "" {
		prefix = "id"
	}

	var buf [8]byte
	r := g.Rand
	if r == nil {
		r = rand.Reader
	}
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		// 只影响 ID 的不可预测性，不影响唯一性：时间 + 进程内计数器。
		binary.BigEndian.PutUint64(buf[:], uint64(time.Now().UnixNano())+g.seq.Add(1))
	}

	return prefix + "_" + strings.ToLower(idEncoding.EncodeToString(buf[:]))
}
