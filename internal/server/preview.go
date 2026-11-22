package server

// In-app previews: /api/view streams a file inline for safe media types, /api/thumb makes small
// JPEG thumbnails. Both decrypt on the fly (nothing readable is ever written to disk) and are
// sent with Cache-Control: no-store so the browser keeps no plaintext copy either.
//
// Only an allowlist of passive types is ever served inline. HTML and SVG are deliberately NOT
// on it: they can run script, which could act as the logged-in user.

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	_ "image/gif" // register decoders for image.Decode
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/Saransh-Jainbu/nestnas/internal/config"
)

var viewTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
	".bmp": "image/bmp", ".avif": "image/avif",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime", ".ogv": "video/ogg",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".wav": "audio/wav", ".ogg": "audio/ogg", ".oga": "audio/ogg", ".flac": "audio/flac", ".opus": "audio/ogg",
	".pdf": "application/pdf",
	".txt": "text/plain; charset=utf-8", ".md": "text/plain; charset=utf-8", ".log": "text/plain; charset=utf-8",
	".csv": "text/plain; charset=utf-8", ".json": "text/plain; charset=utf-8", ".yml": "text/plain; charset=utf-8", ".yaml": "text/plain; charset=utf-8",
}

var thumbExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true}

func (s *Server) view(w http.ResponseWriter, r *http.Request, u config.User) {
	_, abs, err := s.resolve(u, r.URL.Query().Get("path"))
	if err != nil {
		jerr(w, 400, "bad path")
		return
	}
	ct, ok := viewTypes[strings.ToLower(filepath.Ext(abs))]
	if !ok {
		jerr(w, http.StatusUnsupportedMediaType, "This file type can't be previewed. Use Download.")
		return
	}
	rs, fi, done, err := s.openContent(abs)
	if err != nil {
		contentErr(w, err)
		return
	}
	defer done()
	h := w.Header()
	h.Set("Content-Type", ct) // fixed from the allowlist, never sniffed
	h.Set("Content-Disposition", "inline")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "SAMEORIGIN") // lets our own viewer show PDFs in a frame
	if ct != "application/pdf" {
		h.Set("Content-Security-Policy", "sandbox")
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), rs)
}

// ---------- thumbnails ----------

var (
	errNotImage = errors.New("not an image")
	errTooBig   = errors.New("image too large")
)

const maxPixels = 80_000_000 // refuse decompression bombs

func (s *Server) thumb(w http.ResponseWriter, r *http.Request, u config.User) {
	rel := r.URL.Query().Get("path")
	_, abs, err := s.resolve(u, rel)
	if err != nil || !thumbExt[strings.ToLower(filepath.Ext(abs))] {
		jerr(w, http.StatusUnsupportedMediaType, "no thumbnail for this file")
		return
	}
	dim, _ := strconv.Atoi(r.URL.Query().Get("w"))
	if dim < 48 || dim > 640 {
		dim = 320
	}
	rs, fi, done, err := s.openContent(abs)
	if err != nil {
		contentErr(w, err)
		return
	}
	defer done()
	key := u.Name + "|" + rel + "|" + strconv.FormatInt(fi.ModTime().UnixNano(), 10) + "|" + strconv.FormatInt(fi.Size(), 10) + "|" + strconv.Itoa(dim)
	b, ok := s.thumbs.get(key)
	if !ok {
		select { // at most 3 thumbnails are generated at once
		case s.thumbSem <- struct{}{}:
			defer func() { <-s.thumbSem }()
		case <-r.Context().Done():
			return
		}
		if b, err = makeThumb(rs, dim); err != nil {
			if errors.Is(err, errTooBig) {
				jerr(w, http.StatusRequestEntityTooLarge, "image too large for a thumbnail")
			} else {
				jerr(w, http.StatusUnsupportedMediaType, "no thumbnail for this file")
			}
			return
		}
		s.thumbs.put(key, b)
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

// makeThumb decodes an image (JPEG/PNG/GIF), shrinks it to fit maxDim, applies the photo's EXIF
// rotation, and returns it as a JPEG.
func makeThumb(r io.ReadSeeker, maxDim int) ([]byte, error) {
	head := make([]byte, 128<<10)
	n, _ := io.ReadFull(r, head)
	head = head[:n]
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return nil, errNotImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return nil, errTooBig
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(r)
	if err != nil {
		return nil, errNotImage
	}
	small := orient(downscale(img, maxDim), exifOrientation(head))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// downscale shrinks img so its longer side is at most maxDim, averaging a grid of samples per
// output pixel (smooth, and fast because only the output pixels are visited). Transparent
// areas are flattened onto white because JPEG has no alpha.
func downscale(img image.Image, maxDim int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if w >= h && w > maxDim {
		dw, dh = maxDim, h*maxDim/w
	} else if h > w && h > maxDim {
		dw, dh = w*maxDim/h, maxDim
	}
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for dy := 0; dy < dh; dy++ {
		y0, y1 := dy*h/dh, (dy+1)*h/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		ky := minInt(4, y1-y0)
		for dx := 0; dx < dw; dx++ {
			x0, x1 := dx*w/dw, (dx+1)*w/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			kx := minInt(4, x1-x0)
			var sr, sg, sb, sa, cnt uint64
			for j := 0; j < ky; j++ {
				sy := b.Min.Y + y0 + (y1-y0)*(2*j+1)/(2*ky)
				for i := 0; i < kx; i++ {
					sx := b.Min.X + x0 + (x1-x0)*(2*i+1)/(2*kx)
					r, g, bl, a := img.At(sx, sy).RGBA() // premultiplied, 16-bit
					sr, sg, sb, sa, cnt = sr+uint64(r), sg+uint64(g), sb+uint64(bl), sa+uint64(a), cnt+1
				}
			}
			r, g, bl, a := sr/cnt, sg/cnt, sb/cnt, sa/cnt
			out.SetRGBA(dx, dy, color.RGBA{ // composite over white
				R: uint8((r + (0xffff - a)) >> 8), G: uint8((g + (0xffff - a)) >> 8), B: uint8((bl + (0xffff - a)) >> 8), A: 0xff})
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// exifOrientation reads the EXIF "Orientation" tag (1-8) from the start of a JPEG; 1 = upright.
func exifOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		m := b[i+1]
		if m == 0xDA || m == 0xD9 { // image data / end: no EXIF found before it
			return 1
		}
		l := int(b[i+2])<<8 | int(b[i+3])
		if m == 0xE1 && i+10 <= len(b) && string(b[i+4:i+10]) == "Exif\x00\x00" {
			end := i + 2 + l
			if end > len(b) {
				end = len(b)
			}
			return tiffOrientation(b[i+10 : end])
		}
		i += 2 + l
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	if bo.Uint16(t[2:4]) != 42 {
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off : off+2]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:e+2]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8 : e+10])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient applies an EXIF orientation (1-8) so the thumbnail is upright.
func orient(src *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var sx, sy int
			switch o {
			case 2:
				sx, sy = w-1-x, y
			case 3:
				sx, sy = w-1-x, h-1-y
			case 4:
				sx, sy = x, h-1-y
			case 5:
				sx, sy = y, x
			case 6:
				sx, sy = y, h-1-x
			case 7:
				sx, sy = w-1-y, h-1-x
			case 8:
				sx, sy = w-1-y, x
			}
			dst.SetRGBA(x, y, src.RGBAAt(sx, sy))
		}
	}
	return dst
}

// thumbCache is a small in-memory LRU. Thumbnails are never written to disk.
type thumbCache struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List
	items map[string]*list.Element
}

type thumbEntry struct {
	key string
	b   []byte
}

func newThumbCache(maxBytes int) *thumbCache {
	return &thumbCache{max: maxBytes, order: list.New(), items: map[string]*list.Element{}}
}

func (c *thumbCache) get(k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*thumbEntry).b, true
	}
	return nil, false
}

func (c *thumbCache) put(k string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.size -= len(e.Value.(*thumbEntry).b)
		c.order.Remove(e)
	}
	c.items[k] = c.order.PushFront(&thumbEntry{k, b})
	c.size += len(b)
	for c.size > c.max && c.order.Len() > 1 {
		e := c.order.Back()
		te := e.Value.(*thumbEntry)
		c.size -= len(te.b)
		delete(c.items, te.key)
		c.order.Remove(e)
	}
}
