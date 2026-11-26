package repo

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nopLog struct{}

func (nopLog) Infof(string, ...interface{})    {}
func (nopLog) Debugf(string, ...interface{})   {}
func (nopLog) Successf(string, ...interface{}) {}
func (nopLog) Warnf(string, ...interface{})    {}
func (nopLog) Errorf(string, ...interface{})   {}

type warnLog struct {
	nopLog
	warnings []string
}

func (w *warnLog) Warnf(f string, a ...interface{}) {
	w.warnings = append(w.warnings, fmt.Sprintf(f, a...))
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

// newRemote creates a bare repository with one commit on main (file
// challenge.yml) and a tag v1, and returns its file:// URL and a work tree to
// push more commits from.
func newRemote(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	git(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	git(t, root, "clone", "--quiet", bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "challenge.yml"), []byte("v1\n"), 0o644))
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "first")
	git(t, work, "tag", "v1")
	git(t, work, "push", "--quiet", "origin", "main", "--tags")
	return "file://" + bare, work
}

func push(t *testing.T, work, content, msg string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(work, "challenge.yml"), []byte(content), 0o644))
	git(t, work, "commit", "--quiet", "-am", msg)
	git(t, work, "push", "--quiet", "origin", "main")
}

func manager(t *testing.T, prefix string) *Manager {
	return &Manager{BaseDir: t.TempDir(), Config: config.RepositoriesConfig{
		Sources: []config.RepositorySource{{URLPrefix: prefix}},
	}}
}

func TestClone_AndDescribe(t *testing.T) {
	url, _ := newRemote(t)
	m := manager(t, "file://")
	name, err := m.Clone(context.Background(), url, "", "", false, nopLog{})
	require.NoError(t, err)
	assert.Equal(t, "remote", name, "name derived from the URL")

	info := m.Describe(context.Background(), name)
	assert.True(t, info.IsGit)
	assert.Equal(t, "main", info.Branch)
	assert.Equal(t, "main", info.Ref)
	assert.Equal(t, "first", info.CommitMessage)
	assert.False(t, info.Dirty)
	assert.NoDirExists(t, filepath.Join(m.BaseDir, tmpDirName, name), "temporary clone removed")

	list, err := m.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1, "the hidden temporary folder is not a repository")
	assert.Equal(t, "remote", list[0].Name)
}

func TestClone_Refusals(t *testing.T) {
	url, _ := newRemote(t)
	ctx := context.Background()

	disabled := &Manager{BaseDir: t.TempDir()}
	_, err := disabled.Clone(ctx, url, "x", "", false, nopLog{})
	assert.ErrorIs(t, err, ErrCloneDisabled)

	m := manager(t, "https://github.com/org/")
	_, err = m.Clone(ctx, url, "x", "", false, nopLog{})
	assert.ErrorIs(t, err, ErrNotAllowed, "URL outside the allowed sources")

	_, err = m.Clone(ctx, "https://user:token@github.com/org/repo.git", "x", "", false, nopLog{})
	assert.ErrorIs(t, err, ErrNotAllowed, "credentials in the URL")

	m = manager(t, "file://")
	for _, bad := range []string{"../evil", ".hidden", "a/b", "", "x..y"} {
		if bad == "" {
			continue
		}
		_, err = m.Clone(ctx, url, bad, "", false, nopLog{})
		assert.Error(t, err, "name %q", bad)
	}
	_, err = m.Clone(ctx, url, "ok", "--upload-pack=evil", false, nopLog{})
	assert.Error(t, err, "option-like ref refused")
}

func TestClone_ExistingNeedsForceAndForceReplaces(t *testing.T) {
	url, work := newRemote(t)
	m := manager(t, "file://")
	ctx := context.Background()
	_, err := m.Clone(ctx, url, "chals", "", false, nopLog{})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(m.BaseDir, "chals", "local.txt"), []byte("x"), 0o644))

	_, err = m.Clone(ctx, url, "chals", "", false, nopLog{})
	assert.ErrorIs(t, err, ErrExists)

	push(t, work, "v2\n", "second")
	_, err = m.Clone(ctx, url, "chals", "", true, nopLog{})
	require.NoError(t, err)
	assert.Equal(t, "second", m.Describe(ctx, "chals").CommitMessage)
	assert.NoFileExists(t, filepath.Join(m.BaseDir, "chals", "local.txt"), "folder replaced")
}

func TestClone_FailedForceKeepsTheOldFolder(t *testing.T) {
	url, _ := newRemote(t)
	m := manager(t, "file://")
	ctx := context.Background()
	_, err := m.Clone(ctx, url, "chals", "", false, nopLog{})
	require.NoError(t, err)

	_, err = m.Clone(ctx, url+"-missing", "chals", "", true, nopLog{})
	require.Error(t, err)
	assert.FileExists(t, filepath.Join(m.BaseDir, "chals", "challenge.yml"), "the previous clone is kept")
}

func TestPull_FollowsBranchAndDiscardsLocalChanges(t *testing.T) {
	url, work := newRemote(t)
	m := manager(t, "file://")
	ctx := context.Background()
	_, err := m.Clone(ctx, url, "chals", "", false, nopLog{})
	require.NoError(t, err)
	file := filepath.Join(m.BaseDir, "chals", "challenge.yml")
	require.NoError(t, os.WriteFile(file, []byte("local edit\n"), 0o644))
	assert.True(t, m.Describe(ctx, "chals").Dirty)

	push(t, work, "v2\n", "second")
	log := &warnLog{}
	before, after, err := m.Pull(ctx, "chals", log)
	require.NoError(t, err)
	assert.NotEqual(t, before, after)
	data, _ := os.ReadFile(file)
	assert.Equal(t, "v2\n", string(data))
	assert.False(t, m.Describe(ctx, "chals").Dirty)
	require.Len(t, log.warnings, 1)
	assert.Equal(t, "Discarded local changes to 1 tracked file(s) of chals: challenge.yml", log.warnings[0])

	// Regression: with no new commit, local changes were discarded and the
	// pull only said the repository was up to date
	require.NoError(t, os.WriteFile(file, []byte("local edit\n"), 0o644))
	log = &warnLog{}
	before, after, err = m.Pull(ctx, "chals", log)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.Len(t, log.warnings, 1)
	assert.Contains(t, log.warnings[0], "Discarded local changes")
	data, _ = os.ReadFile(file)
	assert.Equal(t, "v2\n", string(data))

	log = &warnLog{}
	_, _, err = m.Pull(ctx, "chals", log)
	require.NoError(t, err)
	assert.Empty(t, log.warnings, "nothing discarded")
}

func TestChangedFiles(t *testing.T) {
	assert.Equal(t, []string{"a.yml", "dir/l'été.md", "new"}, changedFiles(" M a.yml\x00M  dir/l'été.md\x00R  new\x00old\x00"))
	assert.Empty(t, changedFiles(""))
}

func TestRun_CredentialPromptMessage(t *testing.T) {
	// Regression: a missing (or private) GitHub repository failed with
	// "unable to read askpass response from '/bin/false'"
	fake := filepath.Join(t.TempDir(), "git")
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\n"+
		"echo \"error: unable to read askpass response from '/bin/false'\" >&2\n"+
		"echo \"fatal: could not read Username for 'https://github.com': terminal prompts disabled\" >&2\n"+
		"exit 128\n"), 0o755))
	m := &Manager{BaseDir: t.TempDir(), Git: fake, Config: config.RepositoriesConfig{
		Sources: []config.RepositorySource{{URLPrefix: "https://github.com/org/"}},
	}}
	_, err := m.Clone(context.Background(), "https://github.com/org/missing.git", "", "", false, nopLog{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the repository does not exist, or it is private")
	assert.NotContains(t, err.Error(), "askpass")
}

func TestPull_FollowsTag(t *testing.T) {
	url, work := newRemote(t)
	m := manager(t, "file://")
	ctx := context.Background()
	_, err := m.Clone(ctx, url, "chals", "v1", false, nopLog{})
	require.NoError(t, err)
	info := m.Describe(ctx, "chals")
	assert.Equal(t, "", info.Branch, "a tag checks out a detached HEAD")
	assert.Equal(t, "v1", info.Ref)

	push(t, work, "v2\n", "second") // main moves, v1 does not
	before, after, err := m.Pull(ctx, "chals", nopLog{})
	require.NoError(t, err)
	assert.Equal(t, before, after, "still on v1")
}

func TestPull_RefusesRemoteOutsideSources(t *testing.T) {
	url, _ := newRemote(t)
	m := manager(t, "file://")
	ctx := context.Background()
	_, err := m.Clone(ctx, url, "chals", "", false, nopLog{})
	require.NoError(t, err)
	m.Config.Sources = []config.RepositorySource{{URLPrefix: "https://github.com/org/"}}
	_, _, err = m.Pull(ctx, "chals", nopLog{})
	assert.ErrorIs(t, err, ErrNotAllowed)
}

func TestToken_NeverWrittenToTheClone(t *testing.T) {
	url, _ := newRemote(t)
	m := &Manager{BaseDir: t.TempDir(), Config: config.RepositoriesConfig{
		Sources: []config.RepositorySource{{URLPrefix: "file://", Token: "s3cr3t-token"}},
	}}
	_, err := m.Clone(context.Background(), url, "chals", "", false, nopLog{})
	require.NoError(t, err)
	gitConfig, err := os.ReadFile(filepath.Join(m.BaseDir, "chals", ".git", "config"))
	require.NoError(t, err)
	assert.NotContains(t, string(gitConfig), "s3cr3t")
	assert.NotContains(t, string(gitConfig), "extraHeader")
}

func TestRemove(t *testing.T) {
	m := manager(t, "file://")
	require.NoError(t, os.MkdirAll(filepath.Join(m.BaseDir, "folder", "web"), 0o755))
	require.NoError(t, m.Remove("folder"))
	assert.NoDirExists(t, filepath.Join(m.BaseDir, "folder"))
	assert.ErrorIs(t, m.Remove("folder"), ErrNotFound)
	assert.Error(t, m.Remove(".."))
}

func TestHelpers(t *testing.T) {
	assert.Equal(t, "repo", NameFromURL("https://github.com/org/repo.git"))
	assert.Equal(t, "repo", NameFromURL("https://github.com/org/repo/"))
	assert.Equal(t, "repo", NameFromURL("git@github.com:org/repo.git"))
	assert.Equal(t, "https://github.com/org/r.git", MaskURL("https://u:tok@github.com/org/r.git"))

	m := &Manager{BaseDir: "/data/challenges"}
	assert.Equal(t, "repo", m.RepoOf("/data/challenges/repo/web/a"))
	assert.Equal(t, "", m.RepoOf("/data/challenges"))
	assert.Equal(t, "", m.RepoOf("/elsewhere/x"))
}

func TestClone_RelativeChallengeDir(t *testing.T) {
	// challenge_dir is relative in the usual configuration (./data/challenges)
	url, _ := newRemote(t)
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data", "challenges"), 0o755))
	t.Chdir(root)
	m := &Manager{BaseDir: "./data/challenges", Config: config.RepositoriesConfig{
		Sources: []config.RepositorySource{{URLPrefix: "file://"}},
	}}
	name, err := m.Clone(context.Background(), url, "chals", "", false, nopLog{})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(root, "data", "challenges", name, "challenge.yml"))
	assert.NoDirExists(t, filepath.Join(root, "data", "challenges", "data"), "nothing cloned relative to the wrong folder")
	assert.Equal(t, "chals", m.RepoOf("data/challenges/chals/web/x"), "relative paths of the index")
	_, _, err = m.Pull(context.Background(), "chals", nopLog{})
	assert.NoError(t, err)
}

func TestClone_WarnsAboutLFSWithoutGitLFS(t *testing.T) {
	url, work := newRemote(t)
	require.NoError(t, os.MkdirAll(filepath.Join(work, "forensic"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, "forensic", ".gitattributes"), []byte("dump.raw filter=lfs diff=lfs merge=lfs -text\n"), 0o644))
	git(t, work, "add", ".")
	git(t, work, "commit", "--quiet", "-m", "lfs")
	git(t, work, "push", "--quiet", "origin", "main")
	// Hide git-lfs if the machine has it: only git stays on PATH
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	require.NoError(t, os.Symlink(gitPath, filepath.Join(bin, "git")))
	t.Setenv("PATH", bin)

	m := manager(t, "file://")
	log := &warnLog{}
	_, err = m.Clone(context.Background(), url, "chals", "", false, log)
	require.NoError(t, err)
	require.Len(t, log.warnings, 1)
	assert.Contains(t, log.warnings[0], "git-lfs is not installed")
}
