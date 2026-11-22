package server

// Phone/computer backup support: the dashboard's Backup tab uploads photos, videos and folders
// to <user>/Phone Backup/<device>/<year>/<month>/ and must be safe to run again and again.
//   - POST /api/backup/check  tells the client which files the NAS does not have yet.
//   - /api/upload?backup=1    creates the folders, keeps the photo's own date, and NEVER
//     overwrites a different file that happens to have the same name (it saves "name (2).ext").

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/fsx"
)

const maxCheck = 4000

type backupFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// backupCheck answers which of the given files are missing (absent, or present with a different size).
func (s *Server) backupCheck(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct {
		Files []backupFile `json:"files"`
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if len(d.Files) > maxCheck {
		jerr(w, 400, "too many files in one check")
		return
	}
	missing := []int{}
	for i, f := range d.Files {
		_, abs, err := s.resolve(u, f.Path)
		if err != nil {
			missing = append(missing, i) // let the upload report the real problem
			continue
		}
		if fi, err := os.Lstat(abs); err == nil && fi.Mode().IsRegular() && s.plainSize(fi) == f.Size {
			continue
		}
		missing = append(missing, i)
	}
	jsonOut(w, 200, map[string]interface{}{"missing": missing, "have": len(d.Files) - len(missing)})
}

// mkdirChain creates abs (inside root) one folder at a time with os.Mkdir, never MkdirAll, so
// a drive that vanished a moment ago can't be re-created on the internal disk.
func mkdirChain(root, abs string) error {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fsx.ErrEscape
	}
	if rel == "." {
		return nil
	}
	cur := root
	for _, comp := range strings.Split(rel, string(filepath.Separator)) {
		if !fsx.ValidName(comp) {
			return fsx.ErrEscape
		}
		cur = filepath.Join(cur, comp)
		if fi, err := os.Lstat(cur); err == nil {
			if !fi.IsDir() {
				return os.ErrExist
			}
			continue
		}
		if err := os.Mkdir(cur, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// uniqueName returns "name (2).ext", "name (3).ext"... for a path that is already taken.
func uniqueName(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		c := base + " (" + itoa(i) + ")" + ext
		if _, err := os.Lstat(c); os.IsNotExist(err) {
			return c
		}
	}
}

// setPhotoDate gives the saved file the photo's original date (if it is a sane value).
func setPhotoDate(path string, unix int64) {
	if unix <= 0 || unix > time.Now().Add(24*time.Hour).Unix() {
		return
	}
	t := time.Unix(unix, 0)
	_ = os.Chtimes(path, t, t)
}
