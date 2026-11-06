package agenthub

// P3a-07：产物版本 + 可见性。

import (
	"context"
	"os"
	"strings"
	"sync"
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

// P3a-07 复审 B1-b：版本号对应不可变内容副本，源文件覆盖后 v1 仍读旧内容。
func TestRegisterArtifactSnapshotsContent(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := t.TempDir() + "/report.md"
	if err := os.WriteFile(src, []byte("CONTENT-V1"), 0o600); err != nil {
		t.Fatal(err)
	}
	v1 := &Artifact{OwnerAgentID: "ag_aaaaaaaa", Name: "report.md", Path: "report.md", SourcePath: src}
	if err := RegisterArtifact(ctx, st, v1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("CONTENT-V2"), 0o600); err != nil {
		t.Fatal(err)
	}
	v2 := &Artifact{OwnerAgentID: "ag_aaaaaaaa", Name: "report.md", Path: "report.md", SourcePath: src}
	if err := RegisterArtifact(ctx, st, v2); err != nil {
		t.Fatal(err)
	}
	got1, err := st.LoadArtifact(ctx, v1.ID)
	if err != nil || got1.Version != 1 {
		t.Fatalf("v1 = %+v, %v", got1, err)
	}
	got2, _ := st.LoadArtifact(ctx, v2.ID)
	if got2.Version != 2 {
		t.Fatalf("v2 = %+v", got2)
	}
	b1, err := os.ReadFile(got1.SourcePath)
	if err != nil || string(b1) != "CONTENT-V1" {
		t.Fatalf("v1 content = %q, %v", b1, err)
	}
	b2, _ := os.ReadFile(got2.SourcePath)
	if string(b2) != "CONTENT-V2" {
		t.Fatalf("v2 content = %q", b2)
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

// P3a-07 复审 B1：登记入口必须在并发下产生唯一版本 1..N，而不是全部 v1。
func TestRegisterArtifactConcurrentVersions(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a := &Artifact{OwnerAgentID: "ag_aaaaaaaa", Name: "report.md", Path: "out/report.md"}
			if err := RegisterArtifact(ctx, st, a); err != nil {
				t.Errorf("register: %v", err)
			}
		}()
	}
	wg.Wait()
	list, err := st.ListArtifacts(ctx)
	if err != nil || len(list) != n {
		t.Fatalf("list = %d, %v", len(list), err)
	}
	seen := map[int]bool{}
	for _, a := range list {
		if a.Version < 1 || a.Version > n || seen[a.Version] {
			t.Fatalf("duplicate/out-of-range version: %+v", a)
		}
		seen[a.Version] = true
	}
	for v := 1; v <= n; v++ {
		if !seen[v] {
			t.Fatalf("missing version %d: %v", v, seen)
		}
	}
}

// P3a-07 三轮非阻塞：复制失败（源不存在）不能留下元数据。
func TestRegisterArtifactMissingSourceNoMetadata(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &Artifact{OwnerAgentID: "ag_aaaaaaaa", Name: "missing.md", Path: "missing.md", SourcePath: t.TempDir() + "/nope.md"}
	if err := RegisterArtifact(ctx, st, a); err == nil {
		t.Fatal("missing source must fail")
	}
	if list, _ := st.ListArtifacts(ctx); len(list) != 0 {
		t.Fatalf("metadata left behind: %+v", list)
	}
}
