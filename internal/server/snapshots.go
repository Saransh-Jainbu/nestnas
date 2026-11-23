package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
)

// Snapshots: a named point in time for one user's whole folder.
//
// A snapshot is a manifest (path -> stored copy of that file) in <user folder>/.snapshots/m/,
// and the stored copies live in .snapshots/objects/. Files are never modified in place (every
// save writes a new file and renames it), so a stored copy stays an exact image of the file as
// it was. A file that has not changed since the previous snapshot reuses that snapshot's stored
// copy, so only new and changed files cost space. Where the drive supports hard links (ext4,
// NTFS, APFS) the stored copy shares the disk blocks with the live file; on exFAT/FAT drives it
// is a real copy. Snapshots count toward the owner's allowance.
const (
	snapshotsName = ".snapshots"
	snapKeepAuto  = 7
)

var snapIDRe = trashIDRe

type snapEntry struct {
	Obj   string `json:"o"`
	Size  int64  `json:"s"` // size on disk
	MTime int64  `json:"t"` // modification time, nanoseconds
}

type snapManifest struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	Created int64                `json:"created"`
	Auto    bool                 `json:"auto"`
	Files   map[string]snapEntry `json:"files"`
}

type snapSummary struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Created int64  `json:"created"`
	Auto    bool   `json:"auto"`
	Files   int    `json:"files"`
	Bytes   int64  `json:"bytes"`
}

var snapMu sync.Mutex // one snapshot operation at a time

func snapDir(root string) string { return filepath.Join(root, snapshotsName) }

func readManifest(root, id string) (*snapManifest, error) {
	if !snapIDRe.MatchString(id) {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(snapDir(root), "m", id+".json"))
	if err != nil {
		return nil, err
	}
	var m snapManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func listManifests(root string) []*snapManifest {
	des, _ := os.ReadDir(filepath.Join(snapDir(root), "m"))
	var out []*snapManifest
	for _, d := range des {
		id := strings.TrimSuffix(d.Name(), ".json")
		if m, err := readManifest(root, id); err == nil {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

func writeManifest(root string, m *snapManifest) error {
	dir := filepath.Join(snapDir(root), "m")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(m)
	tmp := filepath.Join(dir, "."+m.ID+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, m.ID+".json"))
}

// storeObject saves the file at src as a stored copy: a hard link where possible, else a copy.
func storeObject(root, src, id string) error {
	dir := filepath.Join(snapDir(root), "objects")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, id)
	if os.Link(src, dst) == nil {
		return nil
	}
	tmp, _, err := copyInto(dir, src)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// gcObjects removes stored copies that no snapshot refers to any more.
func gcObjects(root string) {
	used := map[string]bool{}
	for _, m := range listManifests(root) {
		for _, e := range m.Files {
			used[e.Obj] = true
		}
	}
	dir := filepath.Join(snapDir(root), "objects")
	des, _ := os.ReadDir(dir)
	for _, d := range des {
		if !used[d.Name()] {
			os.Remove(filepath.Join(dir, d.Name()))
		}
	}
}

var errSnapSpace = errors.New("Not enough space left in your NAS allowance to take a snapshot")

// takeSnapshot records the current state of u's folder.
func (s *Server) takeSnapshot(u config.User, name string, auto bool) (*snapManifest, error) {
	snapMu.Lock()
	defer snapMu.Unlock()
	root, err := s.userRoot(u.Name)
	if err != nil {
		return nil, err
	}
	var prev *snapManifest
	if all := listManifests(root); len(all) > 0 {
		prev = all[0]
	}
	m := &snapManifest{ID: newID(), Name: name, Created: time.Now().Unix(), Auto: auto, Files: map[string]snapEntry{}}
	type job struct{ src, id string }
	var jobs []job
	var need int64
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if filepath.Dir(p) == root && internalName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || strings.HasPrefix(d.Name(), ".") && strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		rel := relOf(root, p)
		e := snapEntry{Size: fi.Size(), MTime: fi.ModTime().UnixNano()}
		if pe, ok := prev.entry(rel); ok && pe.Size == e.Size && pe.MTime == e.MTime {
			if _, err := os.Stat(filepath.Join(snapDir(root), "objects", pe.Obj)); err == nil {
				e.Obj = pe.Obj // unchanged since the last snapshot: share its stored copy
				m.Files[rel] = e
				return nil
			}
		}
		e.Obj = newID()
		jobs = append(jobs, job{p, e.Obj})
		need += fi.Size()
		m.Files[rel] = e
		return nil
	})
	if need > 0 {
		if used := s.usedBytes(u.Name, root, true); used+need > u.QuotaBytes {
			return nil, errSnapSpace
		}
		if free, ok := diskFree(root); ok && free < uint64(need)+uint64(config.GB/20) {
			return nil, errors.New("The laptop's disk is too full to take a snapshot")
		}
	}
	for _, j := range jobs {
		if err := storeObject(root, j.src, j.id); err != nil {
			gcObjects(root) // nothing refers to the partial work yet
			return nil, errors.New("couldn't store a copy of " + filepath.Base(j.src))
		}
	}
	if err := writeManifest(root, m); err != nil {
		gcObjects(root)
		return nil, errors.New("couldn't save the snapshot")
	}
	if auto { // keep the newest snapKeepAuto automatic snapshots
		n := 0
		for _, o := range listManifests(root) {
			if !o.Auto {
				continue
			}
			if n++; n > snapKeepAuto {
				os.Remove(filepath.Join(snapDir(root), "m", o.ID+".json"))
			}
		}
		gcObjects(root)
	}
	s.invalidate(u.Name)
	return m, nil
}

func (m *snapManifest) entry(rel string) (snapEntry, bool) {
	if m == nil {
		return snapEntry{}, false
	}
	e, ok := m.Files[rel]
	return e, ok
}

func (s *Server) summarize(root string, m *snapManifest) snapSummary {
	sum := snapSummary{ID: m.ID, Name: m.Name, Created: m.Created, Auto: m.Auto, Files: len(m.Files)}
	for _, e := range m.Files {
		sum.Bytes += e.Size
	}
	return sum
}

// snapshotLoop takes the daily automatic snapshots when that setting is on.
func (s *Server) snapshotLoop() {
	t := time.NewTicker(30 * time.Minute)
	for range t.C {
		if !s.st.SnapshotsAuto() || !s.st.Configured() || !s.storageOK() || atomicBusy(&s.convRunning) {
			continue
		}
		for _, u := range s.st.Users() {
			root, err := s.userRoot(u.Name)
			if err != nil {
				continue
			}
			if all := listManifests(root); len(all) > 0 && time.Since(time.Unix(all[0].Created, 0)) < 23*time.Hour {
				continue
			}
			_, _ = s.takeSnapshot(u, "Daily snapshot", true)
		}
	}
}

// ---------- API ----------

func (s *Server) snapshotsList(w http.ResponseWriter, r *http.Request, u config.User) {
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 500, "storage error")
		return
	}
	out := []snapSummary{}
	for _, m := range listManifests(root) {
		out = append(out, s.summarize(root, m))
	}
	jsonOut(w, 200, map[string]interface{}{"items": out, "auto": s.st.SnapshotsAuto(), "keep_auto": snapKeepAuto})
}

func (s *Server) snapshotCreate(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Name string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	name := strings.TrimSpace(d.Name)
	if len(name) > 80 {
		name = name[:80]
	}
	if name == "" {
		name = "Snapshot"
	}
	m, err := s.takeSnapshot(u, name, false)
	if err != nil {
		code := 500
		if errors.Is(err, errSnapSpace) {
			code = 413
		}
		jerr(w, code, err.Error())
		return
	}
	root, _ := s.userRoot(u.Name)
	jsonOut(w, 200, s.summarize(root, m))
}

func (s *Server) snapshotDelete(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ ID string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	root, err := s.userRoot(u.Name)
	if err != nil || !snapIDRe.MatchString(d.ID) {
		jerr(w, 400, "bad request")
		return
	}
	snapMu.Lock()
	os.Remove(filepath.Join(snapDir(root), "m", d.ID+".json"))
	gcObjects(root)
	snapMu.Unlock()
	s.invalidate(u.Name)
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) snapshotAuto(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Enable bool }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	_ = s.st.SetSnapshotsAuto(d.Enable)
	jsonOut(w, 200, map[string]bool{"ok": true})
}

type snapNode struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
}

// snapshotBrowse lists one folder of a snapshot, as it was.
func (s *Server) snapshotBrowse(w http.ResponseWriter, r *http.Request, u config.User) {
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 500, "storage error")
		return
	}
	m, err := readManifest(root, r.URL.Query().Get("id"))
	if err != nil {
		jerr(w, 404, "snapshot not found")
		return
	}
	base := path.Clean("/" + r.URL.Query().Get("path"))
	prefix := base
	if prefix != "/" {
		prefix += "/"
	}
	dirs := map[string]bool{}
	items := []snapNode{}
	for rel, e := range m.Files {
		if !strings.HasPrefix(rel, prefix) {
			continue
		}
		rest := rel[len(prefix):]
		if first, _, more := strings.Cut(rest, "/"); more {
			dirs[first] = true
		} else {
			size := e.Size
			op := filepath.Join(snapDir(root), "objects", e.Obj)
			if fi, err := os.Lstat(op); err == nil {
				size = s.plainSize(op, fi)
			}
			items = append(items, snapNode{Name: rest, Size: size, MTime: e.MTime / 1e9})
		}
	}
	for d := range dirs {
		items = append(items, snapNode{Name: d, Dir: true})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Dir != items[j].Dir {
			return items[i].Dir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	jsonOut(w, 200, map[string]interface{}{"items": items, "path": base})
}

func (s *Server) snapshotDownload(w http.ResponseWriter, r *http.Request, u config.User) {
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 500, "storage error")
		return
	}
	m, err := readManifest(root, r.URL.Query().Get("id"))
	rel := path.Clean("/" + r.URL.Query().Get("path"))
	if err != nil {
		jerr(w, 404, "snapshot not found")
		return
	}
	e, ok := m.Files[rel]
	if !ok {
		jerr(w, 404, "file not in that snapshot")
		return
	}
	s.serveAttachmentAs(w, r, filepath.Join(snapDir(root), "objects", e.Obj), path.Base(rel))
}

// snapshotRestore brings back one file, one folder or everything from a snapshot. It never
// deletes anything: files that exist now and differ are kept as versions first.
func (s *Server) snapshotRestore(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ ID, Path string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 500, "storage error")
		return
	}
	m, err := readManifest(root, d.ID)
	if err != nil {
		jerr(w, 404, "snapshot not found")
		return
	}
	base := path.Clean("/" + d.Path)
	var todo []string
	var need int64
	for rel, e := range m.Files {
		if rel == base || base == "/" || strings.HasPrefix(rel, base+"/") {
			todo = append(todo, rel)
			need += e.Size
		}
	}
	if len(todo) == 0 {
		jerr(w, 404, "nothing to restore at that path")
		return
	}
	if used := s.usedBytes(u.Name, root, true); used+need > u.QuotaBytes {
		jerr(w, 413, "Not enough space left in your NAS allowance")
		return
	}
	sort.Strings(todo)
	restored, same, failed := 0, 0, 0
	for _, rel := range todo {
		e := m.Files[rel]
		_, abs, err := s.resolve(u, rel)
		if err != nil {
			failed++
			continue
		}
		if cur, err := os.Lstat(abs); err == nil {
			if cur.IsDir() {
				failed++
				continue
			}
			if cur.Size() == e.Size && cur.ModTime().UnixNano() == e.MTime {
				same++ // already exactly as in the snapshot
				continue
			}
		}
		if mkdirChain(root, filepath.Dir(abs)) != nil {
			failed++
			continue
		}
		obj := filepath.Join(snapDir(root), "objects", e.Obj)
		tmp, _, err := copyInto(filepath.Dir(abs), obj)
		if err != nil {
			failed++
			continue
		}
		_ = os.Chtimes(tmp, time.Now(), time.Unix(0, e.MTime))
		undo := s.saveVersion(root, abs)
		if err := os.Rename(tmp, abs); err != nil {
			os.Remove(tmp)
			undo()
			failed++
			continue
		}
		restored++
	}
	s.invalidate(u.Name)
	jsonOut(w, 200, map[string]int{"restored": restored, "unchanged": same, "failed": failed})
}
