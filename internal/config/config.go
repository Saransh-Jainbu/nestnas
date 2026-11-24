// Package config holds NestNAS's persistent state: users, quotas and share links.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/Saransh-Jainbu/nestnas/internal/auth"
)

const GB = int64(1) << 30

var nameRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

type User struct {
	Name       string `json:"name"`
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Admin      bool   `json:"admin"`
	QuotaBytes int64  `json:"quota_bytes"`
}

type Share struct {
	Token   string `json:"token"`
	User    string `json:"user"`
	Path    string `json:"path"`    // path relative to the user's folder
	Expires int64  `json:"expires"` // unix seconds; 0 = never
	Created int64  `json:"created"`
}

type Config struct {
	StoragePath   string  `json:"storage_path"`
	StorageID     string  `json:"storage_id,omitempty"`     // random id also stored in <storage>/.nestnas-id
	Encrypted     bool    `json:"encrypted,omitempty"`      // contents are encrypted at rest (see internal/vault)
	AutoUnlock    bool    `json:"auto_unlock,omitempty"`    // key kept on this computer so it unlocks itself on start
	SnapshotsAuto bool    `json:"snapshots_auto,omitempty"` // take a snapshot of every user daily
	FacesOn       bool    `json:"faces_on,omitempty"`       // recognise people in photos
	SmartModel    string  `json:"smart_model,omitempty"`    // photo-search model that is switched on ("" = off)
	Converting    string  `json:"converting,omitempty"`     // "encrypt" or "decrypt" while existing files are being converted
	StorageSub    string  `json:"storage_sub,omitempty"`    // storage folder relative to its drive, to find it again after re-plugging
	Users         []User  `json:"users"`
	Shares        []Share `json:"shares"`
}

type Store struct {
	mu   sync.RWMutex
	file string
	c    Config
}

var (
	ErrExists   = errors.New("already exists")
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid input")
)

// Open loads (or lazily creates) the config in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{file: filepath.Join(dir, "config.json")}
	b, err := os.ReadFile(s.file)
	if err == nil {
		if err := json.Unmarshal(b, &s.c); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.c, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.file)
}

func (s *Store) Configured() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.StoragePath != "" && len(s.c.Users) > 0
}

func (s *Store) StoragePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.StoragePath
}

func (s *Store) Encrypted() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.Encrypted
}

func (s *Store) AutoUnlock() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.AutoUnlock
}

func (s *Store) SetEncryption(enabled, auto bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.Encrypted, s.c.AutoUnlock = enabled, auto
	return s.save()
}

func (s *Store) SnapshotsAuto() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.SnapshotsAuto
}

func (s *Store) SetSnapshotsAuto(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.SnapshotsAuto = on
	return s.save()
}

func (s *Store) FacesOn() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.FacesOn
}

func (s *Store) SetFacesOn(on bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.FacesOn = on
	return s.save()
}

func (s *Store) SmartModel() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.SmartModel
}

func (s *Store) SetSmartModel(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.SmartModel = id
	return s.save()
}

func (s *Store) Converting() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.Converting
}

func (s *Store) SetConverting(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.Converting = mode
	return s.save()
}

func (s *Store) StorageID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.StorageID
}

func (s *Store) StorageSub() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.c.StorageSub
}

func (s *Store) SetStorageIdentity(id, sub string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.c.StorageID, s.c.StorageSub = id, sub
	return s.save()
}

// SetStoragePath records where the storage drive is currently mounted.
func (s *Store) SetStoragePath(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c.StoragePath == p {
		return nil
	}
	s.c.StoragePath = p
	return s.save()
}

func ValidName(n string) bool { return nameRe.MatchString(n) }

// Setup performs first-run setup, creating the admin account.
func (s *Store) Setup(admin, password, storage string, quota int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.c.StoragePath != "" && len(s.c.Users) > 0 {
		return ErrExists
	}
	if !ValidName(admin) || len(password) < 8 || quota <= 0 {
		return ErrInvalid
	}
	salt := auth.NewSalt()
	s.c = Config{
		StoragePath: storage,
		Users:       []User{{Name: admin, Salt: salt, Hash: auth.Hash(password, salt), Admin: true, QuotaBytes: quota}},
	}
	return s.save()
}

func (s *Store) Authenticate(name, password string) (User, bool) {
	s.mu.RLock()
	var u User
	found := false
	for _, x := range s.c.Users {
		if x.Name == name {
			u, found = x, true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		// Burn comparable time so usernames can't be probed by timing.
		auth.Hash(password, "00")
		return User{}, false
	}
	return u, auth.Verify(password, u.Salt, u.Hash)
}

func (s *Store) User(name string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, x := range s.c.Users {
		if x.Name == name {
			return x, true
		}
	}
	return User{}, false
}

func (s *Store) Users() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]User(nil), s.c.Users...)
}

func (s *Store) AddUser(name, password string, quota int64, admin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidName(name) || len(password) < 8 || quota <= 0 {
		return ErrInvalid
	}
	for _, x := range s.c.Users {
		if x.Name == name {
			return ErrExists
		}
	}
	salt := auth.NewSalt()
	s.c.Users = append(s.c.Users, User{Name: name, Salt: salt, Hash: auth.Hash(password, salt), Admin: admin, QuotaBytes: quota})
	return s.save()
}

func (s *Store) SetPassword(name, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(password) < 8 {
		return ErrInvalid
	}
	for i := range s.c.Users {
		if s.c.Users[i].Name == name {
			salt := auth.NewSalt()
			s.c.Users[i].Salt, s.c.Users[i].Hash = salt, auth.Hash(password, salt)
			return s.save()
		}
	}
	return ErrNotFound
}

func (s *Store) SetQuota(name string, quota int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if quota <= 0 {
		return ErrInvalid
	}
	for i := range s.c.Users {
		if s.c.Users[i].Name == name {
			s.c.Users[i].QuotaBytes = quota
			return s.save()
		}
	}
	return ErrNotFound
}

// DeleteUser removes a user and their share links. The last admin cannot be removed.
func (s *Store) DeleteUser(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx, admins := -1, 0
	for i, x := range s.c.Users {
		if x.Admin {
			admins++
		}
		if x.Name == name {
			idx = i
		}
	}
	if idx < 0 {
		return ErrNotFound
	}
	if s.c.Users[idx].Admin && admins <= 1 {
		return ErrInvalid
	}
	s.c.Users = append(s.c.Users[:idx], s.c.Users[idx+1:]...)
	keep := s.c.Shares[:0]
	for _, sh := range s.c.Shares {
		if sh.User != name {
			keep = append(keep, sh)
		}
	}
	s.c.Shares = keep
	return s.save()
}

func (s *Store) AddShare(user, path string, ttl time.Duration) (Share, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	sh := Share{Token: auth.Token(16), User: user, Path: path, Created: now}
	if ttl > 0 {
		sh.Expires = now + int64(ttl.Seconds())
	}
	s.c.Shares = append(s.c.Shares, sh)
	return sh, s.save()
}

// Share returns a live (non-expired) share.
func (s *Store) Share(token string) (Share, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sh := range s.c.Shares {
		if sh.Token == token && (sh.Expires == 0 || sh.Expires > time.Now().Unix()) {
			return sh, true
		}
	}
	return Share{}, false
}

func (s *Store) SharesFor(user string) []Share {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Share{} // non-nil so JSON is [] not null
	now := time.Now().Unix()
	for _, sh := range s.c.Shares {
		if sh.User == user && (sh.Expires == 0 || sh.Expires > now) {
			out = append(out, sh)
		}
	}
	return out
}

func (s *Store) DeleteShare(user, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, sh := range s.c.Shares {
		if sh.Token == token && sh.User == user {
			s.c.Shares = append(s.c.Shares[:i], s.c.Shares[i+1:]...)
			return s.save()
		}
	}
	return ErrNotFound
}
