package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
)

var errStopWalk = errors.New("enough results")

type fileHit struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	MTime   int64  `json:"mtime"`
	Snippet string `json:"snippet,omitempty"`
}

type photoHit struct {
	Path  string  `json:"path"`
	Name  string  `json:"name"`
	Score float32 `json:"score"`
}

func isTextFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".txt", ".md", ".csv", ".json", ".log", ".yml", ".yaml", ".xml", ".ini", ".conf", ".tex", ".go", ".py", ".js", ".ts", ".c", ".h", ".java", ".rs", ".sh", ".html", ".css", ".sql", ".srt":
		return true
	}
	return false
}

// searchNames finds files and folders whose path contains every word of the query, and (within
// a time budget) small text files that contain the whole query.
func (s *Server) searchNames(root, q string, deadline time.Time) (names, texts []fileHit) {
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return
	}
	needle := strings.ToLower(strings.TrimSpace(q))
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && filepath.Dir(p) == root && internalName(d.Name()) {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 || p == root || strings.HasPrefix(d.Name(), ".") && strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		if time.Now().After(deadline) || (len(names) >= 200 && len(texts) >= 50) {
			return errStopWalk
		}
		rel := relOf(root, p)
		low := strings.ToLower(rel)
		ok := true
		for _, w := range words {
			if !strings.Contains(low, w) {
				ok = false
				break
			}
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if ok && len(names) < 200 {
			names = append(names, fileHit{Path: rel, Name: d.Name(), Dir: d.IsDir(), Size: s.plainSize(p, fi), MTime: fi.ModTime().Unix()})
		}
		if !d.IsDir() && isTextFile(d.Name()) && fi.Size() < 1<<20 && len(texts) < 50 {
			if rs, _, done, err := s.openContent(p); err == nil {
				if b, err := io.ReadAll(io.LimitReader(rs, 1<<20)); err == nil {
					if i := strings.Index(strings.ToLower(string(b)), needle); i >= 0 {
						lo, hi := i-40, i+len(needle)+60
						if lo < 0 {
							lo = 0
						}
						if hi > len(b) {
							hi = len(b)
						}
						texts = append(texts, fileHit{Path: rel, Name: d.Name(), Size: int64(len(b)), MTime: fi.ModTime().Unix(),
							Snippet: strings.Join(strings.Fields(strings.ToValidUTF8(string(b[lo:hi]), "")), " ")})
					}
				}
				done()
			}
		}
		return nil
	})
	sort.Slice(names, func(i, j int) bool {
		if names[i].Dir != names[j].Dir {
			return names[i].Dir
		}
		return len(names[i].Path) < len(names[j].Path)
	})
	return
}

// searchPhotos ranks the user's indexed photos by how well they match the words.
func (s *Server) searchPhotos(u config.User, root, q string, loose bool) ([]photoHit, error) {
	clip, release, err := s.useClip()
	if err != nil {
		return nil, err
	}
	defer release()
	qe := clip.EmbedText(q)
	if t := clip.EmbedText("a photo of " + q); len(t) == len(qe) {
		for i := range qe {
			qe[i] = (qe[i] + t[i]) / 2
		}
	}
	ix := s.userIdx(u.Name)
	s.loadIndex(root, ix)
	type cand struct {
		e *idxEntry
		s float32
	}
	var cands []cand
	ix.mu.RLock()
	for _, e := range ix.ents {
		if len(e.Vec) != len(qe) {
			continue
		}
		var f float32
		for i, v := range e.Vec {
			f += float32(v) * qe[i]
		}
		sc := f / 127
		if sc != 0 { // an all-zero vector marks a photo that could not be read
			cands = append(cands, cand{e, sc})
		}
	}
	ix.mu.RUnlock()
	sort.Slice(cands, func(i, j int) bool { return cands[i].s > cands[j].s })
	if len(cands) == 0 {
		return []photoHit{}, nil
	}
	best := cands[0].s
	spec, _ := findModel(s.st.SmartModel())
	floor, window := spec.Floor, float32(0.09)
	if loose {
		floor, window = spec.Loose, 0.2
	}
	out := []photoHit{}
	for _, c := range cands {
		if c.s < floor || c.s < best-window || len(out) >= 120 {
			break
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(c.e.Path, "/")))); err != nil {
			continue
		}
		out = append(out, photoHit{c.e.Path, filepath.Base(c.e.Path), c.s})
	}
	return out, nil
}

func (s *Server) searchAPI(w http.ResponseWriter, r *http.Request, u config.User) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 200 {
		jerr(w, 400, "type something to search for")
		return
	}
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 503, storageGoneMsg)
		return
	}
	names, texts := s.searchNames(root, q, time.Now().Add(3*time.Second))
	resp := map[string]interface{}{"q": q, "files": nonNil(names), "text": nonNil(texts), "photos": []photoHit{}, "people": []map[string]interface{}{}}
	if s.st.FacesOn() {
		resp["people"] = s.peopleMatching(u, root, q)
	}
	if s.st.SmartModel() != "" {
		photos, err := s.searchPhotos(u, root, q, r.URL.Query().Get("loose") == "1")
		if err != nil {
			resp["smart_error"] = err.Error()
		} else {
			resp["photos"] = photos
		}
	}
	jsonOut(w, 200, resp)
}

func nonNil(h []fileHit) []fileHit {
	if h == nil {
		return []fileHit{}
	}
	return h
}
