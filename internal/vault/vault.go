// Package vault implements NestNAS's at-rest encryption (standard library only).
//
// File format ("NNE1"): a 40-byte header, then the plaintext in 64 KiB chunks, each sealed with
// AES-256-GCM.
//
//	header = "NNE1" | version(1) | log2(chunk)(1) | reserved(2) | fileSalt(32)
//	fileKey = HKDF-SHA256(masterKey, salt = fileSalt, info = "nestnas file key v1")
//	chunk i: nonce = 0x00000000 || bigEndian64(i);  AAD = header || lastFlag(1 byte)
//
// A fresh random fileSalt gives every file its own key, so a (key, nonce) pair is never reused.
// The header in the AAD stops header tampering, the counter in the nonce stops chunk reordering,
// and the last-chunk flag stops silent truncation. Because every chunk has a fixed size, the
// plaintext size follows from the file size alone and any offset can be read directly (Range
// requests, video seeking) without decrypting the whole file.
//
// Key management: a random 256-bit master key is wrapped (AES-256-GCM) by two independent keys
// stored in <storage>/.nestnas-vault: one derived from the admin's passphrase with scrypt, and
// one from a random recovery key shown exactly once. File names and folder structure are NOT
// encrypted; file contents are.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Saransh-Jainbu/nestnas/internal/auth"
)

const (
	magic     = "NNE1"
	hdrSize   = 40
	chunkLog2 = 16
	chunkSize = 1 << chunkLog2
	tagSize   = 16
	KeyFile   = ".nestnas-vault"

	scryptN = 1 << 16 // 64 MiB of RAM per guess
	scryptR = 8
	scryptP = 1
)

var (
	ErrLocked       = errors.New("storage is locked")
	ErrBadSecret    = errors.New("wrong passphrase or recovery key")
	ErrNotEncrypted = errors.New("not an encrypted NestNAS file")
	ErrCorrupt      = errors.New("encrypted file is damaged or has been tampered with")
	ErrExists       = errors.New("this storage is already encrypted")
	ErrNoVault      = errors.New("this storage is not encrypted")
)

// Vault holds the unlocked master key in memory.
type Vault struct {
	mu  sync.RWMutex
	key []byte
}

func New() *Vault { return &Vault{} }

func (v *Vault) Locked() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.key == nil
}

// Lock wipes the master key from memory.
func (v *Vault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := range v.key {
		v.key[i] = 0
	}
	v.key = nil
}

func (v *Vault) setKey(k []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.key = append([]byte(nil), k...)
}

// ExportKey returns a copy of the unlocked master key (for the optional auto-unlock key file).
func (v *Vault) ExportKey() ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.key == nil {
		return nil, ErrLocked
	}
	return append([]byte(nil), v.key...), nil
}

// UnlockWithKey unlocks directly with a master key (auto-unlock). It verifies the key by
// unwrapping nothing: the caller must have obtained it from ExportKey, and it is checked against
// the key file's check value.
func (v *Vault) UnlockWithKey(dir string, key []byte) error {
	doc, err := readDoc(dir)
	if err != nil {
		return err
	}
	if !doc.checkKey(key) {
		return ErrBadSecret
	}
	v.setKey(key)
	return nil
}

// ---------- key file ----------

type slot struct {
	Type    string `json:"type"` // "passphrase" | "recovery"
	Salt    string `json:"salt,omitempty"`
	Nonce   string `json:"nonce"`
	Wrapped string `json:"wrapped"`
}

type keyDoc struct {
	V     int    `json:"v"`
	N     int    `json:"n"`
	R     int    `json:"r"`
	P     int    `json:"p"`
	Check string `json:"check"` // HMAC-style fingerprint of the master key, to validate auto-unlock keys
	Slots []slot `json:"slots"`
}

func (d *keyDoc) checkKey(k []byte) bool {
	return hex.EncodeToString(auth.HKDF(k, nil, []byte("nestnas key check v1"), 16)) == d.Check
}

func readDoc(dir string) (*keyDoc, error) {
	b, err := os.ReadFile(filepath.Join(dir, KeyFile))
	if os.IsNotExist(err) {
		return nil, ErrNoVault
	} else if err != nil {
		return nil, err
	}
	var d keyDoc
	if json.Unmarshal(b, &d) != nil || d.V != 1 {
		return nil, errors.New("unreadable key file")
	}
	return &d, nil
}

func writeDoc(dir string, d *keyDoc) error {
	b, _ := json.MarshalIndent(d, "", "  ")
	tmp := filepath.Join(dir, KeyFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, KeyFile))
}

// Exists reports whether dir holds an encrypted-storage key file.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, KeyFile))
	return err == nil
}

func gcm(key []byte) cipher.AEAD {
	b, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(b)
	return g
}

func wrap(kek, master []byte, typ string) (nonce, wrapped string) {
	n := make([]byte, 12)
	_, _ = rand.Read(n)
	ct := gcm(kek).Seal(nil, n, master, []byte("nestnas vault slot v1 "+typ))
	return hex.EncodeToString(n), hex.EncodeToString(ct)
}

func unwrap(kek []byte, s slot) ([]byte, error) {
	n, e1 := hex.DecodeString(s.Nonce)
	ct, e2 := hex.DecodeString(s.Wrapped)
	if e1 != nil || e2 != nil || len(n) != 12 {
		return nil, ErrBadSecret
	}
	pt, err := gcm(kek).Open(nil, n, ct, []byte("nestnas vault slot v1 "+s.Type))
	if err != nil {
		return nil, ErrBadSecret
	}
	return pt, nil
}

func passphraseKEK(pass string, saltHex string, d *keyDoc) ([]byte, error) {
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return nil, err
	}
	return auth.Scrypt([]byte(pass), salt, d.N, d.R, d.P, 32)
}

func recoveryKEK(rec []byte) []byte {
	return auth.HKDF(rec, nil, []byte("nestnas recovery kek v1"), 32)
}

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func formatRecovery(raw []byte) string {
	s := b32.EncodeToString(raw)
	var parts []string
	for i := 0; i < len(s); i += 4 {
		end := i + 4
		if end > len(s) {
			end = len(s)
		}
		parts = append(parts, s[i:end])
	}
	return strings.Join(parts, "-")
}

func parseRecovery(s string) ([]byte, bool) {
	s = strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\n", "").Replace(s))
	raw, err := b32.DecodeString(s)
	return raw, err == nil && len(raw) == 32
}

// Create makes a new encrypted storage in dir: generates the master key, writes the key file
// and leaves the vault unlocked. It returns the recovery key, which must be shown to the user
// exactly once (it is not stored anywhere).
func Create(dir, passphrase string) (*Vault, string, error) {
	if Exists(dir) {
		return nil, "", ErrExists
	}
	master := make([]byte, 32)
	rec := make([]byte, 32)
	salt := make([]byte, 16)
	for _, b := range [][]byte{master, rec, salt} {
		if _, err := rand.Read(b); err != nil {
			return nil, "", err
		}
	}
	d := &keyDoc{V: 1, N: scryptN, R: scryptR, P: scryptP}
	d.Check = hex.EncodeToString(auth.HKDF(master, nil, []byte("nestnas key check v1"), 16))
	kek, err := passphraseKEK(passphrase, hex.EncodeToString(salt), d)
	if err != nil {
		return nil, "", err
	}
	n1, w1 := wrap(kek, master, "passphrase")
	n2, w2 := wrap(recoveryKEK(rec), master, "recovery")
	d.Slots = []slot{
		{Type: "passphrase", Salt: hex.EncodeToString(salt), Nonce: n1, Wrapped: w1},
		{Type: "recovery", Nonce: n2, Wrapped: w2},
	}
	if err := writeDoc(dir, d); err != nil {
		return nil, "", err
	}
	v := New()
	v.setKey(master)
	return v, formatRecovery(rec), nil
}

// Init creates a new encrypted storage in dir and leaves this vault unlocked. It returns the
// recovery key to show the user once.
func (v *Vault) Init(dir, passphrase string) (string, error) {
	nv, rec, err := Create(dir, passphrase)
	if err != nil {
		return "", err
	}
	k, _ := nv.ExportKey()
	nv.Lock()
	v.setKey(k)
	return rec, nil
}

// Unlock opens the vault with the passphrase or the recovery key.
func (v *Vault) Unlock(dir, secret string) error {
	d, err := readDoc(dir)
	if err != nil {
		return err
	}
	if rec, ok := parseRecovery(secret); ok {
		for _, s := range d.Slots {
			if s.Type == "recovery" {
				if m, err := unwrap(recoveryKEK(rec), s); err == nil {
					v.setKey(m)
					return nil
				}
			}
		}
	}
	for _, s := range d.Slots {
		if s.Type == "passphrase" {
			kek, err := passphraseKEK(secret, s.Salt, d)
			if err != nil {
				return err
			}
			if m, err := unwrap(kek, s); err == nil {
				v.setKey(m)
				return nil
			}
		}
	}
	return ErrBadSecret
}

// ChangePassphrase re-wraps the master key under a new passphrase. The vault must be unlocked.
func (v *Vault) ChangePassphrase(dir, oldPass, newPass string) error {
	d, err := readDoc(dir)
	if err != nil {
		return err
	}
	check := New()
	if err := check.Unlock(dir, oldPass); err != nil {
		return err
	}
	defer check.Lock()
	master, _ := check.ExportKey()
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	kek, err := passphraseKEK(newPass, hex.EncodeToString(salt), d)
	if err != nil {
		return err
	}
	n, w := wrap(kek, master, "passphrase")
	for i := range d.Slots {
		if d.Slots[i].Type == "passphrase" {
			d.Slots[i] = slot{Type: "passphrase", Salt: hex.EncodeToString(salt), Nonce: n, Wrapped: w}
		}
	}
	return writeDoc(dir, d)
}

// ---------- file encryption ----------

func (v *Vault) fileAEAD(hdr []byte) (cipher.AEAD, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.key == nil {
		return nil, ErrLocked
	}
	return gcm(auth.HKDF(v.key, hdr[8:40], []byte("nestnas file key v1"), 32)), nil
}

func nonceFor(i uint64) []byte {
	n := make([]byte, 12)
	binary.BigEndian.PutUint64(n[4:], i)
	return n
}

func aadFor(hdr []byte, last bool) []byte {
	a := append(make([]byte, 0, hdrSize+1), hdr...)
	if last {
		return append(a, 1)
	}
	return append(a, 0)
}

type writer struct {
	dst  io.Writer
	aead cipher.AEAD
	hdr  []byte
	buf  []byte
	out  []byte
	n    int
	idx  uint64
	err  error
}

// NewWriter encrypts everything written to it into dst. Close must be called to write the
// final chunk; an unclosed file is detected as truncated when read.
func (v *Vault) NewWriter(dst io.Writer) (io.WriteCloser, error) {
	hdr := make([]byte, hdrSize)
	copy(hdr, magic)
	hdr[4], hdr[5] = 1, chunkLog2
	if _, err := rand.Read(hdr[8:]); err != nil {
		return nil, err
	}
	aead, err := v.fileAEAD(hdr)
	if err != nil {
		return nil, err
	}
	if _, err := dst.Write(hdr); err != nil {
		return nil, err
	}
	return &writer{dst: dst, aead: aead, hdr: hdr, buf: make([]byte, chunkSize), out: make([]byte, 0, chunkSize+tagSize)}, nil
}

func (w *writer) flush(last bool) {
	if w.err != nil {
		return
	}
	w.out = w.aead.Seal(w.out[:0], nonceFor(w.idx), w.buf[:w.n], aadFor(w.hdr, last))
	_, w.err = w.dst.Write(w.out)
	w.idx++
	w.n = 0
}

func (w *writer) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 && w.err == nil {
		if w.n == chunkSize { // only seal a full chunk once we know more data follows
			w.flush(false)
		}
		c := copy(w.buf[w.n:], p)
		w.n += c
		p = p[c:]
	}
	return total, w.err
}

func (w *writer) Close() error {
	w.flush(true)
	return w.err
}

// IsEncrypted reports whether f (of the given on-disk size) starts with a NestNAS file header.
// It needs no key, so it also works while the vault is locked.
func IsEncrypted(f io.ReaderAt, size int64) bool {
	if size < hdrSize+tagSize {
		return false
	}
	hdr := make([]byte, 8)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return false
	}
	return string(hdr[:4]) == magic && hdr[4] == 1 && hdr[5] == chunkLog2
}

// PlainSize returns the plaintext size of an encrypted file of the given on-disk size.
func PlainSize(size int64) int64 {
	if size < hdrSize+tagSize {
		return size
	}
	s := size - hdrSize
	n := (s + chunkSize + tagSize - 1) / (chunkSize + tagSize)
	return s - n*tagSize
}

// Reader decrypts an encrypted file with random access (io.ReadSeeker).
type Reader struct {
	f      io.ReaderAt
	aead   cipher.AEAD
	hdr    []byte
	size   int64 // on-disk size
	plain  int64
	chunks int64
	pos    int64
	cached int64
	cache  []byte
}

// NewReader opens an encrypted file of the given on-disk size.
func (v *Vault) NewReader(f io.ReaderAt, size int64) (*Reader, error) {
	if size < hdrSize+tagSize {
		return nil, ErrNotEncrypted
	}
	hdr := make([]byte, hdrSize)
	if _, err := f.ReadAt(hdr, 0); err != nil {
		return nil, err
	}
	if string(hdr[:4]) != magic || hdr[4] != 1 || hdr[5] != chunkLog2 {
		return nil, ErrNotEncrypted
	}
	aead, err := v.fileAEAD(hdr)
	if err != nil {
		return nil, err
	}
	s := size - hdrSize
	return &Reader{f: f, aead: aead, hdr: hdr, size: size, plain: PlainSize(size),
		chunks: (s + chunkSize + tagSize - 1) / (chunkSize + tagSize), cached: -1}, nil
}

func (r *Reader) Size() int64 { return r.plain }

func (r *Reader) load(i int64) error {
	if r.cached == i {
		return nil
	}
	off := hdrSize + i*(chunkSize+tagSize)
	n := int64(chunkSize + tagSize)
	if off+n > r.size {
		n = r.size - off
	}
	ct := make([]byte, n)
	if _, err := r.f.ReadAt(ct, off); err != nil && err != io.EOF {
		return err
	}
	pt, err := r.aead.Open(r.cache[:0], nonceFor(uint64(i)), ct, aadFor(r.hdr, i == r.chunks-1))
	if err != nil {
		return ErrCorrupt
	}
	r.cache, r.cached = pt, i
	return nil
}

func (r *Reader) Read(p []byte) (int, error) {
	if r.pos >= r.plain {
		return 0, io.EOF
	}
	if err := r.load(r.pos / chunkSize); err != nil {
		return 0, err
	}
	n := copy(p, r.cache[r.pos%chunkSize:])
	r.pos += int64(n)
	return n, nil
}

func (r *Reader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		off += r.pos
	case io.SeekEnd:
		off += r.plain
	default:
		return 0, errors.New("bad whence")
	}
	if off < 0 {
		return 0, errors.New("negative position")
	}
	r.pos = off
	return off, nil
}
