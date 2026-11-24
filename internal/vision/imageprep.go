package vision

import (
	"image"
	"math"
)

// preprocess resizes the shortest side to size (bicubic, anti-aliased), crops the centre
// square and normalises the colours: the same recipe the model was trained with.
func preprocess(img image.Image, size int, mean, std [3]float32) []float32 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w == 0 || h == 0 {
		return make([]float32, 3*size*size)
	}
	nw, nh := size, size
	if w < h {
		nh = int(float64(h) * float64(size) / float64(w))
	} else {
		nw = int(float64(w) * float64(size) / float64(h))
	}
	if nw < size {
		nw = size
	}
	if nh < size {
		nh = size
	}
	// source pixels as float planes
	src := make([][]float32, 3)
	for c := range src {
		src[c] = make([]float32, w*h)
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			src[0][y*w+x], src[1][y*w+x], src[2][y*w+x] = float32(r>>8), float32(g>>8), float32(bl>>8)
		}
	}
	cx, cy := (nw-size)/2, (nh-size)/2
	out := make([]float32, 3*size*size)
	hx := weights(w, nw)
	hy := weights(h, nh)
	tmp := make([]float32, nw*h) // horizontal pass for one channel
	for c := 0; c < 3; c++ {
		for y := 0; y < h; y++ {
			row := src[c][y*w : (y+1)*w]
			for x := cx; x < cx+size; x++ {
				k := hx[x]
				var s float32
				for i, wt := range k.w {
					s += row[k.start+i] * wt
				}
				tmp[y*nw+x] = s
			}
		}
		for y := cy; y < cy+size; y++ {
			k := hy[y]
			for x := cx; x < cx+size; x++ {
				var s float32
				for i, wt := range k.w {
					s += tmp[(k.start+i)*nw+x] * wt
				}
				if s < 0 {
					s = 0
				} else if s > 255 {
					s = 255
				}
				out[c*size*size+(y-cy)*size+(x-cx)] = (float32(math.Round(float64(s)))/255 - mean[c]) / std[c]
			}
		}
	}
	return out
}

type kernel struct {
	start int
	w     []float32
}

func cubic(x float64) float64 { // Keys cubic, a = -0.5 (what PIL uses)
	const a = -0.5
	x = math.Abs(x)
	switch {
	case x < 1:
		return ((a+2)*x-(a+3))*x*x + 1
	case x < 2:
		return (((x-5)*x+8)*x - 4) * a
	}
	return 0
}

// weights builds, for each output pixel, the source pixels and weights that make it.
func weights(in, out int) []kernel {
	scale := float64(in) / float64(out)
	fs := math.Max(scale, 1)
	support := 2 * fs
	ks := make([]kernel, out)
	for o := 0; o < out; o++ {
		center := (float64(o) + 0.5) * scale
		lo := int(math.Max(0, math.Floor(center-support+0.5)))
		hi := int(math.Min(float64(in), math.Floor(center+support+0.5)))
		if hi <= lo {
			hi = lo + 1
		}
		w := make([]float32, hi-lo)
		var sum float64
		for i := lo; i < hi; i++ {
			v := cubic((float64(i) + 0.5 - center) / fs)
			w[i-lo] = float32(v)
			sum += v
		}
		if sum != 0 {
			for i := range w {
				w[i] = float32(float64(w[i]) / sum)
			}
		}
		ks[o] = kernel{lo, w}
	}
	return ks
}

// planes converts an image to three float planes (R, G, B) with values 0..255.
func planes(img image.Image) ([][]float32, int, int) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := make([][]float32, 3)
	for c := range out {
		out[c] = make([]float32, w*h)
	}
	if rgba, ok := img.(*image.RGBA); ok {
		for y := 0; y < h; y++ {
			row := rgba.Pix[y*rgba.Stride : y*rgba.Stride+w*4]
			for x := 0; x < w; x++ {
				out[0][y*w+x], out[1][y*w+x], out[2][y*w+x] = float32(row[x*4]), float32(row[x*4+1]), float32(row[x*4+2])
			}
		}
		return out, w, h
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			out[0][y*w+x], out[1][y*w+x], out[2][y*w+x] = float32(r>>8), float32(g>>8), float32(bl>>8)
		}
	}
	return out, w, h
}

// resizePlanes resamples float planes to nw x nh (bicubic, anti-aliased).
func resizePlanes(src [][]float32, w, h, nw, nh int) [][]float32 {
	hx, hy := weights(w, nw), weights(h, nh)
	out := make([][]float32, len(src))
	tmp := make([]float32, nw*h)
	for c, pl := range src {
		for y := 0; y < h; y++ {
			row := pl[y*w : (y+1)*w]
			for x := 0; x < nw; x++ {
				k := hx[x]
				var s float32
				for i, wt := range k.w {
					s += row[k.start+i] * wt
				}
				tmp[y*nw+x] = s
			}
		}
		dst := make([]float32, nw*nh)
		for y := 0; y < nh; y++ {
			k := hy[y]
			for x := 0; x < nw; x++ {
				var s float32
				for i, wt := range k.w {
					s += tmp[(k.start+i)*nw+x] * wt
				}
				if s < 0 {
					s = 0
				} else if s > 255 {
					s = 255
				}
				dst[y*nw+x] = s
			}
		}
		out[c] = dst
	}
	return out
}
