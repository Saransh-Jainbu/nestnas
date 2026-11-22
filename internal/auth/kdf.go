package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math/bits"
)

// HKDF is HKDF-SHA256 (RFC 5869): derive n bytes from a high-entropy secret.
func HKDF(secret, salt, info []byte, n int) []byte {
	if salt == nil {
		salt = make([]byte, sha256.Size)
	}
	ext := hmac.New(sha256.New, salt)
	ext.Write(secret)
	prk := ext.Sum(nil)
	var out, t []byte
	for i := byte(1); len(out) < n; i++ {
		m := hmac.New(sha256.New, prk)
		m.Write(t)
		m.Write(info)
		m.Write([]byte{i})
		t = m.Sum(nil)
		out = append(out, t...)
	}
	return out[:n]
}

// Scrypt is the memory-hard password KDF from RFC 7914. Use it to turn a passphrase into a key:
// unlike PBKDF2 it costs an attacker lots of RAM per guess, which blunts GPU/ASIC cracking.
func Scrypt(password, salt []byte, N, r, p, keyLen int) ([]byte, error) {
	if N <= 1 || N&(N-1) != 0 || r < 1 || p < 1 || uint64(r)*uint64(p) >= 1<<30 {
		return nil, errors.New("scrypt: invalid parameters")
	}
	b := PBKDF2(password, salt, 1, p*128*r)
	xy := make([]uint32, 64*r)
	v := make([]uint32, 32*r*N)
	var tmp [16]uint32
	for i := 0; i < p; i++ {
		smix(b[i*128*r:], r, N, v, xy, &tmp)
	}
	return PBKDF2(password, b, 1, keyLen), nil
}

func smix(b []byte, r, N int, v, xy []uint32, tmp *[16]uint32) {
	x, y := xy[:32*r], xy[32*r:]
	for i := range x {
		x[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	for i := 0; i < N; i++ {
		copy(v[i*32*r:], x)
		blockMix(tmp, x, y, r)
		x, y = y, x
	}
	for i := 0; i < N; i++ {
		j := int(x[(2*r-1)*16] & uint32(N-1))
		vj := v[j*32*r : (j+1)*32*r]
		for k := range x {
			x[k] ^= vj[k]
		}
		blockMix(tmp, x, y, r)
		x, y = y, x
	}
	for i, w := range x {
		binary.LittleEndian.PutUint32(b[i*4:], w)
	}
}

func blockMix(tmp *[16]uint32, in, out []uint32, r int) {
	copy(tmp[:], in[(2*r-1)*16:])
	for i := 0; i < 2*r; i += 2 {
		salsaXOR(tmp, in[i*16:], out[i*8:])
		salsaXOR(tmp, in[i*16+16:], out[i*8+r*16:])
	}
}

func salsaXOR(tmp *[16]uint32, in, out []uint32) {
	var w [16]uint32
	for i := range w {
		w[i] = tmp[i] ^ in[i]
	}
	x0, x1, x2, x3, x4, x5, x6, x7 := w[0], w[1], w[2], w[3], w[4], w[5], w[6], w[7]
	x8, x9, x10, x11, x12, x13, x14, x15 := w[8], w[9], w[10], w[11], w[12], w[13], w[14], w[15]
	rl := bits.RotateLeft32
	for i := 0; i < 8; i += 2 {
		x4 ^= rl(x0+x12, 7)
		x8 ^= rl(x4+x0, 9)
		x12 ^= rl(x8+x4, 13)
		x0 ^= rl(x12+x8, 18)
		x9 ^= rl(x5+x1, 7)
		x13 ^= rl(x9+x5, 9)
		x1 ^= rl(x13+x9, 13)
		x5 ^= rl(x1+x13, 18)
		x14 ^= rl(x10+x6, 7)
		x2 ^= rl(x14+x10, 9)
		x6 ^= rl(x2+x14, 13)
		x10 ^= rl(x6+x2, 18)
		x3 ^= rl(x15+x11, 7)
		x7 ^= rl(x3+x15, 9)
		x11 ^= rl(x7+x3, 13)
		x15 ^= rl(x11+x7, 18)

		x1 ^= rl(x0+x3, 7)
		x2 ^= rl(x1+x0, 9)
		x3 ^= rl(x2+x1, 13)
		x0 ^= rl(x3+x2, 18)
		x6 ^= rl(x5+x4, 7)
		x7 ^= rl(x6+x5, 9)
		x4 ^= rl(x7+x6, 13)
		x5 ^= rl(x4+x7, 18)
		x11 ^= rl(x10+x9, 7)
		x8 ^= rl(x11+x10, 9)
		x9 ^= rl(x8+x11, 13)
		x10 ^= rl(x9+x8, 18)
		x12 ^= rl(x15+x14, 7)
		x13 ^= rl(x12+x15, 9)
		x14 ^= rl(x13+x12, 13)
		x15 ^= rl(x14+x13, 18)
	}
	res := [16]uint32{x0 + w[0], x1 + w[1], x2 + w[2], x3 + w[3], x4 + w[4], x5 + w[5], x6 + w[6], x7 + w[7],
		x8 + w[8], x9 + w[9], x10 + w[10], x11 + w[11], x12 + w[12], x13 + w[13], x14 + w[14], x15 + w[15]}
	*tmp = res
	copy(out, res[:])
}
