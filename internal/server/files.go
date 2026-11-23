package server

import (
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/fsx"
)

func dirSize(root string) int64 { return fsx.DirSize(root) }

type entry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

// resolve returns the user's root and the absolute path of rel inside it.
func (s *Server) resolve(u config.User, rel string) (root, abs string, err error) {
	root, err = s.userRoot(u.Name)
	if err != nil {
		return
	}
	if inTrash(rel) { // the trash is only reachable through the /api/trash endpoints
		err = fsx.ErrEscape
		return
	}
	abs, err = fsx.Resolve(root, rel)
	return
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, u config.User) {
	root, abs, err := s.resolve(u, r.URL.Query().Get("path"))
	if err != nil {
		jerr(w, 400, "bad path")
		return
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		jerr(w, 404, "folder not found")
		return
	}
	items := make([]entry, 0, len(des))
	for _, d := range des {
		if strings.HasSuffix(d.Name(), ".part") && strings.HasPrefix(d.Name(), ".") {
			continue // in-flight uploads
		}
		if internalName(d.Name()) && abs == root {
			continue // hidden; shown in the Trash tab
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		e := entry{Name: d.Name(), Dir: info.IsDir(), MTime: info.ModTime().Unix()}
		if !e.Dir {
			e.Size = s.plainSize(filepath.Join(abs, d.Name()), info)
		}
		items = append(items, e)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Dir != items[j].Dir {
			return items[i].Dir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	jsonOut(w, 200, map[string]interface{}{"items": items})
}

func (s *Server) serveAttachment(w http.ResponseWriter, r *http.Request, abs string) {
	s.serveAttachmentAs(w, r, abs, "")
}

// serveAttachmentAs is serveAttachment with an explicit download name (used for old versions).
func (s *Server) serveAttachmentAs(w http.ResponseWriter, r *http.Request, abs, asName string) {
	rs, info, done, err := s.openContent(abs)
	if err != nil {
		contentErr(w, err)
		return
	}
	defer done()
	if asName == "" {
		asName = info.Name()
	}
	name := strings.NewReplacer(`"`, "", "\r", "", "\n", "", "\\", "").Replace(asName)
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, asName, info.ModTime(), rs) // supports Range / resume
}

func (s *Server) download(w http.ResponseWriter, r *http.Request, u config.User) {
	_, abs, err := s.resolve(u, r.URL.Query().Get("path"))
	if err != nil {
		jerr(w, 400, "bad path")
		return
	}
	s.serveAttachment(w, r, abs)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, u config.User) {
	if r.Method != http.MethodPut {
		jerr(w, 405, "PUT only")
		return
	}
	root, folder, err := s.resolve(u, r.URL.Query().Get("path"))
	name := r.URL.Query().Get("name")
	if err != nil || !fsx.ValidName(name) {
		jerr(w, 400, "bad upload target")
		return
	}
	q := r.URL.Query()
	backup := q.Get("backup") == "1" // Backup tab: create folders, keep the photo's date, never overwrite different data
	if backup {
		if err := mkdirChain(root, folder); err != nil {
			jerr(w, 400, "can't create the backup folder")
			return
		}
	}
	if fi, err := os.Stat(folder); err != nil || !fi.IsDir() {
		jerr(w, 400, "bad upload target")
		return
	}
	used := s.usedBytes(u.Name, root, true)
	remaining := u.QuotaBytes - used
	if remaining <= 0 || (r.ContentLength > 0 && r.ContentLength > remaining) {
		jerr(w, 413, "Not enough space left in your NAS allowance")
		return
	}
	if free, ok := diskFree(root); ok && r.ContentLength > 0 && free < uint64(r.ContentLength)+uint64(config.GB/20) {
		jerr(w, 507, "The laptop's disk is nearly full")
		return
	}
	dest := filepath.Join(folder, name)
	if fi, err := os.Lstat(dest); err == nil && fi.IsDir() {
		jerr(w, 409, "A folder with that name exists")
		return
	} else if err == nil && backup {
		if fi.Mode().IsRegular() && r.ContentLength >= 0 && s.plainSize(dest, fi) == r.ContentLength {
			jsonOut(w, 200, map[string]interface{}{"ok": true, "skipped": true, "saved": name}) // already backed up
			return
		}
		dest = uniqueName(dest) // same name, different content: keep both
	}
	tmp, err := os.CreateTemp(folder, "."+"upload-*.part")
	if err != nil {
		jerr(w, 500, "can't write")
		return
	}
	cw, err := s.newContentWriter(tmp)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		contentErr(w, err)
		return
	}
	n, err := io.Copy(cw, io.LimitReader(r.Body, remaining+1))
	if cerr := cw.Close(); err == nil { // Close writes the final encrypted chunk
		err = cerr
	}
	tmp.Close()
	if err != nil || n > remaining {
		os.Remove(tmp.Name())
		if err == nil {
			jerr(w, 413, "Not enough space left in your NAS allowance")
		} else {
			jerr(w, 400, "upload interrupted")
		}
		return
	}
	undo := s.saveVersion(root, dest)
	if err := os.Rename(tmp.Name(), dest); err != nil {
		os.Remove(tmp.Name())
		undo()
		jerr(w, 500, "can't save file")
		return
	}
	_ = os.Chmod(dest, 0o644)
	if mt, err := strconv.ParseInt(q.Get("mtime"), 10, 64); err == nil {
		setPhotoDate(dest, mt)
	}
	s.invalidate(u.Name)
	jsonOut(w, 200, map[string]interface{}{"ok": true, "saved": filepath.Base(dest)})
}

func (s *Server) mkdir(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Path string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	_, abs, err := s.resolve(u, d.Path)
	if err != nil || !fsx.ValidName(filepath.Base(abs)) {
		jerr(w, 400, "bad folder name")
		return
	}
	if err := os.Mkdir(abs, 0o755); err != nil {
		jerr(w, 400, "can't create folder (does it already exist?)")
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Path string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	root, abs, err := s.resolve(u, d.Path)
	if err != nil || abs == root {
		jerr(w, 400, "can't delete that")
		return
	}
	if err := moveToTrash(root, abs, d.Path); err != nil {
		jerr(w, 500, "delete failed")
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) rename(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Path, Name string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	root, abs, err := s.resolve(u, d.Path)
	if err != nil || abs == root || !fsx.ValidName(d.Name) {
		jerr(w, 400, "bad name")
		return
	}
	dest := filepath.Join(filepath.Dir(abs), d.Name)
	if _, err := os.Lstat(dest); err == nil {
		jerr(w, 409, "That name is already taken")
		return
	}
	if err := os.Rename(abs, dest); err != nil {
		jerr(w, 500, "rename failed")
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// ---------- share links ----------

func (s *Server) createShare(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct {
		Path  string  `json:"path"`
		Hours float64 `json:"hours"`
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	_, abs, err := s.resolve(u, d.Path)
	if err != nil {
		jerr(w, 400, "bad path")
		return
	}
	if fi, err := os.Stat(abs); err != nil || !fi.Mode().IsRegular() {
		jerr(w, 400, "Only files can be shared")
		return
	}
	sh, err := s.st.AddShare(u.Name, path.Clean("/"+d.Path), time.Duration(d.Hours*float64(time.Hour)))
	if err != nil {
		jerr(w, 500, "can't create link")
		return
	}
	jsonOut(w, 200, map[string]interface{}{"token": sh.Token, "url": "/s/" + sh.Token, "expires": sh.Expires})
}

func (s *Server) listShares(w http.ResponseWriter, r *http.Request, u config.User) {
	jsonOut(w, 200, map[string]interface{}{"shares": s.st.SharesFor(u.Name)})
}

func (s *Server) deleteShare(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Token string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if err := s.st.DeleteShare(u.Name, d.Token); err != nil {
		jerr(w, 404, "not found")
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// publicShare serves /s/<token> without login.
func (s *Server) publicShare(w http.ResponseWriter, r *http.Request) {
	if !s.storageOK() {
		http.Error(w, storageGoneMsg, http.StatusServiceUnavailable)
		return
	}
	if s.locked() {
		http.Error(w, lockedMsg, http.StatusLocked)
		return
	}
	sh, ok := s.st.Share(strings.TrimPrefix(r.URL.Path, "/s/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	u, ok := s.st.User(sh.User)
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, abs, err := s.resolve(u, sh.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveAttachment(w, r, abs)
}

// site serves each user's public website: /sites/<user>/... -> users/<user>/site/...
func (s *Server) site(w http.ResponseWriter, r *http.Request) {
	if !s.storageOK() {
		http.Error(w, storageGoneMsg, http.StatusServiceUnavailable)
		return
	}
	if s.locked() {
		http.Error(w, lockedMsg, http.StatusLocked)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/sites/")
	name, rel, _ := strings.Cut(rest, "/")
	if _, ok := s.st.User(name); !ok {
		http.NotFound(w, r)
		return
	}
	if rel == "" && !strings.HasSuffix(r.URL.Path, "/") {
		http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
		return
	}
	siteRoot := filepath.Join(s.st.StoragePath(), "users", name, "site")
	if _, err := os.Stat(siteRoot); err != nil {
		http.NotFound(w, r)
		return
	}
	abs, err := fsx.Resolve(siteRoot, rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		abs = filepath.Join(abs, "index.html")
	}
	rs, fi, done, err := s.openContent(abs)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			contentErr(w, err)
		}
		return
	}
	defer done()
	// Sandbox: user-uploaded pages get an opaque origin so they can never reach the admin API.
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts allow-forms allow-popups")
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), rs)
}
