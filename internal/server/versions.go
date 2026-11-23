package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/auth"
	"github.com/Saransh-Jainbu/nestnas/internal/config"
)

// File versions: when a file is replaced (an upload or a network-drive save over an existing
// name), the old copy is kept in <user folder>/.versions/<hash of the path>/<id>/ instead of
// being thrown away. The newest versionsKeep versions, no older than versionsDays, are kept.
// Versions count toward the owner's allowance, like the trash.
const (
	versionsName = ".versions"
	versionsKeep = 10
	versionsDays = 90
)

func internalName(n string) bool {
	return strings.EqualFold(n, trashName) || strings.EqualFold(n, versionsName) || strings.EqualFold(n, snapshotsName)
}

type verMeta struct {
	Orig  string `json:"orig"`
	Saved int64  `json:"saved"`
}

type verItem struct {
	ID    string `json:"id"`
	Saved int64  `json:"saved"`
	Size  int64  `json:"size"`
}

func relOf(root, abs string) string {
	r, err := filepath.Rel(root, abs)
	if err != nil {
		return ""
	}
	return "/" + filepath.ToSlash(r)
}

func versionDir(root, rel string) string {
	h := sha256.Sum256([]byte(path.Clean("/" + rel)))
	return filepath.Join(root, versionsName, hex.EncodeToString(h[:8]))
}

// saveVersion moves the existing regular file at abs into the version store, right before it
// is replaced. It returns a function that puts the file back if the replacement then fails.
func (s *Server) saveVersion(root, abs string) (undo func()) {
	undo = func() {}
	fi, err := os.Lstat(abs)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	rel := relOf(root, abs)
	id := newID()
	vd := filepath.Join(versionDir(root, rel), id)
	if err := os.MkdirAll(vd, 0o755); err != nil {
		return
	}
	meta, _ := json.Marshal(verMeta{Orig: rel, Saved: time.Now().Unix()})
	if err := os.WriteFile(filepath.Join(vd, "meta.json"), meta, 0o644); err != nil {
		os.RemoveAll(vd)
		return
	}
	if err := os.Rename(abs, filepath.Join(vd, "item")); err != nil {
		os.RemoveAll(vd)
		return
	}
	undo = func() {
		if os.Rename(filepath.Join(vd, "item"), abs) == nil {
			os.RemoveAll(vd)
		}
	}
	s.pruneVersions(versionDir(root, rel))
	return
}

func newID() string { return fmt.Sprintf("%d-%s", time.Now().UnixNano(), auth.Token(3)) }

func readVerMeta(dir string) (verMeta, bool) {
	var m verMeta
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil || json.Unmarshal(b, &m) != nil {
		return m, false
	}
	return m, true
}

// versionList returns the versions kept for one path, newest first.
func (s *Server) versionList(root, rel string) []verItem {
	dir := versionDir(root, rel)
	des, _ := os.ReadDir(dir)
	items := []verItem{}
	for _, d := range des {
		if !trashIDRe.MatchString(d.Name()) && !snapIDRe.MatchString(d.Name()) {
			continue
		}
		vd := filepath.Join(dir, d.Name())
		m, ok := readVerMeta(vd)
		if !ok {
			continue
		}
		it := verItem{ID: d.Name(), Saved: m.Saved}
		if fi, err := os.Lstat(filepath.Join(vd, "item")); err == nil {
			it.Size = s.plainSize(filepath.Join(vd, "item"), fi)
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID > items[j].ID })
	return items
}

// pruneVersions drops versions that are too old or beyond the newest versionsKeep.
func (s *Server) pruneVersions(dir string) {
	des, _ := os.ReadDir(dir)
	var ids []string
	for _, d := range des {
		ids = append(ids, d.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	cutoff := time.Now().Add(-versionsDays * 24 * time.Hour).Unix()
	for i, id := range ids {
		vd := filepath.Join(dir, id)
		m, ok := readVerMeta(vd)
		if i >= versionsKeep || !ok || m.Saved < cutoff {
			os.RemoveAll(vd)
		}
	}
	if left, _ := os.ReadDir(dir); len(left) == 0 {
		os.Remove(dir)
	}
}

func (s *Server) versionsAPI(w http.ResponseWriter, r *http.Request, u config.User) {
	root, abs, err := s.resolve(u, r.URL.Query().Get("path"))
	if err != nil || abs == root {
		jerr(w, 400, "bad path")
		return
	}
	jsonOut(w, 200, map[string]interface{}{"items": s.versionList(root, relOf(root, abs)), "keep": versionsKeep, "days": versionsDays})
}

func (s *Server) versionDownload(w http.ResponseWriter, r *http.Request, u config.User) {
	root, abs, err := s.resolve(u, r.URL.Query().Get("path"))
	id := r.URL.Query().Get("id")
	if err != nil || abs == root || !trashIDRe.MatchString(id) && !snapIDRe.MatchString(id) {
		jerr(w, 400, "bad request")
		return
	}
	s.serveAttachmentAs(w, r, filepath.Join(versionDir(root, relOf(root, abs)), id, "item"), filepath.Base(abs))
}

func (s *Server) versionDelete(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Path, ID string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	root, abs, err := s.resolve(u, d.Path)
	if err != nil || abs == root || !trashIDRe.MatchString(d.ID) && !snapIDRe.MatchString(d.ID) {
		jerr(w, 400, "bad request")
		return
	}
	dir := versionDir(root, relOf(root, abs))
	os.RemoveAll(filepath.Join(dir, d.ID))
	if left, _ := os.ReadDir(dir); len(left) == 0 {
		os.Remove(dir)
	}
	s.invalidate(u.Name)
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// copyInto writes a copy of src into a hidden temp file in dir and returns its path.
func copyInto(dir, src string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, ".restore-*.part")
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(tmp, in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", 0, err
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	return tmp.Name(), n, nil
}

// versionRestore puts an old version back as the current file. The current file, if any,
// becomes a version itself, so a restore can be undone.
func (s *Server) versionRestore(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Path, ID string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	root, abs, err := s.resolve(u, d.Path)
	if err != nil || abs == root || !trashIDRe.MatchString(d.ID) && !snapIDRe.MatchString(d.ID) {
		jerr(w, 400, "bad request")
		return
	}
	src := filepath.Join(versionDir(root, relOf(root, abs)), d.ID, "item")
	fi, err := os.Lstat(src)
	if err != nil || !fi.Mode().IsRegular() {
		jerr(w, 404, "that version no longer exists")
		return
	}
	if used := s.usedBytes(u.Name, root, true); used+fi.Size() > u.QuotaBytes {
		jerr(w, 413, "Not enough space left in your NAS allowance")
		return
	}
	if ex, err := os.Lstat(abs); err == nil && ex.IsDir() {
		jerr(w, 409, "A folder with that name exists")
		return
	}
	if err := mkdirChain(root, filepath.Dir(abs)); err != nil {
		jerr(w, 400, "can't recreate the folder")
		return
	}
	tmp, _, err := copyInto(filepath.Dir(abs), src)
	if err != nil {
		jerr(w, 500, "couldn't restore that version")
		return
	}
	_ = os.Chtimes(tmp, time.Now(), fi.ModTime())
	undo := s.saveVersion(root, abs)
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		undo()
		jerr(w, 500, "couldn't restore that version")
		return
	}
	s.invalidate(u.Name)
	jsonOut(w, 200, map[string]bool{"ok": true})
}
