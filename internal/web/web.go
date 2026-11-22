// Package web embeds the dashboard so NestNAS ships as a single binary.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html
var Index []byte

// Icon is the app icon (also used for "Add to Home Screen" on phones).
//
//go:embed icon.png
var Icon []byte

// Favicon is the square app icon as SVG (browser tab icon).
//
//go:embed favicon.svg
var Favicon []byte

// Icon512 is the large app icon for "Add to Home Screen".
//
//go:embed icon-512.png
var Icon512 []byte

//go:embed fonts/*.woff2
var fontFiles embed.FS

// Fonts serves the self-hosted typefaces (SIL Open Font License) so the dashboard never
// contacts a third-party font service.
func Fonts() fs.FS {
	sub, _ := fs.Sub(fontFiles, "fonts")
	return sub
}
