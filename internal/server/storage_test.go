package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
)

// newTestServer sets up a configured server whose storage lives at <mount>/NestNAS.
func newTestServer(t *testing.T, mount string) *Server {
	t.Helper()
	storage := filepath.Join(mount, "NestNAS")
	if err := os.MkdirAll(filepath.Join(storage, "users"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Setup("admin", "password123", storage, config.GB); err != nil {
		t.Fatal(err)
	}
	s := New(st, "test", 0)
	placesFn = func() []place { return []place{{Path: mount}} }
	t.Cleanup(func() { placesFn = listPlaces })
	s.initStorageIdentity(storage)
	return s
}

func recheck(s *Server) bool {
	s.okAt = s.okAt.AddDate(-1, 0, 0) // expire the marker cache
	s.relocAt = s.relocAt.AddDate(-1, 0, 0)
	return s.storageOK()
}

func TestUnplugAndReplugSamePath(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "DRIVE")
	s := newTestServer(t, mount)
	if !recheck(s) {
		t.Fatal("fresh storage should be OK")
	}
	if s.st.StorageSub() != "NestNAS" || s.st.StorageID() == "" {
		t.Fatalf("identity not recorded: sub=%q id=%q", s.st.StorageSub(), s.st.StorageID())
	}
	away := filepath.Join(root, "unplugged")
	os.Rename(mount, away) // unplug
	if recheck(s) {
		t.Fatal("storage should be unavailable while unplugged")
	}
	if _, err := os.Stat(mount); err == nil {
		t.Fatal("nothing may be recreated at the old mount point")
	}
	os.Rename(away, mount) // plug back in
	if !recheck(s) {
		t.Fatal("should resume automatically after re-plugging")
	}
}

func TestReplugAtDifferentPath(t *testing.T) {
	root := t.TempDir()
	mountA := filepath.Join(root, "DRIVE")
	s := newTestServer(t, mountA)
	mountB := filepath.Join(root, "DRIVE1") // desktop mounted it somewhere else this time
	os.Rename(mountA, mountB)
	placesFn = func() []place { return []place{{Path: mountB}} }
	if !recheck(s) {
		t.Fatal("should find the drive at its new location")
	}
	if got, want := s.st.StoragePath(), filepath.Join(mountB, "NestNAS"); got != want {
		t.Fatalf("storage path = %q, want %q", got, want)
	}
}

func TestWrongDriveRejected(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "DRIVE")
	s := newTestServer(t, mount)
	os.Rename(mount, filepath.Join(root, "real"))
	// A different drive that happens to land at the same path (even with a "users" folder).
	os.MkdirAll(filepath.Join(mount, "NestNAS", "users"), 0o755)
	if recheck(s) {
		t.Fatal("a drive without our identity marker must be rejected")
	}
	os.WriteFile(filepath.Join(mount, "NestNAS", markerName), []byte("someone-elses-id\n"), 0o644)
	if recheck(s) {
		t.Fatal("a drive with a different marker must be rejected")
	}
}

func TestLegacyStorageAdopted(t *testing.T) {
	root := t.TempDir()
	s := newTestServer(t, filepath.Join(root, "D"))
	s.st.SetStorageIdentity("", "") // simulate a config from before identities existed
	os.Remove(filepath.Join(s.st.StoragePath(), markerName))
	if !recheck(s) || s.st.StorageID() == "" {
		t.Fatal("existing storage should be adopted and stamped")
	}
}
