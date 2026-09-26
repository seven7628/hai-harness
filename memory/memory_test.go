package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixedResolver struct{ id string }

func (r fixedResolver) Resolve(context.Context, string) (Identity, error) {
	return Identity{ID: r.id, RepoRoot: "/repo", RemoteURL: "https://example.test/acme/repo.git", ViaGit: true}, nil
}

func newTestService(t *testing.T) *MemoryService {
	t.Helper()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	return NewService(Config{
		Root:     t.TempDir(),
		Resolver: fixedResolver{id: "acme-repo-abc123"},
		Now:      func() time.Time { return now },
	})
}

func TestServiceBootstrapIndexIsBoundedAndIdempotent(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	idx, err := s.BootstrapIndex()
	if err != nil {
		t.Fatal(err)
	}
	if idx.RepoID != "acme-repo-abc123" {
		t.Fatalf("unexpected bootstrap repo id: %+v", idx)
	}
	again, err := s.BootstrapIndex()
	if err != nil {
		t.Fatal(err)
	}
	if again.Bootstrap {
		t.Fatal("second bootstrap should read the existing index")
	}
	if len(again.Raw) > IndexMaxBytes || countLines(again.Raw) > IndexMaxLines {
		t.Fatalf("index exceeded bounds: %d bytes / %d lines", len(again.Raw), countLines(again.Raw))
	}
}

func TestServiceWriteTopicUpdatesIndexAndView(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BootstrapIndex(); err != nil {
		t.Fatal(err)
	}
	topic := &Topic{
		Name:    "Build Recovery",
		Title:   "Build recovery",
		Summary: "Use this when the project build fails",
		Tags:    []string{"build", "test"},
		Body:    "Run the project verification command first.",
	}
	if err := s.WriteTopic(topic); err != nil {
		t.Fatal(err)
	}
	view, err := s.View("build-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(view, "id: build-recovery") || !strings.Contains(view, "description: Use this when the project build fails") {
		t.Fatalf("topic frontmatter missing: %q", view)
	}
	idx, err := s.BootstrapIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 1 || idx.Entries[0].Name != "build-recovery" {
		t.Fatalf("index was not updated: %+v", idx.Entries)
	}
	if !strings.Contains(idx.Raw, "topics/build-recovery.md") {
		t.Fatalf("index does not link topic: %q", idx.Raw)
	}
}

func TestWriteTopicRejectsSecretsAndUnsafeNames(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BootstrapIndex(); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteTopic(&Topic{Name: "safe", Body: "token sk-ant-12345678901234567890"}); err == nil {
		t.Fatal("expected secret rejection")
	}
	if _, err := s.View("../outside"); err == nil {
		t.Fatal("expected unsafe ref rejection")
	}
}

func TestFileStorePermissionsAndAtomicFiles(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	store := s.Bound().Store.(*FileStore)
	if err := store.WriteTopic(&Topic{Name: "one", Title: "One", Body: "body"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(store.Dir()); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != dirPerm {
		t.Fatalf("store dir mode = %o, want %o", fi.Mode().Perm(), dirPerm)
	}
	if fi, err := os.Stat(filepath.Join(store.TopicsDir(), "one.md")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != filePerm {
		t.Fatalf("topic file mode = %o, want %o", fi.Mode().Perm(), filePerm)
	}
	entries, err := os.ReadDir(store.TopicsDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestIndexLimitTruncatesReadButDoesNotRewrite(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileStore(root, "repo-test-12345678")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.TopicsDir(), dirPerm); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < IndexMaxLines+20; i++ {
		b.WriteString("- [topic")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString("](topics/topic")
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(".md) — summary\n")
	}
	if err := os.WriteFile(store.IndexPath(), []byte(b.String()), filePerm); err != nil {
		t.Fatal(err)
	}
	idx, err := store.ReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if !idx.Truncated {
		t.Fatal("expected truncated index")
	}
	if countLines(idx.Raw) > IndexMaxLines {
		t.Fatalf("read index lines = %d", countLines(idx.Raw))
	}
}

func TestMissingTopicReturnsNotExist(t *testing.T) {
	s := newTestService(t)
	if _, err := s.Open(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.View("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}
