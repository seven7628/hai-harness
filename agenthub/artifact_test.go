package agenthub

// P3a-07：产物版本 + 可见性。

import (
	"context"
	"strings"
	"testing"
)

func TestArtifactVersionPerOwnerName(t *testing.T) {
	list := []*Artifact{
		{ID: "art_aaaaaa", OwnerAgentID: "ag_aaaaaaaa", Name: "report.md", Version: 1},
		{ID: "art_bbbbbb", OwnerAgentID: "ag_aaaaaaaa", Name: "REPORT.md", Version: 3},
		{ID: "art_cccccc", OwnerAgentID: "ag_bbbbbbbb", Name: "report.md", Version: 7},
	}
	if v := NextArtifactVersion(list, "ag_aaaaaaaa", "report.md"); v != 4 {
		t.Fatalf("same owner version = %d, want 4", v)
	}
	if v := NextArtifactVersion(list, "ag_cccccccc", "report.md"); v != 1 {
		t.Fatalf("new owner version = %d, want 1", v)
	}
	a := &Artifact{OwnerAgentID: "ag_aaaaaaaa", Name: "report.md", Path: "out/report.md"}
	if err := NewArtifactVersion(list, a); err != nil {
		t.Fatal(err)
	}
	if a.Version != 4 || a.Visibility != VisibilityGroup || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
		t.Fatalf("artifact = %+v", a)
	}
}

func TestArtifactPathSafety(t *testing.T) {
	for _, bad := range []string{"", "  ", "/etc/passwd", "../secret", "a/../../b"} {
		if err := ValidateArtifactPath(bad); err == nil {
			t.Fatalf("path %q must be rejected", bad)
		}
	}
	for _, ok := range []string{"report.md", "out/dir/report.md", "a/../b.txt"} {
		if err := ValidateArtifactPath(ok); err != nil {
			t.Fatalf("path %q: %v", ok, err)
		}
	}
}

func TestArtifactVisibility(t *testing.T) {
	ownerOnly := &Artifact{OwnerAgentID: "ag_aaaaaaaa", Visibility: VisibilityOwner}
	if !ArtifactVisibleTo(ownerOnly, "", true) {
		t.Fatal("human should see owner-only")
	}
	if ArtifactVisibleTo(ownerOnly, "ag_bbbbbbbb", false) {
		t.Fatal("other agent must not see owner-only")
	}
	group := &Artifact{OwnerAgentID: "ag_aaaaaaaa"}
	if !ArtifactVisibleTo(group, "ag_bbbbbbbb", false) {
		t.Fatal("group artifact should be visible")
	}
}

func TestArtifactStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Artifact{ID: "art_aaaaaa", OwnerAgentID: "ag_aaaaaaaa", Name: "report.md", Path: "out/report.md"}
	if err := NewArtifactVersion(nil, a); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveArtifact(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadArtifact(ctx, a.ID)
	if err != nil || got.Version != 1 || got.Name != "report.md" {
		t.Fatalf("load = %+v, %v", got, err)
	}
	list, err := st.ListArtifacts(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	// 预览按 id 取（不是路径）。
	if !strings.HasSuffix(string(list[0].ID), "aaaaaa") {
		t.Fatalf("id = %s", list[0].ID)
	}
}
