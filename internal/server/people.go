package server

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/auth"
	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/vision"
)

// People: faces found in photos are grouped by who they look like, and you can give each group a
// name. Everything stays on this computer, in <user folder>/.search/faces/, written through the
// same content writer as your files (so it is encrypted when encryption is on).

const (
	faceMagic     = "NNF1"
	faceDim       = 128
	faceMinScore  = 0.7
	faceMinSide   = 0.02 // ignore specks smaller than 2% of the picture's width
	assignSim     = 0.42 // how alike a face must be to a person's average to join them
	faceMaxDim    = 1280
	personMaxName = 60
)

type faceRec struct {
	Key   string
	Box   [4]float32 // x, y, w, h as fractions of the picture
	Score float32
	Emb   []float32
}

type facePhoto struct {
	Path  string
	Size  int64
	MTime int64
	Faces []faceRec
}

type personRec struct {
	Name    string `json:"name"`
	Hidden  bool   `json:"hidden,omitempty"`
	Created int64  `json:"created"`
}

type peopleData struct {
	People map[string]*personRec `json:"people"`
	Assign map[string]string     `json:"assign"` // face key -> person id
}

type faceIndex struct {
	mu     sync.RWMutex
	loaded bool
	photos map[string]*facePhoto
	byKey  map[string]*faceRec
	segs   int
	pd     peopleData
}

func (s *Server) faceIdx(user string) *faceIndex {
	s.smart.mu.Lock()
	defer s.smart.mu.Unlock()
	if s.smart.fidx == nil {
		s.smart.fidx = map[string]*faceIndex{}
	}
	fx := s.smart.fidx[user]
	if fx == nil {
		fx = &faceIndex{photos: map[string]*facePhoto{}, byKey: map[string]*faceRec{}, pd: peopleData{People: map[string]*personRec{}, Assign: map[string]string{}}}
		s.smart.fidx[user] = fx
	}
	return fx
}

func (s *Server) faceDir(root string) string { return filepath.Join(root, searchName, "faces") }

func faceKey(size, mtime int64, i int) string { return fmt.Sprintf("%d-%d-%d", size, mtime, i) }

// ---------- loading and saving ----------

func (s *Server) loadFaces(root string, fx *faceIndex) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.loaded {
		return
	}
	fx.loaded = true
	dir := s.faceDir(root)
	des, _ := os.ReadDir(dir)
	var names []string
	for _, d := range des {
		if strings.HasPrefix(d.Name(), "face-") {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if err := s.readFaceSegment(filepath.Join(dir, n), fx); err != nil {
			os.Remove(filepath.Join(dir, n))
		}
	}
	fx.segs = len(names)
	if rs, _, done, err := s.openContent(filepath.Join(dir, "people.json")); err == nil {
		if b, err := io.ReadAll(io.LimitReader(rs, 32<<20)); err == nil {
			var pd peopleData
			if json.Unmarshal(b, &pd) == nil && pd.People != nil && pd.Assign != nil {
				fx.pd = pd
			}
		}
		done()
	}
	if len(fx.photos) > 0 && fx.assignNew() { // also repairs groups that put two faces of one photo together
		_ = s.savePeople(root, fx)
	}
}

func (s *Server) readFaceSegment(p string, fx *faceIndex) error {
	rs, _, done, err := s.openContent(p)
	if err != nil {
		return err
	}
	defer done()
	br := bufio.NewReaderSize(io.NewSectionReader(readerAt{rs}, 0, 1<<40), 1<<20)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil || string(magic[:]) != faceMagic {
		return errors.New("bad segment")
	}
	var count uint32
	if binary.Read(br, binary.LittleEndian, &count) != nil || count > 5_000_000 {
		return errors.New("bad segment")
	}
	for i := uint32(0); i < count; i++ {
		var pl uint16
		var size, mt int64
		var nf uint8
		if binary.Read(br, binary.LittleEndian, &pl) != nil {
			return errors.New("short segment")
		}
		pb := make([]byte, pl)
		if _, err := io.ReadFull(br, pb); err != nil {
			return err
		}
		if binary.Read(br, binary.LittleEndian, &size) != nil || binary.Read(br, binary.LittleEndian, &mt) != nil || binary.Read(br, binary.LittleEndian, &nf) != nil {
			return errors.New("short segment")
		}
		ph := &facePhoto{Path: string(pb), Size: size, MTime: mt}
		for k := 0; k < int(nf); k++ {
			var box [5]float32
			if binary.Read(br, binary.LittleEndian, &box) != nil {
				return errors.New("short segment")
			}
			raw := make([]int16, faceDim)
			if binary.Read(br, binary.LittleEndian, &raw) != nil {
				return errors.New("short segment")
			}
			emb := make([]float32, faceDim)
			for j, v := range raw {
				emb[j] = float32(v) / 32767
			}
			ph.Faces = append(ph.Faces, faceRec{Key: faceKey(size, mt, k), Box: [4]float32{box[0], box[1], box[2], box[3]}, Score: box[4], Emb: emb})
		}
		fx.setPhoto(ph)
	}
	return nil
}

func (fx *faceIndex) setPhoto(ph *facePhoto) {
	if old := fx.photos[ph.Path]; old != nil {
		for _, f := range old.Faces {
			delete(fx.byKey, f.Key)
		}
	}
	fx.photos[ph.Path] = ph
	for i := range ph.Faces {
		fx.byKey[ph.Faces[i].Key] = &ph.Faces[i]
	}
}

func (s *Server) writeFaceSegment(root string, photos []*facePhoto) error {
	dir := s.faceDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".face-*.part")
	if err != nil {
		return err
	}
	cw, err := s.newContentWriter(tmp)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	bw := bufio.NewWriterSize(cw, 1<<20)
	bw.WriteString(faceMagic)
	binary.Write(bw, binary.LittleEndian, uint32(len(photos)))
	for _, ph := range photos {
		binary.Write(bw, binary.LittleEndian, uint16(len(ph.Path)))
		bw.WriteString(ph.Path)
		binary.Write(bw, binary.LittleEndian, ph.Size)
		binary.Write(bw, binary.LittleEndian, ph.MTime)
		binary.Write(bw, binary.LittleEndian, uint8(len(ph.Faces)))
		for _, f := range ph.Faces {
			binary.Write(bw, binary.LittleEndian, [5]float32{f.Box[0], f.Box[1], f.Box[2], f.Box[3], f.Score})
			raw := make([]int16, faceDim)
			for j, v := range f.Emb {
				raw[j] = int16(v * 32767)
			}
			binary.Write(bw, binary.LittleEndian, raw)
		}
	}
	err = bw.Flush()
	if cerr := cw.Close(); err == nil {
		err = cerr
	}
	tmp.Close()
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, "face-"+newID()+".bin"))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

func (s *Server) compactFaces(root string, fx *faceIndex) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if fx.segs < maxSegments {
		return
	}
	dir := s.faceDir(root)
	old, _ := filepath.Glob(filepath.Join(dir, "face-*.bin"))
	ph := make([]*facePhoto, 0, len(fx.photos))
	for _, p := range fx.photos {
		ph = append(ph, p)
	}
	sort.Slice(ph, func(i, j int) bool { return ph[i].Path < ph[j].Path })
	if s.writeFaceSegment(root, ph) != nil {
		return
	}
	for _, p := range old {
		os.Remove(p)
	}
	fx.segs = 1
}

// savePeople writes the names and assignments. The caller holds fx.mu.
func (s *Server) savePeople(root string, fx *faceIndex) error {
	dir := s.faceDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(fx.pd)
	tmp, err := os.CreateTemp(dir, ".people-*.part")
	if err != nil {
		return err
	}
	cw, err := s.newContentWriter(tmp)
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	_, err = cw.Write(b)
	if cerr := cw.Close(); err == nil {
		err = cerr
	}
	tmp.Close()
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, "people.json"))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// ---------- grouping ----------

func centroid(embs [][]float32) []float32 {
	c := make([]float32, faceDim)
	for _, e := range embs {
		for i, v := range e {
			c[i] += v
		}
	}
	return vision.Normalize(c)
}

// assignNew puts every face that has no person yet with the person it most resembles, or starts
// a new group. The caller holds fx.mu.
func (fx *faceIndex) assignNew() bool {
	// Two faces in one photo are two different people, so a person can have at most one face per
	// photo. Older groupings may break that: keep the clearest face and let the others regroup.
	pathOf := map[string]string{}
	for p, ph := range fx.photos {
		for i := range ph.Faces {
			pathOf[ph.Faces[i].Key] = p
		}
	}
	seen := map[string]map[string]string{} // person -> photo -> face kept
	for k, pid := range fx.pd.Assign {
		f, p := fx.byKey[k], pathOf[k]
		if f == nil || p == "" {
			continue
		}
		if seen[pid] == nil {
			seen[pid] = map[string]string{}
		}
		if other, dup := seen[pid][p]; dup {
			o := fx.byKey[other]
			if f.Score*f.Box[2]*f.Box[3] > o.Score*o.Box[2]*o.Box[3] || (f.Score*f.Box[2]*f.Box[3] == o.Score*o.Box[2]*o.Box[3] && k < other) {
				delete(fx.pd.Assign, other)
				seen[pid][p] = k
			} else {
				delete(fx.pd.Assign, k)
			}
		} else {
			seen[pid][p] = k
		}
	}
	inPhoto := map[string]map[string]bool{} // person -> photos they already appear in
	for pid, m := range seen {
		inPhoto[pid] = map[string]bool{}
		for p := range m {
			inPhoto[pid][p] = true
		}
	}
	members := map[string][][]float32{}
	for k, pid := range fx.pd.Assign {
		if f := fx.byKey[k]; f != nil && fx.pd.People[pid] != nil {
			members[pid] = append(members[pid], f.Emb)
		}
	}
	cents := map[string][]float32{}
	for pid, m := range members {
		cents[pid] = centroid(m)
	}
	var todo []*faceRec
	for _, ph := range fx.photos {
		for i := range ph.Faces {
			if _, ok := fx.pd.Assign[ph.Faces[i].Key]; !ok {
				todo = append(todo, &ph.Faces[i])
			}
		}
	}
	if len(todo) == 0 {
		return false
	}
	// big, confident faces first: they make the best seeds for new groups
	sort.Slice(todo, func(i, j int) bool {
		ai, aj := todo[i].Box[2]*todo[i].Box[3]*todo[i].Score, todo[j].Box[2]*todo[j].Box[3]*todo[j].Score
		if ai != aj {
			return ai > aj
		}
		return todo[i].Key < todo[j].Key
	})
	for _, f := range todo {
		best, bs := "", float32(assignSim)
		here := pathOf[f.Key]
		for pid, c := range cents {
			if inPhoto[pid][here] {
				continue // that person is already in this photo, so this must be someone else
			}
			if s := vision.Similarity(f.Emb, c); s >= bs {
				best, bs = pid, s
			}
		}
		if best == "" {
			best = "p" + auth.Token(5)
			fx.pd.People[best] = &personRec{Created: time.Now().Unix()}
			cents[best] = f.Emb
			members[best] = [][]float32{f.Emb}
		} else {
			members[best] = append(members[best], f.Emb)
			if len(members[best]) < 40 || len(members[best])%8 == 0 {
				cents[best] = centroid(members[best])
			}
		}
		fx.pd.Assign[f.Key] = best
		if inPhoto[best] == nil {
			inPhoto[best] = map[string]bool{}
		}
		inPhoto[best][here] = true
	}
	return true
}

// forget removes faces of photos that are gone, and people left with no faces (unless named).
func (fx *faceIndex) forget(present map[string]bool) bool {
	changed := false
	for p, ph := range fx.photos {
		if !present[p] {
			for _, f := range ph.Faces {
				delete(fx.byKey, f.Key)
				delete(fx.pd.Assign, f.Key)
			}
			delete(fx.photos, p)
			changed = true
		}
	}
	used := map[string]bool{}
	for _, pid := range fx.pd.Assign {
		used[pid] = true
	}
	for pid, p := range fx.pd.People {
		if !used[pid] && p.Name == "" {
			delete(fx.pd.People, pid)
			changed = true
		}
	}
	return changed
}

// ---------- API ----------

type personOut struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Photos int    `json:"photos"`
	Faces  int    `json:"faces"`
	Cover  string `json:"cover"`
	Hidden bool   `json:"hidden"`
}

func (s *Server) peopleList(w http.ResponseWriter, r *http.Request, u config.User) {
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 503, storageGoneMsg)
		return
	}
	fx := s.faceIdx(u.Name)
	s.loadFaces(root, fx)
	fx.mu.RLock()
	defer fx.mu.RUnlock()
	type agg struct {
		photos map[string]bool
		faces  int
		cover  string
		cs     float32
	}
	by := map[string]*agg{}
	for k, pid := range fx.pd.Assign {
		f := fx.byKey[k]
		if f == nil || fx.pd.People[pid] == nil {
			continue
		}
		a := by[pid]
		if a == nil {
			a = &agg{photos: map[string]bool{}}
			by[pid] = a
		}
		a.faces++
		if sc := f.Score * f.Box[2] * f.Box[3]; sc > a.cs {
			a.cs, a.cover = sc, k
		}
	}
	for _, ph := range fx.photos {
		for _, f := range ph.Faces {
			if a := by[fx.pd.Assign[f.Key]]; a != nil {
				a.photos[ph.Path] = true
			}
		}
	}
	showHidden := r.URL.Query().Get("hidden") == "1"
	out := []personOut{}
	for pid, a := range by {
		p := fx.pd.People[pid]
		if p.Hidden && !showHidden {
			continue
		}
		out = append(out, personOut{pid, p.Name, len(a.photos), a.faces, a.cover, p.Hidden})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Name != "") != (out[j].Name != "") {
			return out[i].Name != ""
		}
		if out[i].Photos != out[j].Photos {
			return out[i].Photos > out[j].Photos
		}
		return out[i].ID < out[j].ID
	})
	jsonOut(w, 200, map[string]interface{}{"enabled": s.st.FacesOn(), "ready": s.facesReady(), "people": out})
}

func (s *Server) peoplePhotos(w http.ResponseWriter, r *http.Request, u config.User) {
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 503, storageGoneMsg)
		return
	}
	fx := s.faceIdx(u.Name)
	s.loadFaces(root, fx)
	id := r.URL.Query().Get("id")
	fx.mu.RLock()
	defer fx.mu.RUnlock()
	p := fx.pd.People[id]
	if p == nil {
		jerr(w, 404, "no such person")
		return
	}
	type item struct {
		Path  string `json:"path"`
		Name  string `json:"name"`
		Face  string `json:"face"` // this person's face in the photo
		MTime int64  `json:"mtime"`
	}
	items := []item{}
	for path, ph := range fx.photos {
		for _, f := range ph.Faces {
			if fx.pd.Assign[f.Key] == id {
				if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(path, "/")))); err == nil {
					items = append(items, item{path, filepath.Base(path), f.Key, ph.MTime / 1e9})
				}
				break
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].MTime > items[j].MTime })
	jsonOut(w, 200, map[string]interface{}{"id": id, "name": p.Name, "hidden": p.Hidden, "photos": items})
}

func validPersonName(n string) (string, bool) {
	n = strings.Join(strings.Fields(n), " ")
	if len([]rune(n)) > personMaxName || strings.ContainsAny(n, "<>\n\r\t") {
		return "", false
	}
	return n, true
}

// mergePeople moves every face of from into to. The caller holds fx.mu.
func (fx *faceIndex) mergePeople(from, to string) {
	if from == to || fx.pd.People[from] == nil || fx.pd.People[to] == nil {
		return
	}
	for k, pid := range fx.pd.Assign {
		if pid == from {
			fx.pd.Assign[k] = to
		}
	}
	if fx.pd.People[to].Name == "" {
		fx.pd.People[to].Name = fx.pd.People[from].Name
	}
	delete(fx.pd.People, from)
}

func (s *Server) peopleEdit(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct {
		Op, ID, Name, To, Face string
		Hidden                 bool
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 503, storageGoneMsg)
		return
	}
	fx := s.faceIdx(u.Name)
	s.loadFaces(root, fx)
	fx.mu.Lock()
	defer fx.mu.Unlock()
	p := fx.pd.People[d.ID]
	switch d.Op {
	case "rename":
		name, ok := validPersonName(d.Name)
		if p == nil || !ok {
			jerr(w, 400, "that name can't be used")
			return
		}
		if name != "" { // the same name as someone else means they are the same person
			for pid, o := range fx.pd.People {
				if pid != d.ID && strings.EqualFold(o.Name, name) {
					fx.mergePeople(d.ID, pid)
					fx.pd.People[pid].Hidden = false
					if err := s.savePeople(root, fx); err != nil {
						jerr(w, 500, "couldn't save")
						return
					}
					jsonOut(w, 200, map[string]interface{}{"ok": true, "merged_into": pid})
					return
				}
			}
		}
		p.Name = name
	case "merge":
		if p == nil || fx.pd.People[d.To] == nil || d.To == d.ID {
			jerr(w, 400, "choose another person to merge with")
			return
		}
		fx.mergePeople(d.ID, d.To)
	case "hide":
		if p == nil {
			jerr(w, 404, "no such person")
			return
		}
		p.Hidden = d.Hidden
	case "notthis": // this face is someone else: give it a group of its own
		if fx.byKey[d.Face] == nil || fx.pd.Assign[d.Face] != d.ID {
			jerr(w, 400, "that face isn't in this group")
			return
		}
		np := "p" + auth.Token(5)
		fx.pd.People[np] = &personRec{Created: time.Now().Unix()}
		fx.pd.Assign[d.Face] = np
	default:
		jerr(w, 400, "unknown action")
		return
	}
	if err := s.savePeople(root, fx); err != nil {
		jerr(w, 500, "couldn't save")
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

// peopleInPhoto lists everyone found in one photo (two people in a photo appear under both).
func (s *Server) peopleInPhoto(w http.ResponseWriter, r *http.Request, u config.User) {
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 503, storageGoneMsg)
		return
	}
	_, abs, err := s.resolve(u, r.URL.Query().Get("path"))
	if err != nil {
		jerr(w, 400, "bad path")
		return
	}
	out := []map[string]interface{}{}
	if s.st.FacesOn() {
		fx := s.faceIdx(u.Name)
		s.loadFaces(root, fx)
		fx.mu.RLock()
		if ph := fx.photos[relOf(root, abs)]; ph != nil {
			faces := append([]faceRec(nil), ph.Faces...)
			sort.Slice(faces, func(i, j int) bool { return faces[i].Box[0] < faces[j].Box[0] }) // left to right
			for _, f := range faces {
				pid := fx.pd.Assign[f.Key]
				if p := fx.pd.People[pid]; p != nil && !p.Hidden {
					out = append(out, map[string]interface{}{"id": pid, "name": p.Name, "face": f.Key})
				}
			}
		}
		fx.mu.RUnlock()
	}
	jsonOut(w, 200, map[string]interface{}{"people": out})
}

// faceImage serves a square crop of one face.
func (s *Server) faceImage(w http.ResponseWriter, r *http.Request, u config.User) {
	root, err := s.userRoot(u.Name)
	if err != nil {
		jerr(w, 503, storageGoneMsg)
		return
	}
	fx := s.faceIdx(u.Name)
	s.loadFaces(root, fx)
	key := r.URL.Query().Get("key")
	fx.mu.RLock()
	var ph *facePhoto
	var f *faceRec
	for _, p := range fx.photos {
		for i := range p.Faces {
			if p.Faces[i].Key == key {
				ph, f = p, &p.Faces[i]
			}
		}
		if f != nil {
			break
		}
	}
	var box [4]float32
	var path string
	if f != nil {
		box, path = f.Box, ph.Path
	}
	fx.mu.RUnlock()
	if f == nil {
		http.NotFound(w, r)
		return
	}
	ck := "face:" + u.Name + ":" + key
	if b, ok := s.thumbs.get(ck); ok {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=300")
		w.Write(b)
		return
	}
	_, abs, err := s.resolve(u, path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	img, err := s.loadPhoto(abs, faceMaxDim)
	if err != nil {
		contentErr(w, err)
		return
	}
	b := img.Bounds()
	cx, cy := (box[0]+box[2]/2)*float32(b.Dx()), (box[1]+box[3]/2)*float32(b.Dy())
	side := maxf32(box[2]*float32(b.Dx()), box[3]*float32(b.Dy())) * 1.5
	rect := image.Rect(int(cx-side/2), int(cy-side/2), int(cx+side/2), int(cy+side/2)).Intersect(b)
	if rect.Empty() {
		http.NotFound(w, r)
		return
	}
	crop := downscale(subImage(img, rect), 192)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, crop, &jpeg.Options{Quality: 82}); err != nil {
		jerr(w, 500, "can't encode")
		return
	}
	s.thumbs.put(ck, buf.Bytes())
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Write(buf.Bytes())
}

func maxf32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

func subImage(img image.Image, r image.Rectangle) image.Image {
	if si, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return si.SubImage(r)
	}
	return img
}

// peopleMatching returns the photos of people whose name contains the query words.
func (s *Server) peopleMatching(u config.User, root, q string) []map[string]interface{} {
	fx := s.faceIdx(u.Name)
	s.loadFaces(root, fx)
	words := strings.Fields(strings.ToLower(q))
	fx.mu.RLock()
	defer fx.mu.RUnlock()
	out := []map[string]interface{}{}
	for pid, p := range fx.pd.People {
		if p.Name == "" || p.Hidden {
			continue
		}
		low := strings.ToLower(p.Name)
		ok := len(words) > 0
		for _, wd := range words {
			if !strings.Contains(low, wd) {
				ok = false
			}
		}
		if !ok {
			continue
		}
		n, cover := 0, ""
		for _, ph := range fx.photos {
			for _, f := range ph.Faces {
				if fx.pd.Assign[f.Key] == pid {
					n++
					if cover == "" {
						cover = f.Key
					}
					break
				}
			}
		}
		out = append(out, map[string]interface{}{"id": pid, "name": p.Name, "photos": n, "cover": cover})
	}
	return out
}

var _ = fmt.Sprintf
