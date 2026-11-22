package vault

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func newVault(t *testing.T) (*Vault, string, string) {
	dir := t.TempDir()
	v, rec, err := Create(dir, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	return v, dir, rec
}

func seal(t *testing.T, v *Vault, plain []byte) []byte {
	var buf bytes.Buffer
	w, err := v.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	// write in awkward pieces to exercise chunk buffering
	for off := 0; off < len(plain); {
		n := 7919
		if off+n > len(plain) {
			n = len(plain) - off
		}
		w.Write(plain[off : off+n])
		off += n
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundTripAndSizeFormula(t *testing.T) {
	v, _, _ := newVault(t)
	for _, n := range []int{0, 1, 15, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 7, 5 * chunkSize} {
		plain := make([]byte, n)
		rand.Read(plain)
		ct := seal(t, v, plain)
		if got := PlainSize(int64(len(ct))); got != int64(n) {
			t.Errorf("n=%d: PlainSize = %d", n, got)
		}
		r, err := v.NewReader(bytes.NewReader(ct), int64(len(ct)))
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("n=%d: round trip failed (err %v)", n, err)
		}
		if n > 100 && bytes.Contains(ct, plain[10:90]) {
			t.Errorf("n=%d: plaintext visible in ciphertext", n)
		}
	}
}

func TestRandomAccess(t *testing.T) {
	v, _, _ := newVault(t)
	plain := make([]byte, 4*chunkSize+1234)
	rand.Read(plain)
	ct := seal(t, v, plain)
	r, _ := v.NewReader(bytes.NewReader(ct), int64(len(ct)))
	for _, off := range []int64{0, 1, chunkSize - 3, chunkSize, 2*chunkSize + 5, int64(len(plain)) - 10} {
		r.Seek(off, io.SeekStart)
		buf := make([]byte, 50)
		n, _ := io.ReadFull(r, buf)
		if !bytes.Equal(buf[:n], plain[off:off+int64(n)]) {
			t.Errorf("wrong data at offset %d", off)
		}
	}
	if end, _ := r.Seek(0, io.SeekEnd); end != int64(len(plain)) {
		t.Errorf("SeekEnd = %d", end)
	}
}

func TestTamperingDetected(t *testing.T) {
	v, _, _ := newVault(t)
	plain := make([]byte, 3*chunkSize+100)
	rand.Read(plain)
	ct := seal(t, v, plain)
	read := func(b []byte) error {
		r, err := v.NewReader(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return err
		}
		_, err = io.ReadAll(r)
		return err
	}
	if err := read(ct); err != nil {
		t.Fatal(err)
	}
	flip := func(i int) []byte { c := append([]byte(nil), ct...); c[i] ^= 1; return c }
	for name, c := range map[string][]byte{
		"flipped byte in chunk 0":     flip(hdrSize + 5),
		"flipped byte in last chunk":  flip(len(ct) - 20),
		"flipped tag":                 flip(len(ct) - 1),
		"flipped header salt":         flip(20),
		"truncated at chunk boundary": ct[:hdrSize+2*(chunkSize+tagSize)],
		"truncated mid chunk":         ct[:len(ct)-30],
		"extra data appended":         append(append([]byte(nil), ct...), make([]byte, 40)...),
		"chunks swapped":              swapChunks(ct),
		"first chunk dropped":         append(append([]byte(nil), ct[:hdrSize]...), ct[hdrSize+chunkSize+tagSize:]...),
	} {
		if err := read(c); err == nil {
			t.Errorf("%s: tampering not detected", name)
		}
	}
}

func swapChunks(ct []byte) []byte {
	c := append([]byte(nil), ct...)
	sz := chunkSize + tagSize
	a, b := hdrSize, hdrSize+sz
	tmp := append([]byte(nil), c[a:a+sz]...)
	copy(c[a:a+sz], c[b:b+sz])
	copy(c[b:b+sz], tmp)
	return c
}

func TestPerFileKeysDiffer(t *testing.T) {
	v, _, _ := newVault(t)
	plain := bytes.Repeat([]byte("A"), 1000)
	a, b := seal(t, v, plain), seal(t, v, plain)
	if bytes.Equal(a[hdrSize:], b[hdrSize:]) || bytes.Equal(a[8:40], b[8:40]) {
		t.Error("identical files must not produce identical ciphertext (per-file key)")
	}
}

func TestLockedAndWrongKey(t *testing.T) {
	v, dir, _ := newVault(t)
	ct := seal(t, v, []byte("secret"))
	v.Lock()
	if !v.Locked() {
		t.Fatal("should be locked")
	}
	if _, err := v.NewWriter(io.Discard); err != ErrLocked {
		t.Errorf("writer while locked: %v", err)
	}
	if _, err := v.NewReader(bytes.NewReader(ct), int64(len(ct))); err != ErrLocked {
		t.Errorf("reader while locked: %v", err)
	}
	// a different vault (different master key) must not read it
	other, _, _ := Create(t.TempDir(), "another passphrase here")
	r, _ := other.NewReader(bytes.NewReader(ct), int64(len(ct)))
	if _, err := io.ReadAll(r); err == nil {
		t.Error("a different key decrypted the file")
	}
	if err := v.Unlock(dir, "wrong passphrase"); err != ErrBadSecret {
		t.Errorf("wrong passphrase: %v", err)
	}
	if err := v.Unlock(dir, "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	r, _ = v.NewReader(bytes.NewReader(ct), int64(len(ct)))
	if got, _ := io.ReadAll(r); string(got) != "secret" {
		t.Errorf("after unlock: %q", got)
	}
}

func TestRecoveryKeyAndPassphraseChange(t *testing.T) {
	v, dir, rec := newVault(t)
	ct := seal(t, v, []byte("hello"))
	v.Lock()
	fresh := New()
	if err := fresh.Unlock(dir, rec); err != nil {
		t.Fatalf("recovery key failed: %v", err)
	}
	if err := New().Unlock(dir, rec[:len(rec)-5]+"AAAAA"); err == nil {
		t.Error("damaged recovery key accepted")
	}
	if err := fresh.ChangePassphrase(dir, "correct horse battery staple", "a brand new passphrase!"); err != nil {
		t.Fatal(err)
	}
	if err := New().Unlock(dir, "correct horse battery staple"); err == nil {
		t.Error("old passphrase still works")
	}
	again := New()
	if err := again.Unlock(dir, "a brand new passphrase!"); err != nil {
		t.Fatal(err)
	}
	r, _ := again.NewReader(bytes.NewReader(ct), int64(len(ct)))
	if got, _ := io.ReadAll(r); string(got) != "hello" {
		t.Errorf("data unreadable after passphrase change: %q", got)
	}
	if err := New().Unlock(dir, rec); err != nil {
		t.Errorf("recovery key must survive a passphrase change: %v", err)
	}
}

func TestAutoUnlockKeyAndCreateOnce(t *testing.T) {
	v, dir, _ := newVault(t)
	k, _ := v.ExportKey()
	v.Lock()
	if err := v.UnlockWithKey(dir, k); err != nil || v.Locked() {
		t.Fatalf("unlock with key: %v", err)
	}
	if err := New().UnlockWithKey(dir, make([]byte, 32)); err != ErrBadSecret {
		t.Errorf("bogus key accepted: %v", err)
	}
	if _, _, err := Create(dir, "x y z"); err != ErrExists {
		t.Errorf("Create over an existing vault: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, KeyFile)); bytes.Contains(b, k) {
		t.Error("master key stored in the clear")
	}
}

func TestNotEncryptedDetected(t *testing.T) {
	v, _, _ := newVault(t)
	plain := []byte("just a plain text file that is long enough to pass the size check....")
	if _, err := v.NewReader(bytes.NewReader(plain), int64(len(plain))); err != ErrNotEncrypted {
		t.Errorf("plain file: %v", err)
	}
}
