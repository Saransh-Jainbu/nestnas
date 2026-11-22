package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveJail(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Skip("symlinks unavailable")
	}
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)

	ok := []string{"", "/", "a", "a/b", "a/b/new.txt", "../../a", "a/../a/b"}
	for _, p := range ok {
		got, err := Resolve(root, p)
		if err != nil {
			t.Errorf("Resolve(%q) unexpected error %v", p, err)
			continue
		}
		if got != root && !hasPrefix(got, root) {
			t.Errorf("Resolve(%q)=%q outside root", p, got)
		}
	}
	for _, p := range []string{"evil", "evil/secret", "a/../evil/x"} {
		if _, err := Resolve(root, p); err == nil {
			t.Errorf("Resolve(%q) should have been rejected", p)
		}
	}
}

func hasPrefix(p, root string) bool {
	return len(p) >= len(root) && p[:len(root)] == root
}

func TestValidName(t *testing.T) {
	for _, n := range []string{"", ".", "..", "a/b", "a\\b"} {
		if ValidName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
	if !ValidName("photo 1.jpg") {
		t.Error("normal name rejected")
	}
}
