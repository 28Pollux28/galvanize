package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/28Pollux28/galvanize/internal/auth"
	server "github.com/28Pollux28/galvanize/pkg"
	"github.com/28Pollux28/galvanize/pkg/api"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "test-jwt-secret"

// newAPI serves Galvanize's API behind the same JWT middleware as serve
func newAPI(version string) *echo.Echo {
	e := echo.New()
	e.Use(jwtMiddleware(testSecret))
	api.RegisterHandlers(e, server.NewServerWithOpts(server.ServerOpts{Version: version}))
	return e
}

func signed(t *testing.T, method jwt.SigningMethod, key interface{}, role string, expires time.Time) string {
	t.Helper()
	claims := &auth.Claims{Role: role, TeamID: "team1", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(expires)}}
	token, err := jwt.NewWithClaims(method, claims).SignedString(key)
	require.NoError(t, err)
	return token
}

func get(e *echo.Echo, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestVersion_OnlyForAdmins(t *testing.T) {
	e := newAPI("9.8.7")
	hour := time.Now().Add(time.Hour)

	rec := get(e, "/admin/version", signed(t, jwt.SigningMethodHS256, []byte(testSecret), "admin", hour))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"version":"9.8.7"}`, rec.Body.String())

	for name, tc := range map[string]struct {
		token string
		code  int
	}{
		"no token":        {"", http.StatusUnauthorized},
		"wrong key":       {signed(t, jwt.SigningMethodHS256, []byte("another-secret"), "admin", hour), http.StatusUnauthorized},
		"unsigned (none)": {signed(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, "admin", hour), http.StatusUnauthorized},
		"expired admin":   {signed(t, jwt.SigningMethodHS256, []byte(testSecret), "admin", time.Now().Add(-time.Minute)), http.StatusUnauthorized},
		"player":          {signed(t, jwt.SigningMethodHS256, []byte(testSecret), "player", hour), http.StatusForbidden},
		"no role":         {signed(t, jwt.SigningMethodHS256, []byte(testSecret), "", hour), http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(e, "/admin/version", tc.token)
			assert.Equal(t, tc.code, rec.Code)
			assert.NotContains(t, rec.Body.String(), "9.8.7", "the version is not disclosed")
		})
	}
}

func TestVersion_NotInHealth(t *testing.T) {
	rec := get(newAPI("9.8.7"), "/health", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "9.8.7")
}

func TestVersion_UnknownWhenUnset(t *testing.T) {
	rec := get(newAPI(""), "/admin/version", signed(t, jwt.SigningMethodHS256, []byte(testSecret), "admin", time.Now().Add(time.Hour)))
	assert.JSONEq(t, `{"version":"unknown"}`, rec.Body.String())
}
