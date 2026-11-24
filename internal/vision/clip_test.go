package vision

import (
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// Run with NN_CLIP_DIR=<model folder> NN_CLIP_IMG=<image> NN_CLIP_OUT=<file> to dump the numbers
// that scripts/ref_clip.py checks against an independent numpy implementation.
func TestDumpForReference(t *testing.T) {
	dir, imgPath, out := os.Getenv("NN_CLIP_DIR"), os.Getenv("NN_CLIP_IMG"), os.Getenv("NN_CLIP_OUT")
	if dir == "" || imgPath == "" || out == "" {
		t.Skip("no model given")
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	img, _, err := image.Decode(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	res := map[string]interface{}{}
	texts := []string{"a photo of a dog", "A photo of a CAT, sitting on a sofa!", "sunset over the beach", "invoice 2023 total €45.50", "naïve café"}
	toks := map[string][]int{}
	embs := map[string][]float32{}
	for _, s := range texts {
		toks[s] = m.tok.encode(s, m.maxTok)
		embs[s] = m.EmbedText(s)
	}
	res["tokens"], res["text"] = toks, embs
	res["pixels"] = preprocess(img, m.size, m.mean, m.std)
	res["image"] = m.EmbedImage(img)
	b, _ := json.Marshal(res)
	os.WriteFile(out, b, 0o644)
}

// NN_CLIP_PHOTOS=<folder of jpegs>: prints how queries rank the photos, and the time per photo.
func TestRetrievalSanity(t *testing.T) {
	dir, photos := os.Getenv("NN_CLIP_DIR"), os.Getenv("NN_CLIP_PHOTOS")
	if dir == "" || photos == "" {
		t.Skip("no model or photos given")
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(photos, "*.jpg"))
	embs := map[string][]float32{}
	var total time.Duration
	for _, p := range files {
		f, _ := os.Open(p)
		img, _, err := image.Decode(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		st := time.Now()
		embs[filepath.Base(p)] = m.EmbedImage(img)
		total += time.Since(st)
	}
	t.Logf("%d photos, %.0f ms each", len(files), float64(total.Milliseconds())/float64(len(files)))
	for _, q := range []string{"a cat", "a dog", "pizza", "snowy mountains", "a car", "food", "an animal", "invoice"} {
		qe := m.EmbedText(q)
		type r struct {
			n string
			s float32
		}
		var rs []r
		for n, e := range embs {
			rs = append(rs, r{n, dot(qe, e)})
		}
		sort.Slice(rs, func(i, j int) bool { return rs[i].s > rs[j].s })
		line := q + ":"
		for _, x := range rs {
			line += fmt.Sprintf("  %s %.3f", x.n, x.s)
		}
		t.Log(line)
	}
}

// NN_FACES_DIR=<folder with yunet.onnx and sface.onnx> NN_FACES_PHOTOS=<folder> prints detections
// and, as JSON in NN_FACES_OUT, the faces and embeddings for scripts/ref_faces.py.
func TestFacesDump(t *testing.T) {
	dir, photos, out := os.Getenv("NN_FACES_DIR"), os.Getenv("NN_FACES_PHOTOS"), os.Getenv("NN_FACES_OUT")
	if dir == "" || photos == "" {
		t.Skip("no models given")
	}
	f, err := LoadFaces(dir)
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(photos, "*.jpg"))
	res := map[string][]Face{}
	for _, p := range files {
		fh, _ := os.Open(p)
		img, _, err := image.Decode(fh)
		fh.Close()
		if err != nil {
			t.Fatal(err)
		}
		st := time.Now()
		faces, err := f.Analyze(img, 0.7)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d faces in %d ms", filepath.Base(p), len(faces), time.Since(st).Milliseconds())
		res[filepath.Base(p)] = faces
	}
	if out != "" {
		b, _ := json.Marshal(res)
		os.WriteFile(out, b, 0o644)
	}
}
