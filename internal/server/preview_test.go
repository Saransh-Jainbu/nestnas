package server

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// testJPEG makes a w x h image: left half red, right half blue.
func testJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < w/2 {
				img.SetRGBA(x, y, color.RGBA{255, 0, 0, 255})
			} else {
				img.SetRGBA(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 95})
	return b.Bytes()
}

// withOrientation inserts an EXIF APP1 segment with the given orientation right after the SOI marker.
func withOrientation(jpg []byte, o uint16) []byte {
	var tiff bytes.Buffer
	tiff.WriteString("II")
	binary.Write(&tiff, binary.LittleEndian, uint16(42))
	binary.Write(&tiff, binary.LittleEndian, uint32(8))
	binary.Write(&tiff, binary.LittleEndian, uint16(1)) // one entry
	binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	binary.Write(&tiff, binary.LittleEndian, uint16(3)) // SHORT
	binary.Write(&tiff, binary.LittleEndian, uint32(1))
	binary.Write(&tiff, binary.LittleEndian, o)
	binary.Write(&tiff, binary.LittleEndian, uint16(0))
	binary.Write(&tiff, binary.LittleEndian, uint32(0))
	seg := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(jpg[:2])
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(seg)+2))
	out.Write(seg)
	out.Write(jpg[2:])
	return out.Bytes()
}

func decode(t *testing.T, b []byte) image.Image {
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestThumbnailShrinksAndKeepsAspect(t *testing.T) {
	b, err := makeThumb(bytes.NewReader(testJPEG(1200, 600)), 300)
	if err != nil {
		t.Fatal(err)
	}
	cfg := decode(t, b).Bounds()
	if cfg.Dx() != 300 || cfg.Dy() != 150 {
		t.Errorf("thumb is %dx%d, want 300x150", cfg.Dx(), cfg.Dy())
	}
	// small images are not enlarged
	b, _ = makeThumb(bytes.NewReader(testJPEG(100, 50)), 300)
	if c := decode(t, b).Bounds(); c.Dx() != 100 || c.Dy() != 50 {
		t.Errorf("small image resized to %dx%d", c.Dx(), c.Dy())
	}
}

func TestEXIFOrientationApplied(t *testing.T) {
	jpg := withOrientation(testJPEG(400, 200), 6) // phone photo that must be rotated 90° clockwise
	if got := exifOrientation(jpg); got != 6 {
		t.Fatalf("orientation parsed as %d", got)
	}
	b, err := makeThumb(bytes.NewReader(jpg), 200)
	if err != nil {
		t.Fatal(err)
	}
	img := decode(t, b)
	if img.Bounds().Dx() != 100 || img.Bounds().Dy() != 200 {
		t.Fatalf("rotated thumb is %dx%d, want 100x200", img.Bounds().Dx(), img.Bounds().Dy())
	}
	// Rotating 90° clockwise puts the red (left) half at the top and blue at the bottom.
	tr, _, tb, _ := img.At(50, 20).RGBA()
	br, _, bb, _ := img.At(50, 180).RGBA()
	if !(tr > 0xC000 && tb < 0x4000 && br < 0x4000 && bb > 0xC000) {
		t.Errorf("wrong rotation: top rgb=(%x,_,%x) bottom=(%x,_,%x)", tr, tb, br, bb)
	}
	for o := uint16(1); o <= 8; o++ { // every orientation must produce a valid thumbnail
		if _, err := makeThumb(bytes.NewReader(withOrientation(testJPEG(60, 40), o)), 50); err != nil {
			t.Errorf("orientation %d: %v", o, err)
		}
	}
	if exifOrientation([]byte("not a jpeg")) != 1 || exifOrientation(nil) != 1 || exifOrientation(testJPEG(10, 10)) != 1 {
		t.Error("missing/garbage EXIF must mean upright")
	}
}

func TestTransparentPNGFlattenedOnWhite(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 20, 20)) // fully transparent
	var b bytes.Buffer
	png.Encode(&b, img)
	out, err := makeThumb(bytes.NewReader(b.Bytes()), 20)
	if err != nil {
		t.Fatal(err)
	}
	r, g, bl, _ := decode(t, out).At(10, 10).RGBA()
	if r < 0xF000 || g < 0xF000 || bl < 0xF000 {
		t.Errorf("transparent area should become white, got %x %x %x", r, g, bl)
	}
}

func TestThumbRejectsNonImagesAndBombs(t *testing.T) {
	if _, err := makeThumb(strings.NewReader("hello, I am text, not an image"), 100); err != errNotImage {
		t.Errorf("text: %v", err)
	}
	// a PNG header claiming 20000x20000 pixels (400 MP) must be refused before decoding
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	chunk := make([]byte, 13)
	binary.BigEndian.PutUint32(chunk[0:], 20000)
	binary.BigEndian.PutUint32(chunk[4:], 20000)
	chunk[8], chunk[9] = 8, 2
	binary.Write(&b, binary.BigEndian, uint32(13))
	b.WriteString("IHDR")
	b.Write(chunk)
	crc := crc32IEEE(append([]byte("IHDR"), chunk...))
	binary.Write(&b, binary.BigEndian, crc)
	if _, err := makeThumb(bytes.NewReader(b.Bytes()), 100); err != errTooBig {
		t.Errorf("decompression bomb: got %v, want errTooBig", err)
	}
}

func TestThumbCacheEvicts(t *testing.T) {
	c := newThumbCache(100)
	c.put("a", make([]byte, 60))
	c.put("b", make([]byte, 60)) // pushes out "a"
	if _, ok := c.get("a"); ok {
		t.Error("oldest entry should be evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Error("newest entry should remain")
	}
}

func TestViewAllowlist(t *testing.T) {
	for _, bad := range []string{".html", ".htm", ".svg", ".js", ".xml", ".php", ".exe", ".xhtml"} {
		if _, ok := viewTypes[bad]; ok {
			t.Errorf("%s must never be served inline (can run script)", bad)
		}
	}
	if viewTypes[".png"] != "image/png" || viewTypes[".pdf"] != "application/pdf" {
		t.Error("expected common types missing")
	}
}

func crc32IEEE(b []byte) uint32 { return crc32.ChecksumIEEE(b) }
