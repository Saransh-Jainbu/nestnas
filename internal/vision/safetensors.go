package vision

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// A minimal reader for the safetensors format: an 8-byte header length, a JSON header that
// maps tensor names to dtype, shape and byte range, then the raw tensor bytes.

type tensorInfo struct {
	DType   string   `json:"dtype"`
	Shape   []int    `json:"shape"`
	Offsets [2]int64 `json:"data_offsets"`
}

type tensorFile struct {
	f     *os.File
	base  int64
	infos map[string]tensorInfo
}

func openTensors(path string) (*tensorFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var n uint64
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil || n > 64<<20 {
		f.Close()
		return nil, errors.New("not a safetensors file")
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(f, hdr); err != nil {
		f.Close()
		return nil, err
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(hdr, &raw); err != nil {
		f.Close()
		return nil, err
	}
	t := &tensorFile{f: f, base: 8 + int64(n), infos: map[string]tensorInfo{}}
	for k, v := range raw {
		if k == "__metadata__" {
			continue
		}
		var ti tensorInfo
		if err := json.Unmarshal(v, &ti); err != nil {
			f.Close()
			return nil, err
		}
		t.infos[k] = ti
	}
	return t, nil
}

func (t *tensorFile) close() { t.f.Close() }

// get reads a tensor as float32 (converting from half precision if needed).
func (t *tensorFile) get(name string, want ...int) ([]float32, error) {
	ti, ok := t.infos[name]
	if !ok {
		return nil, fmt.Errorf("model is missing %s", name)
	}
	count := 1
	for _, d := range ti.Shape {
		count *= d
	}
	if len(want) > 0 {
		w := 1
		for _, d := range want {
			w *= d
		}
		if w != count {
			return nil, fmt.Errorf("%s has %d values, expected %d", name, count, w)
		}
	}
	size := ti.Offsets[1] - ti.Offsets[0]
	buf := make([]byte, size)
	if _, err := t.f.ReadAt(buf, t.base+ti.Offsets[0]); err != nil {
		return nil, err
	}
	out := make([]float32, count)
	switch ti.DType {
	case "F32":
		if int(size) != count*4 {
			return nil, fmt.Errorf("%s: bad size", name)
		}
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
		}
	case "F16":
		for i := range out {
			out[i] = half(binary.LittleEndian.Uint16(buf[i*2:]))
		}
	case "BF16":
		for i := range out {
			out[i] = math.Float32frombits(uint32(binary.LittleEndian.Uint16(buf[i*2:])) << 16)
		}
	default:
		return nil, fmt.Errorf("%s: unsupported type %s", name, ti.DType)
	}
	return out, nil
}

func half(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	frac := uint32(h) & 0x3ff
	switch {
	case exp == 0 && frac == 0:
		return math.Float32frombits(sign)
	case exp == 0: // subnormal
		e := uint32(127 - 15 + 1)
		for frac&0x400 == 0 {
			frac <<= 1
			e--
		}
		frac &= 0x3ff
		return math.Float32frombits(sign | e<<23 | frac<<13)
	case exp == 31:
		return math.Float32frombits(sign | 0x7f800000 | frac<<13)
	}
	return math.Float32frombits(sign | (exp+127-15)<<23 | frac<<13)
}
