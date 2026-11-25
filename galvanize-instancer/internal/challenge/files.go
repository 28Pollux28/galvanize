package challenge

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/28Pollux28/galvanize/internal/pathutil"
)

// File is a file a definition declares (its files list), as found in the
// challenge folder.
type File struct {
	Declared string
	// Path is the file found, inside the challenge folder (symlinks not
	// resolved); empty when there is none.
	Path string
	Size int64
	// Error says why the file cannot be served (Path may still be set).
	Error string
}

// lfsPointer starts the small text files Git LFS leaves in place of the
// real ones when the repository is cloned without git-lfs.
var lfsPointer = []byte("version https://git-lfs.github.com/spec/")

// Files resolves the definition's files list. Each path is looked up in the
// challenge folder, then in its files/ subfolder. Absolute paths, paths
// leaving the challenge folder (symlinks included) or going through .git
// are refused, and so are Git LFS pointers.
func (c *Challenge) Files() []File {
	list, _ := c.Definition["files"].([]interface{})
	out := make([]File, 0, len(list))
	for _, v := range list {
		if v == nil || v == "" {
			continue
		}
		declared, ok := v.(string)
		if !ok {
			out = append(out, File{Declared: fmt.Sprint(v), Error: "not a file path"})
			continue
		}
		out = append(out, c.resolveFile(declared))
	}
	return out
}

func (c *Challenge) resolveFile(declared string) File {
	f := File{Declared: declared}
	rel := strings.ReplaceAll(declared, "\\", "/")
	if path.IsAbs(rel) {
		f.Error = "absolute file paths are not allowed"
		return f
	}
	rel = path.Clean(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		f.Error = "the path leaves the challenge folder"
		return f
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".git" {
			f.Error = "files inside .git are not allowed"
			return f
		}
	}
	for _, dir := range []string{c.Dir, filepath.Join(c.Dir, "files")} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		real, err := pathutil.Resolve(c.Dir, p)
		if errors.Is(err, pathutil.ErrOutside) {
			f.Error = "the path leads outside the challenge folder"
			return f
		}
		if err != nil {
			continue
		}
		st, err := os.Stat(real)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		f.Path, f.Size = p, st.Size()
		if isLFSPointer(real, st.Size()) {
			f.Error = "a Git LFS pointer, not the file: Galvanize needs git-lfs to fetch it"
		}
		return f
	}
	f.Error = "not found (looked in the challenge folder and its files/ subfolder)"
	return f
}

func isLFSPointer(path string, size int64) bool {
	if size > 1024 {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	head := make([]byte, len(lfsPointer))
	n, _ := io.ReadFull(file, head)
	return bytes.Equal(head[:n], lfsPointer)
}
