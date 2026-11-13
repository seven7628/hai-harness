package agenthub

// router.go：群内 @ 路由 + round + 回执（P3a-03，§6.6.3；结构化 mentions 见 P3b-02）。
//
// 规则（严格解析器，不降级）：
//   - 正文无 @ 且无结构化 mentions → 不唤醒任何智能体；
//   - @all（正文或 mention_all）→ 仅 owner 或人类可用（member 直接拒绝，
//     不降级成普通 @）；targets = 除发送者外的全部成员（按 JoinedAt、AgentID 排序）；
//   - @Name / 结构化 agent id → 取并集：正文出现顺序在前，结构化 id 顺序在后，
//     去重并过滤 self，再按 GroupPolicy.MaxMentions（默认 3）统一截断；
//   - 超上限的唯一成员记 Dropped（正文与结构化共用一条上限，不是 3+3）；
//   - 未知 @ 只进 Unknown 警告，不唤醒；结构化 id 必须是当前活跃成员，否则报错
//     （调用方显式点名，静默丢会让它误以为目标已被唤醒）。
//
// round 快照记录 trigger seq、显式 mentions 与 targets；回执作为活动流落盘
// （独立于消息日志）。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// MentionRequest 是一条群消息的 @ 输入：正文里的 @Name 与结构化 agent id 是
// 两个独立信号（模型/人类可能只给其中一种），Router 负责并集、去重与上限。
type MentionRequest struct {
	Text       string
	Mentions   []AgentID // 稳定 agent id；调用方顺序 = 并集顺序
	MentionAll bool
}

// Round 是一次 @ 路由的快照（冻结本轮目标，后续成员变更不影响）。
//
// Mentions 是解析后真正生效的显式成员（去重、过滤 self、已按上限截断）；
// @all 时 Targets 是全体成员而 Mentions 只含被点名的成员，两者不要混用。
type Round struct {
	ID         string    `json:"id"`
	GroupID    GroupID   `json:"group_id"`
	TriggerSeq int64     `json:"trigger_seq"`
	Targets    []AgentID `json:"targets"`
	Mentions   []AgentID `json:"mentions,omitempty"`
	MentionAll bool      `json:"mention_all,omitempty"`
	Unknown    []string  `json:"unknown,omitempty"`
	Dropped    int       `json:"dropped,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	// Sender/SenderHuman 是本轮的**来源**（P6-11）：快照据此标注「人」还是
	// 「owner/member 同事请求」，审计与提示词都靠它区分人类授权与智能体请求。
	// omitempty：P6-11 之前落盘的旧 round 没有该字段（空 = 来源未知）。
	Sender      AgentID `json:"sender_agent_id,omitempty"`
	SenderHuman bool    `json:"sender_human,omitempty"`
}

// Router 绑定一个群的成员与策略；成员变化后重新构造（parser 缓存名字）。
// members 是构造时的快照：只包含「本群成员 ∩ 未删除 profile」，@all 不再读实时
// Group.Members，软删/purge/跨群 profile 都不会被唤醒（P3a-03 B-3）。
type Router struct {
	group   *Group
	parser  *MentionParser
	byID    map[AgentID]Member
	members []Member
}

func NewGroupRouter(g *Group, profiles []*AgentProfile) (*Router, error) {
	if g == nil {
		return nil, fmt.Errorf("%w: group is nil", ErrInvalid)
	}
	if g.Deleted {
		return nil, fmt.Errorf("%w: group %s 已删除", ErrNotFound, g.ID)
	}
	if len(g.Members) == 0 {
		return nil, fmt.Errorf("%w: group has no members", ErrInvalid)
	}
	byProfile := make(map[AgentID]*AgentProfile, len(profiles))
	for _, p := range profiles {
		if p != nil && !p.Deleted {
			byProfile[p.ID] = p
		}
	}
	eligible := make([]*AgentProfile, 0, len(g.Members))
	members := make([]Member, 0, len(g.Members))
	for _, m := range g.Members {
		if p := byProfile[m.AgentID]; p != nil {
			eligible = append(eligible, p)
			members = append(members, m)
		}
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("%w: group has no active members", ErrInvalid)
	}
	parser, err := NewMentionParser(eligible)
	if err != nil {
		return nil, err
	}
	byID := make(map[AgentID]Member, len(members))
	for _, m := range members {
		byID[m.AgentID] = m
	}
	return &Router{group: g, parser: parser, byID: byID, members: members}, nil
}

// Route 解析正文并冻结一轮目标。sender 的群角色用于 @all 鉴权；isHuman 可直接 @all。
// 保留原签名：只做正文解析，等价于 RouteMentions(MentionRequest{Text: text})。
func (r *Router) Route(text string, sender AgentID, isHuman bool, triggerSeq int64, now time.Time) (Round, error) {
	return r.RouteMentions(MentionRequest{Text: text}, sender, isHuman, triggerSeq, now)
}

// RouteMentions 解析「正文 @ + 结构化 mentions」并冻结一轮目标（P3b-02）。
//
// 先做 @all 鉴权（正文与 mention_all 任一命中即需 owner/人类），再做结构化 id 校验，
// 两者失败都返回空 Round 与错误 —— 调用方不得降级成「只发消息」或「只唤醒部分人」。
func (r *Router) RouteMentions(in MentionRequest, sender AgentID, isHuman bool, triggerSeq int64, now time.Time) (Round, error) {
	if r == nil || r.parser == nil || r.group == nil {
		return Round{}, fmt.Errorf("%w: router 未初始化", ErrInvalid)
	}
	role := RoleMember
	if m, ok := r.byID[sender]; ok {
		role = m.Role
	}
	// 正文先全量解析：结构化 id 还要参与同一条上限，这里截断会算错 Dropped。
	text, err := r.parser.parseAuthorized(in.Text, sender, isHuman, role, -1)
	if err != nil {
		return Round{}, err
	}
	mentionAll := text.MentionAll || in.MentionAll
	// @all 鉴权先于成员校验：越权者拿不到「这个 id 是不是成员」的探测结果。
	if err := (MentionResult{MentionAll: mentionAll}).ValidateMentionAll(isHuman, role); err != nil {
		return Round{}, err
	}
	structured, err := r.resolveStructured(in.Mentions)
	if err != nil {
		return Round{}, err
	}
	mentions, dropped := capMentions(append(text.Mentions, structured...), sender, r.mentionLimit())
	targets := mentions
	if mentionAll {
		targets = make([]AgentID, 0, len(r.members))
		for _, m := range r.sortedMembers() {
			if m.AgentID != sender {
				targets = append(targets, m.AgentID)
			}
		}
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return Round{
		ID: NewID(PrefixRound), GroupID: r.group.ID, TriggerSeq: triggerSeq,
		Targets: targets, Mentions: mentions, MentionAll: mentionAll,
		Unknown: text.Unknown, Dropped: dropped, CreatedAt: now,
		Sender: sender, SenderHuman: isHuman,
	}, nil
}

// mentionLimit 是 GroupPolicy.MaxMentions 的有效值；未 Normalize 的群按默认 3 处理。
func (r *Router) mentionLimit() int {
	if r == nil || r.group == nil || r.group.Policy.MaxMentions <= 0 {
		return DefaultGroupMaxMentions
	}
	return r.group.Policy.MaxMentions
}

// resolveStructured 校验显式点名的 id：必须是本 Router 快照里的活跃成员。
// 未知/已删除/非本群一律报错而不是静默丢弃 —— 结构化 id 是调用方明确给出的目标，
// 悄悄忽略会让模型以为对方已被唤醒。self 不报错，由 capMentions 过滤（与正文同口径）。
func (r *Router) resolveStructured(ids []AgentID) ([]AgentID, error) {
	out := make([]AgentID, 0, len(ids))
	for _, raw := range ids {
		id := AgentID(strings.TrimSpace(string(raw)))
		if id == "" {
			return nil, fmt.Errorf("%w: mention id 不能为空", ErrInvalid)
		}
		if _, ok := r.byID[id]; !ok {
			return nil, fmt.Errorf("%w: mention %s 不是本群活跃成员", ErrInvalid, id)
		}
		out = append(out, id)
	}
	return out, nil
}

// capMentions 按顺序去重、过滤 self，并截到 limit；dropped 只数唯一且超上限的成员
// （重复/self 不计，与正文解析同口径）。
func capMentions(ids []AgentID, sender AgentID, limit int) ([]AgentID, int) {
	if limit <= 0 {
		limit = DefaultGroupMaxMentions
	}
	out := make([]AgentID, 0, limit)
	seen := make(map[AgentID]bool, len(ids))
	dropped := 0
	for _, id := range ids {
		if id == "" || id == sender || seen[id] {
			continue
		}
		seen[id] = true
		if len(out) < limit {
			out = append(out, id)
		} else {
			dropped++
		}
	}
	return out, dropped
}

func (r *Router) sortedMembers() []Member {
	members := append([]Member(nil), r.members...)
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

// AppendRoundReceipt 落盘回执：消息日志写一条 KindRouting 系统消息（快照会过滤），
// activity 流保留路由摘要供 Run 视图使用（P3a-03 B-1）。
func AppendRoundReceipt(ctx context.Context, st Store, round Round) error {
	if st == nil {
		return fmt.Errorf("%w: store 为空", ErrInvalid)
	}
	a := round.Receipt()
	a.RunID = round.ID
	if _, err := AppendGroupMessage(ctx, st, &Message{
		GroupID: round.GroupID, Kind: KindRouting, Sender: Sender{Kind: SenderSystem},
		Text: a.Summary, RoundID: round.ID, CreatedAt: a.At,
	}); err != nil {
		return err
	}
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
