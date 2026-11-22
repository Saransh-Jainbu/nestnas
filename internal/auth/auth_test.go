package auth

import (
	"encoding/hex"
	"testing"
)

// RFC 7914 / well-known vector: PBKDF2-HMAC-SHA256("password","salt",1) first 32 bytes.
func TestPBKDF2Vector(t *testing.T) {
	got := hex.EncodeToString(pbkdf2([]byte("password"), []byte("salt"), 1))
	want := "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	got = hex.EncodeToString(pbkdf2([]byte("password"), []byte("salt"), 4096))
	want = "c5e478d59288c841aa530db6845c4c8d962893a001ce4e11a4963873aa98134a"
	if got != want {
		t.Fatalf("4096: got %s want %s", got, want)
	}
}

func TestVerify(t *testing.T) {
	s := NewSalt()
	h := Hash("hunter2hunter2", s)
	if !Verify("hunter2hunter2", s, h) || Verify("nope", s, h) {
		t.Fatal("verify broken")
	}
}
