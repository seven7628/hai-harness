package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestConcurrentServicesDoNotLoseIndexEntries(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	newSvc := func() *MemoryService {
		s := NewService(Config{Root: root, Resolver: fixedResolver{id: "concurrent-repo-123456"}, Now: func() time.Time { return now }})
		if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
			t.Fatal(err)
		}
		return s
	}
	a, b := newSvc(), newSvc()
	if _, err := a.BootstrapIndex(); err != nil {
		t.Fatal(err)
	}
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			errs <- a.WriteTopic(&Topic{Name: "a-" + string(rune('a'+i)), Body: "a"})
		}(i)
		go func(i int) {
			defer wg.Done()
			errs <- b.WriteTopic(&Topic{Name: "b-" + string(rune('a'+i)), Body: "b"})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	store := a.Bound().Store.(*FileStore)
	idx, err := store.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 2*n {
		raw, _ := os.ReadFile(store.IndexPath())
		t.Fatalf("index entries = %d, want %d; raw=%q", len(idx.Entries), 2*n, raw)
	}
}

func TestBootstrapIndexIsIdempotentAndPreservesContext(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BootstrapIndex(); err != nil {
		t.Fatal(err)
	}
	store := s.Bound().Store.(*FileStore)
	idx, err := store.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	idx.Context = "USER HAND WRITTEN CONTEXT"
	if err := store.WriteIndex(idx); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(store.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got, err := s.BootstrapIndex()
		if err != nil {
			t.Fatal(err)
		}
		if got.Context != "USER HAND WRITTEN CONTEXT" {
			t.Fatalf("context lost: %q", got.Context)
		}
	}
	after, err := os.Stat(store.IndexPath())
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("idempotent bootstrap rewrote index")
	}
	if err := s.WriteTopic(&Topic{Name: "keep-context", Body: "body"}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if got.Context != "USER HAND WRITTEN CONTEXT" {
		t.Fatalf("context lost after topic write: %q", got.Context)
	}
}

func TestPathLikeTopicNameIsRejectedNotNormalized(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.View("../outside"); err == nil {
		t.Fatal("path-like ref must be rejected")
	}
	if err := s.WriteTopic(&Topic{Name: "../../etc/evil", Body: "body"}); err == nil {
		t.Fatal("path-like topic name must be rejected")
	}
}

func TestWorktreeIdentityUsesCommonDir(t *testing.T) {
	resolver := &GitIdentityResolver{Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--show-toplevel":
			return []byte("/repo/main\n"), nil
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--git-common-dir":
			return []byte("/repo/main/.git\n"), nil
		case len(args) >= 2 && args[0] == "remote" && args[1] == "get-url":
			return nil, errors.New("no remote")
		}
		return nil, errors.New("unexpected git args")
	}}
	dir := t.TempDir()
	// Resolve requires the workdir to exist; the fake git output is independent.
	if err := os.MkdirAll(filepath.Join(dir, "worktree"), 0o700); err != nil {
		t.Fatal(err)
	}
	ident, err := resolver.Resolve(context.Background(), filepath.Join(dir, "worktree"))
	if err != nil {
		t.Fatal(err)
	}
	if !ident.ViaGit || ident.ID == "" {
		t.Fatalf("unexpected identity: %+v", ident)
	}
}
