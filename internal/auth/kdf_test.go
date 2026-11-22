package auth

import (
	"encoding/hex"
	"testing"
)

// RFC 7914 section 12 test vectors.
func TestScryptVectors(t *testing.T) {
	cases := []struct {
		pw, salt string
		N, r, p  int
		want     string
	}{
		{"", "", 16, 1, 1, "77d6576238657b203b19ca42c18a0497f16b4844e3074ae8dfdffa3fede21442fcd0069ded0948f8326a753a0fc81f17e8d3e0fb2e0d3628cf35e20c38d18906"},
		{"password", "NaCl", 1024, 8, 16, "fdbabe1c9d3472007856e7190d01e9fe7c6ad7cbc8237830e77376634b3731622eaf30d92e22a3886ff109279d9830dac727afb94a83ee6d8360cbdfa2cc0640"},
	}
	for _, c := range cases {
		got, err := Scrypt([]byte(c.pw), []byte(c.salt), c.N, c.r, c.p, 64)
		if err != nil || hex.EncodeToString(got) != c.want {
			t.Errorf("scrypt(%q,%q,%d,%d,%d) = %x, err %v", c.pw, c.salt, c.N, c.r, c.p, got, err)
		}
	}
}

// RFC 5869 test case 1.
func TestHKDFVector(t *testing.T) {
	ikm, _ := hex.DecodeString("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b")
	salt, _ := hex.DecodeString("000102030405060708090a0b0c")
	info, _ := hex.DecodeString("f0f1f2f3f4f5f6f7f8f9")
	got := hex.EncodeToString(HKDF(ikm, salt, info, 42))
	want := "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"
	if got != want {
		t.Errorf("hkdf = %s", got)
	}
}

// RFC 7914 PBKDF2-HMAC-SHA256 multi-block vector (c=1, dkLen=64).
func TestPBKDF2MultiBlock(t *testing.T) {
	got := hex.EncodeToString(PBKDF2([]byte("passwd"), []byte("salt"), 1, 64))
	want := "55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc49ca9cccf179b645991664b39d77ef317c71b845b1e30bd509112041d3a19783"
	if got != want {
		t.Errorf("pbkdf2 = %s", got)
	}
}

// Cross-checked against Python's hashlib.scrypt (OpenSSL) at the cost we actually use.
func TestScryptMatchesOpenSSL(t *testing.T) {
	got, err := Scrypt([]byte("correct horse"), []byte("saltsaltsalt"), 1<<15, 8, 1, 32)
	want := "262c9bf374f62f221265e7cee9d5df389a0becf1bfad3779b11a6b2fd23ce1fb"
	if err != nil || hex.EncodeToString(got) != want {
		t.Errorf("got %x err %v", got, err)
	}
}
