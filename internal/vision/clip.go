// Package vision runs a small CLIP model (an image encoder and a text encoder that share one
// embedding space) in pure Go, so NestNAS can search photos by what they show without any
// extra software. Weights are read from a Hugging Face "CLIPModel" safetensors file.
package vision

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type tower struct {
	hidden, inter, layers, heads int
	eps                          float32
	preLN                        *norm // vision only
	postLN                       *norm // vision: after pooling; text: final layer norm
	blocks                       []block
}

type norm struct{ w, b []float32 }

type lin struct {
	w, b    []float32 // w is [out][in]
	in, out int
}

type block struct {
	ln1, ln2   norm
	q, k, v, o lin
	fc1, fc2   lin
}

// Model holds both encoders.
type Model struct {
	dim    int // size of the shared embedding
	vis    tower
	txt    tower
	patch  int
	size   int
	mean   [3]float32
	std    [3]float32
	cls    []float32
	vpos   []float32
	patchW lin
	vproj  lin
	tokEmb []float32
	tpos   []float32
	tproj  lin
	tok    *tokenizer
	maxTok int
	mu     sync.Mutex // encoders reuse scratch memory per call; keep calls apart
}

type config struct {
	ProjectionDim int `json:"projection_dim"`
	Text          struct {
		Hidden int     `json:"hidden_size"`
		Inter  int     `json:"intermediate_size"`
		Layers int     `json:"num_hidden_layers"`
		Heads  int     `json:"num_attention_heads"`
		Vocab  int     `json:"vocab_size"`
		MaxPos int     `json:"max_position_embeddings"`
		Eps    float32 `json:"layer_norm_eps"`
		Act    string  `json:"hidden_act"`
	} `json:"text_config"`
	Vision struct {
		Hidden int     `json:"hidden_size"`
		Inter  int     `json:"intermediate_size"`
		Layers int     `json:"num_hidden_layers"`
		Heads  int     `json:"num_attention_heads"`
		Patch  int     `json:"patch_size"`
		Size   int     `json:"image_size"`
		Eps    float32 `json:"layer_norm_eps"`
		Act    string  `json:"hidden_act"`
	} `json:"vision_config"`
}

// Load reads config.json, tokenizer.json and model.safetensors from dir.
func Load(dir string) (*Model, error) {
	cb, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	var c config
	if err := json.Unmarshal(cb, &c); err != nil {
		return nil, err
	}
	if c.Text.Act != "gelu" || c.Vision.Act != "gelu" {
		return nil, fmt.Errorf("unsupported activation %q/%q", c.Text.Act, c.Vision.Act)
	}
	tok, err := loadTokenizer(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	tf, err := openTensors(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, err
	}
	defer tf.close()
	m := &Model{dim: c.ProjectionDim, patch: c.Vision.Patch, size: c.Vision.Size, tok: tok, maxTok: c.Text.MaxPos,
		mean: [3]float32{0.48145466, 0.4578275, 0.40821073}, std: [3]float32{0.26862954, 0.26130258, 0.27577711}}
	var lerr error
	get := func(name string, shape ...int) []float32 {
		if lerr != nil {
			return nil
		}
		v, err := tf.get(name, shape...)
		if err != nil {
			lerr = err
		}
		return v
	}
	getLin := func(prefix string, out, in int, bias bool) lin {
		l := lin{w: get(prefix+".weight", out, in), in: in, out: out}
		if bias {
			l.b = get(prefix+".bias", out)
		}
		return l
	}
	getNorm := func(prefix string, d int) norm { return norm{get(prefix+".weight", d), get(prefix+".bias", d)} }
	build := func(prefix string, hidden, inter, layers, heads int, eps float32) tower {
		t := tower{hidden: hidden, inter: inter, layers: layers, heads: heads, eps: eps}
		for i := 0; i < layers; i++ {
			p := fmt.Sprintf("%s.encoder.layers.%d.", prefix, i)
			t.blocks = append(t.blocks, block{
				ln1: getNorm(p+"layer_norm1", hidden), ln2: getNorm(p+"layer_norm2", hidden),
				q: getLin(p+"self_attn.q_proj", hidden, hidden, true), k: getLin(p+"self_attn.k_proj", hidden, hidden, true),
				v: getLin(p+"self_attn.v_proj", hidden, hidden, true), o: getLin(p+"self_attn.out_proj", hidden, hidden, true),
				fc1: getLin(p+"mlp.fc1", inter, hidden, true), fc2: getLin(p+"mlp.fc2", hidden, inter, true),
			})
		}
		return t
	}
	v := c.Vision
	if v.Heads == 0 || v.Hidden%v.Heads != 0 || c.Text.Heads == 0 || c.Text.Hidden%c.Text.Heads != 0 || v.Patch == 0 || v.Size%v.Patch != 0 {
		return nil, errors.New("unsupported model shape")
	}
	m.vis = build("vision_model", v.Hidden, v.Inter, v.Layers, v.Heads, v.Eps)
	n := v.Size / v.Patch
	m.cls = get("vision_model.embeddings.class_embedding", v.Hidden)
	m.patchW = getLin("vision_model.embeddings.patch_embedding", v.Hidden, 3*v.Patch*v.Patch, false)
	m.vpos = get("vision_model.embeddings.position_embedding.weight", n*n+1, v.Hidden)
	pre := getNorm("vision_model.pre_layrnorm", v.Hidden)
	post := getNorm("vision_model.post_layernorm", v.Hidden)
	m.vis.preLN, m.vis.postLN = &pre, &post
	m.vproj = getLin("visual_projection", c.ProjectionDim, v.Hidden, false)
	t := c.Text
	m.txt = build("text_model", t.Hidden, t.Inter, t.Layers, t.Heads, t.Eps)
	m.tokEmb = get("text_model.embeddings.token_embedding.weight", t.Vocab, t.Hidden)
	m.tpos = get("text_model.embeddings.position_embedding.weight", t.MaxPos, t.Hidden)
	fin := getNorm("text_model.final_layer_norm", t.Hidden)
	m.txt.postLN = &fin
	m.tproj = getLin("text_projection", c.ProjectionDim, t.Hidden, false)
	if lerr != nil {
		return nil, lerr
	}
	return m, nil
}

// Dim is the length of the embeddings.
func (m *Model) Dim() int { return m.dim }

// ---------- numeric kernels ----------

var workers = func() int {
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	if n > 8 {
		n = 8
	}
	return n
}()

// SetWorkers limits how many CPU cores the model may use at once.
func SetWorkers(n int) {
	if n < 1 {
		n = 1
	}
	workers = n
}

func dot(a, b []float32) float32 {
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}

// matmul computes y[n][out] = x[n][in] * w^T + b, splitting the output columns over workers.
func (l *lin) apply(x []float32, n int) []float32 {
	y := make([]float32, n*l.out)
	wk := workers
	if wk > l.out {
		wk = l.out
	}
	var wg sync.WaitGroup
	chunk := (l.out + wk - 1) / wk
	for s := 0; s < l.out; s += chunk {
		e := s + chunk
		if e > l.out {
			e = l.out
		}
		wg.Add(1)
		go func(s, e int) {
			defer wg.Done()
			for o := s; o < e; o++ {
				wr := l.w[o*l.in : (o+1)*l.in]
				var bias float32
				if l.b != nil {
					bias = l.b[o]
				}
				for i := 0; i < n; i++ {
					y[i*l.out+o] = dot(x[i*l.in:(i+1)*l.in], wr) + bias
				}
			}
		}(s, e)
	}
	wg.Wait()
	return y
}

func (nm *norm) apply(x []float32, n, d int, eps float32) []float32 {
	y := make([]float32, len(x))
	for i := 0; i < n; i++ {
		r := x[i*d : (i+1)*d]
		var mean float64
		for _, v := range r {
			mean += float64(v)
		}
		mean /= float64(d)
		var vr float64
		for _, v := range r {
			dv := float64(v) - mean
			vr += dv * dv
		}
		inv := float32(1 / math.Sqrt(vr/float64(d)+float64(eps)))
		o := y[i*d : (i+1)*d]
		for j, v := range r {
			o[j] = (v-float32(mean))*inv*nm.w[j] + nm.b[j]
		}
	}
	return y
}

func gelu(x []float32) {
	for i, v := range x {
		x[i] = 0.5 * v * float32(1+math.Erf(float64(v)*0.7071067811865476))
	}
}

// attention does multi-head self attention over n tokens (optionally causal).
func (t *tower) attention(b *block, x []float32, n int, causal bool) []float32 {
	d := t.hidden
	q, k, v := b.q.apply(x, n), b.k.apply(x, n), b.v.apply(x, n)
	hd := d / t.heads
	scale := float32(1 / math.Sqrt(float64(hd)))
	out := make([]float32, n*d)
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)
	for h := 0; h < t.heads; h++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(h int) {
			defer func() { <-sem; wg.Done() }()
			sc := make([]float32, n)
			off := h * hd
			for i := 0; i < n; i++ {
				qi := q[i*d+off : i*d+off+hd]
				lim := n
				if causal {
					lim = i + 1
				}
				mx := float32(math.Inf(-1))
				for j := 0; j < lim; j++ {
					s := dot(qi, k[j*d+off:j*d+off+hd]) * scale
					sc[j] = s
					if s > mx {
						mx = s
					}
				}
				var sum float32
				for j := 0; j < lim; j++ {
					sc[j] = float32(math.Exp(float64(sc[j] - mx)))
					sum += sc[j]
				}
				oi := out[i*d+off : i*d+off+hd]
				for j := 0; j < lim; j++ {
					w := sc[j] / sum
					vj := v[j*d+off : j*d+off+hd]
					for c := range oi {
						oi[c] += w * vj[c]
					}
				}
			}
		}(h)
	}
	wg.Wait()
	return b.o.apply(out, n)
}

func (t *tower) run(x []float32, n int, causal bool) []float32 {
	d := t.hidden
	for i := range t.blocks {
		b := &t.blocks[i]
		a := t.attention(b, b.ln1.apply(x, n, d, t.eps), n, causal)
		for j := range x {
			x[j] += a[j]
		}
		h := b.fc1.apply(b.ln2.apply(x, n, d, t.eps), n)
		gelu(h)
		f := b.fc2.apply(h, n)
		for j := range x {
			x[j] += f[j]
		}
	}
	return x
}

func l2(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(s))
	for i := range v {
		v[i] *= inv
	}
	return v
}

// EmbedText returns the unit-length embedding of a text query.
func (m *Model) EmbedText(text string) []float32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.tok.encode(text, m.maxTok)
	n, d := len(ids), m.txt.hidden
	x := make([]float32, n*d)
	for i, id := range ids {
		e, p := m.tokEmb[id*d:(id+1)*d], m.tpos[i*d:(i+1)*d]
		for j := 0; j < d; j++ {
			x[i*d+j] = e[j] + p[j]
		}
	}
	x = m.txt.run(x, n, true)
	x = m.txt.postLN.apply(x, n, d, m.txt.eps)
	return l2(m.tproj.apply(x[(n-1)*d:n*d], 1)) // the end token holds the summary
}

// EmbedImage returns the unit-length embedding of a photo.
func (m *Model) EmbedImage(img image.Image) []float32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	pix := preprocess(img, m.size, m.mean, m.std)
	return m.embedPixels(pix)
}

// embedPixels takes a normalised CHW float image of m.size x m.size.
func (m *Model) embedPixels(pix []float32) []float32 {
	p, s, d := m.patch, m.size, m.vis.hidden
	g := s / p
	n := g*g + 1
	patches := make([]float32, g*g*3*p*p)
	for gy := 0; gy < g; gy++ {
		for gx := 0; gx < g; gx++ {
			dst := patches[(gy*g+gx)*3*p*p:]
			k := 0
			for c := 0; c < 3; c++ {
				for y := 0; y < p; y++ {
					copy(dst[k:k+p], pix[c*s*s+(gy*p+y)*s+gx*p:])
					k += p
				}
			}
		}
	}
	emb := m.patchW.apply(patches, g*g)
	x := make([]float32, n*d)
	copy(x[:d], m.cls)
	copy(x[d:], emb)
	for i := range x {
		x[i] += m.vpos[i]
	}
	x = m.vis.preLN.apply(x, n, d, m.vis.eps)
	x = m.vis.run(x, n, false)
	cls := m.vis.postLN.apply(x[:d], 1, d, m.vis.eps)
	return l2(m.vproj.apply(cls, 1))
}

// Normalize scales v to unit length (in place) and returns it.
func Normalize(v []float32) []float32 { return l2(v) }
