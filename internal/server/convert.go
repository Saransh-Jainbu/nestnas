package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
	"github.com/Saransh-Jainbu/nestnas/internal/vault"
)

// Turning encryption on or off converts the files that are already stored, in the background.
// Both forms are readable at all times (each file is recognised by its own header), so the app
// keeps working during the conversion and a restart can simply resume it.

type convProgress struct {
	Running bool   `json:"running"`
	Mode    string `json:"mode,omitempty"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Failed  int    `json:"failed"`
	Err     string `json:"error,omitempty"`
}

type converter struct {
	mu sync.Mutex
	p  convProgress
}

func (c *converter) get() convProgress {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.p
}

func (c *converter) update(f func(p *convProgress)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.p)
}

// convSkip lists files that are never converted: unfinished uploads and the trash metadata.
func convSkip(p string, name string) bool {
	if strings.HasPrefix(name, ".") && (strings.HasSuffix(name, ".part") || strings.HasPrefix(name, ".nestnas")) {
		return true
	}
	sp := filepath.ToSlash(p)
	if strings.Contains(sp, "/"+snapshotsName+"/") && strings.HasSuffix(name, ".json") {
		return true // snapshot manifests
	}
	return name == "meta.json" && (strings.Contains(sp, "/"+trashName+"/") || strings.Contains(sp, "/"+versionsName+"/"))
}

// convertFiles walks every user's files and rewrites each one that is in the wrong form.
// It returns how many files could not be converted.
func (s *Server) convertFiles(mode string, count bool) (seen, failed int, lastErr error) {
	root := filepath.Join(s.st.StoragePath(), "users")
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 || !d.Type().IsRegular() || convSkip(p, d.Name()) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		isEnc := false
		if f, err := os.Open(p); err == nil {
			isEnc = vault.IsEncrypted(f, fi.Size())
			f.Close()
		}
		if isEnc == (mode == "encrypt") {
			return nil // already in the wanted form
		}
		seen++
		if count {
			return nil
		}
		if err := s.convertOne(p, fi, mode); err != nil {
			failed++
			lastErr = err
		}
		s.conv.update(func(c *convProgress) { c.Done++ })
		return nil
	})
	return
}

func (s *Server) convertOne(p string, fi os.FileInfo, mode string) error {
	in, err := os.Open(p)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(p), ".convert-*.part")
	if err != nil {
		return err
	}
	tn := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(tn)
		}
	}()
	if mode == "encrypt" {
		w, err := s.vault.NewWriter(tmp)
		if err != nil {
			return err
		}
		if _, err = io.Copy(w, in); err == nil {
			err = w.Close()
		}
		if err != nil {
			return err
		}
	} else {
		r, err := s.vault.NewReader(in, fi.Size())
		if err != nil {
			return err
		}
		if _, err := io.Copy(tmp, r); err != nil {
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	_ = os.Chmod(tn, fi.Mode().Perm())
	_ = os.Chtimes(tn, time.Now(), fi.ModTime())
	// Don't replace a file that changed while we were reading it (a newer upload wins).
	if cur, err := os.Lstat(p); err != nil || cur.Size() != fi.Size() || !cur.ModTime().Equal(fi.ModTime()) {
		return errors.New("file changed during conversion")
	}
	if err := os.Rename(tn, p); err != nil {
		return err
	}
	ok = true
	return nil
}

// startConvert runs the conversion in the background (one at a time).
func (s *Server) startConvert(mode string) bool {
	if !atomic.CompareAndSwapInt32(&s.convRunning, 0, 1) {
		return false
	}
	s.conv.update(func(c *convProgress) { *c = convProgress{Running: true, Mode: mode} })
	go func() {
		defer atomic.StoreInt32(&s.convRunning, 0)
		var failed int
		var lastErr error
		for pass := 0; pass < 3; pass++ { // later passes catch files that changed form meanwhile
			total, _, _ := s.convertFiles(mode, true)
			s.conv.update(func(c *convProgress) { c.Total, c.Done = total, 0 })
			if total == 0 {
				failed = 0
				break
			}
			_, failed, lastErr = s.convertFiles(mode, false)
			if failed > 0 {
				break
			}
		}
		if failed == 0 {
			if left, _, _ := s.convertFiles(mode, true); left > 0 {
				failed, lastErr = left, errors.New("some files changed while converting")
			}
		}
		if failed > 0 {
			msg := "Some files could not be converted; they are still readable. Try again."
			if lastErr != nil {
				msg += " (" + lastErr.Error() + ")"
			}
			s.conv.update(func(c *convProgress) { c.Running, c.Failed, c.Err = false, failed, msg })
			return
		}
		if mode == "decrypt" {
			_ = s.st.SetEncryption(false, false)
			_ = os.Remove(s.autoKeyPath())
			_ = os.Remove(filepath.Join(s.st.StoragePath(), vault.KeyFile))
			s.vault.Lock()
		}
		_ = s.st.SetConverting("")
		s.conv.update(func(c *convProgress) { c.Running = false })
	}()
	return true
}

func atomicBusy(p *int32) bool { return atomic.LoadInt32(p) == 1 }

// ResumeConversion restarts an interrupted conversion when the key is available.
func (s *Server) resumeConversion() {
	m := s.st.Converting()
	if m == "" || !s.storageOK() || s.locked() {
		return
	}
	s.startConvert(m)
}

func (s *Server) vaultConvert(w http.ResponseWriter, r *http.Request, u config.User) {
	var d struct {
		Mode       string `json:"mode"` // "encrypt" | "decrypt" | "resume"
		Passphrase string `json:"passphrase"`
	}
	if r.Method != http.MethodPost || !readJSON(w, r, &d) {
		return
	}
	if atomic.LoadInt32(&s.convRunning) == 1 {
		jerr(w, 409, "A conversion is already running")
		return
	}
	storage := s.st.StoragePath()
	switch d.Mode {
	case "resume":
		m := s.st.Converting()
		if m == "" {
			jerr(w, 400, "Nothing to resume")
			return
		}
		if s.locked() {
			jerr(w, http.StatusLocked, lockedMsg)
			return
		}
		s.startConvert(m)
		jsonOut(w, 200, map[string]bool{"ok": true})
	case "encrypt":
		if s.encrypted() {
			jerr(w, 400, "Encryption is already on")
			return
		}
		if len(d.Passphrase) < 12 {
			jerr(w, 400, "Passphrase must be at least 12 characters (a few random words work well)")
			return
		}
		if _, same := s.st.Authenticate(u.Name, d.Passphrase); same {
			jerr(w, 400, "Use a different passphrase than your login password")
			return
		}
		rec, err := s.vault.Init(storage, d.Passphrase)
		if errors.Is(err, vault.ErrExists) {
			jerr(w, 409, "This storage already holds encryption keys")
			return
		} else if err != nil {
			jerr(w, 500, "couldn't set up encryption")
			return
		}
		_ = s.st.SetEncryption(true, true) // keep the key on this computer so the app opens files without asking
		_ = s.writeAutoKey()
		_ = s.st.SetConverting("encrypt")
		s.startConvert("encrypt")
		jsonOut(w, 200, map[string]interface{}{"ok": true, "recovery_key": rec})
	case "decrypt":
		if !s.encrypted() {
			jerr(w, 400, "Encryption is already off")
			return
		}
		ip := clientIP(r)
		if s.tooManyFails(ip) {
			jerr(w, 429, "Too many attempts. Try again in a few minutes.")
			return
		}
		if err := s.vault.Unlock(storage, d.Passphrase); err != nil {
			s.mu.Lock()
			s.fails[ip] = append(s.fails[ip], time.Now())
			s.mu.Unlock()
			time.Sleep(time.Second)
			jerr(w, 401, "Wrong passphrase or recovery key")
			return
		}
		_ = s.st.SetConverting("decrypt")
		s.startConvert("decrypt")
		jsonOut(w, 200, map[string]bool{"ok": true})
	default:
		jerr(w, 400, "unknown mode")
	}
}
