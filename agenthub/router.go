package agenthub

// router.go：群内 @ 路由 + round + 回执（P3a-03，§6.6.3）。
//
// 规则（严格解析器，不降级）：
//   - 无 @ → 不唤醒任何智能体；
//   - @all → 仅 owner 或人类可用（member/observer 直接拒绝，不降级成普通 @）；
//     targets = 除发送者外的全部成员（按 JoinedAt、AgentID 排序，确定性）；
//   - @Name → 命中成员；超过策略上限（默认 3）的记 Dropped；
//   - 未知 @ 只进 Unknown 警告，不唤醒。
//
// round 快照记录 trigger seq 与 targets；回执作为活动流落盘（独立于消息日志）。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Round 是一次 @ 路由的快照（冻结本轮目标，后续成员变更不影响）。
type Round struct {
	ID         string    `json:"id"`
	GroupID    GroupID   `json:"group_id"`
	TriggerSeq int64     `json:"trigger_seq"`
	Targets    []AgentID `json:"targets"`
	MentionAll bool      `json:"mention_all,omitempty"`
	Unknown    []string  `json:"unknown,omitempty"`
	Dropped    int       `json:"dropped,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Router 绑定一个群的成员与策略；成员变化后重新构造（parser 缓存名字）。
type Router struct {
	group  *Group
	parser *MentionParser
	byID   map[AgentID]Member
}

func NewGroupRouter(g *Group, profiles []*AgentProfile) (*Router, error) {
	if g == nil {
		return nil, fmt.Errorf("%w: group is nil", ErrInvalid)
	}
	if len(g.Members) == 0 {
		return nil, fmt.Errorf("%w: group has no members", ErrInvalid)
	}
	parser, err := NewMentionParser(profiles)
	if err != nil {
		return nil, err
	}
	byID := make(map[AgentID]Member, len(g.Members))
	for _, m := range g.Members {
		byID[m.AgentID] = m
	}
	return &Router{group: g, parser: parser, byID: byID}, nil
}

// Route 解析并冻结一轮目标。sender 的群角色用于 @all 鉴权；isHuman 可直接 @all。
func (r *Router) Route(text string, sender AgentID, isHuman bool, triggerSeq int64, now time.Time) (Round, error) {
	if r == nil || r.parser == nil || r.group == nil {
		return Round{}, fmt.Errorf("%w: router 未初始化", ErrInvalid)
	}
	role := RoleMember
	if m, ok := r.byID[sender]; ok {
		role = m.Role
	}
	limit := r.group.Policy.MaxMentions
	res, err := r.parser.ParseAuthorizedWithLimit(text, sender, isHuman, role, limit)
	if err != nil {
		return Round{}, err
	}
	targets := make([]AgentID, 0, len(res.Mentions))
	if res.MentionAll {
		for _, m := range r.sortedMembers() {
			if m.AgentID != sender {
				targets = append(targets, m.AgentID)
			}
		}
	} else {
		targets = append(targets, res.Mentions...)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return Round{
		ID: NewID(PrefixRound), GroupID: r.group.ID, TriggerSeq: triggerSeq,
		Targets: targets, MentionAll: res.MentionAll, Unknown: res.Unknown,
		Dropped: res.Dropped, CreatedAt: now,
	}, nil
}

func (r *Router) sortedMembers() []Member {
	members := append([]Member(nil), r.group.Members...)
	sort.SliceStable(members, func(i, j int) bool {
		if members[i].JoinedAt.Equal(members[j].JoinedAt) {
			return members[i].AgentID < members[j].AgentID
		}
		return members[i].JoinedAt.Before(members[j].JoinedAt)
	})
	return members
}

// Receipt 把路由结果写成确定性的活动回执（kind=routing，AfterSeq=触发消息）。
func (r Round) Receipt() Activity {
	names := make([]string, 0, len(r.Targets))
	for _, id := range r.Targets {
		names = append(names, string(id))
	}
	summary := "targets=" + strings.Join(names, ",")
	if r.MentionAll {
		summary = "@all " + summary
	}
	if r.Dropped > 0 {
		summary += fmt.Sprintf(" dropped=%d", r.Dropped)
	}
	return Activity{
		GroupID: r.GroupID, AfterSeq: r.TriggerSeq, Kind: "routing",
		Name: "round", Status: "done", Summary: summary, At: r.CreatedAt,
	}
}

// AppendRoundReceipt 落盘回执；round 本身不写消息日志（活动流独立）。
func AppendRoundReceipt(ctx context.Context, st Store, round Round) error {
	if st == nil {
		return fmt.Errorf("%w: store 为空", ErrInvalid)
	}
	a := round.Receipt()
	a.RunID = round.ID
	return st.AppendActivity(ctx, &a)
}

// TargetsOf 把 targets 转成集合（含去重校验；重复目标视为调用方 bug）。
func (r Round) TargetsOf() (map[AgentID]bool, error) {
	out := make(map[AgentID]bool, len(r.Targets))
	for _, id := range r.Targets {
		if out[id] {
			return nil, fmt.Errorf("%w: duplicate round target %s", ErrInvalid, id)
		}
		out[id] = true
	}
	return out, nil
}
