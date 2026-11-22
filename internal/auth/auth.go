// Package auth provides password hashing (PBKDF2-HMAC-SHA256, stdlib only).
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
)

const iterations = 200_000

// PBKDF2 derives keyLen bytes using PBKDF2-HMAC-SHA256 (RFC 8018).
func PBKDF2(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hLen := prf.Size()
	n := (keyLen + hLen - 1) / hLen
	dk := make([]byte, 0, n*hLen)
	var blk [4]byte
	for i := 1; i <= n; i++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(blk[:], uint32(i))
		prf.Write(blk[:])
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for j := 1; j < iter; j++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for k := range t {
				t[k] ^= u[k]
			}
		}
		dk = append(dk, t...)
	}
	return dk[:keyLen]
}

func pbkdf2(password, salt []byte, iter int) []byte { return PBKDF2(password, salt, iter, 32) }

// NewSalt returns a random hex salt.
func NewSalt() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Hash returns the hex hash of password with the given hex salt.
func Hash(password, saltHex string) string {
	salt, _ := hex.DecodeString(saltHex)
	return hex.EncodeToString(pbkdf2([]byte(password), salt, iterations))
}

// Verify checks password against a stored hash in constant time.
func Verify(password, saltHex, wantHex string) bool {
	got := Hash(password, saltHex)
	return subtle.ConstantTimeCompare([]byte(got), []byte(wantHex)) == 1
}

// Token returns a random URL-safe token with n random bytes.
func Token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
