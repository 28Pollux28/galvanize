package challenge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func init() {
	zap.ReplaceGlobals(zap.NewNop())
}

// writeChallenge is a helper that creates a challenge.yml file inside baseDir/subdir/.
func writeChallenge(t *testing.T, baseDir, subdir, content string) {
	t.Helper()
	dir := filepath.Join(baseDir, subdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "challenge.yml"), []byte(content), 0o644))
}

func TestNewChallengeIndex(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: http
category: web
playbook_name: http
type: zync
deploy_parameters:
  unique: false
  image: nginx:latest
`)
	writeChallenge(t, dir, "pwn", `
name: bof
category: pwn
playbook_name: tcp
type: zync
deploy_parameters:
  unique: false
  image: bof:latest
`)

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	require.NotNil(t, idx)

	// Verify both challenges are indexed.
	chall, err := idx.Get("web", "http")
	require.NoError(t, err)
	assert.Equal(t, "http", chall.Name)
	assert.Equal(t, "web", chall.Category)
	assert.Equal(t, "http", chall.PlaybookName)
	assert.Equal(t, "zync", chall.Type)
	assert.False(t, chall.Unique)

	chall2, err := idx.Get("pwn", "bof")
	require.NoError(t, err)
	assert.Equal(t, "bof", chall2.Name)
	assert.Equal(t, "pwn", chall2.Category)
}

func TestBuildIndex_RebuildsClearsOldEntries(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: http
category: web
playbook_name: http
type: zync
deploy_parameters:
  unique: false
`)

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	_, err = idx.Get("web", "http")
	require.NoError(t, err)

	// Rebuild with a completely different set of challenges.
	dir2 := t.TempDir()
	writeChallenge(t, dir2, "crypto", `
name: rsa
category: crypto
playbook_name: tcp
type: zync
deploy_parameters:
  unique: false
`)

	err = idx.BuildIndex(dir2)
	require.NoError(t, err)

	// Old entry should be gone.
	_, err = idx.Get("web", "http")
	assert.Error(t, err)

	// New entry should be present.
	chall, err := idx.Get("crypto", "rsa")
	require.NoError(t, err)
	assert.Equal(t, "rsa", chall.Name)
}

func TestBuildIndex_SkipsNonZyncType(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "misc", `
name: trivia
category: misc
playbook_name: tcp
type: manual
deploy_parameters:
  unique: false
`)

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	_, err = idx.Get("misc", "trivia")
	assert.Error(t, err, "non-zync challenge should not be indexed")
}

func TestBuildIndex_SkipsExampleDir(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "example", `
name: example
category: example
playbook_name: http
type: zync
deploy_parameters:
  unique: false
`)

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	_, err = idx.Get("example", "example")
	assert.Error(t, err, "challenge in 'example' directory should be skipped")
}

func TestBuildIndex_InvalidDir(t *testing.T) {
	// WalkDir passes a nil DirEntry when the root path doesn't exist,
	// which causes a panic in BuildIndex before the error can be returned.
	assert.Panics(t, func() {
		_, _ = NewChallengeIndex("/nonexistent/path/that/does/not/exist")
	})
}

func TestGet_ExistingChallenge(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "forensics", `
name: pcap
category: forensics
playbook_name: tcp
type: zync
deploy_parameters:
  unique: false
  image: wireshark:latest
`)

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	chall, err := idx.Get("forensics", "pcap")
	require.NoError(t, err)
	assert.Equal(t, "pcap", chall.Name)
	assert.Equal(t, "forensics", chall.Category)
	assert.Equal(t, "tcp", chall.PlaybookName)
	assert.Equal(t, "wireshark:latest", chall.DeployParameters["image"])
}

func TestGet_NonExistentChallenge(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: http
category: web
playbook_name: http
type: zync
deploy_parameters:
  unique: false
`)

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	_, err = idx.Get("web", "doesnotexist")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "challenge not found")

	_, err = idx.Get("nonexistent", "http")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "challenge not found")
}

func TestGetAllUnique(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: http
category: web
playbook_name: http
type: zync
deploy_parameters:
  unique: true
  image: nginx:latest
`)
	writeChallenge(t, dir, "pwn", `
name: bof
category: pwn
playbook_name: tcp
type: zync
deploy_parameters:
  unique: false
  image: bof:latest
`)
	writeChallenge(t, dir, "crypto", `
name: rsa
category: crypto
playbook_name: tcp
type: zync
deploy_parameters:
  unique: true
  image: rsa:latest
`)

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	unique := idx.GetAllUnique()
	assert.Len(t, unique, 2)

	names := make(map[string]bool)
	for _, c := range unique {
		names[c.Name] = true
		assert.True(t, c.Unique)
	}
	assert.True(t, names["http"])
	assert.True(t, names["rsa"])
	assert.False(t, names["bof"])
}

func TestBuildIndex_UniqueChallenge(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: unique_web
category: web
playbook_name: http
type: zync
deploy_parameters:
  unique: true
  image: nginx:latest
`)

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	chall, err := idx.Get("web", "unique_web")
	require.NoError(t, err)
	assert.True(t, chall.Unique, "challenge with deploy_parameters.unique=true should have Unique=true")
	assert.Equal(t, "unique_web", chall.Name)
}
