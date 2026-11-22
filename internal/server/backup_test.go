package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMkdirChainStaysInsideAndNeverRecreatesRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "users", "a")
	os.MkdirAll(root, 0o755)
	if err := mkdirChain(root, filepath.Join(root, "Phone Backup", "iPhone", "2024", "05")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(root, "Phone Backup", "iPhone", "2024", "05")); err != nil || !fi.IsDir() {
		t.Fatal("folders not created")
	}
	if err := mkdirChain(root, filepath.Join(root, "Phone Backup")); err != nil { // already there: fine
		t.Fatal(err)
	}
	if mkdirChain(root, filepath.Join(base, "elsewhere")) == nil {
		t.Error("a folder outside the user's root must be refused")
	}
	os.WriteFile(filepath.Join(root, "afile"), []byte("x"), 0o644)
	if mkdirChain(root, filepath.Join(root, "afile", "sub")) == nil {
		t.Error("must not create a folder inside a file")
	}
	// drive "unplugged": the root is gone. It must fail, not re-create users/a on the internal disk.
	gone := filepath.Join(base, "unplugged", "users", "a")
	if mkdirChain(gone, filepath.Join(gone, "Phone Backup")) == nil {
		t.Error("must fail when the user's root does not exist")
	}
	if _, err := os.Stat(filepath.Join(base, "unplugged")); err == nil {
		t.Error("the missing drive's folders were re-created on the internal disk")
	}
}

func TestUniqueNameKeepsBothFiles(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "IMG_0001.JPG")
	os.WriteFile(p, []byte("a"), 0o644)
	if got := uniqueName(p); got != filepath.Join(d, "IMG_0001 (2).JPG") {
		t.Errorf("got %s", got)
	}
	os.WriteFile(filepath.Join(d, "IMG_0001 (2).JPG"), []byte("b"), 0o644)
	if got := uniqueName(p); got != filepath.Join(d, "IMG_0001 (3).JPG") {
		t.Errorf("got %s", got)
	}
}
