package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// A "place" is a drive or mounted volume the owner can pick as the NAS storage location.
type place struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	FS        string `json:"fs"`
	Free      uint64 `json:"free"`
	Total     uint64 `json:"total"`
	Removable bool   `json:"removable"`
}

type mount struct{ point, fs string }

// realFS: filesystems that hold ordinary user data (skips tmpfs, squashfs, proc, loop snaps...).
var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "btrfs": true, "xfs": true, "f2fs": true,
	"ntfs": true, "ntfs3": true, "fuseblk": true, "exfat": true, "vfat": true, "msdos": true, "hfsplus": true, "zfs": true}

func unescapeMount(s string) string { // /proc/mounts encodes spaces etc. as \040
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return r.Replace(s)
}

func readMounts() []mount {
	b, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return nil
	}
	var out []mount
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 {
			out = append(out, mount{unescapeMount(f[1]), f[2]})
		}
	}
	return out
}

func isRemovablePath(p string) bool {
	return strings.HasPrefix(p, "/media/") || strings.HasPrefix(p, "/run/media/") ||
		strings.HasPrefix(p, "/mnt/") || strings.HasPrefix(p, "/Volumes/") || (len(p) == 3 && p[1] == ':' && p[0] > 'C')
}

// listPlaces returns the drives worth offering in the folder picker (internal data
// partitions and external/removable drives), excluding the system root itself.
func listPlaces() []place {
	out := []place{} // non-nil so JSON is [] not null
	add := func(name, p, fs string) {
		free, _ := diskFree(p)
		total, _ := diskTotal(p)
		out = append(out, place{Name: name, Path: p, FS: fs, Free: free, Total: total, Removable: isRemovablePath(p)})
	}
	switch runtime.GOOS {
	case "linux":
		for _, m := range readMounts() {
			p := m.point
			skip := p == "/" || !realFS[m.fs] || strings.HasPrefix(p, "/boot") || strings.HasPrefix(p, "/snap") ||
				strings.HasPrefix(p, "/var/lib") || strings.HasPrefix(p, "/sys") || strings.HasPrefix(p, "/proc")
			if !skip {
				add(filepath.Base(p), p, m.fs)
			}
		}
	case "darwin":
		des, _ := os.ReadDir("/Volumes")
		for _, d := range des {
			p := filepath.Join("/Volumes", d.Name())
			if target, err := os.Readlink(p); err == nil && target == "/" {
				continue // the startup disk
			}
			add(d.Name(), p, "")
		}
	case "windows":
		for c := 'D'; c <= 'Z'; c++ {
			p := string(c) + `:\`
			if _, err := os.Stat(p); err == nil {
				add(string(c)+": drive", p, "")
			}
		}
	}
	return out
}

// mountFor finds the filesystem that holds path (longest matching mount point). Linux only.
func mountFor(path string) (fs, point string) {
	best := -1
	for _, m := range readMounts() {
		p := strings.TrimRight(m.point, "/")
		if path == m.point || path == p || strings.HasPrefix(path, p+"/") || m.point == "/" {
			if len(m.point) > best {
				best, fs, point = len(m.point), m.fs, m.point
			}
		}
	}
	return
}

// fsNote explains any practical limits of the filesystem the user picked.
func fsNote(fs string) string {
	switch fs {
	case "vfat", "msdos":
		return "This drive is FAT32: it can't store any single file bigger than 4 GB. Reformatting it as exFAT or ext4 works better."
	case "ntfs", "ntfs3", "fuseblk", "exfat":
		return "Windows-style drive (NTFS/exFAT): works, but Linux file permissions aren't kept."
	}
	return ""
}
