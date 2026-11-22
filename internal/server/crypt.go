package server

import (
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/vault"
)

const lockedMsg = "Your storage is locked. An admin needs to unlock it first."

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func (s *Server) encrypted() bool { return s.st.Encrypted() }

// locked reports whether encrypted storage is currently locked. If the owner chose
// "unlock automatically", the key file kept on this computer is used (retried every few seconds).
func (s *Server) locked() bool {
	if !s.st.Encrypted() {
		return false
	}
	if !s.vault.Locked() {
		return false
	}
	s.tryAutoUnlock()
	return s.vault.Locked()
}

func (s *Server) autoKeyPath() string { return filepath.Join(s.home, "vault-auto.key") }

func (s *Server) tryAutoUnlock() {
	if !s.st.AutoUnlock() || s.home == "" || !s.storageOK() {
		return
	}
	s.mu.Lock()
	recent := time.Since(s.autoAt) < 3*time.Second
	if !recent {
		s.autoAt = time.Now()
	}
	s.mu.Unlock()
	if recent {
		return
	}
	b, err := os.ReadFile(s.autoKeyPath())
	if err != nil || len(b) != 64 {
		return
	}
	key, err := hexDecode(string(b))
	if err != nil {
		return
	}
	_ = s.vault.UnlockWithKey(s.st.StoragePath(), key)
}

// newContentWriter wraps f so everything written is encrypted when storage encryption is on.
func (s *Server) newContentWriter(f io.Writer) (io.WriteCloser, error) {
	if !s.encrypted() || s.st.Converting() == "decrypt" { // while decrypting, new files are stored plain
		return nopWriteCloser{f}, nil
	}
	return s.vault.NewWriter(f)
}

// openContent opens a stored file for reading (decrypting on the fly, with random access).
// Files without the encryption header (placed there by hand) are served as they are.
func (s *Server) openContent(abs string) (io.ReadSeeker, os.FileInfo, func(), error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		f.Close()
		return nil, nil, nil, os.ErrNotExist
	}
	done := func() { f.Close() }
	if !s.encrypted() {
		return f, fi, done, nil
	}
	r, err := s.vault.NewReader(f, fi.Size())
	if errors.Is(err, vault.ErrNotEncrypted) {
		return f, fi, done, nil
	}
	if err != nil {
		f.Close()
		return nil, nil, nil, err
	}
	return r, fi, done, nil
}

// plainSize is the size the user sees (the file's real size, not its encrypted size on disk).
// Each file is checked on its own, because encryption can be switched on or off while some
// files are still in the old form.
func (s *Server) plainSize(path string, fi os.FileInfo) int64 {
	if s.encrypted() && fi.Mode().IsRegular() && fi.Size() >= 56 {
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			if vault.IsEncrypted(f, fi.Size()) {
				return vault.PlainSize(fi.Size())
			}
		}
	}
	return fi.Size()
}

// contentErr turns a storage/vault error into the right HTTP response.
func contentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, vault.ErrLocked):
		jerr(w, http.StatusLocked, lockedMsg)
	case errors.Is(err, vault.ErrCorrupt):
		jerr(w, 500, "This file is damaged or has been tampered with, so it can't be decrypted.")
	case os.IsNotExist(err):
		jerr(w, 404, "file not found")
	default:
		jerr(w, 500, "can't read file")
	}
}

func hexDecode(s string) ([]byte, error) { return hex.DecodeString(strings.TrimSpace(s)) }
