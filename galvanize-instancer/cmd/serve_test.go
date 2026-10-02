package cmd

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/28Pollux28/galvanize/pkg/scheduler"
	"github.com/28Pollux28/galvanize/pkg/worker"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func newQueuelessTestScheduler() *scheduler.ExpiryScheduler {
	return scheduler.NewExpiryScheduler(nil, nil, zap.NewNop().Sugar())
}

// Without Redis, team requests and expiries share one LimitedDeployer with
// max_concurrent_ansible slots, and the scheduler terminates expiries itself
func TestConfigureQueueless_WithoutRedis(t *testing.T) {
	for _, tc := range []struct{ configured, want int }{{0, 5}, {2, 2}} {
		cfg := &config.Config{Instancer: config.InstancerConfig{MaxConcurrentAnsible: tc.configured}}
		sched := newQueuelessTestScheduler()

		got := configureQueueless(cfg, nil, sched, nil, &config.StaticProvider{Cfg: cfg})

		limited, ok := got.(*ansible.LimitedDeployer)
		require.True(t, ok, "a LimitedDeployer, got %T", got)
		assert.Equal(t, tc.want, limited.Limit())
		assert.Same(t, limited, sched.DirectTerminationDeployer(), "the scheduler terminates with the same slots")
	}
}

// With Redis, the job queue and its workers handle both: nothing changes
func TestConfigureQueueless_WithRedis(t *testing.T) {
	sched := newQueuelessTestScheduler()
	got := configureQueueless(&config.Config{}, &worker.Queue{}, sched, nil, nil)
	assert.Nil(t, got, "the server keeps its default deployer")
	assert.Nil(t, sched.DirectTerminationDeployer(), "expiries go to the queue")
}
