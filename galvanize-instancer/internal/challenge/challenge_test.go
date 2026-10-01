package challenge

import (
	"fmt"
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

// writeFile is a helper that creates an arbitrary file inside baseDir/subdir/.
func writeFile(t *testing.T, baseDir, subdir, name, content string) {
	t.Helper()
	dir := filepath.Join(baseDir, subdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
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
	_, err := NewChallengeIndex("/nonexistent/path/that/does/not/exist")
	assert.Error(t, err)
}

const validChall = `
name: %s
category: %s
playbook_name: http
type: zync
deploy_parameters:
  unique: false
`

func TestBuildIndex_SkipsInvalidFile(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "web/ok", fmt.Sprintf(validChall, "ok", "web"))
	writeChallenge(t, dir, "web/broken", "name: [unterminated\n")
	writeChallenge(t, dir, "pwn/ok", fmt.Sprintf(validChall, "ok", "pwn"))

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	assert.Len(t, idx.GetAll(), 2)
	_, err = idx.Get("web", "ok")
	assert.NoError(t, err)
	_, err = idx.Get("pwn", "ok")
	assert.NoError(t, err)

	skipped := idx.Skipped()
	require.Len(t, skipped, 1)
	assert.Equal(t, filepath.Join(dir, "web/broken/challenge.yml"), skipped[0].Path)
	assert.NotEmpty(t, skipped[0].Reason)
}

func TestBuildIndex_SkipsDuplicate(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "a", fmt.Sprintf(validChall, "login", "web"))
	writeChallenge(t, dir, "b", fmt.Sprintf(validChall, "login", "web"))

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	// WalkDir is lexical: a is indexed, b is the duplicate
	assert.Len(t, idx.GetAll(), 1)
	skipped := idx.Skipped()
	require.Len(t, skipped, 1)
	assert.Equal(t, filepath.Join(dir, "b/challenge.yml"), skipped[0].Path)
	assert.Contains(t, skipped[0].Reason, filepath.Join(dir, "a/challenge.yml"))
	assert.Contains(t, skipped[0].Reason, "web/login")
}

func TestBuildIndex_SameNameDifferentCategories(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "web", fmt.Sprintf(validChall, "login", "web"))
	writeChallenge(t, dir, "pwn", fmt.Sprintf(validChall, "login", "pwn"))

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	assert.Len(t, idx.GetAll(), 2)
	assert.Empty(t, idx.Skipped())
}

func TestBuildIndex_UnreadableSubdirSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	writeChallenge(t, dir, "web", fmt.Sprintf(validChall, "ok", "web"))
	locked := filepath.Join(dir, "locked")
	require.NoError(t, os.MkdirAll(locked, 0o755))
	require.NoError(t, os.Chmod(locked, 0))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	assert.Len(t, idx.GetAll(), 1)
	require.Len(t, idx.Skipped(), 1)
	assert.Equal(t, locked, idx.Skipped()[0].Path)
}

func TestBuildIndex_FailedRebuildKeepsIndex(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "web", fmt.Sprintf(validChall, "ok", "web"))
	writeChallenge(t, dir, "broken", "name: [unterminated\n")

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	require.Len(t, idx.GetAll(), 1)

	assert.Error(t, idx.BuildIndex("/nonexistent/path/that/does/not/exist"))
	assert.Len(t, idx.GetAll(), 1, "the previous index is kept")
	assert.Len(t, idx.Skipped(), 1, "and so is its skipped list")
}

func TestBuildIndex_RebuildClearsSkipped(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "web", fmt.Sprintf(validChall, "ok", "web"))
	writeChallenge(t, dir, "broken", "name: [unterminated\n")

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	require.Len(t, idx.Skipped(), 1)

	writeChallenge(t, dir, "broken", fmt.Sprintf(validChall, "fixed", "web"))
	require.NoError(t, idx.BuildIndex(dir))
	assert.Len(t, idx.GetAll(), 2)
	assert.Empty(t, idx.Skipped())
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

func TestLoadComposeDefinition_AutoDetectsSiblingFile(t *testing.T) {
	dir := t.TempDir()

	// No playbook_name and no inline compose_definition: a standalone
	// compose file next to challenge.yml should be picked up automatically.
	writeChallenge(t, dir, "web", `
name: multi
category: web
type: zync
deploy_parameters:
  unique: false
`)
	compose := `services:
  web:
    image: nginx:alpine
    ports:
      - "80:80"
  db:
    image: postgres:16-alpine
`
	writeFile(t, dir, "web", "docker-compose.yml", compose)

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	chall, err := idx.Get("web", "multi")
	require.NoError(t, err)
	assert.Equal(t, "custom_compose", chall.PlaybookName, "playbook_name should default to custom_compose")
	assert.Equal(t, compose, chall.DeployParameters["compose_definition"])
}

func TestLoadComposeDefinition_PrefersCanonicalFileName(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: multi
category: web
type: zync
deploy_parameters:
  unique: false
`)
	// compose.yaml has higher precedence than docker-compose.yml.
	writeFile(t, dir, "web", "compose.yaml", "services:\n  a:\n    image: a\n")
	writeFile(t, dir, "web", "docker-compose.yml", "services:\n  b:\n    image: b\n")

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	chall, err := idx.Get("web", "multi")
	require.NoError(t, err)
	assert.Contains(t, chall.DeployParameters["compose_definition"], "image: a")
	assert.NotContains(t, chall.DeployParameters["compose_definition"], "image: b")
}

func TestLoadComposeDefinition_ExplicitComposeFile(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: multi
category: web
playbook_name: custom_compose
type: zync
deploy_parameters:
  unique: false
  compose_file: stack.yaml
`)
	writeFile(t, dir, "web", "stack.yaml", "services:\n  web:\n    image: nginx\n")

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	chall, err := idx.Get("web", "multi")
	require.NoError(t, err)
	assert.Contains(t, chall.DeployParameters["compose_definition"], "image: nginx")
	// The helper key should be stripped so it does not leak into Ansible vars.
	_, ok := chall.DeployParameters["compose_file"]
	assert.False(t, ok, "compose_file should be removed after loading")
}

func TestLoadComposeDefinition_InlineTakesPrecedence(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: multi
category: web
playbook_name: custom_compose
type: zync
deploy_parameters:
  unique: false
  compose_definition: |-
    services:
      inline:
        image: inline-image
`)
	// A sibling file exists but must be ignored in favour of the inline value.
	writeFile(t, dir, "web", "docker-compose.yml", "services:\n  file:\n    image: file-image\n")

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	chall, err := idx.Get("web", "multi")
	require.NoError(t, err)
	assert.Contains(t, chall.DeployParameters["compose_definition"], "inline-image")
	assert.NotContains(t, chall.DeployParameters["compose_definition"], "file-image")
}

func TestLoadComposeDefinition_MissingExplicitFileErrors(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: multi
category: web
playbook_name: custom_compose
type: zync
deploy_parameters:
  unique: false
  compose_file: nope.yaml
`)

	// The challenge is skipped, with the reason, instead of failing the index
	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	assert.Empty(t, idx.GetAll())
	require.Len(t, idx.Skipped(), 1)
	assert.Contains(t, idx.Skipped()[0].Reason, "compose_file")
}

func TestLoadComposeDefinition_NoServicesSectionErrors(t *testing.T) {
	dir := t.TempDir()

	writeChallenge(t, dir, "web", `
name: multi
category: web
type: zync
deploy_parameters:
  unique: false
`)
	writeFile(t, dir, "web", "docker-compose.yml", "version: \"3\"\nnetworks:\n  default: {}\n")

	// The challenge is skipped, with the reason, instead of failing the index
	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)
	assert.Empty(t, idx.GetAll())
	require.Len(t, idx.Skipped(), 1)
	assert.Contains(t, idx.Skipped()[0].Reason, "services")
}

func TestLoadComposeDefinition_NoComposeFileLeavesHTTPUntouched(t *testing.T) {
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

	idx, err := NewChallengeIndex(dir)
	require.NoError(t, err)

	chall, err := idx.Get("web", "http")
	require.NoError(t, err)
	assert.Equal(t, "http", chall.PlaybookName)
	_, ok := chall.DeployParameters["compose_definition"]
	assert.False(t, ok, "non-compose challenges should not gain a compose_definition")
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
