package vision

import (
	"math"
	"testing"
)

func TestHalfPrecision(t *testing.T) {
	cases := map[uint16]float32{0x3c00: 1, 0xc000: -2, 0x0000: 0, 0x3555: 0.33325195, 0x7bff: 65504, 0x0001: 5.9604645e-08}
	for h, want := range cases {
		if got := half(h); math.Abs(float64(got-want)) > 1e-6*math.Abs(float64(want))+1e-12 {
			t.Errorf("half(%#x) = %v, want %v", h, got, want)
		}
	}
}

func TestResampleWeightsSumToOne(t *testing.T) {
	for _, c := range [][2]int{{1000, 224}, {224, 640}, {640, 640}, {3, 224}} {
		for i, k := range weights(c[0], c[1]) {
			var s float32
			for _, w := range k.w {
				s += w
			}
			if math.Abs(float64(s-1)) > 1e-4 || k.start < 0 || k.start+len(k.w) > c[0] {
				t.Fatalf("weights(%d,%d)[%d]: sum %v start %d len %d", c[0], c[1], i, s, k.start, len(k.w))
			}
		}
	}
}

func TestAlignmentOfTemplateIsIdentity(t *testing.T) {
	// A picture that already is the 112x112 template must come out unchanged.
	const n = 112
	src := make([][]float32, 3)
	for c := range src {
		src[c] = make([]float32, n*n)
		for i := range src[c] {
			src[c][i] = float32((i*7 + c*13) % 255)
		}
	}
	var marks [5][2]float32
	for i, p := range template112 {
		marks[i] = [2]float32{float32(p[0] / n), float32(p[1] / n)}
	}
	out := alignFace(src, n, n, marks)
	for c := range out {
		for i := 100; i < n*n-100; i += 37 {
			if math.Abs(float64(out[c][i]-src[c][i])) > 1.5 {
				t.Fatalf("pixel %d channel %d: %v vs %v", i, c, out[c][i], src[c][i])
			}
		}
	}
}

func TestAlignmentScalesAndRotates(t *testing.T) {
	// the same template points drawn at twice the size must give the same transform back
	var marks [5][2]float32
	for i, p := range template112 {
		marks[i] = [2]float32{float32(p[0] * 2 / 224), float32(p[1] * 2 / 224)}
	}
	src := make([][]float32, 3)
	for c := range src {
		src[c] = make([]float32, 224*224)
		for i := range src[c] {
			src[c][i] = 100
		}
	}
	out := alignFace(src, 224, 224, marks)
	if out == nil || out[0][56*112+56] != 100 {
		t.Fatal("scaled alignment lost the picture")
	}
}

func TestIoU(t *testing.T) {
	a := rawDet{x1: 0, y1: 0, x2: 10, y2: 10}
	if iou(a, a) < 0.999 || iou(a, rawDet{x1: 20, y1: 20, x2: 30, y2: 30}) != 0 {
		t.Fatal("iou")
	}
	if v := iou(a, rawDet{x1: 5, y1: 0, x2: 15, y2: 10}); math.Abs(float64(v)-1.0/3) > 1e-5 {
		t.Fatalf("half overlap should be 1/3, got %v", v)
	}
}

func TestTransposeReshape(t *testing.T) {
	x := &tensor{[]int{1, 2, 2, 3}, []float32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}}
	y, err := transposeOp(x, map[string]*attr{"perm": {ints: []int64{0, 2, 3, 1}}})
	if err != nil || y.shape[3] != 2 || y.data[0] != 1 || y.data[1] != 7 || y.data[2] != 2 {
		t.Fatalf("%v %v", y, err)
	}
	r, err := reshapeOp(x, []int64{1, -1, 3})
	if err != nil || r.shape[1] != 4 {
		t.Fatalf("%v %v", r, err)
	}
}

func TestConvAgainstHandComputed(t *testing.T) {
	// 1 channel 3x3 image, one 3x3 kernel of ones with padding 1: each output is a local sum
	x := &tensor{[]int{1, 1, 3, 3}, []float32{1, 2, 3, 4, 5, 6, 7, 8, 9}}
	w := &tensor{[]int{1, 1, 3, 3}, []float32{1, 1, 1, 1, 1, 1, 1, 1, 1}}
	y, err := convOp(x, w, nil, map[string]*attr{"pads": {ints: []int64{1, 1, 1, 1}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []float32{12, 21, 16, 27, 45, 33, 24, 39, 28}
	for i, v := range want {
		if y.data[i] != v {
			t.Fatalf("output %d = %v, want %v", i, y.data[i], v)
		}
	}
}

func TestSimilarityOfUnitVectors(t *testing.T) {
	a := Normalize([]float32{1, 2, 3})
	if s := Similarity(a, a); math.Abs(float64(s)-1) > 1e-6 {
		t.Fatal(s)
	}
}
