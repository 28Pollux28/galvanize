package pkg

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/28Pollux28/galvanize/internal/auth"
	"github.com/28Pollux28/galvanize/internal/challenge"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func init() {
	zap.ReplaceGlobals(zap.NewNop())
}

// createEchoContextWithClaims builds an Echo context with JWT claims pre-set
// so that auth.GetClaims can extract them.
func createEchoContextWithClaims(method, path string, claims *auth.Claims) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	c.Set("user", token)
	return c, rec
}

// createEchoContextNoClaims builds an Echo context without any JWT token set.
func createEchoContextNoClaims(method, path string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	return c, rec
}

// writeChallenge creates a challenge.yml inside baseDir/subdir/.
func writeChallenge(t *testing.T, baseDir, subdir, content string) {
	t.Helper()
	dir := filepath.Join(baseDir, subdir)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "challenge.yml"), []byte(content), 0o644))
}

// newTestServer creates a Server with an in-memory DB and a real ChallengeIndex
// built from the given challengeDir. It uses a StaticProvider for config injection.
func newTestServer(t *testing.T, challengeDir string) (*Server, *config.Config) {
	t.Helper()

	cfg := &config.Config{
		Auth: config.AuthConfig{
			JWTSecret: "defaultsecret",
		},
		Instancer: config.InstancerConfig{
			AnsibleDir:   "/opt/ansible",
			ChallengeDir: challengeDir,
		},
	}

	db, err := InitDB(":memory:")
	require.NoError(t, err)

	challIdx, err := challenge.NewChallengeIndex(challengeDir)
	require.NoError(t, err)

	srv := NewServerWithOpts(ServerOpts{
		DB:               db,
		ChallengeIndexer: challIdx,
		ConfigProvider:   &config.StaticProvider{Cfg: cfg},
	})

	return srv, cfg
}

func TestReloadChallenges_Success(t *testing.T) {
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

	srv, _ := newTestServer(t, dir)

	claims := &auth.Claims{Role: "admin"}
	ctx, rec := createEchoContextWithClaims(http.MethodPost, "/admin/reload-challenges", claims)

	err := srv.ReloadChallenges(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestReloadChallenges_Unauthorized(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "web", `
name: http
category: web
playbook_name: http
type: zync
deploy_parameters:
  unique: false
`)

	srv, _ := newTestServer(t, dir)

	// No JWT token set in context.
	ctx, rec := createEchoContextNoClaims(http.MethodPost, "/admin/reload-challenges")

	err := srv.ReloadChallenges(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestReloadChallenges_Forbidden(t *testing.T) {
	dir := t.TempDir()
	writeChallenge(t, dir, "web", `
name: http
category: web
playbook_name: http
type: zync
deploy_parameters:
  unique: false
`)

	srv, _ := newTestServer(t, dir)

	claims := &auth.Claims{Role: "user", TeamID: "team1"}
	ctx, rec := createEchoContextWithClaims(http.MethodPost, "/admin/reload-challenges", claims)

	err := srv.ReloadChallenges(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestReloadChallenges_InvalidDir(t *testing.T) {
	// Start with a valid dir so newTestServer can initialise the index.
	dir := t.TempDir()
	writeChallenge(t, dir, "web", `
name: http
category: web
playbook_name: http
type: zync
deploy_parameters:
  unique: false
`)

	srv, cfg := newTestServer(t, dir)

	// Point config to a non-existent directory before calling ReloadChallenges.
	cfg.Instancer.ChallengeDir = "/nonexistent/path/that/does/not/exist"

	claims := &auth.Claims{Role: "admin"}
	ctx, _ := createEchoContextWithClaims(http.MethodPost, "/admin/reload-challenges", claims)

	// BuildIndex panics on a non-existent root dir because WalkDir passes
	// a nil DirEntry when it can't stat the root path.
	assert.Panics(t, func() {
		_ = srv.ReloadChallenges(ctx)
	})
}

func TestReloadChallenges_RefreshesIndex(t *testing.T) {
	dir := t.TempDir()

	// Start with one challenge.
	writeChallenge(t, dir, "web", `
name: http
category: web
playbook_name: http
type: zync
deploy_parameters:
  unique: false
  image: nginx:latest
`)

	srv, _ := newTestServer(t, dir)

	// Verify initial state: only "web/http" exists.
	chall, err := srv.challIdx.Get("web", "http")
	require.NoError(t, err)
	assert.Equal(t, "http", chall.Name)

	_, err = srv.challIdx.Get("pwn", "bof")
	assert.Error(t, err, "pwn/bof should not exist yet")

	// Add a second challenge to the directory.
	writeChallenge(t, dir, "pwn", `
name: bof
category: pwn
playbook_name: tcp
type: zync
deploy_parameters:
  unique: false
  image: bof:latest
`)

	// Reload via the handler.
	claims := &auth.Claims{Role: "admin"}
	ctx, rec := createEchoContextWithClaims(http.MethodPost, "/admin/reload-challenges", claims)

	err = srv.ReloadChallenges(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Now both challenges should be present.
	chall, err = srv.challIdx.Get("web", "http")
	require.NoError(t, err)
	assert.Equal(t, "http", chall.Name)

	chall2, err := srv.challIdx.Get("pwn", "bof")
	require.NoError(t, err)
	assert.Equal(t, "bof", chall2.Name)
	assert.Equal(t, "pwn", chall2.Category)
}
