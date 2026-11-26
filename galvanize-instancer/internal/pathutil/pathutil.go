// Package pathutil checks that paths stay inside a folder.
package pathutil

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrOutside is returned by Resolve for a path leading outside its folder.
var ErrOutside = errors.New("leads outside the folder")

// Inside reports whether path is dir or below it, lexically: ".." segments
// count, symlinks do not (see Resolve). Both must be absolute, or both
// relative.
func Inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Resolve returns path with its symlinks resolved, when it stays inside dir
// (whose symlinks are resolved too). It fails with ErrOutside when it leads
// outside, or with the error of filepath.EvalSymlinks (fs.ErrNotExist for a
// missing path).
func Resolve(dir, path string) (string, error) {
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !Inside(realDir, real) {
		return "", ErrOutside
	}
	return real, nil
}
