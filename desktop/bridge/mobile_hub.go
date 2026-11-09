// mobile_hub.go —— 手机端事件扇出。
//
// 设计约束（见 docs/mobile-remote/TECH_PLAN.md §3.3）：
//   - 复用 emit 已经产出的那一行 JSON（注入好 session_id/workspace），不二次序列化；
//   - 订阅者慢（手机弱网/息屏）时**只丢流式 chunk/delta**，绝不丢 agent_end / 审批 / 提问 /
//     hub 控制事件；
//   - 丢过就置 dropped，由写循环补一帧 {"type":"resync"}（会话）或 resync 让手机重拉快照。
//
// 两类订阅严格隔离（P4-03a）：
//   - 会话订阅（workspace+session）：只收 bs.emit 的会话事件，永不收 hub_*；
//   - Hub 订阅（scope=hub）：只收 hub_* 应用级事件，不收任何会话事件。
package main

import (
	"bytes"
	"sync"
	"sync/atomic"
)

// mobileSubBuf 每个订阅者的缓冲行数。256 行 ≈ 一次工具调用的完整事件量；
// 再大只是把内存换成延迟，没有收益。
const mobileSubBuf = 256

type mobileHub struct {
	mu   sync.RWMutex
	subs map[*mobileSub]struct{}
}

type mobileSub struct {
	ws  string // 空 = 不限工作区
	sid string // 空 = 不限会话
	// hub=true 表示 Hub 应用级订阅：只收 hub_*（见 broadcastHub），
	// 不参与任何会话事件投递。Hub 数据不属于某个 workspace/session。
	hub bool
	ch  chan []byte
	// dropped 标记「这个订阅者漏过事件」。写循环看到它就先发 resync，
	// 再继续增量——手机据此重新拉快照。
	dropped atomic.Bool
}

func newMobileHub() *mobileHub {
	return &mobileHub{subs: map[*mobileSub]struct{}{}}
}

// subscribe 会话订阅：workspace + session 双过滤（空过滤不等于订阅全部）。
func (h *mobileHub) subscribe(ws, sid string) *mobileSub {
	s := &mobileSub{ws: ws, sid: sid, ch: make(chan []byte, mobileSubBuf)}
	h.add(s)
	return s
}

// subscribeHub Hub 应用级订阅：只收 hub_*，不绑定 workspace/session。
func (h *mobileHub) subscribeHub() *mobileSub {
	s := &mobileSub{hub: true, ch: make(chan []byte, mobileSubBuf)}
	h.add(s)
	return s
}

func (h *mobileHub) add(s *mobileSub) {
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
}

func (h *mobileHub) unsubscribe(s *mobileSub) {
	if h == nil || s == nil {
		return
	}
	h.mu.Lock()
	if _, ok := h.subs[s]; ok {
		delete(h.subs, s)
		close(s.ch)
	}
	h.mu.Unlock()
}

// broadcast 把一行会话事件投给所有匹配的会话订阅者。非阻塞：宁可丢 chunk，
// 也不拖住 agent 的 emit。Hub 订阅者不在此列（否则会话事件会串到 Hub 屏）。
func (h *mobileHub) broadcast(ws, sid string, line []byte) {
	if h == nil || len(line) == 0 {
		return
	}
	droppable := isDroppableEvent(line)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if s.hub {
			continue // Hub 订阅只收 hub_*，见 broadcastHub
		}
		// 只投给「同一工作区 + 同一会话」：空过滤**不等于**订阅全部 ——
		// 否则一个漏传 session_id 的客户端会收下整个工作区的流（会话串台）。
		if s.ws == "" || ws == "" || s.ws != ws {
			continue
		}
		if s.sid == "" || sid == "" || s.sid != sid {
			continue
		}
		deliverMobile(s, line, droppable)
	}
}

// broadcastHub 把 hub_* 应用级事件投给 hub 作用域订阅者。
// Hub 是应用级数据面（不属于任何 workspace/session），因此不按会话过滤；
// 普通会话订阅者永远收不到 hub_*（反之亦然），避免手机端串台。
func (h *mobileHub) broadcastHub(line []byte) {
	if h == nil || len(line) == 0 || !isHubEvent(line) {
		return
	}
	droppable := isHubDroppableEvent(line)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if !s.hub {
			continue
		}
		deliverMobile(s, line, droppable)
	}
}

// deliverMobile 非阻塞投递。droppable=true（chunk/delta）缓冲满即丢并标记
// dropped；控制事件必须挤出一格。若挤掉的也是控制事件，同样标记 dropped，
// 让写循环发 resync —— 控制事件可以靠快照重建，但不能无声消失。
func deliverMobile(s *mobileSub, line []byte, droppable bool) {
	select {
	case s.ch <- line:
		return
	default:
	}
	if droppable {
		s.dropped.Store(true)
		return
	}
	select {
	case old := <-s.ch:
		if !isDroppableEvent(old) {
			s.dropped.Store(true)
		}
	default:
	}
	select {
	case s.ch <- line:
	default:
		s.dropped.Store(true)
	}
}

// drainControl 排空订阅者积压，只留下控制事件。
// 场景：订阅者丢过帧，服务端先补一帧 live 快照——快照已包含全部已产出的 chunk，
// 积压里的 chunk 再发一遍会让同一段文本出现两次。closed=true 表示通道已被 close
// （订阅者被移除），调用方应直接退出写循环。
func drainControl(sub *mobileSub) (kept [][]byte, closed bool) {
	for {
		select {
		case next, ok := <-sub.ch:
			if !ok {
				return kept, true
			}
			if !isDroppableEvent(next) {
				kept = append(kept, next)
			}
		default:
			return kept, false
		}
	}
}

// isChunkEvent 流式增量（可丢、可重建）。用前缀扫描而不是反序列化：热路径上每秒几十次。
func isChunkEvent(line []byte) bool {
	t := eventTypeOf(line)
	return t == "content_chunk" || t == "reasoning_chunk"
}

// isDroppableEvent 可丢的流式增量：会话 chunk + Hub 草稿 delta。
// 其余一律按控制事件处理（宁可挤占缓冲也不静默丢）。
func isDroppableEvent(line []byte) bool {
	switch eventTypeOf(line) {
	case "content_chunk", "reasoning_chunk", "hub_message_delta":
		return true
	}
	return false
}

// isHubEvent 判断一行是否为 Hub 事件（hub_*）。未知事件仍按现有协议忽略。
func isHubEvent(line []byte) bool {
	const prefix = "hub_"
	t := eventTypeOf(line)
	return len(t) >= len(prefix) && t[:len(prefix)] == prefix
}

// isHubDroppableEvent Hub 里只有草稿 delta 可丢；hub_message/hub_task_update/
// hub_round/hub_conversation/hub_alert/hub_need 等控制事件必须保留。
func isHubDroppableEvent(line []byte) bool {
	return eventTypeOf(line) == "hub_message_delta"
}

// eventTypeOf 从 emit 注入过 session_id/workspace 的行里取 event_type。
// 注入发生在对象首部，所以不能假设 event_type 是第一个键。
func eventTypeOf(line []byte) string {
	const key = `"event_type":"`
	if i := bytes.Index(line, []byte(key)); i >= 0 {
		rest := line[i+len(key):]
		if j := bytes.IndexByte(rest, '"'); j >= 0 {
			return string(rest[:j])
		}
	}
	// 容错：JSON 编码器可能在冒号后留空格（当前不用 indent，纯防御）
	const key2 = `"event_type": "`
	if i := bytes.Index(line, []byte(key2)); i >= 0 {
		rest := line[i+len(key2):]
		if j := bytes.IndexByte(rest, '"'); j >= 0 {
			return string(rest[:j])
		}
	}
	return ""
}

// broadcastGlobal 应用级事件（不属于任何会话，如「电脑选目录」的结果）：
// 不受 workspace/session 过滤，投给所有**会话**订阅者（保持原行为）。
// hub 订阅只收 hub_*，因此这里跳过，避免把应用事件混进 Hub 协议面。
func (h *mobileHub) broadcastGlobal(line []byte) {
	if h == nil || len(line) == 0 {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if s.hub {
			continue
		}
		deliverMobile(s, line, false)
	}
}
