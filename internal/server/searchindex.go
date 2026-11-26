package server

import (
	"bufio"
	"encoding/binary"
	"errors"
	"image"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/vision"
)

// The photo index: for each photo, the model's description of it (a short list of numbers) is
// kept in <user folder>/.search/<model>/ as small segment files. They go through the same
// content writer as every other file, so with encryption on they are encrypted too.

const (
	segMagic    = "NNS1"
	maxSegments = 40
)

type idxEntry struct {
	Path  string
	Size  int64
	MTime int64
	Vec   []int8
}

type userIndex struct {
	mu   sync.RWMutex
	dim  int
	ents map[string]*idxEntry
	segs int
	load bool
}

type idxProgress struct {
	Running bool   `json:"running"`
	Total   int    `json:"total"`
	Done    int    `json:"done"`
	Paused  string `json:"paused,omitempty"`
	Updated int64  `json:"updated"`
}

func isPhoto(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".gif":
		return true
	}
	return false
}

func (s *Server) indexDir(root string) string {
	return filepath.Join(root, searchName, s.st.SmartModel())
}

func (s *Server) userIdx(user string) *userIndex {
	s.smart.mu.Lock()
	defer s.smart.mu.Unlock()
	if s.smart.indexes == nil {
		s.smart.indexes = map[string]*userIndex{}
	}
	ix := s.smart.indexes[user]
	if ix == nil {
		ix = &userIndex{ents: map[string]*idxEntry{}}
		s.smart.indexes[user] = ix
	}
	return ix
}

func (s *Server) setProgress(user string, f func(p *idxProgress)) {
	s.smart.mu.Lock()
	defer s.smart.mu.Unlock()
	if s.smart.progress == nil {
		s.smart.progress = map[string]*idxProgress{}
	}
	p := s.smart.progress[user]
	if p == nil {
		p = &idxProgress{}
		s.smart.progress[user] = p
	}
	f(p)
	p.Updated = time.Now().Unix()
}

// dropIndexes forgets every user's index and removes the files (after a model change or removal).
func (s *Server) dropIndexes() {
	s.smart.mu.Lock()
	s.smart.indexes, s.smart.progress = nil, nil
	s.smart.mu.Unlock()
	for _, u := range s.st.Users() {
		if root, err := s.userRoot(u.Name); err == nil {
			for _, m := range clipModels { // the faces folder is separate and is never dropped here
				os.RemoveAll(filepath.Join(root, searchName, m.ID))
			}
		}
	}
}

// ---------- reading and writing segments ----------

func (s *Server) loadIndex(root string, ix *userIndex) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.load {
		return
	}
	ix.load = true
	dir := s.indexDir(root)
	des, _ := os.ReadDir(dir)
	var names []string
	for _, d := range des {
		if strings.HasPrefix(d.Name(), "seg-") {
			names = append(names, d.Name())
		}
	}
	sort.Strings(names) // oldest first: later segments override earlier entries
	for _, n := range names {
		if err := s.readSegment(filepath.Join(dir, n), ix); err != nil {
			os.Remove(filepath.Join(dir, n)) // unreadable: it will be rebuilt
		}
	}
	ix.segs = len(names)
}

func (s *Server) readSegment(p string, ix *userIndex) error {
	rs, _, done, err := s.openContent(p)
	if err != nil {
		return err
	}
	defer done()
	br := bufio.NewReaderSize(io.NewSectionReader(readerAt{rs}, 0, 1<<40), 1<<20)
	var magic [4]byte
	if _, err := io.ReadFull(br, magic[:]); err != nil || string(magic[:]) != segMagic {
		return errors.New("bad segment")
	}
	var dim uint16
	var count uint32
	if binary.Read(br, binary.LittleEndian, &dim) != nil || binary.Read(br, binary.LittleEndian, &count) != nil || dim == 0 || count > 5_000_000 {
		return errors.New("bad segment")
	}
	for i := uint32(0); i < count; i++ {
		var pl uint16
		if err := binary.Read(br, binary.LittleEndian, &pl); err != nil {
			return err
		}
		pb := make([]byte, pl)
		var size, mt int64
		if _, err := io.ReadFull(br, pb); err != nil {
			return err
		}
		if binary.Read(br, binary.LittleEndian, &size) != nil || binary.Read(br, binary.LittleEndian, &mt) != nil {
			return errors.New("short segment")
		}
		vec := make([]byte, dim)
		if _, err := io.ReadFull(br, vec); err != nil {
			return err
		}
		v := make([]int8, dim)
		for j, b := range vec {
			v[j] = int8(b)
		}
		ix.dim = int(dim)
		ix.ents[string(pb)] = &idxEntry{string(pb), size, mt, v}
	}
	return nil
}

// readerAt adapts a ReadSeeker to an io.ReaderAt for section reading (single reader at a time).
type readerAt struct{ rs io.ReadSeeker }

func (r readerAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := r.rs.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(r.rs, p)
}

func (s *Server) writeSegment(root string, ents []*idxEntry, dim int) error {
	dir := s.indexDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".seg-*.part")
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
	bw.WriteString(segMagic)
	binary.Write(bw, binary.LittleEndian, uint16(dim))
	binary.Write(bw, binary.LittleEndian, uint32(len(ents)))
	for _, e := range ents {
		binary.Write(bw, binary.LittleEndian, uint16(len(e.Path)))
		bw.WriteString(e.Path)
		binary.Write(bw, binary.LittleEndian, e.Size)
		binary.Write(bw, binary.LittleEndian, e.MTime)
		b := make([]byte, dim)
		for i, v := range e.Vec {
			b[i] = byte(v)
		}
		bw.Write(b)
	}
	err = bw.Flush()
	if cerr := cw.Close(); err == nil {
		err = cerr
	}
	tmp.Close()
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, "seg-"+newID()+".bin"))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// compact rewrites all entries as one segment when there are many small ones.
func (s *Server) compact(root string, ix *userIndex) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.segs < maxSegments || ix.dim == 0 {
		return
	}
	dir := s.indexDir(root)
	old, _ := filepath.Glob(filepath.Join(dir, "seg-*.bin"))
	ents := make([]*idxEntry, 0, len(ix.ents))
	for _, e := range ix.ents {
		ents = append(ents, e)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Path < ents[j].Path })
	if err := s.writeSegment(root, ents, ix.dim); err != nil {
		return
	}
	for _, p := range old {
		os.Remove(p)
	}
	ix.segs = 1
}

// ---------- the background indexer ----------

func quantize(v []float32) []int8 {
	q := make([]int8, len(v))
	for i, x := range v {
		r := math.Round(float64(x) * 127)
		if r > 127 {
			r = 127
		} else if r < -127 {
			r = -127
		}
		q[i] = int8(r)
	}
	return q
}

// loadPhoto decodes a photo for the model: rotated upright and no larger than maxDim.
func (s *Server) loadPhoto(abs string, maxDim int) (image.Image, error) {
	rs, _, done, err := s.openContent(abs)
	if err != nil {
		return nil, err
	}
	defer done()
	head := make([]byte, 128<<10)
	n, _ := io.ReadFull(rs, head)
	head = head[:n]
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(rs)
	if err != nil {
		return nil, errNotImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return nil, errTooBig
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(rs)
	if err != nil {
		return nil, errNotImage
	}
	return orient(downscale(img, maxDim), exifOrientation(head)), nil
}

type photoFile struct {
	rel, abs string
	size, mt int64
}

func (s *Server) listPhotos(root string) []photoFile {
	var out []photoFile
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
		if d.Type()&os.ModeSymlink != 0 || !isPhoto(d.Name()) || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() && fi.Size() < 100<<20 {
			out = append(out, photoFile{relOf(root, p), p, fi.Size(), fi.ModTime().UnixNano()})
		}
		return nil
	})
	return out
}

// smartLoop keeps every user's photo index up to date while the feature is on.
func (s *Server) smartLoop() {
	t := time.NewTicker(20 * time.Second)
	for {
		select {
		case <-t.C:
		case <-s.smart.kick:
		}
		s.unloadIdleModel()
		s.ensureDownloads()
		if s.st.SmartModel() == "" && !s.st.FacesOn() || !s.st.Configured() || !s.storageOK() || s.home == "" {
			continue
		}
		for _, u := range s.st.Users() {
			s.indexUser(u)
		}
	}
}

// ensureDownloads starts the next missing model when nothing is downloading (both features can be on).
func (s *Server) ensureDownloads() {
	s.smart.mu.Lock()
	busy := s.smart.dl.Running || s.smart.dl.Err != ""
	s.smart.mu.Unlock()
	if busy || s.home == "" {
		return
	}
	if spec, ok := findModel(s.st.SmartModel()); ok && !s.modelReady(spec) {
		s.startDownload(spec)
	} else if s.st.FacesOn() && !s.modelReady(facesModel) {
		s.startDownload(facesModel)
	}
}

func (s *Server) indexPause(user string) string {
	switch {
	case s.locked():
		return "locked"
	case atomicBusy(&s.convRunning):
		return "converting"
	}
	return ""
}

func (s *Server) indexUser(u config.User) {
	wantClip := false
	if spec, ok := findModel(s.st.SmartModel()); ok && s.modelReady(spec) {
		wantClip = true
	}
	wantFaces := s.facesReady()
	if !wantClip && !wantFaces {
		return
	}
	if why := s.indexPause(u.Name); why != "" {
		s.setProgress(u.Name, func(p *idxProgress) { p.Paused, p.Running = why, false })
		return
	}
	root, err := s.userRoot(u.Name)
	if err != nil {
		return
	}
	ix, fx := s.userIdx(u.Name), s.faceIdx(u.Name)
	if wantClip {
		s.loadIndex(root, ix)
	}
	if wantFaces {
		s.loadFaces(root, fx)
	}
	photos := s.listPhotos(root)
	present := map[string]bool{}
	type job struct {
		p         photoFile
		clip, fac bool
	}
	var todo []job
	ix.mu.RLock()
	fx.mu.RLock()
	for _, p := range photos {
		present[p.rel] = true
		j := job{p: p}
		if wantClip {
			if e := ix.ents[p.rel]; e == nil || e.Size != p.size || e.MTime != p.mt {
				j.clip = true
			}
		}
		if wantFaces {
			if e := fx.photos[p.rel]; e == nil || e.Size != p.size || e.MTime != p.mt {
				j.fac = true
			}
		}
		if j.clip || j.fac {
			todo = append(todo, j)
		}
	}
	fx.mu.RUnlock()
	ix.mu.RUnlock()
	if wantClip { // forget deleted photos right away; the segments are tidied on the next compaction
		ix.mu.Lock()
		for k := range ix.ents {
			if !present[k] {
				delete(ix.ents, k)
			}
		}
		ix.mu.Unlock()
	}
	if wantFaces {
		fx.mu.Lock()
		if fx.forget(present) {
			_ = s.savePeople(root, fx)
		}
		fx.mu.Unlock()
	}
	s.setProgress(u.Name, func(p *idxProgress) {
		p.Total, p.Done, p.Paused, p.Running = len(photos), len(photos)-len(todo), "", len(todo) > 0
	})
	if len(todo) == 0 {
		return
	}
	var clip *vision.Model
	var fdet *vision.Faces
	if wantClip {
		c, release, err := s.useClip()
		if err != nil {
			wantClip = false
		} else {
			clip = c
			defer release()
		}
	}
	if wantFaces {
		f, release, err := s.useFaces()
		if err != nil {
			wantFaces = false
		} else {
			fdet = f
			defer release()
		}
	}
	vision.SetWorkers(workersForIndexing())
	oldGC := debug.SetGCPercent(25) // the models are big and long-lived: collect garbage early instead of letting the heap double
	defer func() {
		debug.SetGCPercent(oldGC)
		debug.FreeOSMemory()
	}()
	var cbatch []*idxEntry
	var fbatch []*facePhoto
	flush := func() {
		if len(cbatch) > 0 {
			if err := s.writeSegment(root, cbatch, clip.Dim()); err == nil {
				ix.mu.Lock()
				ix.segs++
				ix.mu.Unlock()
			}
			cbatch = nil
		}
		if len(fbatch) > 0 {
			if err := s.writeFaceSegment(root, fbatch); err == nil {
				fx.mu.Lock()
				fx.segs++
				fx.assignNew()
				_ = s.savePeople(root, fx)
				fx.mu.Unlock()
			}
			fbatch = nil
		}
	}
	last := time.Now()
	for i, j := range todo {
		if s.indexPause(u.Name) != "" || !s.storageOK() || (j.clip && s.st.SmartModel() == "") || (j.fac && !s.st.FacesOn()) {
			break
		}
		st := time.Now()
		img, lerr := s.loadPhoto(j.p.abs, faceMaxDim)
		if j.clip && wantClip {
			var vec []int8
			if lerr == nil {
				vec = quantize(clip.EmbedImage(img))
			} else {
				vec = make([]int8, clip.Dim()) // unreadable photo: record it so we don't retry every pass
			}
			e := &idxEntry{j.p.rel, j.p.size, j.p.mt, vec}
			ix.mu.Lock()
			ix.dim = clip.Dim()
			ix.ents[j.p.rel] = e
			ix.mu.Unlock()
			cbatch = append(cbatch, e)
		}
		if j.fac && wantFaces {
			ph := &facePhoto{Path: j.p.rel, Size: j.p.size, MTime: j.p.mt}
			if lerr == nil {
				if found, err := fdet.Analyze(img, faceMinScore); err == nil {
					for k, f := range found {
						if f.W < faceMinSide || len(f.Emb) != faceDim {
							continue
						}
						ph.Faces = append(ph.Faces, faceRec{Key: faceKey(ph.Size, ph.MTime, len(ph.Faces)), Box: [4]float32{f.X, f.Y, f.W, f.H}, Score: f.Score, Emb: f.Emb})
						_ = k
					}
				}
			}
			fx.mu.Lock()
			fx.setPhoto(ph)
			fx.mu.Unlock()
			fbatch = append(fbatch, ph)
		}
		s.setProgress(u.Name, func(pr *idxProgress) { pr.Done = len(photos) - len(todo) + i + 1; pr.Running = true })
		if len(cbatch) >= 40 || len(fbatch) >= 40 || time.Since(last) > 45*time.Second {
			flush()
			last = time.Now()
		}
		time.Sleep(time.Since(st) / 2) // leave a third of the time for everything else
	}
	flush()
	if wantClip {
		s.compact(root, ix)
	}
	if wantFaces {
		s.compactFaces(root, fx)
	}
	s.setProgress(u.Name, func(pr *idxProgress) { pr.Running = false })
}

func workersForIndexing() int {
	n := 4
	return n
}
