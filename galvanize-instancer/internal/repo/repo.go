// Package repo manages the challenge repositories kept in challenge_dir:
// each top-level folder of challenge_dir is a repository (a git clone, or a
// plain folder copied there by hand).
//
// Clones are only allowed from the URL prefixes listed in the configuration
// (repositories.sources). Credentials of a source are given to git through
// its environment for the duration of one command: they never appear in a
// URL, a command line or the clone's .git/config.
package repo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/28Pollux28/galvanize/internal/operation"
	"github.com/28Pollux28/galvanize/internal/pathutil"
	"github.com/28Pollux28/galvanize/pkg/config"
)

// tmpDirName holds clones in progress, inside challenge_dir so that the
// final rename stays on one filesystem. The challenge index skips hidden
// folders.
const tmpDirName = ".galvanize-tmp"

// refConfigKey records, in the clone's git config, the branch or tag it was
// cloned from, so that Pull follows the same ref.
const refConfigKey = "galvanize.ref"

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	refRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

	ErrNotFound      = errors.New("repository not found")
	ErrExists        = errors.New("repository already exists")
	ErrNotAllowed    = errors.New("URL not allowed")
	ErrCloneDisabled = errors.New("cloning is disabled: no repositories.sources configured")
)

// Info describes a repository.
type Info struct {
	Name          string
	IsGit         bool
	Remote        string // without credentials
	Branch        string // empty when HEAD is detached
	Ref           string // branch or tag followed by Pull
	Commit        string
	CommitMessage string
	CommitTime    *time.Time
	Dirty         bool
	Error         string
}

// Manager works on the repositories of one challenge_dir.
type Manager struct {
	BaseDir string
	Config  config.RepositoriesConfig
	// Git is the git executable (default "git").
	Git string
}

func (m *Manager) git() string {
	if m.Git != "" {
		return m.Git
	}
	return "git"
}

// ValidName reports whether name can be a repository folder.
func ValidName(name string) bool { return nameRe.MatchString(name) && !strings.Contains(name, "..") }

// Path returns the folder of an existing repository.
func (m *Manager) Path(name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid repository name %q", name)
	}
	p := filepath.Join(m.BaseDir, name)
	st, err := os.Stat(p)
	if err != nil || !st.IsDir() {
		return "", ErrNotFound
	}
	return p, nil
}

// RepoOf returns the repository holding path (a path inside BaseDir), or ""
// when path is not inside a repository folder.
func (m *Manager) RepoOf(path string) string {
	base, err1 := filepath.Abs(m.BaseDir)
	abs, err2 := filepath.Abs(path)
	if err1 != nil || err2 != nil {
		return ""
	}
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == "." || !pathutil.Inside(base, abs) {
		return ""
	}
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	if !ValidName(first) {
		return ""
	}
	return first
}

// List returns every repository, sorted by name.
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	entries, err := os.ReadDir(m.BaseDir)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		out = append(out, m.Describe(ctx, e.Name()))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Describe returns the git state of a repository folder.
func (m *Manager) Describe(ctx context.Context, name string) Info {
	info := Info{Name: name}
	dir := filepath.Join(m.BaseDir, name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return info
	}
	info.IsGit = true
	if out, err := m.run(ctx, dir, nil, "remote", "get-url", "origin"); err == nil {
		info.Remote = MaskURL(strings.TrimSpace(out))
	}
	if out, err := m.run(ctx, dir, nil, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		info.Branch = strings.TrimSpace(out)
	}
	if out, err := m.run(ctx, dir, nil, "config", "--get", refConfigKey); err == nil {
		info.Ref = strings.TrimSpace(out)
	} else {
		info.Ref = info.Branch
	}
	out, err := m.run(ctx, dir, nil, "log", "-1", "--format=%h%x00%ct%x00%s")
	if err != nil {
		info.Error = firstLine(err.Error())
		return info
	}
	parts := strings.SplitN(strings.TrimSpace(out), "\x00", 3)
	if len(parts) == 3 {
		info.Commit = parts[0]
		if ts, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			t := time.Unix(ts, 0).UTC()
			info.CommitTime = &t
		}
		info.CommitMessage = parts[2]
	}
	if out, err := m.run(ctx, dir, nil, "status", "--porcelain", "--untracked-files=no"); err == nil {
		info.Dirty = strings.TrimSpace(out) != ""
	}
	return info
}

// Commit returns the HEAD commit of a repository (abbreviated, as Describe
// gives it), "" when it is not a git clone.
func (m *Manager) Commit(ctx context.Context, name string) string {
	dir := filepath.Join(m.BaseDir, name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return ""
	}
	out, err := m.run(ctx, dir, nil, "log", "-1", "--format=%h")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// source returns the configured source allowing rawURL (see
// config.RepositorySource.Allows: scheme, host, port and path folders are
// compared, never the raw string).
func (m *Manager) source(rawURL string) (*config.RepositorySource, error) {
	if len(m.Config.Sources) == 0 {
		return nil, ErrCloneDisabled
	}
	if err := config.CheckRepositoryURL(rawURL); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotAllowed, err)
	}
	if src := m.Config.SourceFor(rawURL); src != nil {
		return src, nil
	}
	return nil, fmt.Errorf("%w: %s is not inside an allowed source", ErrNotAllowed, rawURL)
}

// NameFromURL derives a folder name from a repository URL.
func NameFromURL(rawURL string) string {
	name := strings.TrimSuffix(strings.TrimRight(rawURL, "/"), ".git")
	if i := strings.LastIndexAny(name, "/:"); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// Clone clones rawURL into BaseDir/name (name defaults from the URL). With
// force, an existing folder of that name is replaced once the new clone has
// succeeded.
func (m *Manager) Clone(ctx context.Context, rawURL, name, ref string, force bool, log operation.Logger) (string, error) {
	src, err := m.source(rawURL)
	if err != nil {
		return "", err
	}
	if name == "" {
		name = NameFromURL(rawURL)
	}
	if !ValidName(name) {
		return "", fmt.Errorf("invalid repository name %q", name)
	}
	if ref != "" && !refRe.MatchString(ref) {
		return "", fmt.Errorf("invalid branch or tag %q", ref)
	}
	// Absolute paths: git runs in another folder than Galvanize, and
	// challenge_dir is usually relative (./data/challenges)
	base, err := filepath.Abs(m.BaseDir)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(base, name)
	if _, err := os.Stat(dest); err == nil && !force {
		return "", fmt.Errorf("%w: %s (use force to replace it)", ErrExists, name)
	}

	tmpRoot := filepath.Join(base, tmpDirName)
	if err := os.MkdirAll(tmpRoot, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(tmpRoot, name+"-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	clone := filepath.Join(tmp, "clone")

	args := []string{"clone", "--quiet"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, "--", rawURL, clone)
	if ref != "" {
		log.Infof("Cloning %s (%s) into %s", rawURL, ref, name)
	} else {
		log.Infof("Cloning %s into %s", rawURL, name)
	}
	if _, err := m.run(ctx, base, src, args...); err != nil {
		return "", fmt.Errorf("git clone failed: %s", firstLine(err.Error()))
	}
	if ref != "" {
		if _, err := m.run(ctx, clone, nil, "config", refConfigKey, ref); err != nil {
			return "", err
		}
	}

	if _, err := os.Stat(dest); err == nil {
		old := filepath.Join(tmp, "previous")
		if err := os.Rename(dest, old); err != nil {
			return "", fmt.Errorf("cannot replace %s: %w", name, err)
		}
		log.Infof("Replacing the previous %s", name)
	}
	if err := os.Rename(clone, dest); err != nil {
		return "", fmt.Errorf("cannot move the clone into place: %w", err)
	}
	m.warnLFS(dest, log)
	return name, nil
}

// warnLFS warns when the repository stores files with Git LFS but git-lfs is
// not installed: the folder then holds small pointer files instead of them.
func (m *Manager) warnLFS(dir string, log operation.Logger) {
	if _, err := exec.LookPath("git-lfs"); err == nil {
		return
	}
	found := ""
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return filepath.SkipDir
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == ".gitattributes" {
			if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "filter=lfs") {
				found = path
			}
		}
		return nil
	})
	if found != "" {
		rel, _ := filepath.Rel(dir, found)
		log.Warnf("The repository uses Git LFS (%s) but git-lfs is not installed: LFS files are pointers, not their content", rel)
	}
}

// Pull fetches the ref the repository follows and hard-resets to it: the
// folder becomes an exact copy of the remote (local changes to tracked files
// are discarded). Returns the commit before and after.
func (m *Manager) Pull(ctx context.Context, name string, log operation.Logger) (string, string, error) {
	dir, err := m.Path(name)
	if err != nil {
		return "", "", err
	}
	info := m.Describe(ctx, name)
	if !info.IsGit {
		return "", "", fmt.Errorf("%s is not a git repository", name)
	}
	if info.Ref == "" {
		return "", "", fmt.Errorf("%s: HEAD is detached and no branch or tag is recorded", name)
	}
	remote, err := m.run(ctx, dir, nil, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("%s has no origin remote", name)
	}
	src, err := m.source(strings.TrimSpace(remote))
	if err != nil {
		return "", "", err
	}
	log.Infof("Fetching %s from %s", info.Ref, info.Remote)
	if _, err := m.run(ctx, dir, src, "fetch", "--quiet", "--force", "--tags", "origin", info.Ref); err != nil {
		return "", "", fmt.Errorf("git fetch failed: %s", firstLine(err.Error()))
	}
	changed, _ := m.run(ctx, dir, nil, "status", "--porcelain", "-z", "--untracked-files=no")
	if _, err := m.run(ctx, dir, nil, "reset", "--quiet", "--hard", "FETCH_HEAD"); err != nil {
		return "", "", fmt.Errorf("git reset failed: %s", firstLine(err.Error()))
	}
	if files := changedFiles(changed); len(files) > 0 {
		shown := files
		if len(shown) > maxListedChanges {
			shown = append(shown[:maxListedChanges:maxListedChanges], fmt.Sprintf("and %d more", len(files)-maxListedChanges))
		}
		log.Warnf("Discarded local changes to %d tracked file(s) of %s: %s", len(files), name, strings.Join(shown, ", "))
	}
	after := m.Describe(ctx, name).Commit
	m.warnLFS(dir, log)
	return info.Commit, after, nil
}

// maxListedChanges is how many discarded files Pull names.
const maxListedChanges = 10

// changedFiles reads the paths of git status --porcelain -z (unquoted;
// a rename or copy is followed by its origin, which is skipped).
func changedFiles(porcelain string) []string {
	var out []string
	entries := strings.Split(porcelain, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) <= 3 {
			continue
		}
		out = append(out, e[3:])
		if e[0] == 'R' || e[0] == 'C' {
			i++
		}
	}
	return out
}

// Remove deletes a repository folder.
func (m *Manager) Remove(name string) error {
	dir, err := m.Path(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// run runs git in dir. src, when set, provides the credentials.
func (m *Manager) run(ctx context.Context, dir string, src *config.RepositorySource, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, m.Config.GitTimeoutOrDefault())
	defer cancel()
	cmd := exec.CommandContext(ctx, m.git(), args...)
	// A killed git can leave children (git-remote-https, git-lfs) holding
	// the output pipes: do not wait for them forever
	cmd.WaitDelay = 10 * time.Second
	cmd.Dir = dir
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
	)
	// Configuration through the environment (git 2.31+): nothing is written
	// to disk and nothing shows in the process list. safe.directory: the
	// folders may belong to another user (copied by hand, or by setup tools).
	// The other settings keep a repository's content from running anything
	// in Galvanize (hooks, fsmonitor) and git from using transports that run
	// commands or read local paths given by a URL.
	gitConf := [][2]string{
		{"safe.directory", "*"},
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"protocol.ext.allow", "never"},
	}
	if src != nil && src.Token != "" {
		user := src.Username
		if user == "" {
			user = "x-access-token"
		}
		basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + src.Token))
		// Scoped to the source: git (and git-lfs) send the header only to
		// URLs below url_prefix, not to an LFS server a repository's
		// .lfsconfig names. No redirects: git would send the header to the
		// redirected host for the rest of the command.
		gitConf = append(gitConf,
			[2]string{"http." + src.URLPrefix + ".extraHeader", "Authorization: Basic " + basic},
			[2]string{"http.followRedirects", "false"},
		)
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(gitConf)))
	for i, kv := range gitConf {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if ctx.Err() == context.DeadlineExceeded {
			msg = "timed out"
		} else if strings.Contains(msg, "could not read Username") || strings.Contains(msg, "could not read Password") {
			// Prompts are disabled: git's first line would be "unable to
			// read askpass response from '/bin/false'"
			msg = "the server asked for credentials: the repository does not exist, or it is private and its source in repositories.sources has no token able to read it"
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return stdout.String(), nil
}

// MaskURL removes credentials from a URL.
func MaskURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
