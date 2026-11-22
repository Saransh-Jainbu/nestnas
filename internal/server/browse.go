package server

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Saransh-Jainbu/nestnas/internal/fsx"
)

// The first-run folder picker lists folders on the NAS laptop so the owner can click instead
// of typing a path. Because it is unauthenticated, it is only available (1) before setup has
// been completed and (2) to requests from this computer itself.
func (s *Server) setupOnlyLocal(w http.ResponseWriter, r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(host)
	h := r.Host
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	localHost := h == "localhost" || h == "127.0.0.1" || h == "[::1]" // blocks DNS-rebinding from other sites
	if s.st.Configured() || err != nil || ip == nil || !ip.IsLoopback() || !localHost {
		jerr(w, 403, "The folder picker only works on this computer during first-time setup. Type the path instead.")
		return false
	}
	return true
}

type dirItem struct {
	Name string `json:"name"`
}

func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	if !s.setupOnlyLocal(w, r) {
		return
	}
	home, _ := os.UserHomeDir()
	p := r.URL.Query().Get("path")
	if p == "" || !filepath.IsAbs(p) {
		p = home
	}
	p = filepath.Clean(p)
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		p = home
	}
	dirs := []dirItem{}
	if des, err := os.ReadDir(p); err == nil {
		for _, d := range des {
			if !strings.HasPrefix(d.Name(), ".") && d.IsDir() {
				dirs = append(dirs, dirItem{d.Name()})
			}
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	free, _ := diskFree(p)
	total, _ := diskTotal(p)
	writable := false
	if f, err := os.CreateTemp(p, ".nestnas-write-test-*"); err == nil {
		f.Close()
		os.Remove(f.Name())
		writable = true
	}
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	fs, point := mountFor(p)
	jsonOut(w, 200, map[string]interface{}{
		"path": p, "parent": parent, "home": home, "dirs": dirs,
		"free": free, "total": total, "writable": writable,
		"fs": fs, "note": fsNote(fs), "removable": isRemovablePath(point) || isRemovablePath(p),
		"places": listPlaces(),
	})
}

func (s *Server) browseMkdir(w http.ResponseWriter, r *http.Request) {
	if !s.setupOnlyLocal(w, r) {
		return
	}
	var d struct{ Path, Name string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if !filepath.IsAbs(d.Path) || !fsx.ValidName(d.Name) {
		jerr(w, 400, "bad folder name")
		return
	}
	if err := os.Mkdir(filepath.Join(filepath.Clean(d.Path), d.Name), 0o755); err != nil {
		jerr(w, 400, "Can't create that folder here")
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}
