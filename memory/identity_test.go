package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityResolverRemoteAndCommonDir(t *testing.T) {
	dir := t.TempDir()
	resolver := &GitIdentityResolver{Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--show-toplevel":
			return []byte("/repo/main\n"), nil
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--git-common-dir":
			return []byte("/repo/main/.git\n"), nil
		case len(args) >= 2 && args[0] == "remote" && args[1] == "get-url":
			return []byte("git@github.com:acme/repo.git\n"), nil
		}
		return nil, errors.New("unexpected args")
	}}
	if err := os.MkdirAll(filepath.Join(dir, "worktree"), 0o700); err != nil {
		t.Fatal(err)
	}
	ident, err := resolver.Resolve(context.Background(), filepath.Join(dir, "worktree"))
	if err != nil {
		t.Fatal(err)
	}
	if ident.ID != "github.com-acme-repo" || !ident.ViaGit {
		t.Fatalf("unexpected identity: %+v", ident)
	}
}

func TestIdentityResolverNoRemoteUsesCommonDir(t *testing.T) {
	dir := t.TempDir()
	resolver := &GitIdentityResolver{Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--show-toplevel":
			return []byte("/repo/worktree\n"), nil
		case len(args) >= 2 && args[0] == "rev-parse" && args[1] == "--git-common-dir":
			return []byte("/repo/main/.git\n"), nil
		case len(args) >= 2 && args[0] == "remote":
			return nil, errors.New("no remote")
		}
		return nil, errors.New("unexpected args")
	}}
	if err := os.MkdirAll(filepath.Join(dir, "worktree"), 0o700); err != nil {
		t.Fatal(err)
	}
	ident, err := resolver.Resolve(context.Background(), filepath.Join(dir, "worktree"))
	if err != nil {
		t.Fatal(err)
	}
	if ident.ID == "" || ident.GitCommonDir != "/repo/main/.git" {
		t.Fatalf("unexpected common-dir identity: %+v", ident)
	}
}

func TestNormalizeRemoteForms(t *testing.T) {
	want := "github.com/acme/repo"
	for _, in := range []string{
		"https://github.com/acme/repo.git",
		"git@github.com:acme/repo.git",
		"ssh://git@github.com:22/acme/repo",
	} {
		if got := normalizeRemote(in); got != want {
			t.Errorf("normalizeRemote(%q) = %q, want %q", in, got, want)
		}
	}
}
