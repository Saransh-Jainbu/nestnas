package vision

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
)

// A small ONNX interpreter, just big enough for the two face models (YuNet and SFace): image
// networks made of convolutions, batch norm, a few activations, pooling and reshapes. It reads
// the protobuf file directly, so no extra software is needed. Unknown operators are rejected
// rather than guessed at.

type tensor struct {
	shape []int
	data  []float32
}

func (t *tensor) size() int {
	n := 1
	for _, d := range t.shape {
		n *= d
	}
	return n
}

type attr struct {
	i    int64
	f    float32
	s    string
	ints []int64
	has  map[string]bool
}

type node struct {
	op      string
	in, out []string
	a       map[string]*attr
}

// Net is a loaded network.
type Net struct {
	nodes []node
	inits map[string]*tensor
	ints  map[string][]int64 // integer constants (reshape targets, resize scales)
	input string
	outs  []string
	last  map[string]int // index of the last node that reads each tensor
	mu    sync.Mutex
}

// ---------- protobuf wire reading ----------

type pbReader struct {
	b []byte
	i int
}

func (r *pbReader) done() bool { return r.i >= len(r.b) }

func (r *pbReader) varint() (uint64, error) {
	var v uint64
	for s := uint(0); s < 64; s += 7 {
		if r.i >= len(r.b) {
			return 0, errors.New("truncated")
		}
		c := r.b[r.i]
		r.i++
		v |= uint64(c&0x7f) << s
		if c < 0x80 {
			return v, nil
		}
	}
	return 0, errors.New("bad varint")
}

// next returns field number, wire type and (for length-delimited) the payload.
func (r *pbReader) next() (field int, wt int, val uint64, payload []byte, err error) {
	k, err := r.varint()
	if err != nil {
		return
	}
	field, wt = int(k>>3), int(k&7)
	switch wt {
	case 0:
		val, err = r.varint()
	case 1:
		if r.i+8 > len(r.b) {
			err = errors.New("truncated")
			return
		}
		val = binary.LittleEndian.Uint64(r.b[r.i:])
		r.i += 8
	case 2:
		var n uint64
		if n, err = r.varint(); err != nil {
			return
		}
		if r.i+int(n) > len(r.b) {
			err = errors.New("truncated")
			return
		}
		payload = r.b[r.i : r.i+int(n)]
		r.i += int(n)
	case 5:
		if r.i+4 > len(r.b) {
			err = errors.New("truncated")
			return
		}
		val = uint64(binary.LittleEndian.Uint32(r.b[r.i:]))
		r.i += 4
	default:
		err = fmt.Errorf("unsupported wire type %d", wt)
	}
	return
}

func packedInts(p []byte) []int64 {
	r := &pbReader{b: p}
	var out []int64
	for !r.done() {
		v, err := r.varint()
		if err != nil {
			break
		}
		out = append(out, int64(v))
	}
	return out
}

func parseTensor(b []byte) (name string, t *tensor, ints []int64, err error) {
	r := &pbReader{b: b}
	var dims []int64
	var dtype int
	var raw []byte
	var floats []float32
	var i64 []int64
	for !r.done() {
		f, wt, v, p, e := r.next()
		if e != nil {
			return "", nil, nil, e
		}
		switch f {
		case 1:
			if wt == 2 {
				dims = append(dims, packedInts(p)...)
			} else {
				dims = append(dims, int64(v))
			}
		case 2:
			dtype = int(v)
		case 4:
			if wt == 2 {
				for k := 0; k+4 <= len(p); k += 4 {
					floats = append(floats, math.Float32frombits(binary.LittleEndian.Uint32(p[k:])))
				}
			} else {
				floats = append(floats, math.Float32frombits(uint32(v)))
			}
		case 7:
			if wt == 2 {
				i64 = append(i64, packedInts(p)...)
			} else {
				i64 = append(i64, int64(v))
			}
		case 8:
			name = string(p)
		case 9:
			raw = p
		}
	}
	shape := make([]int, len(dims))
	n := 1
	for i, d := range dims {
		shape[i] = int(d)
		n *= int(d)
	}
	switch dtype {
	case 1: // float
		if raw != nil {
			if len(raw) != n*4 {
				return "", nil, nil, errors.New("bad tensor size")
			}
			floats = make([]float32, n)
			for k := range floats {
				floats[k] = math.Float32frombits(binary.LittleEndian.Uint32(raw[k*4:]))
			}
		}
		if len(floats) != n {
			return "", nil, nil, errors.New("bad tensor size")
		}
		return name, &tensor{shape, floats}, nil, nil
	case 7: // int64
		if raw != nil {
			for k := 0; k+8 <= len(raw); k += 8 {
				i64 = append(i64, int64(binary.LittleEndian.Uint64(raw[k:])))
			}
		}
		return name, nil, i64, nil
	}
	return name, nil, nil, fmt.Errorf("unsupported tensor type %d", dtype)
}

func parseAttr(b []byte) (string, *attr, error) {
	r := &pbReader{b: b}
	a := &attr{}
	var name string
	for !r.done() {
		f, wt, v, p, e := r.next()
		if e != nil {
			return "", nil, e
		}
		switch f {
		case 1:
			name = string(p)
		case 2:
			a.f = math.Float32frombits(uint32(v))
		case 3:
			a.i = int64(v)
		case 4:
			a.s = string(p)
		case 8:
			if wt == 2 {
				a.ints = append(a.ints, packedInts(p)...)
			} else {
				a.ints = append(a.ints, int64(v))
			}
		}
	}
	return name, a, nil
}

func parseNode(b []byte) (node, error) {
	n := node{a: map[string]*attr{}}
	r := &pbReader{b: b}
	for !r.done() {
		f, _, _, p, e := r.next()
		if e != nil {
			return n, e
		}
		switch f {
		case 1:
			n.in = append(n.in, string(p))
		case 2:
			n.out = append(n.out, string(p))
		case 4:
			n.op = string(p)
		case 5:
			name, a, e := parseAttr(p)
			if e != nil {
				return n, e
			}
			n.a[name] = a
		}
	}
	return n, nil
}

// LoadNet reads an ONNX file.
func LoadNet(path string) (*Net, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var graph []byte
	r := &pbReader{b: b}
	for !r.done() {
		f, _, _, p, e := r.next()
		if e != nil {
			return nil, e
		}
		if f == 7 {
			graph = p
		}
	}
	if graph == nil {
		return nil, errors.New("not an ONNX model")
	}
	net := &Net{inits: map[string]*tensor{}, ints: map[string][]int64{}}
	gr := &pbReader{b: graph}
	var inputs []string
	for !gr.done() {
		f, _, _, p, e := gr.next()
		if e != nil {
			return nil, e
		}
		switch f {
		case 1:
			n, e := parseNode(p)
			if e != nil {
				return nil, e
			}
			net.nodes = append(net.nodes, n)
		case 5:
			name, t, ints, e := parseTensor(p)
			if e != nil {
				return nil, e
			}
			if t != nil {
				net.inits[name] = t
			} else {
				net.ints[name] = ints
			}
		case 11, 12:
			vr := &pbReader{b: p}
			for !vr.done() {
				vf, _, _, vp, e := vr.next()
				if e != nil {
					return nil, e
				}
				if vf == 1 {
					if f == 11 {
						inputs = append(inputs, string(vp))
					} else {
						net.outs = append(net.outs, string(vp))
					}
				}
			}
		}
	}
	for _, in := range inputs {
		if net.inits[in] == nil && net.ints[in] == nil {
			net.input = in
		}
	}
	if net.input == "" || len(net.outs) == 0 {
		return nil, errors.New("model has no input or output")
	}
	net.last = map[string]int{}
	for i, n := range net.nodes {
		for _, in := range n.in {
			net.last[in] = i
		}
	}
	for _, n := range net.nodes {
		switch n.op {
		case "Conv", "Relu", "PRelu", "Sigmoid", "MaxPool", "Resize", "Add", "Sub", "Mul", "Transpose", "Reshape", "BatchNormalization", "Dropout", "Flatten", "Gemm":
		default:
			return nil, fmt.Errorf("the model uses an operator NestNAS does not support: %s", n.op)
		}
	}
	return net, nil
}

// Run executes the network on one input and returns every named output.
func (n *Net) Run(in *tensor) (map[string]*tensor, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	vals := map[string]*tensor{n.input: in}
	get := func(name string) *tensor {
		if t := vals[name]; t != nil {
			return t
		}
		return n.inits[name]
	}
	isOut := map[string]bool{}
	for _, o := range n.outs {
		isOut[o] = true
	}
	for ni, nd := range n.nodes {
		var out *tensor
		var err error
		switch nd.op {
		case "Conv":
			out, err = convOp(get(nd.in[0]), get(nd.in[1]), optional(nd.in, 2, get), nd.a)
		case "Relu":
			out = mapT(get(nd.in[0]), func(x float32) float32 {
				if x < 0 {
					return 0
				}
				return x
			})
		case "Sigmoid":
			out = mapT(get(nd.in[0]), func(x float32) float32 { return float32(1 / (1 + math.Exp(-float64(x)))) })
		case "PRelu":
			out, err = preluOp(get(nd.in[0]), get(nd.in[1]))
		case "BatchNormalization":
			out, err = bnOp(get(nd.in[0]), get(nd.in[1]), get(nd.in[2]), get(nd.in[3]), get(nd.in[4]), nd.a)
		case "Add", "Sub", "Mul":
			out, err = binOp(nd.op, get(nd.in[0]), get(nd.in[1]))
		case "MaxPool":
			out, err = maxPoolOp(get(nd.in[0]), nd.a)
		case "Resize":
			out, err = n.resizeOp(get(nd.in[0]), nd)
		case "Transpose":
			out, err = transposeOp(get(nd.in[0]), nd.a)
		case "Reshape":
			out, err = reshapeOp(get(nd.in[0]), n.ints[nd.in[1]])
		case "Flatten":
			t := get(nd.in[0])
			rest := 1
			for _, d := range t.shape[1:] {
				rest *= d
			}
			out = &tensor{[]int{t.shape[0], rest}, t.data}
		case "Dropout":
			out = get(nd.in[0])
		case "Gemm":
			out, err = gemmOp(get(nd.in[0]), get(nd.in[1]), optional(nd.in, 2, get), nd.a)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", nd.op, err)
		}
		if out == nil {
			return nil, fmt.Errorf("%s: missing input", nd.op)
		}
		vals[nd.out[0]] = out
		for _, in := range nd.in { // let go of tensors nothing else needs: a big picture has many layers
			if n.last[in] == ni && !isOut[in] && in != n.input {
				delete(vals, in)
			}
		}
	}
	res := map[string]*tensor{}
	for _, o := range n.outs {
		res[o] = vals[o]
	}
	return res, nil
}

func optional(names []string, i int, get func(string) *tensor) *tensor {
	if i < len(names) && names[i] != "" {
		return get(names[i])
	}
	return nil
}

func mapT(t *tensor, f func(float32) float32) *tensor {
	out := &tensor{t.shape, make([]float32, len(t.data))}
	for i, v := range t.data {
		out.data[i] = f(v)
	}
	return out
}

func getInts(a *attr, def ...int) []int {
	if a == nil || len(a.ints) == 0 {
		return def
	}
	o := make([]int, len(a.ints))
	for i, v := range a.ints {
		o[i] = int(v)
	}
	return o
}

func convOp(x, w, b *tensor, a map[string]*attr) (*tensor, error) {
	if x == nil || w == nil || len(x.shape) != 4 || len(w.shape) != 4 || x.shape[0] != 1 {
		return nil, errors.New("unsupported shapes")
	}
	cin, h, wd := x.shape[1], x.shape[2], x.shape[3]
	cout, cg, kh, kw := w.shape[0], w.shape[1], w.shape[2], w.shape[3]
	groups := 1
	if g := a["group"]; g != nil {
		groups = int(g.i)
	}
	st := getInts(a["strides"], 1, 1)
	pads := getInts(a["pads"], 0, 0, 0, 0)
	dil := getInts(a["dilations"], 1, 1)
	if cg*groups != cin || cout%groups != 0 {
		return nil, errors.New("bad group configuration")
	}
	oh := (h+pads[0]+pads[2]-dil[0]*(kh-1)-1)/st[0] + 1
	ow := (wd+pads[1]+pads[3]-dil[1]*(kw-1)-1)/st[1] + 1
	out := &tensor{[]int{1, cout, oh, ow}, make([]float32, cout*oh*ow)}
	opg := cout / groups
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for o := 0; o < cout; o++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(o int) {
			defer func() { <-sem; wg.Done() }()
			g := o / opg
			var bias float32
			if b != nil {
				bias = b.data[o]
			}
			dst := out.data[o*oh*ow : (o+1)*oh*ow]
			for i := range dst {
				dst[i] = bias
			}
			for c := 0; c < cg; c++ {
				src := x.data[(g*cg+c)*h*wd : (g*cg+c+1)*h*wd]
				ker := w.data[(o*cg+c)*kh*kw : (o*cg+c+1)*kh*kw]
				for ky := 0; ky < kh; ky++ {
					for kx := 0; kx < kw; kx++ {
						wv := ker[ky*kw+kx]
						if wv == 0 {
							continue
						}
						for y := 0; y < oh; y++ {
							iy := y*st[0] - pads[0] + ky*dil[0]
							if iy < 0 || iy >= h {
								continue
							}
							row := src[iy*wd : (iy+1)*wd]
							d := dst[y*ow : (y+1)*ow]
							ix := -pads[1] + kx*dil[1]
							if st[1] == 1 {
								lo, hi := 0, ow
								if ix < 0 {
									lo = -ix
								}
								if ix+ow > wd {
									hi = wd - ix
								}
								for xx := lo; xx < hi; xx++ {
									d[xx] += wv * row[ix+xx]
								}
							} else {
								for xx := 0; xx < ow; xx++ {
									sx := xx*st[1] + ix
									if sx >= 0 && sx < wd {
										d[xx] += wv * row[sx]
									}
								}
							}
						}
					}
				}
			}
		}(o)
	}
	wg.Wait()
	return out, nil
}

func preluOp(x, slope *tensor) (*tensor, error) {
	out := &tensor{x.shape, make([]float32, len(x.data))}
	c := 1
	if len(x.shape) == 4 {
		c = x.shape[1]
	}
	per := len(x.data) / c
	for i, v := range x.data {
		s := slope.data[0]
		if len(slope.data) == c {
			s = slope.data[i/per]
		} else if len(slope.data) != 1 {
			return nil, errors.New("unsupported slope shape")
		}
		if v < 0 {
			v *= s
		}
		out.data[i] = v
	}
	return out, nil
}

func bnOp(x, scale, bias, mean, vr *tensor, a map[string]*attr) (*tensor, error) {
	eps := float32(1e-5)
	if e := a["epsilon"]; e != nil {
		eps = e.f
	}
	c := x.shape[1]
	per := len(x.data) / c
	out := &tensor{x.shape, make([]float32, len(x.data))}
	for ch := 0; ch < c; ch++ {
		inv := scale.data[ch] / float32(math.Sqrt(float64(vr.data[ch]+eps)))
		off := bias.data[ch] - mean.data[ch]*inv
		for i := ch * per; i < (ch+1)*per; i++ {
			out.data[i] = x.data[i]*inv + off
		}
	}
	return out, nil
}

func binOp(op string, a, b *tensor) (*tensor, error) {
	f := func(x, y float32) float32 {
		switch op {
		case "Add":
			return x + y
		case "Sub":
			return x - y
		}
		return x * y
	}
	if len(b.data) == 1 {
		out := &tensor{a.shape, make([]float32, len(a.data))}
		for i, v := range a.data {
			out.data[i] = f(v, b.data[0])
		}
		return out, nil
	}
	if len(a.data) == 1 {
		out := &tensor{b.shape, make([]float32, len(b.data))}
		for i, v := range b.data {
			out.data[i] = f(a.data[0], v)
		}
		return out, nil
	}
	if len(a.data) != len(b.data) {
		return nil, errors.New("unsupported broadcast")
	}
	out := &tensor{a.shape, make([]float32, len(a.data))}
	for i := range a.data {
		out.data[i] = f(a.data[i], b.data[i])
	}
	return out, nil
}

func maxPoolOp(x *tensor, a map[string]*attr) (*tensor, error) {
	k := getInts(a["kernel_shape"])
	st := getInts(a["strides"], 1, 1)
	pads := getInts(a["pads"], 0, 0, 0, 0)
	if len(k) != 2 {
		return nil, errors.New("unsupported pooling")
	}
	c, h, w := x.shape[1], x.shape[2], x.shape[3]
	oh := (h+pads[0]+pads[2]-k[0])/st[0] + 1
	ow := (w+pads[1]+pads[3]-k[1])/st[1] + 1
	out := &tensor{[]int{1, c, oh, ow}, make([]float32, c*oh*ow)}
	for ch := 0; ch < c; ch++ {
		for y := 0; y < oh; y++ {
			for xx := 0; xx < ow; xx++ {
				best := float32(math.Inf(-1))
				for ky := 0; ky < k[0]; ky++ {
					for kx := 0; kx < k[1]; kx++ {
						iy, ix := y*st[0]-pads[0]+ky, xx*st[1]-pads[1]+kx
						if iy >= 0 && iy < h && ix >= 0 && ix < w {
							if v := x.data[(ch*h+iy)*w+ix]; v > best {
								best = v
							}
						}
					}
				}
				out.data[(ch*oh+y)*ow+xx] = best
			}
		}
	}
	return out, nil
}

// resizeOp supports nearest-neighbour resizing by constant scales (what the face detector uses).
func (n *Net) resizeOp(x *tensor, nd node) (*tensor, error) {
	var scales []float32
	if len(nd.in) >= 3 && nd.in[2] != "" {
		if t := n.inits[nd.in[2]]; t != nil {
			scales = t.data
		}
	}
	if len(scales) != 4 {
		return nil, errors.New("only constant scales are supported")
	}
	c, h, w := x.shape[1], x.shape[2], x.shape[3]
	oh, ow := int(float32(h)*scales[2]), int(float32(w)*scales[3])
	out := &tensor{[]int{1, c, oh, ow}, make([]float32, c*oh*ow)}
	for ch := 0; ch < c; ch++ {
		for y := 0; y < oh; y++ {
			sy := int(math.Floor(float64(y) / float64(scales[2])))
			if sy >= h {
				sy = h - 1
			}
			for xx := 0; xx < ow; xx++ {
				sx := int(math.Floor(float64(xx) / float64(scales[3])))
				if sx >= w {
					sx = w - 1
				}
				out.data[(ch*oh+y)*ow+xx] = x.data[(ch*h+sy)*w+sx]
			}
		}
	}
	return out, nil
}

func transposeOp(x *tensor, a map[string]*attr) (*tensor, error) {
	perm := getInts(a["perm"])
	r := len(x.shape)
	if len(perm) != r {
		return nil, errors.New("bad permutation")
	}
	ns := make([]int, r)
	for i, p := range perm {
		ns[i] = x.shape[p]
	}
	stride := make([]int, r)
	s := 1
	for i := r - 1; i >= 0; i-- {
		stride[i] = s
		s *= x.shape[i]
	}
	out := &tensor{ns, make([]float32, len(x.data))}
	idx := make([]int, r)
	for o := range out.data {
		src := 0
		for i := 0; i < r; i++ {
			src += idx[i] * stride[perm[i]]
		}
		out.data[o] = x.data[src]
		for i := r - 1; i >= 0; i-- {
			idx[i]++
			if idx[i] < ns[i] {
				break
			}
			idx[i] = 0
		}
	}
	return out, nil
}

func reshapeOp(x *tensor, shape []int64) (*tensor, error) {
	ns := make([]int, len(shape))
	neg, known := -1, 1
	for i, d := range shape {
		switch {
		case d == 0:
			ns[i] = x.shape[i]
		case d < 0:
			neg = i
			continue
		default:
			ns[i] = int(d)
		}
		known *= ns[i]
	}
	if neg >= 0 {
		if known == 0 {
			return nil, errors.New("bad reshape")
		}
		ns[neg] = len(x.data) / known
	}
	out := &tensor{ns, x.data}
	if out.size() != len(x.data) {
		return nil, errors.New("reshape size mismatch")
	}
	return out, nil
}

func gemmOp(a, b, c *tensor, at map[string]*attr) (*tensor, error) {
	transB := at["transB"] != nil && at["transB"].i == 1
	alpha, beta := float32(1), float32(1)
	if v := at["alpha"]; v != nil && v.f != 0 {
		alpha = v.f
	}
	if v := at["beta"]; v != nil && v.f != 0 {
		beta = v.f
	}
	m, k := a.shape[0], a.shape[1]
	var nOut int
	if transB {
		nOut = b.shape[0]
	} else {
		nOut = b.shape[1]
	}
	out := &tensor{[]int{m, nOut}, make([]float32, m*nOut)}
	for i := 0; i < m; i++ {
		for j := 0; j < nOut; j++ {
			var s float32
			for l := 0; l < k; l++ {
				if transB {
					s += a.data[i*k+l] * b.data[j*k+l]
				} else {
					s += a.data[i*k+l] * b.data[l*nOut+j]
				}
			}
			s *= alpha
			if c != nil {
				if len(c.data) == nOut {
					s += beta * c.data[j]
				} else {
					s += beta * c.data[0]
				}
			}
			out.data[i*nOut+j] = s
		}
	}
	return out, nil
}
