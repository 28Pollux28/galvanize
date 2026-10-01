package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func newPanickingEcho(withMiddleware bool) *echo.Echo {
	e := echo.New()
	if withMiddleware {
		useMiddleware(e)
	}
	e.GET("/panic", func(echo.Context) error { panic("boom") })
	e.GET("/ok", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
	return e
}

func serve(e *echo.Echo, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestUseMiddleware_RecoversPanics(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	t.Cleanup(zap.ReplaceGlobals(zap.New(core)))
	e := newPanickingEcho(true)

	rec := serve(e, "/panic")
	assert.Equal(t, http.StatusInternalServerError, rec.Code)

	panics := logs.FilterMessageSnippet("Panic serving GET /panic: boom").All()
	assert.Len(t, panics, 1, "the panic is logged with its stack trace")
	assert.Len(t, logs.FilterMessageSnippet("| GET | /panic | 500").All(), 1, "and by the request logger")

	assert.Equal(t, http.StatusOK, serve(e, "/ok").Code, "the server keeps serving")
}

// Without the middleware, the panic leaves the handler: net/http then drops
// the connection without a response
func TestWithoutMiddleware_PanicEscapes(t *testing.T) {
	e := newPanickingEcho(false)
	assert.Panics(t, func() { serve(e, "/panic") })
}
