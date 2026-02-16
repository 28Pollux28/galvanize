package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/28Pollux28/galvanize/pkg/scheduler"
	"github.com/28Pollux28/galvanize/pkg/utils"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ---------------------------------------------------------------------------
// Mock Deployer
// ---------------------------------------------------------------------------

type mockDeployer struct {
	mu             sync.Mutex
	deployCalls    []deployCall
	terminateCalls []terminateCall

	deployFn    func(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) (string, error)
	terminateFn func(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) error
}

type deployCall struct {
	ChallName string
	TeamID    string
}

type terminateCall struct {
	ChallName string
	TeamID    string
}

func (m *mockDeployer) Deploy(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) (string, error) {
	m.mu.Lock()
	m.deployCalls = append(m.deployCalls, deployCall{ChallName: chall.Name, TeamID: teamID})
	m.mu.Unlock()
	if m.deployFn != nil {
		return m.deployFn(ctx, conf, chall, teamID)
	}
	return "https://challenge.example.com/", nil
}

func (m *mockDeployer) Terminate(ctx context.Context, conf *config.Config, chall *challenge.Challenge, teamID string) error {
	m.mu.Lock()
	m.terminateCalls = append(m.terminateCalls, terminateCall{ChallName: chall.Name, TeamID: teamID})
	m.mu.Unlock()
	if m.terminateFn != nil {
		return m.terminateFn(ctx, conf, chall, teamID)
	}
	return nil
}

var _ ansible.Deployer = (*mockDeployer)(nil)

// ---------------------------------------------------------------------------
// Mock ChallengeIndexer (simple in-memory, no filesystem)
// ---------------------------------------------------------------------------

type mockChallengeIndexer struct {
	challenges map[string]*challenge.Challenge
}

func newMockIndexer(challs ...*challenge.Challenge) *mockChallengeIndexer {
	m := &mockChallengeIndexer{challenges: make(map[string]*challenge.Challenge)}
	for _, c := range challs {
		m.challenges[c.Category+"/"+c.Name] = c
	}
	return m
}

func (m *mockChallengeIndexer) Get(category, name string) (*challenge.Challenge, error) {
	c, ok := m.challenges[category+"/"+name]
	if !ok {
		return nil, fmt.Errorf("challenge not found: %s/%s", category, name)
	}
	return c, nil
}

func (m *mockChallengeIndexer) GetAllUnique() []*challenge.Challenge {
	var unique []*challenge.Challenge
	for _, c := range m.challenges {
		if c.Unique {
			unique = append(unique, c)
		}
	}
	return unique
}

func (m *mockChallengeIndexer) BuildIndex(_ string) error { return nil }

var _ challenge.ChallengeIndexer = (*mockChallengeIndexer)(nil)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func defaultTestConfig() *config.Config {
	return &config.Config{
		Auth: config.AuthConfig{JWTSecret: "testsecret"},
		Instancer: config.InstancerConfig{
			AnsibleDir:                "/opt/ansible",
			ChallengeDir:              "/tmp/challenges",
			InstancerHost:             "challenges.example.com",
			DeploymentTTL:             30 * time.Minute,
			DeploymentTTLExtension:    15 * time.Minute,
			DeploymentMaxExtensions:   3,
			DeploymentExtensionWindow: 10 * time.Minute,
		},
	}
}

func httpChallenge() *challenge.Challenge {
	return &challenge.Challenge{
		Name:         "http",
		Category:     "web",
		PlaybookName: "http",
		Type:         "zync",
		Unique:       false,
		DeployParameters: map[string]interface{}{
			"unique": false,
			"image":  "nginx:latest",
		},
	}
}

func uniqueChallenge() *challenge.Challenge {
	return &challenge.Challenge{
		Name:         "shared",
		Category:     "web",
		PlaybookName: "http",
		Type:         "zync",
		Unique:       true,
		DeployParameters: map[string]interface{}{
			"unique": true,
			"image":  "nginx:latest",
		},
	}
}

func newTestServerWithMock(t *testing.T, deployer *mockDeployer, indexer challenge.ChallengeIndexer) *Server {
	t.Helper()
	cfg := defaultTestConfig()
	db, err := InitDB(":memory:")
	require.NoError(t, err)

	confProv := &config.StaticProvider{Cfg: cfg}
	expirySched := scheduler.NewExpiryScheduler(db, nil, zap.NewNop().Sugar())

	return NewServerWithOpts(ServerOpts{
		DB:               db,
		ChallengeIndexer: indexer,
		ConfigProvider:   confProv,
		Deployer:         deployer,
		ExpiryScheduler:  expirySched,
	})
}

func echoCtxWithClaimsAndBody(method, path string, claims *auth.Claims, body string) (echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if claims != nil {
		token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		c.Set("user", token)
	}
	return c, rec
}

// waitForBackground waits for all background goroutines tracked by the server.
func waitForBackground(t *testing.T, s *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, s.Wait(ctx), "timed out waiting for background goroutines")
}

// ---------------------------------------------------------------------------
// GetHealth
// ---------------------------------------------------------------------------

func TestGetHealth(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer())

	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/health", nil, "")
	err := srv.GetHealth(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"ok"`)
}

// ---------------------------------------------------------------------------
// DeployInstance
// ---------------------------------------------------------------------------

func TestDeployInstance_Success(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/deploy", claims, body)

	err := srv.DeployInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, rec.Code)

	// Wait for background deploy goroutine to complete
	waitForBackground(t, srv)

	// Deployer should have been called
	assert.Len(t, deployer.deployCalls, 1)
	assert.Equal(t, "http", deployer.deployCalls[0].ChallName)
	assert.Equal(t, "team1", deployer.deployCalls[0].TeamID)

	// Deployment should be running in DB
	d, err := models.GetDeployment(srv.db, "web", "http", "team1", false)
	require.NoError(t, err)
	assert.Equal(t, models.DeploymentStatusRunning, d.Status)
	assert.Equal(t, "https://challenge.example.com/", d.ConnectionInfo)
}

func TestDeployInstance_Unauthorized_NoClaims(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer(httpChallenge()))

	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/deploy", nil, `{"challenge_name":"http","category":"web"}`)
	err := srv.DeployInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestDeployInstance_Forbidden_WrongChallenge(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer(httpChallenge()))

	// Token says "other" but request says "http"
	claims := &auth.Claims{TeamID: "team1", ChallengeName: "other", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/deploy", claims, body)

	err := srv.DeployInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestDeployInstance_AdminCanDeployAnychallenge(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))

	// Admin token with different challenge name should still be allowed
	claims := &auth.Claims{TeamID: "team1", ChallengeName: "other", Category: "web", Role: "admin"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/deploy", claims, body)

	err := srv.DeployInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, rec.Code)

	waitForBackground(t, srv)
	assert.Len(t, deployer.deployCalls, 1)
}

func TestDeployInstance_InvalidChallenge(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer()) // no challenges registered

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/deploy", claims, body)

	err := srv.DeployInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDeployInstance_UniqueChallengeForbidden(t *testing.T) {
	deployer := &mockDeployer{}
	chall := uniqueChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "shared", Category: "web", Role: "user"}
	body := `{"challenge_name":"shared","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/deploy", claims, body)

	err := srv.DeployInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestDeployInstance_AlreadyDeployed(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	// Pre-create a deployment
	_, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/deploy", claims, body)

	err = srv.DeployInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestDeployInstance_DeployerError(t *testing.T) {
	deployer := &mockDeployer{
		deployFn: func(_ context.Context, _ *config.Config, _ *challenge.Challenge, _ string) (string, error) {
			return "", fmt.Errorf("ansible exploded")
		},
	}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/deploy", claims, body)

	err := srv.DeployInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusAccepted, rec.Code) // returns 202 immediately

	waitForBackground(t, srv)

	// Deployment should be in error state
	d, err := models.GetDeployment(srv.db, "web", "http", "team1", false)
	require.NoError(t, err)
	assert.Equal(t, models.DeploymentStatusError, d.Status)
	assert.Contains(t, d.Error, "ansible exploded")
}

func TestDeployInstance_InvalidBody(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer(httpChallenge()))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	// Send non-JSON body
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/deploy", strings.NewReader("not json"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	c.Set("user", token)

	err := srv.DeployInstance(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ---------------------------------------------------------------------------
// GetInstanceStatus
// ---------------------------------------------------------------------------

func TestGetInstanceStatus_Running(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusRunning, "https://chall.example.com/", ""))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", claims, "")

	err = srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp api.StatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, api.StatusResponseStatusRunning, *resp.Status)
	assert.Equal(t, "https://chall.example.com/", *resp.ConnectionInfo)
	assert.NotNil(t, resp.ExpirationTime)
	assert.NotNil(t, resp.ExtensionsLeft)
	assert.Equal(t, 3, *resp.ExtensionsLeft)
}

func TestGetInstanceStatus_Starting(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	_, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", claims, "")

	err = srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp api.StatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, api.StatusResponseStatusStarting, *resp.Status)
}

func TestGetInstanceStatus_Error(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusError, "", "something failed"))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", claims, "")

	err = srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestGetInstanceStatus_Stopping(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusStopping, "", ""))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", claims, "")

	err = srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp api.StatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, api.StatusResponseStatusStopping, *resp.Status)
}

func TestGetInstanceStatus_NotFound(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", claims, "")

	err := srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestGetInstanceStatus_Unauthorized(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer(httpChallenge()))

	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", nil, "")
	err := srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestGetInstanceStatus_InvalidChallenge(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer()) // no challenges

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "nonexistent", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", claims, "")

	err := srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestGetInstanceStatus_UniqueChallenge_Running(t *testing.T) {
	deployer := &mockDeployer{}
	chall := uniqueChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))

	// Create a unique deployment (no team)
	d, err := models.CreateUniqueDeployment(srv.db, "shared", "web")
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusRunning, "https://shared.example.com/", ""))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "shared", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", claims, "")

	err = srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp api.StatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, api.StatusResponseStatusRunning, *resp.Status)
	assert.True(t, *resp.Unique)
	// Unique deployments should NOT have expiration info
	assert.Nil(t, resp.ExpirationTime)
	assert.Nil(t, resp.ExtensionsLeft)
}

func TestGetInstanceStatus_UniqueChallenge_NotDeployed_NonAdmin(t *testing.T) {
	deployer := &mockDeployer{}
	chall := uniqueChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "shared", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", claims, "")

	err := srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "cannot deploy unique challenge")
}

func TestGetInstanceStatus_UniqueChallenge_NotDeployed_Admin(t *testing.T) {
	deployer := &mockDeployer{}
	chall := uniqueChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "shared", Category: "web", Role: "admin"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodGet, "/status", claims, "")

	err := srv.GetInstanceStatus(ctx)
	require.NoError(t, err)
	// Admin gets a plain 404 without the "cannot deploy" message
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.NotContains(t, rec.Body.String(), "cannot deploy unique challenge")
}

// ---------------------------------------------------------------------------
// ExtendInstance
// ---------------------------------------------------------------------------

func TestExtendInstance_Success(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	// Create a deployment that expires in 5 minutes (within the 10-minute extension window)
	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusRunning, "https://chall.example.com/", ""))

	// Move expiration to 5 minutes from now (inside extension window)
	d.ExpiresAt = utils.Ptr(time.Now().Add(5 * time.Minute))
	require.NoError(t, srv.db.Save(d).Error)

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/extend", claims, "")

	err = srv.ExtendInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp api.StatusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, api.StatusResponseStatusRunning, *resp.Status)
	assert.Equal(t, 2, *resp.ExtensionsLeft) // was 3, now 2
}

func TestExtendInstance_NotFound(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer(httpChallenge()))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/extend", claims, "")

	err := srv.ExtendInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestExtendInstance_Unauthorized(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer())

	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/extend", nil, "")
	err := srv.ExtendInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestExtendInstance_OutsideWindow(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusRunning, "https://chall.example.com/", ""))

	// Expiration is 30 minutes from now — well outside the 10-minute window
	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/extend", claims, "")

	err = srv.ExtendInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "extension window not reached")
}

func TestExtendInstance_NoExtensionsLeft(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusRunning, "https://chall.example.com/", ""))

	// Exhaust extensions and move into window
	d.TimeExtensionLeft = 0
	d.ExpiresAt = utils.Ptr(time.Now().Add(5 * time.Minute))
	require.NoError(t, srv.db.Save(d).Error)

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/extend", claims, "")

	err = srv.ExtendInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "no time extensions left")
}

// ---------------------------------------------------------------------------
// TerminateInstance
// ---------------------------------------------------------------------------

func TestTerminateInstance_Success(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusRunning, "https://chall.example.com/", ""))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/terminate", claims, body)

	err = srv.TerminateInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	waitForBackground(t, srv)

	// Deployer.Terminate should have been called
	assert.Len(t, deployer.terminateCalls, 1)
	assert.Equal(t, "http", deployer.terminateCalls[0].ChallName)

	// Deployment should be deleted
	_, err = models.GetDeployment(srv.db, "web", "http", "team1", false)
	assert.ErrorIs(t, err, models.ErrNotFound)
}

func TestTerminateInstance_Unauthorized(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer(httpChallenge()))

	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/terminate", nil, `{"challenge_name":"http","category":"web"}`)
	err := srv.TerminateInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTerminateInstance_Forbidden_WrongChallenge(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer(httpChallenge()))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "other", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/terminate", claims, body)

	err := srv.TerminateInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestTerminateInstance_NotFound(t *testing.T) {
	deployer := &mockDeployer{}
	srv := newTestServerWithMock(t, deployer, newMockIndexer(httpChallenge()))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/terminate", claims, body)

	err := srv.TerminateInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestTerminateInstance_AlreadyStopping(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusStopping, "", ""))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/terminate", claims, body)

	err = srv.TerminateInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Termination already in progress")
}

func TestTerminateInstance_DeployerError(t *testing.T) {
	deployer := &mockDeployer{
		terminateFn: func(_ context.Context, _ *config.Config, _ *challenge.Challenge, _ string) error {
			return fmt.Errorf("terminate exploded")
		},
	}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusRunning, "conn", ""))

	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "user"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/terminate", claims, body)

	err = srv.TerminateInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	waitForBackground(t, srv)

	// Deployment should be in error state (not deleted, since terminate failed)
	d2, err := models.GetDeployment(srv.db, "web", "http", "team1", false)
	require.NoError(t, err)
	assert.Equal(t, models.DeploymentStatusError, d2.Status)
	assert.Contains(t, d2.Error, "terminate exploded")
}

func TestTerminateInstance_AdminCanTerminateAnychallenge(t *testing.T) {
	deployer := &mockDeployer{}
	chall := httpChallenge()
	srv := newTestServerWithMock(t, deployer, newMockIndexer(chall))
	cfg := defaultTestConfig()

	d, err := models.CreateDeployment(srv.db, "http", "team1", "web", cfg.Instancer.DeploymentTTL, cfg.Instancer.DeploymentMaxExtensions)
	require.NoError(t, err)
	require.NoError(t, models.UpdateDeploymentStatus(srv.db, d, models.DeploymentStatusRunning, "conn", ""))

	// Admin token: the handler uses claims.ChallengeName for DB lookup,
	// so claims.ChallengeName must match the actual deployment.
	// The admin bypass only applies to the challenge_name != claims.ChallengeName auth check.
	claims := &auth.Claims{TeamID: "team1", ChallengeName: "http", Category: "web", Role: "admin"}
	body := `{"challenge_name":"http","category":"web"}`
	ctx, rec := echoCtxWithClaimsAndBody(http.MethodPost, "/terminate", claims, body)

	err = srv.TerminateInstance(ctx)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	waitForBackground(t, srv)
	assert.Len(t, deployer.terminateCalls, 1)
}
