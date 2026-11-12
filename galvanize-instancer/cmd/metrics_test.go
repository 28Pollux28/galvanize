package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
)

// The API port no longer serves /metrics, which skipped authentication:
// metrics are only on the metrics server (:5001)
func TestAPIPort_NoMetrics(t *testing.T) {
	e := newAPI("9.8.7")
	assert.Equal(t, http.StatusUnauthorized, get(e, "/metrics", "").Code, "requires a token like any route")

	admin := signed(t, jwt.SigningMethodHS256, []byte(testSecret), "admin", time.Now().Add(time.Hour))
	assert.Equal(t, http.StatusNotFound, get(e, "/metrics", admin).Code, "and is not served")
}

func scrape(h http.Handler, user, pass string) int {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// The metrics server's authentication is unchanged: none without a
// password, basic auth with one (username defaults to prometheus)
func TestMetricsHandler(t *testing.T) {
	metrics := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	assert.Equal(t, http.StatusOK, scrape(metricsHandler("", "", metrics), "", ""), "open without a password")

	h := metricsHandler("", "s3cret", metrics)
	assert.Equal(t, http.StatusUnauthorized, scrape(h, "", ""))
	assert.Equal(t, http.StatusUnauthorized, scrape(h, "prometheus", "wrong"))
	assert.Equal(t, http.StatusUnauthorized, scrape(h, "admin", "s3cret"))
	assert.Equal(t, http.StatusOK, scrape(h, "prometheus", "s3cret"))

	assert.Equal(t, http.StatusOK, scrape(metricsHandler("scraper", "s3cret", metrics), "scraper", "s3cret"))
}
