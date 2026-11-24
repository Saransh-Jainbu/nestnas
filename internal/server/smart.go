package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/vision"
)

// Smart photo search: a small CLIP model (MIT licence) that NestNAS runs itself, in pure Go, on
// this computer. It is downloaded once from Hugging Face when an admin switches the feature on
// (the only network use), checked against pinned SHA-256 checksums, and kept in NestNAS's own
// settings folder. Photos are then described to it in the background and searched by meaning.

const searchName = ".search"

type modelFile struct {
	Name   string
	SHA    string
	Size   int64
	Remote string // path on the server when it differs from Name
}

type modelSpec struct {
	ID, Name, Note string
	Repo, Rev      string
	Base           string  // full download address prefix (instead of Repo/Rev)
	Floor, Loose   float32 // lowest similarity that counts as a match (normal, and "show more")
	Files          []modelFile
}

func (m modelSpec) total() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

var clipModels = []modelSpec{
	{ID: "tinyclip-40m", Name: "Standard", Note: "Better results", Floor: 0.20, Loose: 0.15, Repo: "wkcn/TinyCLIP-ViT-40M-32-Text-19M-LAION400M", Rev: "95ec8197b3f2fe7f747865c61ca556cf0768b2f7",
		Files: []modelFile{
			{"config.json", "b786ac49b5cdcb16496a33c8ab4884de036b2f64de27d0b73caf8391f94c9bee", 4602, ""},
			{"tokenizer.json", "b556ac8c99757ffb677208af34bc8c6721572114111a6e0aaf5fa69ff0b8d842", 2224041, ""},
			{"model.safetensors", "1b08131ac0ac54429aab216c397c550527d31f5682afb5d21d35091ad39538ae", 336859508, ""}}},
	{ID: "tinyclip-8m", Name: "Small", Note: "Smaller download, rougher results", Floor: 0.31, Loose: 0.26, Repo: "wkcn/TinyCLIP-ViT-8M-16-Text-3M-YFCC15M", Rev: "a2a8c6eaa2549ad66eb7c31b85022bf58273a26c",
		Files: []modelFile{
			{"config.json", "c586d856dbd5589b67525db9e791ee0917402405672c7773aead373efb7d7bca", 4602, ""},
			{"tokenizer.json", "b556ac8c99757ffb677208af34bc8c6721572114111a6e0aaf5fa69ff0b8d842", 2224041, ""},
			{"model.safetensors", "9339ee3d736344d0ddcaa6c03edc9f89688f08caaea5401220885233da726fcc", 93812468, ""}}},
}

// The face models come from the OpenCV model zoo (YuNet, MIT licence, finds faces; SFace,
// Apache-2.0, tells them apart), pinned to one commit and checked by SHA-256.
var facesModel = modelSpec{ID: "faces", Name: "Faces", Note: "Finds and recognises people in photos",
	Base: "https://github.com/opencv/opencv_zoo/raw/48b10fa505aeb51fbd127fc60a5c660040e823b7/models/",
	Files: []modelFile{
		{"yunet.onnx", "8f2383e4dd3cfbb4553ea8718107fc0423210dc964f9f4280604804ed2552fa4", 232589, "face_detection_yunet/face_detection_yunet_2023mar.onnx"},
		{"sface.onnx", "0ba9fbfa01b5270c96627c4ef784da859931e02f04419c829e83484087c34e79", 38696353, "face_recognition_sface/face_recognition_sface_2021dec.onnx"}}}

func findModel(id string) (modelSpec, bool) {
	if id == facesModel.ID {
		return facesModel, true
	}
	for _, m := range clipModels {
		if m.ID == id {
			return m, true
		}
	}
	return modelSpec{}, false
}

type dlState struct {
	ID      string `json:"id"`
	Running bool   `json:"running"`
	Done    int64  `json:"done"`
	Total   int64  `json:"total"`
	Err     string `json:"error,omitempty"`
}

type smartState struct {
	mu       sync.Mutex
	dl       dlState
	cancel   context.CancelFunc
	clip     *vision.Model
	clipID   string
	lastUse  time.Time
	progress map[string]*idxProgress // per user
	indexes  map[string]*userIndex
	busy     int // active uses of the loaded model
	kick     chan struct{}
	faces    *vision.Faces
	fidx     map[string]*faceIndex
}

func (s *Server) kickIndexer() {
	select {
	case s.smart.kick <- struct{}{}:
	default:
	}
}

func (s *Server) modelDir(id string) string { return filepath.Join(s.home, "models", id) }

// modelReady reports whether every file of the model is on disk at its full size.
func (s *Server) modelReady(m modelSpec) bool {
	if s.home == "" {
		return false
	}
	for _, f := range m.Files {
		if fi, err := os.Stat(filepath.Join(s.modelDir(m.ID), f.Name)); err != nil || fi.Size() != f.Size {
			return false
		}
	}
	return true
}

// startDownload fetches the model in the background. It is safe to call again to retry.
func (s *Server) startDownload(m modelSpec) {
	s.smart.mu.Lock()
	if s.smart.dl.Running {
		s.smart.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.smart.cancel = cancel
	s.smart.dl = dlState{ID: m.ID, Running: true, Total: m.total()}
	s.smart.mu.Unlock()
	go func() {
		err := s.downloadModel(ctx, m)
		s.smart.mu.Lock()
		s.smart.dl.Running = false
		if err != nil && ctx.Err() == nil {
			s.smart.dl.Err = err.Error()
		}
		s.smart.mu.Unlock()
		s.kickIndexer()
	}()
}

func (s *Server) addDone(n int64) {
	s.smart.mu.Lock()
	s.smart.dl.Done += n
	s.smart.mu.Unlock()
}

func (s *Server) downloadModel(ctx context.Context, m modelSpec) error {
	dir := s.modelDir(m.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errors.New("can't create the model folder")
	}
	for _, f := range m.Files {
		dest := filepath.Join(dir, f.Name)
		if fi, err := os.Stat(dest); err == nil && fi.Size() == f.Size {
			s.addDone(f.Size)
			continue
		}
		url := fmt.Sprintf("https://huggingface.co/%s/resolve/%s/%s", m.Repo, m.Rev, f.Name)
		if m.Base != "" {
			url = m.Base + f.Remote
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("User-Agent", "NestNAS")
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			return errors.New("couldn't reach huggingface.co: check your internet connection")
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("the download was refused (%d)", resp.StatusCode)
		}
		part := dest + ".part"
		out, err := os.Create(part)
		if err != nil {
			resp.Body.Close()
			return errors.New("can't write the model file")
		}
		h := sha256.New()
		buf := make([]byte, 256<<10)
		var got int64
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				h.Write(buf[:n])
				if _, werr := out.Write(buf[:n]); werr != nil {
					rerr = werr
				}
				got += int64(n)
				s.addDone(int64(n))
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				out.Close()
				resp.Body.Close()
				os.Remove(part)
				return errors.New("the download was interrupted: try again")
			}
		}
		out.Close()
		resp.Body.Close()
		if got != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA {
			os.Remove(part)
			return errors.New("the downloaded file did not match its checksum, so it was discarded")
		}
		if err := os.Rename(part, dest); err != nil {
			return errors.New("can't save the model file")
		}
	}
	return nil
}

// useClip loads the model if needed and returns it with a release function. The model is
// unloaded again after a few idle minutes to give its memory back.
func (s *Server) useClip() (*vision.Model, func(), error) {
	id := s.st.SmartModel()
	spec, ok := findModel(id)
	if !ok || !s.modelReady(spec) {
		return nil, nil, errors.New("smart search is not ready")
	}
	s.smart.mu.Lock()
	defer s.smart.mu.Unlock()
	if s.smart.clip == nil || s.smart.clipID != id {
		m, err := vision.Load(s.modelDir(id))
		if err != nil {
			return nil, nil, errors.New("the search model could not be loaded: " + err.Error())
		}
		s.smart.clip, s.smart.clipID = m, id
	}
	s.smart.busy++
	s.smart.lastUse = time.Now()
	m := s.smart.clip
	return m, func() {
		s.smart.mu.Lock()
		s.smart.busy--
		s.smart.lastUse = time.Now()
		s.smart.mu.Unlock()
	}, nil
}

func (s *Server) facesReady() bool { return s.st.FacesOn() && s.modelReady(facesModel) }

func (s *Server) useFaces() (*vision.Faces, func(), error) {
	if !s.facesReady() {
		return nil, nil, errors.New("face recognition is not ready")
	}
	s.smart.mu.Lock()
	defer s.smart.mu.Unlock()
	if s.smart.faces == nil {
		f, err := vision.LoadFaces(s.modelDir(facesModel.ID))
		if err != nil {
			return nil, nil, errors.New("the face models could not be loaded: " + err.Error())
		}
		s.smart.faces = f
	}
	s.smart.busy++
	s.smart.lastUse = time.Now()
	f := s.smart.faces
	return f, func() {
		s.smart.mu.Lock()
		s.smart.busy--
		s.smart.lastUse = time.Now()
		s.smart.mu.Unlock()
	}, nil
}

func (s *Server) unloadIdleModel() {
	s.smart.mu.Lock()
	defer s.smart.mu.Unlock()
	if s.smart.clip != nil && s.smart.busy == 0 && time.Since(s.smart.lastUse) > 5*time.Minute {
		s.smart.clip, s.smart.clipID, s.smart.faces = nil, "", nil
	}
}

// ---------- API ----------

type modelInfo struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Note  string `json:"note"`
	Bytes int64  `json:"bytes"`
	Ready bool   `json:"ready"`
}

func (s *Server) smartStatus(w http.ResponseWriter, r *http.Request, u config.User) {
	models := []modelInfo{}
	for _, m := range clipModels {
		models = append(models, modelInfo{m.ID, m.Name, m.Note, m.total(), s.modelReady(m)})
	}
	s.smart.mu.Lock()
	dl := s.smart.dl
	prog := idxProgress{}
	if p := s.smart.progress[u.Name]; p != nil {
		prog = *p
	}
	s.smart.mu.Unlock()
	cur := s.st.SmartModel()
	spec, _ := findModel(cur)
	jsonOut(w, 200, map[string]interface{}{
		"enabled": cur != "", "model": cur, "ready": cur != "" && s.modelReady(spec),
		"models": models, "download": dl, "index": prog, "available": s.home != "",
		"faces": map[string]interface{}{"enabled": s.st.FacesOn(), "ready": s.facesReady(), "bytes": facesModel.total()},
	})
}

func (s *Server) smartEnable(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct{ Model string }
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	spec, ok := findModel(d.Model)
	if !ok || s.home == "" {
		jerr(w, 400, "unknown model")
		return
	}
	if cur := s.st.SmartModel(); cur != "" && cur != d.Model {
		s.dropIndexes() // an index is only valid for the model that made it
	}
	_ = s.st.SetSmartModel(spec.ID)
	if !s.modelReady(spec) {
		s.startDownload(spec)
	} else {
		s.kickIndexer()
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) smartFaces(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct {
		Enable bool `json:"enable"`
		Delete bool `json:"delete"` // when turning off: also forget every face and name
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) || s.home == "" {
		return
	}
	if d.Enable {
		_ = s.st.SetFacesOn(true)
		if !s.modelReady(facesModel) {
			s.startDownload(facesModel)
		} else {
			s.kickIndexer()
		}
	} else {
		_ = s.st.SetFacesOn(false)
		s.smart.mu.Lock()
		if s.smart.cancel != nil && s.smart.dl.ID == facesModel.ID {
			s.smart.cancel()
		}
		s.smart.faces = nil
		s.smart.mu.Unlock()
		if d.Delete {
			os.RemoveAll(s.modelDir(facesModel.ID))
			s.smart.mu.Lock()
			s.smart.fidx = nil
			s.smart.mu.Unlock()
			for _, usr := range s.st.Users() {
				if root, err := s.userRoot(usr.Name); err == nil {
					os.RemoveAll(s.faceDir(root))
				}
			}
		}
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func (s *Server) smartDisable(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct {
		Delete bool `json:"delete"` // also remove the downloaded model and every index
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	_ = s.st.SetSmartModel("")
	s.smart.mu.Lock()
	if s.smart.cancel != nil {
		s.smart.cancel()
	}
	s.smart.clip, s.smart.clipID = nil, ""
	s.smart.mu.Unlock()
	if d.Delete {
		s.dropIndexes()
		for _, m := range clipModels {
			os.RemoveAll(s.modelDir(m.ID))
		}
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}
