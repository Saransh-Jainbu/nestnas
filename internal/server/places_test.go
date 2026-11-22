package server

import "testing"

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/media/alice/My\040Passport`); got != "/media/alice/My Passport" {
		t.Errorf("got %q", got)
	}
}

func TestRemovable(t *testing.T) {
	for p, want := range map[string]bool{"/media/a/USB": true, "/run/media/a/USB": true, "/mnt/data": true, "/home/a": false, "/": false, "/Volumes/X": true} {
		if isRemovablePath(p) != want {
			t.Errorf("isRemovablePath(%q) = %v, want %v", p, !want, want)
		}
	}
}

func TestFsNote(t *testing.T) {
	if fsNote("vfat") == "" || fsNote("ntfs") == "" || fsNote("exfat") == "" {
		t.Error("expected warnings for FAT/NTFS/exFAT")
	}
	if fsNote("ext4") != "" || fsNote("btrfs") != "" {
		t.Error("no warning expected for Linux filesystems")
	}
}

func TestMountForRoot(t *testing.T) {
	if fs, _ := mountFor("/"); fs == "" {
		t.Skip("no /proc/self/mounts")
	}
	fs, point := mountFor("/usr/bin")
	if fs == "" || point == "" {
		t.Errorf("mountFor(/usr/bin) = %q %q", fs, point)
	}
}

func TestListPlacesNeverNil(t *testing.T) {
	if listPlaces() == nil {
		t.Error("listPlaces must return an empty slice, not nil (JSON null crashes the UI)")
	}
}
