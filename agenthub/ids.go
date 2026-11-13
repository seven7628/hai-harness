package agenthub

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// NeedID / ArtifactID 是账本与产物记录的稳定标识。
// 由 Store 用对应前缀（need_/art_）生成；类型化避免跨实体串用。
type NeedID string
type ArtifactID string

// ID 前缀。日志、事件、mesh 寻址都靠前缀判断实体类型。
// Message 默认是确定性的 msg_<seq>（见 MessageID）；带流式草稿的消息在 AgentStart
// 先分配一个 msg_<random>，final 落账时由 store 保留同一个 ID。
const (
	PrefixMessage  = "msg"
	PrefixAgent    = "ag"
	PrefixGroup    = "grp"
	PrefixTask     = "task"
	PrefixRound    = "rnd"
	PrefixNeed     = "need"
	PrefixArtifact = "art"
	PrefixHandoff  = "ho"
)

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// MessageID 返回 msg_<seq>（默认 ID；没有预分配流式 ID 的消息走这里）。
// 带流式草稿的消息在 AgentStart 用 NewID(PrefixMessage) 预分配，final 必须复用同一个 ID。
func MessageID(seq int64) string {
	return "msg_" + strconv.FormatInt(seq, 10)
}

// messageIDFor 保留调用方预分配的流式 ID（AgentStart → final 同一 message_id）；
// 其余消息仍用确定性的 msg_<seq>。只接受 msg_ 前缀，避免任意字符串写进日志。
func messageIDFor(m *Message, seq int64) string {
	if m != nil && strings.HasPrefix(m.ID, PrefixMessage+"_") && validID(m.ID) {
		return m.ID
	}
	return MessageID(seq)
}

// IDGenerator 生成“前缀_随机串”的 ID。
//
// Rand 可注入，测试用 bytes.Reader 得到稳定结果；零值使用 crypto/rand。
// 同一个 Reader 不要跨生成器共享（每个生成器有独立 mutex）；单生成器内的读取已串行化。
// 默认 crypto/rand.Reader 可跨生成器共享。随机失败时退回时间+计数器，保证 New 不返回空串。
type IDGenerator struct {
	Rand io.Reader
	mu   sync.Mutex
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
	if g == nil {
		g = defaultGenerator
	}
	prefix = normalizePrefix(prefix)
	n := idBytes(prefix)

	var buf [8]byte
	g.mu.Lock()
	r := g.Rand
	if r == nil {
		r = rand.Reader
	}
	_, err := io.ReadFull(r, buf[:n])
	g.mu.Unlock()
	if err != nil {
		// 只影响 ID 的不可预测性，不影响唯一性：时间 + 进程内计数器混合。
		v := uint64(time.Now().UnixNano()) + g.seq.Add(1)*0x9E3779B97F4A7C15
		binary.BigEndian.PutUint64(buf[:], v)
	}

	suffix := strings.ToLower(idEncoding.EncodeToString(buf[:n]))
	return prefix + "_" + suffix[:idChars(prefix)]
}

// idBytes 是随机源读取字节数：agent 5 字节（8 位 base32），其余 4 字节（截到 6 位）。
func idBytes(prefix string) int {
	if prefix == PrefixAgent {
		return 5
	}
	return 4
}

// idChars 是 §5.8 的展示长度：agent 8 位，其余 6 位。
func idChars(prefix string) int {
	if prefix == PrefixAgent {
		return 8
	}
	return 6
}

func normalizePrefix(prefix string) string {
	prefix = strings.Trim(strings.ToLower(prefix), "_")
	if prefix == "" {
		return "id"
	}
	return prefix
}
