package agenthub

// P3a-06：handoff / ack / 护栏。

import (
	"errors"
	"testing"
	"time"
)

func handoffGroup() *Group {
	return &Group{
		ID: "grp_aaaaaa", Name: "交接群", OwnerAgentID: "ag_aaaaaaaa",
		Members: []Member{
			{AgentID: "ag_aaaaaaaa", Role: RoleOwner},
			{AgentID: "ag_bbbbbbbb", Role: RoleMember},
			{AgentID: "ag_cccccccc", Role: RoleMember},
			{AgentID: "ag_dddddddd", Role: RoleObserver},
		},
		Policy: GroupPolicy{MaxHandoffDepth: 3},
	}
}

func TestHandoffDepthAndObserver(t *testing.T) {
	g := handoffGroup()
	now := time.Now().UTC()
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", 2, nil, now); err != nil {
		t.Fatalf("depth 2 should pass: %v", err)
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", 3, nil, now); !errors.Is(err, ErrHandoffTooDeep) {
		t.Fatalf("depth 3 err = %v", err)
	}
	if err := CheckHandoff(g, "ag_dddddddd", "ag_bbbbbbbb", 0, nil, now); !errors.Is(err, ErrHandoffObserver) {
		t.Fatalf("observer sender err = %v", err)
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_dddddddd", 0, nil, now); !errors.Is(err, ErrHandoffObserver) {
		t.Fatalf("observer target err = %v", err)
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_aaaaaaaa", 0, nil, now); err == nil {
		t.Fatal("self handoff must fail")
	}
}

func TestHandoffPairRateLimit(t *testing.T) {
	g := handoffGroup()
	now := time.Now().UTC()
	history := []Handoff{
		{GroupID: g.ID, FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now.Add(-5 * time.Minute)},
		{GroupID: g.ID, FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now.Add(-1 * time.Minute)},
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", 0, history, now); !errors.Is(err, ErrHandoffRateLimited) {
		t.Fatalf("third in window err = %v", err)
	}
	// 不同对不受影响。
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_cccccccc", 0, history, now); err != nil {
		t.Fatalf("other pair: %v", err)
	}
	// 窗口外旧记录不计数。
	old := []Handoff{
		{GroupID: g.ID, FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now.Add(-11 * time.Minute)},
		{GroupID: g.ID, FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now.Add(-12 * time.Minute)},
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", 0, old, now); err != nil {
		t.Fatalf("old history: %v", err)
	}
}

// P3a-06 B1：角色从 g.Members 解析；非成员/跨群/软删群一律 fail-closed。
func TestHandoffMembershipFailClosed(t *testing.T) {
	g := handoffGroup()
	now := time.Now().UTC()
	if err := CheckHandoff(g, "ag_zzzzzzzz", "ag_bbbbbbbb", 0, nil, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-member from err = %v", err)
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_zzzzzzzz", 0, nil, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("non-member to err = %v", err)
	}
	gone := *g
	gone.Deleted = true
	if err := CheckHandoff(&gone, "ag_aaaaaaaa", "ag_bbbbbbbb", 0, nil, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted group err = %v", err)
	}
	if err := CheckHandoff(nil, "ag_aaaaaaaa", "ag_bbbbbbbb", 0, nil, now); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil group err = %v", err)
	}
	// 限流按无序对计数：反向踢皮球也受限；空 GroupID 的历史不污染本群。
	history := []Handoff{
		{GroupID: g.ID, FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now.Add(-time.Minute)},
		{GroupID: g.ID, FromAgentID: "ag_bbbbbbbb", ToAgentID: "ag_aaaaaaaa", CreatedAt: now.Add(-time.Minute)},
	}
	if err := CheckHandoff(g, "ag_bbbbbbbb", "ag_aaaaaaaa", 0, history, now); !errors.Is(err, ErrHandoffRateLimited) {
		t.Fatalf("reverse pair err = %v", err)
	}
	foreign := []Handoff{
		{GroupID: "", FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now},
		{GroupID: "grp_other", FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", CreatedAt: now},
	}
	if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", 0, foreign, now); err != nil {
		t.Fatalf("foreign history leaked: %v", err)
	}
}

func TestNextHandoffDepth(t *testing.T) {
	history := []Handoff{
		{FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_bbbbbbbb", ChainDepth: 0},
		{FromAgentID: "ag_bbbbbbbb", ToAgentID: "ag_cccccccc", ChainDepth: 1},
	}
	if got := NextHandoffDepth(history, "ag_cccccccc"); got != 2 {
		t.Fatalf("depth = %d, want 2", got)
	}
	// declined 不延长链。
	history = append(history, Handoff{FromAgentID: "ag_aaaaaaaa", ToAgentID: "ag_dddddddd", ChainDepth: 2, Status: HandoffDeclined})
	if got := NextHandoffDepth(history, "ag_dddddddd"); got != 0 {
		t.Fatalf("declined depth = %d, want 0", got)
	}
}

func TestHandoffAckIdempotent(t *testing.T) {
	now := time.Now().UTC()
	h := &Handoff{Status: HandoffPendingAck}
	if err := h.Ack(true, now); err != nil || h.Status != HandoffAccepted || h.AckedAt == nil {
		t.Fatalf("ack = %+v, %v", h, err)
	}
	// 同状态重复 ack 幂等。
	if err := h.Ack(true, now.Add(time.Minute)); err != nil {
		t.Fatalf("repeat ack: %v", err)
	}
	// 已接受不能再拒绝。
	if err := h.Ack(false, now); !errors.Is(err, ErrHandoffNotPending) {
		t.Fatalf("flip err = %v", err)
	}
	// 零值状态按 pending_ack 处理（反序列化遗漏字段也能 ack）。
	zero := &Handoff{}
	if err := zero.Ack(false, now); err != nil || zero.Status != HandoffDeclined {
		t.Fatalf("zero status ack = %+v, %v", zero, err)
	}
	var nilH *Handoff
	if err := nilH.Ack(true, now); err == nil {
		t.Fatal("nil handoff must fail")
	}
}
