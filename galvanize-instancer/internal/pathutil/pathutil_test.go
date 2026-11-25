package pathutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInside(t *testing.T) {
	assert.True(t, Inside("/a/b", "/a/b"))
	assert.True(t, Inside("/a/b", "/a/b/c/../d"))
	assert.True(t, Inside("/a/b", "/a/b/..foo"), "a name starting with .. is inside")
	assert.False(t, Inside("/a/b", "/a/b/../c"))
	assert.False(t, Inside("/a/b", "/a"))
	assert.False(t, Inside("/a/b", "/a/bc"))
	assert.False(t, Inside("/a/b", "b"), "absolute and relative")
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "f"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "secret"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink("sub/f", filepath.Join(dir, "in")))
	require.NoError(t, os.Symlink("../secret", filepath.Join(dir, "out")))

	real, err := Resolve(dir, filepath.Join(dir, "in"))
	require.NoError(t, err)
	assert.Equal(t, "f", filepath.Base(real))
	_, err = Resolve(dir, filepath.Join(dir, "out"))
	assert.ErrorIs(t, err, ErrOutside)
	_, err = Resolve(dir, filepath.Join(dir, "missing"))
	assert.True(t, errors.Is(err, fs.ErrNotExist))
}
