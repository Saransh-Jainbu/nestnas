// Package fsx contains filesystem helpers: jail-safe path resolution and usage accounting.
package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

var ErrEscape = errors.New("path escapes storage")

// Resolve maps a user-supplied relative path (slash separated) to an absolute path
// guaranteed to be inside root, even through symlinks. The target need not exist.
func Resolve(root, rel string) (string, error) {
	clean := path.Clean("/" + rel) // collapses .. so it can never climb above "/"
	full := filepath.Join(root, filepath.FromSlash(clean))

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	// Resolve the deepest existing ancestor and make sure it is still inside root.
	p := full
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			if r != realRoot && !strings.HasPrefix(r, realRoot+string(filepath.Separator)) {
				return "", ErrEscape
			}
			return full, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", ErrEscape
		}
		p = parent
	}
}

// ValidName reports whether name is a single safe path component.
func ValidName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00") && len(name) <= 255
}

// DirSize sums regular file sizes under root, ignoring symlinks.
func DirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
