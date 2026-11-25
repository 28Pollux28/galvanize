package pkg

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/auth"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/api"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/28Pollux28/galvanize/pkg/models"
	"github.com/28Pollux28/galvanize/pkg/utils"
	"github.com/golang-jwt/jwt/v5"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "repo-test-secret"

type recordingRunner struct {
	mu    sync.Mutex
	calls []map[string]interface{}
}

func (r *recordingRunner) Run(ctx context.Context, conf *config.Config, name string, vars map[string]interface{}, _ time.Duration) ([]ansible.HostResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := map[string]interface{}{"playbook": name}
	for k, v := range vars {
		call[k] = v
	}
	r.calls = append(r.calls, call)
	if name == ansible.PlaybookImages {
		var lines []string
		for _, i := range vars["images"].([]string) {
			lines = append(lines, "present "+i)
		}
		return []ansible.HostResult{{Host: "h1", Task: "Images", Stdout: lines}}, nil
	}
	return []ansible.HostResult{{Host: "h1", Task: "Build"}}, nil
}

type repoEnv struct {
	t      *testing.T
	e      *echo.Echo
	srv    *Server
	dir    string
	runner *recordingRunner
}

// newRepoEnv serves the API like `galvanize serve` (generated routes, JWT
// middleware) on a challenge_dir holding one repository.
func newRepoEnv(t *testing.T, sources ...config.RepositorySource) *repoEnv {
	t.Helper()
	dir := t.TempDir()
	writeChallenge(t, dir, "Repo/web/app", "name: App\ncategory: Web\ntype: zync\nplaybook_name: http\ndeploy_parameters:\n  image: app\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Repo/web/app/Dockerfile"), []byte("FROM busybox\n"), 0o644))
	writeChallenge(t, dir, "Repo/misc/intro", "name: Intro\ncategory: Misc\ntype: standard\nflags: [\"F{intro}\"]\nfiles: [notes.txt]\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Repo/misc/intro/notes.txt"), []byte("notes"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "Repo/.git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Repo/.git/config"), []byte("[core]"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Repo/secret.txt"), []byte("not a challenge file"), 0o644))
	writeChallenge(t, dir, "Other/pwn/bin", "name: Bin\ncategory: Pwn\ntype: zync\nplaybook_name: tcp\ndeploy_parameters:\n  image: ghcr.io/org/bin:1\n")

	cfg := &config.Config{
		Auth: config.AuthConfig{JWTSecret: testSecret},
		Instancer: config.InstancerConfig{
			ChallengeDir: dir,
			Repositories: config.RepositoriesConfig{Sources: sources},
			Ansible:      config.AnsibleConfig{Inventory: "h1,"},
		},
	}
	db, err := InitDB(":memory:")
	require.NoError(t, err)
	idx, err := challenge.NewChallengeIndex(dir)
	require.NoError(t, err)
	runner := &recordingRunner{}
	srv := NewServerWithOpts(ServerOpts{DB: db, ChallengeIndexer: idx, ConfigProvider: &config.StaticProvider{Cfg: cfg}, UtilityRunner: runner})

	e := echo.New()
	e.Use(echojwt.WithConfig(echojwt.Config{
		NewClaimsFunc: func(c echo.Context) jwt.Claims { return new(auth.Claims) },
		SigningKey:    []byte(testSecret),
	}))
	api.RegisterHandlers(e, srv)
	return &repoEnv{t: t, e: e, srv: srv, dir: dir, runner: runner}
}

func (env *repoEnv) do(method, path, role string, body interface{}) *httptest.ResponseRecorder {
	env.t.Helper()
	var reader io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		reader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	// "admin" is the platform server's token (manage scope); "admin-browser"
	// the admin token a dashboard hands to browsers (no scope)
	claims := &auth.Claims{Role: role, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	switch role {
	case "admin":
		claims.Scope = auth.ScopeManage
	case "admin-browser":
		claims.Role = "admin"
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, _ := token.SignedString([]byte(testSecret))
	req.Header.Set("Authorization", "Bearer "+signed)
	rec := httptest.NewRecorder()
	env.e.ServeHTTP(rec, req)
	return rec
}

func sha1Hex(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v), rec.Body.String())
	return v
}

// waitOp polls an operation until it finishes.
func (env *repoEnv) waitOp(id string) api.Operation {
	env.t.Helper()
	for i := 0; i < 500; i++ {
		rec := env.do("GET", "/admin/operations/"+id, "admin", nil)
		require.Equal(env.t, 200, rec.Code)
		op := decode[api.Operation](env.t, rec)
		if op.Status != "running" {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	env.t.Fatal("operation did not finish")
	return api.Operation{}
}

func TestRepoAPI_RequiresAdmin(t *testing.T) {
	env := newRepoEnv(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/admin/repos"}, {"POST", "/admin/repos"}, {"POST", "/admin/repos/Repo/pull"},
		{"DELETE", "/admin/repos/Repo"}, {"GET", "/admin/challenges"}, {"GET", "/admin/challenges/file?path=Repo/misc/intro/notes.txt"},
		{"POST", "/admin/build"}, {"POST", "/admin/images"}, {"GET", "/admin/operations"},
	} {
		assert.Equal(t, 403, env.do(c.method, c.path, "user", map[string]string{}).Code, "%s %s", c.method, c.path)
		rec := env.do(c.method, c.path, "admin-browser", map[string]string{})
		assert.Equal(t, 403, rec.Code, "admin without the manage scope: %s %s", c.method, c.path)
		assert.Contains(t, rec.Body.String(), "manage")
	}
	req := httptest.NewRequest("GET", "/admin/repos", nil)
	rec := httptest.NewRecorder()
	env.e.ServeHTTP(rec, req)
	assert.Equal(t, 401, rec.Code, "no token")
}

func TestListRepositories(t *testing.T) {
	env := newRepoEnv(t, config.RepositorySource{URLPrefix: "https://github.com/org/", Token: "secret"})
	rec := env.do("GET", "/admin/repos", "admin", nil)
	require.Equal(t, 200, rec.Code)
	assert.NotContains(t, rec.Body.String(), "secret", "tokens are never returned")
	list := decode[api.RepositoryList](t, rec)
	assert.Equal(t, []string{"https://github.com/org/"}, list.CloneSources)
	require.Len(t, list.Repositories, 2)
	assert.Equal(t, "Other", list.Repositories[0].Name)
	assert.Equal(t, 2, list.Repositories[1].Challenges)
}

func TestListChallengeDefinitions(t *testing.T) {
	env := newRepoEnv(t)
	rec := env.do("GET", "/admin/challenges?repo=Repo", "admin", nil)
	require.Equal(t, 200, rec.Code)
	list := decode[api.ChallengeDefinitionList](t, rec)
	require.Len(t, list.Challenges, 2)
	byName := map[string]api.ChallengeDefinition{}
	for _, c := range list.Challenges {
		byName[c.Name] = c
	}
	intro := byName["Intro"]
	assert.Equal(t, "Repo", intro.Repository)
	assert.Equal(t, "Repo/misc/intro/challenge.yml", intro.Path)
	assert.Equal(t, "Repo/misc/intro", intro.Dir)
	assert.False(t, intro.Deployable, "standard challenges are listed but not deployable")
	assert.Equal(t, []interface{}{"F{intro}"}, intro.Definition["flags"])
	assert.Equal(t, api.BuildPlanKind("none"), intro.Build.Kind)
	assert.False(t, intro.Build.Buildable)
	require.Len(t, intro.Files, 1)
	assert.Equal(t, api.ChallengeFile{Declared: "notes.txt", Path: utils.Ptr("Repo/misc/intro/notes.txt"), Size: utils.Ptr(int64(5))}, intro.Files[0])
	assert.Nil(t, intro.Commit, "not a git clone")
	app := byName["App"]
	assert.True(t, app.Deployable)
	assert.Equal(t, api.BuildPlanKind("image"), app.Build.Kind)
	assert.True(t, app.Build.Buildable)
	assert.Empty(t, app.Files)

	all := decode[api.ChallengeDefinitionList](t, env.do("GET", "/admin/challenges", "admin", nil))
	assert.Len(t, all.Challenges, 3)

	// Regression: an unknown repository listed nothing with 200, and
	// clients took it for an empty repository
	rec = env.do("GET", "/admin/challenges?repo=Missing", "admin", nil)
	assert.Equal(t, 404, rec.Code)
	assert.Contains(t, rec.Body.String(), "Repository not found: Missing")
	assert.Equal(t, 404, env.do("GET", "/admin/challenges?repo=..", "admin", nil).Code)
}

func TestGetChallengeFile(t *testing.T) {
	env := newRepoEnv(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o644))
	require.NoError(t, os.Symlink(outside, filepath.Join(env.dir, "Repo/misc/intro/link.txt")))

	rec := env.do("GET", "/admin/challenges/file?path=Repo/misc/intro/notes.txt", "admin", nil)
	require.Equal(t, 200, rec.Code)
	assert.Equal(t, "notes", rec.Body.String())

	sum := decode[api.FileChecksum](t, env.do("GET", "/admin/challenges/file?path=Repo/misc/intro/notes.txt&checksum=true", "admin", nil))
	assert.Equal(t, api.FileChecksum{Sha1: sha1Hex("notes"), Size: 5}, sum)

	for path, code := range map[string]int{
		"Repo/misc/intro/../../secret.txt": 404, // inside the repository, but not in a challenge folder
		"Repo/secret.txt":                  404,
		"Repo/.git/config":                 400,
		"../outside.txt":                   400,
		"/etc/passwd":                      400,
		"Repo/misc/intro/link.txt":         400, // symlink leading outside
		"Repo/misc/intro/missing.txt":      404,
		"Repo/misc/intro":                  404, // a folder
	} {
		assert.Equal(t, code, env.do("GET", "/admin/challenges/file?path="+path, "admin", nil).Code, path)
	}
}

func TestCloneRepository_Refusals(t *testing.T) {
	env := newRepoEnv(t)
	assert.Equal(t, 403, env.do("POST", "/admin/repos", "admin", map[string]string{"url": "https://github.com/org/x.git"}).Code, "no sources: cloning disabled")

	env = newRepoEnv(t, config.RepositorySource{URLPrefix: "https://github.com/org/"})
	assert.Equal(t, 403, env.do("POST", "/admin/repos", "admin", map[string]string{"url": "https://evil.example/x.git"}).Code)
	assert.Equal(t, 400, env.do("POST", "/admin/repos", "admin", map[string]string{}).Code)
	assert.Equal(t, 400, env.do("POST", "/admin/repos", "admin", map[string]string{"url": "https://github.com/org/x.git", "name": "../x"}).Code)
	assert.Equal(t, 409, env.do("POST", "/admin/repos", "admin", map[string]string{"url": "https://github.com/org/Repo.git"}).Code, "exists")
	for _, url := range []string{
		"https://github.com/org-evil/x.git",
		"https://github.com.evil.tld/org/x.git",
		"https://github.com/org/../attacker/x.git",
		"https://github.com/org/%2e%2e/attacker/x.git",
		"https://user:pw@github.com/org/x.git",
	} {
		assert.Equal(t, 403, env.do("POST", "/admin/repos", "admin", map[string]string{"url": url, "name": "x"}).Code, url)
	}
}

func TestListChallengeDefinitions_UnencodableDefinitionIsSkipped(t *testing.T) {
	env := newRepoEnv(t)
	writeChallenge(t, env.dir, "Repo/misc/nan", "name: Nan\ncategory: Misc\ntype: standard\nvalue: .nan\n")
	writeChallenge(t, env.dir, "Repo/misc/keys", "name: Keys\ncategory: Misc\ntype: standard\nextra:\n  1: one\n  true: yes\n")
	require.NoError(t, env.srv.challIdx.BuildIndex(env.dir))
	rec := env.do("GET", "/admin/challenges", "admin", nil)
	require.Equal(t, 200, rec.Code, "one bad definition does not break the catalog")
	list := decode[api.ChallengeDefinitionList](t, rec)
	names := map[string]api.ChallengeDefinition{}
	for _, c := range list.Challenges {
		names[c.Name] = c
	}
	assert.NotContains(t, names, "Nan")
	assert.Equal(t, map[string]interface{}{"1": "one", "true": "yes"}, names["Keys"].Definition["extra"], "scalar keys become strings")
	var reasons []string
	for _, sk := range list.Skipped {
		reasons = append(reasons, sk.Path+": "+sk.Reason)
	}
	assert.Contains(t, strings.Join(reasons, "\n"), "Repo/misc/nan/challenge.yml: challenge file cannot be served as JSON")
}

func TestBuild_RefusesAnImageOfAnotherChallenge(t *testing.T) {
	env := newRepoEnv(t)
	// Other/pwn/bin deploys ghcr.io/org/bin:1; a challenge of Repo builds it
	writeChallenge(t, env.dir, "Repo/pwn/evil", "name: Evil\ncategory: Pwn\ntype: zync\nplaybook_name: tcp\ndeploy_parameters:\n  image: ghcr.io/org/bin:1\n")
	require.NoError(t, os.WriteFile(filepath.Join(env.dir, "Repo/pwn/evil/Dockerfile"), []byte("FROM busybox\n"), 0o644))
	writeChallenge(t, env.dir, "Repo/web/proxy", "name: Proxy\ncategory: Web\ntype: zync\nplaybook_name: http\ndeploy_parameters:\n  image: traefik:v3.7\n")
	require.NoError(t, os.WriteFile(filepath.Join(env.dir, "Repo/web/proxy/Dockerfile"), []byte("FROM busybox\n"), 0o644))
	require.NoError(t, env.srv.challIdx.BuildIndex(env.dir))

	list := decode[api.ChallengeDefinitionList](t, env.do("GET", "/admin/challenges?repo=Repo", "admin", nil))
	for _, c := range list.Challenges {
		switch c.Name {
		case "Evil":
			assert.Equal(t, api.BuildPlanKind("none"), c.Build.Kind)
			assert.Contains(t, *c.Build.Reason, "also used by")
			assert.True(t, *c.Build.Refused)
			assert.False(t, c.Build.Buildable)
		case "Proxy":
			assert.Contains(t, *c.Build.Reason, "platform image")
		}
	}

	rec := env.do("POST", "/admin/build", "admin", map[string]interface{}{"challenges": []map[string]string{{"category": "Pwn", "name": "Evil"}, {"category": "Web", "name": "Proxy"}}})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	op := env.waitOp(decode[api.Operation](t, rec).Id)
	assert.Equal(t, api.OperationStatus("failed"), op.Status, "nothing built")
	assert.Equal(t, []interface{}{"Repo/pwn/evil/challenge.yml", "Repo/web/proxy/challenge.yml"}, (*op.Result)["failed"], "refused builds fail")
	assert.Empty(t, env.runner.calls, "no build ran")

	rec = env.do("POST", "/admin/images", "admin", map[string]interface{}{"action": "remove", "challenges": []map[string]string{{"category": "Pwn", "name": "Evil"}}})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	env.waitOp(decode[api.Operation](t, rec).Id)
	assert.Empty(t, env.runner.calls, "the other challenge's image is not removed")
}

func TestCloneAndPullRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	bare := filepath.Join(root, "chals.git")
	work := filepath.Join(root, "work")
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run(root, "init", "--quiet", "--bare", "-b", "main", bare)
	run(root, "clone", "--quiet", bare, work)
	writeChallenge(t, work, "web/new", "name: New\ncategory: Web\ntype: zync\nplaybook_name: http\ndeploy_parameters:\n  image: new\n")
	run(work, "add", ".")
	run(work, "commit", "--quiet", "-m", "one")
	run(work, "push", "--quiet", "origin", "main")

	env := newRepoEnv(t, config.RepositorySource{URLPrefix: "file://"})
	rec := env.do("POST", "/admin/repos", "admin", map[string]string{"url": "file://" + bare})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	op := env.waitOp(decode[api.Operation](t, rec).Id)
	require.Equal(t, api.OperationStatus("success"), op.Status, "%+v", op.Lines)
	_, err := env.srv.challIdx.Get("Web", "New")
	assert.NoError(t, err, "the index is reloaded after the clone")
	listing := decode[api.ChallengeDefinitionList](t, env.do("GET", "/admin/challenges?repo=chals", "admin", nil))
	require.Len(t, listing.Challenges, 1)
	require.NotNil(t, listing.Challenges[0].Commit, "the repository's commit")
	first := *listing.Challenges[0].Commit

	// Busy while running is covered by the operation tests; here: pull
	writeChallenge(t, work, "web/new2", "name: New2\ncategory: Web\ntype: zync\nplaybook_name: http\ndeploy_parameters:\n  image: new2\n")
	run(work, "add", ".")
	run(work, "commit", "--quiet", "-m", "two")
	run(work, "push", "--quiet", "origin", "main")
	rec = env.do("POST", "/admin/repos/chals/pull", "admin", nil)
	require.Equal(t, 202, rec.Code, rec.Body.String())
	op = env.waitOp(decode[api.Operation](t, rec).Id)
	require.Equal(t, api.OperationStatus("success"), op.Status)
	_, err = env.srv.challIdx.Get("Web", "New2")
	assert.NoError(t, err)
	listing = decode[api.ChallengeDefinitionList](t, env.do("GET", "/admin/challenges?repo=chals", "admin", nil))
	require.Len(t, listing.Challenges, 2)
	assert.NotEqual(t, first, *listing.Challenges[0].Commit, "a pull changes the commit")
	assert.Empty(t, env.runner.calls, "a clone or a pull builds nothing")

	assert.Equal(t, 404, env.do("POST", "/admin/repos/missing/pull", "admin", nil).Code)
}

func TestRemoveRepository_RefusedWhileInstancesExist(t *testing.T) {
	env := newRepoEnv(t)
	team := "7"
	require.NoError(t, env.srv.db.Create(&models.Deployment{ChallengeName: "App", Category: "Web", TeamID: &team, Status: models.DeploymentStatusRunning}).Error)

	rec := env.do("DELETE", "/admin/repos/Repo", "admin", nil)
	assert.Equal(t, 409, rec.Code)
	assert.Contains(t, rec.Body.String(), "Web/App (team 7, running)")
	assert.DirExists(t, filepath.Join(env.dir, "Repo"))

	rec = env.do("DELETE", "/admin/repos/Other", "admin", nil)
	require.Equal(t, 202, rec.Code)
	op := env.waitOp(decode[api.Operation](t, rec).Id)
	assert.Equal(t, api.OperationStatus("success"), op.Status)
	assert.NoDirExists(t, filepath.Join(env.dir, "Other"))
	_, err := env.srv.challIdx.Get("Pwn", "Bin")
	assert.Error(t, err, "index reloaded without the removed repository")
}

func TestBuildAndImages(t *testing.T) {
	env := newRepoEnv(t)
	rec := env.do("POST", "/admin/build", "admin", map[string]interface{}{"repo": "Repo", "no_cache": true})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	op := env.waitOp(decode[api.Operation](t, rec).Id)
	require.Equal(t, api.OperationStatus("success"), op.Status)
	assert.Equal(t, []interface{}{"Repo/web/app/challenge.yml"}, (*op.Result)["built"], "results name definition files")
	require.Len(t, env.runner.calls, 1)
	argv := strings.Join(env.runner.calls[0]["build_argv"].([]string), " ")
	assert.Equal(t, "docker build --progress=plain --no-cache -t app -f Dockerfile .", argv)

	rec = env.do("POST", "/admin/build", "admin", map[string]interface{}{"challenges": []map[string]string{{"category": "Misc", "name": "Intro"}}})
	assert.Equal(t, 400, rec.Code, "only deployable challenges can be built")
	rec = env.do("POST", "/admin/build", "admin", map[string]interface{}{"challenges": []map[string]string{{"path": "Repo/misc/intro/challenge.yml"}}})
	assert.Equal(t, 400, rec.Code, "by path too")

	// By definition path, with a challenge that has nothing to build
	rec = env.do("POST", "/admin/build", "admin", map[string]interface{}{"challenges": []map[string]string{
		{"path": "Repo/web/app/challenge.yml"}, {"path": "Other/pwn/bin/challenge.yml"}}})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	op = env.waitOp(decode[api.Operation](t, rec).Id)
	assert.Equal(t, []interface{}{"Repo/web/app/challenge.yml"}, (*op.Result)["built"])
	assert.Equal(t, []interface{}{"Other/pwn/bin/challenge.yml"}, (*op.Result)["skipped"])

	rec = env.do("POST", "/admin/images", "admin", map[string]interface{}{"action": "status"})
	require.Equal(t, 202, rec.Code, rec.Body.String())
	op = env.waitOp(decode[api.Operation](t, rec).Id)
	images := (*op.Result)["images"].(map[string]interface{})
	assert.Equal(t, map[string]interface{}{"h1": "present"}, images["app"])
	assert.Contains(t, images, "ghcr.io/org/bin:1")
	assert.Equal(t, []interface{}{"app"}, (*op.Result)["challenges"].(map[string]interface{})["Repo/web/app/challenge.yml"])

	team := "1"
	require.NoError(t, env.srv.db.Create(&models.Deployment{ChallengeName: "App", Category: "Web", TeamID: &team, Status: models.DeploymentStatusRunning}).Error)
	rec = env.do("POST", "/admin/images", "admin", map[string]interface{}{"action": "remove", "repo": "Repo"})
	assert.Equal(t, 409, rec.Code, "images in use are not removed")
	assert.Equal(t, 400, env.do("POST", "/admin/images", "admin", map[string]interface{}{"action": "explode"}).Code)

	list := decode[[]api.Operation](t, env.do("GET", "/admin/operations", "admin", nil))
	require.Len(t, list, 3)
	assert.Equal(t, api.OperationKind("images"), list[0].Kind, "most recent first")
	assert.Empty(t, list[0].Lines, "the list has no logs")
	assert.Equal(t, 404, env.do("GET", "/admin/operations/nope", "admin", nil).Code)

	// Regression: cancelling a finished operation answered 202
	rec = env.do("POST", "/admin/operations/"+list[0].Id+"/cancel", "admin", nil)
	assert.Equal(t, 409, rec.Code)
	assert.Contains(t, rec.Body.String(), "already finished")
	assert.Equal(t, 404, env.do("POST", "/admin/operations/nope/cancel", "admin", nil).Code)
}

func TestBuildCatalog_MadeOncePerIndex(t *testing.T) {
	env := newRepoEnv(t)
	cat := env.srv.buildCatalog()
	assert.Same(t, cat, env.srv.buildCatalog(), "kept while the index is unchanged")
	require.NoError(t, env.srv.challIdx.BuildIndex(env.dir))
	assert.NotSame(t, cat, env.srv.buildCatalog(), "made again after a reindex")
}

func TestListChallengeDefinitions_DeclaredFiles(t *testing.T) {
	env := newRepoEnv(t)
	dir := filepath.Join(env.dir, "Repo/misc/files")
	writeChallenge(t, env.dir, "Repo/misc/files", "name: Files\ncategory: Misc\ntype: standard\n"+
		"files: [a.txt, b.txt, missing.txt, /etc/passwd, ../intro/notes.txt, .git/config, out.txt, lfs.bin]\n")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "files"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "files/b.txt"), []byte("bb"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join(env.dir, "Repo/secret.txt"), filepath.Join(dir, "out.txt")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lfs.bin"), []byte("version https://git-lfs.github.com/spec/v1\noid sha256:x\nsize 9\n"), 0o644))
	require.NoError(t, env.srv.challIdx.BuildIndex(env.dir))

	list := decode[api.ChallengeDefinitionList](t, env.do("GET", "/admin/challenges?repo=Repo", "admin", nil))
	var files []api.ChallengeFile
	for _, c := range list.Challenges {
		if c.Name == "Files" {
			files = c.Files
		}
	}
	require.Len(t, files, 8)
	assert.Equal(t, "Repo/misc/files/a.txt", *files[0].Path)
	assert.Equal(t, "Repo/misc/files/files/b.txt", *files[1].Path, "then the files/ subfolder")
	assert.Equal(t, int64(2), *files[1].Size)
	for i, want := range []string{"not found", "absolute", "leaves the challenge folder", ".git", "leads outside", "Git LFS pointer"} {
		f := files[i+2]
		require.NotNil(t, f.Error, f.Declared)
		assert.Contains(t, *f.Error, want, f.Declared)
	}
	assert.Nil(t, files[0].Error)
}
