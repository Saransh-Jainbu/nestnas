// Package web embeds the dashboard so NestNAS ships as a single binary.
package web

import _ "embed"

//go:embed index.html
var Index []byte

// Icon is the app icon (also used for "Add to Home Screen" on phones).
//
//go:embed icon.png
var Icon []byte
