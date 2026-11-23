package server

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/auth"
	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/fsx"
)

// Deleted files are moved into <user folder>/.trash/<id>/ instead of being erased,
// so a mistaken click is never fatal. Items are purged automatically after trashDays.
const (
	trashName = ".trash"
	trashDays = 30
)

var trashIDRe = regexp.MustCompile(`^[0-9]+-[0-9a-f]+$`)

type trashMeta struct {
	Orig    string `json:"orig"` // original path relative to the user's folder
	Deleted int64  `json:"deleted"`
	Dir     bool   `json:"dir"`
}

type trashItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Orig    string `json:"orig"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	Deleted int64  `json:"deleted"`
}

// inTrash reports whether a user-supplied path points into one of NestNAS's own hidden folders
// (trash, file versions, snapshots). Those are only reachable through their own API endpoints.
// The comparison ignores case because external drives (exFAT, NTFS) are case-insensitive.
func inTrash(rel string) bool {
	first, _, _ := strings.Cut(strings.TrimPrefix(path.Clean("/"+rel), "/"), "/")
	return internalName(first)
}

func moveToTrash(root, abs, rel string) error {
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%d-%s", time.Now().UnixNano(), auth.Token(3))
	dir := filepath.Join(root, trashName, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	meta, _ := json.Marshal(trashMeta{Orig: path.Clean("/" + rel), Deleted: time.Now().Unix(), Dir: fi.IsDir()})
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o644); err != nil {
		os.RemoveAll(dir)
		return err
	}
	if err := os.Rename(abs, filepath.Join(dir, "item")); err != nil {
		os.RemoveAll(dir)
		return err
	}
	return nil
}

func readMeta(dir string) (trashMeta, bool) {
	var m trashMeta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil || json.Unmarshal(b, &m) != nil {
		return m, false
	}
	return m, true
}

func (s *Server) trashList(w http.ResponseWriter, r *http.Request, u config.User) {
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 500, "storage error")
		return
	}
	items := []trashItem{}
	des, _ := os.ReadDir(filepath.Join(root, trashName))
	cutoff := time.Now().Add(-trashDays * 24 * time.Hour).Unix()
	purged := false
	for _, d := range des {
		if !trashIDRe.MatchString(d.Name()) {
			continue
		}
		dir := filepath.Join(root, trashName, d.Name())
		m, ok := readMeta(dir)
		if !ok {
			continue
		}
		if m.Deleted < cutoff {
			os.RemoveAll(dir)
			purged = true
			continue
		}
		it := trashItem{ID: d.Name(), Name: path.Base(m.Orig), Orig: m.Orig, Dir: m.Dir, Deleted: m.Deleted}
		if m.Dir {
			it.Size = fsx.DirSize(filepath.Join(dir, "item"))
		} else if fi, err := os.Lstat(filepath.Join(dir, "item")); err == nil {
			it.Size = s.plainSize(filepath.Join(dir, "item"), fi)
		}
		items = append(items, it)
	}
	if purged {
		s.invalidate(u.Name)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Deleted > items[j].Deleted })
	jsonOut(w, 200, map[string]interface{}{"items": items, "days": trashDays})
}

func (s *Server) trashRestore(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ ID string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if !trashIDRe.MatchString(d.ID) {
		jerr(w, 400, "bad id")
		return
	}
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 500, "storage error")
		return
	}
	dir := filepath.Join(root, trashName, d.ID)
	m, ok := readMeta(dir)
	if !ok {
		jerr(w, 404, "not found in trash")
		return
	}
	dest, err := fsx.Resolve(root, m.Orig)
	if err != nil || inTrash(m.Orig) {
		jerr(w, 400, "can't restore there")
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		jerr(w, 500, "can't recreate the folder")
		return
	}
	dest = freeName(dest)
	if err := os.Rename(filepath.Join(dir, "item"), dest); err != nil {
		jerr(w, 500, "restore failed")
		return
	}
	os.RemoveAll(dir)
	rel, _ := filepath.Rel(root, dest)
	jsonOut(w, 200, map[string]string{"restored": "/" + filepath.ToSlash(rel)})
}

// freeName returns p, or "name (restored).ext" / "(restored 2)" if p is taken.
func freeName(p string) string {
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		tag := " (restored)"
		if i > 1 {
			tag = fmt.Sprintf(" (restored %d)", i)
		}
		if c := base + tag + ext; func() bool { _, err := os.Lstat(c); return os.IsNotExist(err) }() {
			return c
		}
	}
}

func (s *Server) trashDelete(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ ID string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if !trashIDRe.MatchString(d.ID) {
		jerr(w, 400, "bad id")
		return
	}
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 500, "storage error")
		return
	}
	if err := os.RemoveAll(filepath.Join(root, trashName, d.ID)); err != nil {
		jerr(w, 500, "delete failed")
		return
	}
	s.invalidate(u.Name)
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) trashEmpty(w http.ResponseWriter, r *http.Request, u config.User) {
	if r.Method != http.MethodPost {
		jerr(w, 405, "POST only")
		return
	}
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 500, "storage error")
		return
	}
	if err := os.RemoveAll(filepath.Join(root, trashName)); err != nil {
		jerr(w, 500, "couldn't empty trash")
		return
	}
	s.invalidate(u.Name)
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// zipFolder streams a folder as a .zip without building it in memory or on disk.
func (s *Server) zipFolder(w http.ResponseWriter, r *http.Request, u config.User) {
	root, abs, err := s.resolve(u, r.URL.Query().Get("path"))
	if err != nil {
		jerr(w, 400, "bad path")
		return
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		jerr(w, 404, "folder not found")
		return
	}
	name := filepath.Base(abs)
	if abs == root {
		name = u.Name + "-files"
	}
	name = strings.NewReplacer(`"`, "", "\r", "", "\n", "", "\\", "").Replace(name)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.zip"`)
	zw := zip.NewWriter(w)
	defer zw.Close()
	_ = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil || p == abs {
			return nil
		}
		if d.IsDir() && filepath.Dir(p) == root && internalName(d.Name()) {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 || strings.HasSuffix(d.Name(), ".part") && strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(abs, p)
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		h.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			h.Name += "/"
			_, err = zw.CreateHeader(h)
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		h.Method = zip.Deflate
		rs, _, closeFile, err := s.openContent(p)
		if err != nil {
			return nil
		}
		defer closeFile()
		h.UncompressedSize64 = uint64(s.plainSize(p, info))
		fw, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = io.Copy(fw, rs)
		return err
	})
}
