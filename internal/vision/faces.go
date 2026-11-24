package vision

import (
	"errors"
	"image"
	"math"
	"path/filepath"
	"sort"
)

// Face detection (YuNet, MIT licence) and recognition (SFace, Apache-2.0), both from the OpenCV
// model zoo, run on the small ONNX interpreter in this package.

const detSize = 640

// Face is one detected face. Coordinates are fractions (0..1) of the image's width and height.
type Face struct {
	X, Y, W, H float32
	Score      float32
	Marks      [5][2]float32 // right eye, left eye, nose tip, right and left mouth corners (as seen in the picture)
	Emb        []float32     // unit-length 128 numbers that describe who it is
}

// Faces holds both networks.
type Faces struct {
	det, rec *Net
}

// LoadFaces reads yunet.onnx and sface.onnx from dir.
func LoadFaces(dir string) (*Faces, error) {
	det, err := LoadNet(filepath.Join(dir, "yunet.onnx"))
	if err != nil {
		return nil, err
	}
	rec, err := LoadNet(filepath.Join(dir, "sface.onnx"))
	if err != nil {
		return nil, err
	}
	return &Faces{det, rec}, nil
}

type rawDet struct {
	x1, y1, x2, y2, score float32
	marks                 [5][2]float32
}

// Detect finds faces whose confidence is at least minScore.
func (f *Faces) Detect(img image.Image, minScore float32) ([]Face, error) {
	src, w, h := planes(img)
	if w == 0 || h == 0 {
		return nil, nil
	}
	scale := float64(detSize) / float64(maxInt(w, h))
	nw, nh := maxInt(1, int(math.Round(float64(w)*scale))), maxInt(1, int(math.Round(float64(h)*scale)))
	if nw > detSize {
		nw = detSize
	}
	if nh > detSize {
		nh = detSize
	}
	rs := resizePlanes(src, w, h, nw, nh)
	in := &tensor{[]int{1, 3, detSize, detSize}, make([]float32, 3*detSize*detSize)}
	for c := 0; c < 3; c++ { // the model wants blue, green, red
		dst := in.data[(2-c)*detSize*detSize:]
		for y := 0; y < nh; y++ {
			copy(dst[y*detSize:y*detSize+nw], rs[c][y*nw:(y+1)*nw])
		}
	}
	out, err := f.det.Run(in)
	if err != nil {
		return nil, err
	}
	var dets []rawDet
	for _, stride := range []int{8, 16, 32} {
		k := itoa(stride)
		cls, obj, box, kps := out["cls_"+k], out["obj_"+k], out["bbox_"+k], out["kps_"+k]
		if cls == nil || obj == nil || box == nil || kps == nil {
			return nil, errors.New("unexpected detector outputs")
		}
		cols := detSize / stride
		for i := 0; i < cols*cols; i++ {
			cs, os := clamp01(cls.data[i]), clamp01(obj.data[i])
			score := float32(math.Sqrt(float64(cs * os)))
			if score < minScore {
				continue
			}
			r, c := i/cols, i%cols
			b := box.data[i*4 : i*4+4]
			cx, cy := (float32(c)+b[0])*float32(stride), (float32(r)+b[1])*float32(stride)
			bw, bh := float32(math.Exp(float64(b[2])))*float32(stride), float32(math.Exp(float64(b[3])))*float32(stride)
			d := rawDet{x1: cx - bw/2, y1: cy - bh/2, x2: cx + bw/2, y2: cy + bh/2, score: score}
			for n := 0; n < 5; n++ {
				d.marks[n] = [2]float32{(kps.data[i*10+2*n] + float32(c)) * float32(stride), (kps.data[i*10+2*n+1] + float32(r)) * float32(stride)}
			}
			dets = append(dets, d)
		}
	}
	sort.Slice(dets, func(i, j int) bool { return dets[i].score > dets[j].score })
	var keep []rawDet
	for _, d := range dets { // drop overlapping duplicates (IoU > 0.3)
		ok := true
		for _, k := range keep {
			if iou(d, k) > 0.3 {
				ok = false
				break
			}
		}
		if ok {
			keep = append(keep, d)
		}
		if len(keep) >= 50 {
			break
		}
	}
	inv := float32(1 / scale)
	faces := make([]Face, 0, len(keep))
	for _, d := range keep {
		fc := Face{Score: d.score}
		x1, y1, x2, y2 := d.x1*inv, d.y1*inv, d.x2*inv, d.y2*inv
		fc.X, fc.Y = clampf(x1/float32(w), 0, 1), clampf(y1/float32(h), 0, 1)
		fc.W, fc.H = clampf(x2/float32(w), 0, 1)-fc.X, clampf(y2/float32(h), 0, 1)-fc.Y
		for n := 0; n < 5; n++ {
			fc.Marks[n] = [2]float32{d.marks[n][0] * inv / float32(w), d.marks[n][1] * inv / float32(h)}
		}
		if fc.W > 0.01 && fc.H > 0.01 {
			faces = append(faces, fc)
		}
	}
	return faces, nil
}

func iou(a, b rawDet) float32 {
	ix1, iy1 := maxf(a.x1, b.x1), maxf(a.y1, b.y1)
	ix2, iy2 := minf(a.x2, b.x2), minf(a.y2, b.y2)
	iw, ih := ix2-ix1, iy2-iy1
	if iw <= 0 || ih <= 0 {
		return 0
	}
	inter := iw * ih
	return inter / ((a.x2-a.x1)*(a.y2-a.y1) + (b.x2-b.x1)*(b.y2-b.y1) - inter)
}

// the standard five-point face template at 112 x 112 (ArcFace)
var template112 = [5][2]float64{{38.2946, 51.6963}, {73.5318, 51.5014}, {56.0252, 71.7366}, {41.5493, 92.3655}, {70.7299, 92.2041}}

// alignFace cuts the face out upright at 112 x 112 using its eye, nose and mouth points.
func alignFace(src [][]float32, w, h int, marks [5][2]float32) [][]float32 {
	// least-squares similarity transform (scale, rotation, shift) from template to image points
	var pts [5][2]float64
	for i := range pts {
		pts[i] = [2]float64{float64(marks[i][0]) * float64(w), float64(marks[i][1]) * float64(h)}
	}
	var mx, my, tx, ty float64
	for i := 0; i < 5; i++ {
		mx += pts[i][0] / 5
		my += pts[i][1] / 5
		tx += template112[i][0] / 5
		ty += template112[i][1] / 5
	}
	var sxx, a, b float64
	for i := 0; i < 5; i++ {
		px, py := template112[i][0]-tx, template112[i][1]-ty
		qx, qy := pts[i][0]-mx, pts[i][1]-my
		sxx += px*px + py*py
		a += px*qx + py*qy
		b += px*qy - py*qx
	}
	if sxx == 0 {
		return nil
	}
	// image = s*R*template + t, with (a, b)/sxx = (s cos, s sin)
	c, s := a/sxx, b/sxx
	tX, tY := mx-(c*tx-s*ty), my-(s*tx+c*ty)
	out := make([][]float32, 3)
	for ch := range out {
		out[ch] = make([]float32, 112*112)
	}
	for y := 0; y < 112; y++ {
		for x := 0; x < 112; x++ {
			sx := c*float64(x) - s*float64(y) + tX
			sy := s*float64(x) + c*float64(y) + tY
			x0, y0 := int(math.Floor(sx)), int(math.Floor(sy))
			fx, fy := float32(sx-float64(x0)), float32(sy-float64(y0))
			for ch := 0; ch < 3; ch++ {
				px := func(xx, yy int) float32 {
					if xx < 0 || yy < 0 || xx >= w || yy >= h {
						return 0
					}
					return src[ch][yy*w+xx]
				}
				v := px(x0, y0)*(1-fx)*(1-fy) + px(x0+1, y0)*fx*(1-fy) + px(x0, y0+1)*(1-fx)*fy + px(x0+1, y0+1)*fx*fy
				out[ch][y*112+x] = v
			}
		}
	}
	return out
}

// Embed computes the identity numbers for a detected face.
func (f *Faces) Embed(img image.Image, fc Face) ([]float32, error) {
	src, w, h := planes(img)
	return f.embedPlanes(src, w, h, fc)
}

func (f *Faces) embedPlanes(src [][]float32, w, h int, fc Face) ([]float32, error) {
	al := alignFace(src, w, h, fc.Marks)
	if al == nil {
		return nil, errors.New("can't align the face")
	}
	in := &tensor{[]int{1, 3, 112, 112}, make([]float32, 3*112*112)}
	for c := 0; c < 3; c++ { // red, green, blue, as OpenCV's recogniser feeds it
		copy(in.data[c*112*112:], al[c])
	}
	out, err := f.rec.Run(in)
	if err != nil {
		return nil, err
	}
	for _, t := range out {
		return l2(append([]float32(nil), t.data...)), nil
	}
	return nil, errors.New("no output")
}

// Analyze finds the faces in a photo and describes each one.
func (f *Faces) Analyze(img image.Image, minScore float32) ([]Face, error) {
	faces, err := f.Detect(img, minScore)
	if err != nil || len(faces) == 0 {
		return nil, err
	}
	src, w, h := planes(img)
	for i := range faces {
		if e, err := f.embedPlanes(src, w, h, faces[i]); err == nil {
			faces[i].Emb = e
		}
	}
	out := faces[:0]
	for _, fc := range faces {
		if fc.Emb != nil {
			out = append(out, fc)
		}
	}
	return out, nil
}

// Similarity is the cosine of two face descriptions: about 0.36 and up usually means the same person.
func Similarity(a, b []float32) float32 { return dot(a, b) }

func clamp01(x float32) float32 { return clampf(x, 0, 1) }
func clampf(x, lo, hi float32) float32 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}
func maxf(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}
func minf(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
