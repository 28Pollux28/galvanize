package cmd

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/28Pollux28/galvanize/internal/ansible"
	"github.com/28Pollux28/galvanize/internal/auth"
	"github.com/28Pollux28/galvanize/internal/challenge"
	server "github.com/28Pollux28/galvanize/pkg"
	"github.com/28Pollux28/galvanize/pkg/api"
	"github.com/28Pollux28/galvanize/pkg/config"
	pkgmetrics "github.com/28Pollux28/galvanize/pkg/metrics"
	"github.com/28Pollux28/galvanize/pkg/scheduler"
	"github.com/28Pollux28/galvanize/pkg/utils"
	"github.com/28Pollux28/galvanize/pkg/worker"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo-contrib/echoprometheus"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the Poly Instancer server",
	Long:  "Starts the Poly Instancer server to handle requests from CTFd for deploying challenges.",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		portStr, _ := cmd.Flags().GetString("port")
		if !validatePort(portStr) {
			fmt.Fprintf(os.Stderr, "Invalid port: %s\n", portStr)
			os.Exit(1)
		}

		// 1. Config provider (DI: all downstream consumers receive this)
		confProv := config.GlobalProvider{}
		cfg := confProv.GetConfig()

		e := echo.New()
		e.HideBanner = true
		e.HidePort = true

		// 2. Middleware
		skipper := func(c echo.Context) bool {
			// Skip health check endpoint
			return c.Request().URL.Path == "/health"
		}
		e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
			LogStatus:   true,
			LogMethod:   true,
			LogRemoteIP: true,
			LogURI:      true,
			Skipper:     skipper,
			LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
				zap.S().Infof("| %v | %v | %v | %v", v.RemoteIP, v.Method, v.URI, v.Status)
				return nil
			},
		}))
		//e.Use(middleware.Recover())
		e.Use(middleware.CORS())

		// 3. Prometheus
		e.Use(echoprometheus.NewMiddleware("instancer")) // register middleware to gather metrics from requests
		e.GET("/metrics", echoprometheus.NewHandler())

		// JWT secret from env (for security);
		jwtSecret := os.Getenv("JWT_SECRET")
		if jwtSecret == "" {
			jwtSecret = cfg.Auth.JWTSecret
		}
		if jwtSecret == "" {
			zap.S().Fatal("JWT_SECRET (or JWT_SECRET) is required")
		}
		cfg.Auth.JWTSecret = jwtSecret
		zap.S().Debugf("Using JWT secret: %s", cfg.Auth.JWTSecret)

		ansiblePath := os.Getenv("ANSIBLE_PATH")
		if ansiblePath == "" {
			ansiblePath = cfg.Instancer.AnsibleDir
		}
		if ansiblePath == "" {
			zap.S().Fatal("ANSIBLE_PATH is required")
		}
		// Ensure the resolved value is propagated into the config so that
		// PreparePlaybook (which reads conf.Instancer.AnsibleDir) uses it.
		cfg.Instancer.AnsibleDir = ansiblePath

		// 4. Auth
		jwtConfig := echojwt.Config{
			NewClaimsFunc: func(c echo.Context) jwt.Claims {
				return new(auth.Claims)
			},
			SigningKey: []byte(jwtSecret),
			Skipper: func(c echo.Context) bool {
				return c.Path() == "/health" || c.Path() == "/metrics"
			},
		}
		e.Use(echojwt.WithConfig(jwtConfig))

		if err := utils.RegisterSSHHosts(cfg); err != nil {
			zap.S().Fatalf("Failed to register SSH hosts: %v", err)
		}

		// 5. Build dependencies
		db, err := server.InitDB(cfg.Instancer.DBPath)
		if err != nil {
			zap.S().Fatalf("Failed to initialize database: %v", err)
		}

		// Register the DB-backed deployment collector so Prometheus can report
		// current deployment counts by status/challenge/team on each scrape.
		prometheus.MustRegister(pkgmetrics.NewDeploymentCollector(db))

		// Start dedicated metrics server on port 5001.
		// This exposes all registered metrics (echoprometheus HTTP metrics +
		// instancer_deploy/terminate_duration_seconds, instancer_deployments, etc.)
		// on a separate port so scrape traffic does not mix with API traffic.
		// Note: 404 responses on /status, /extend, and /terminate are expected
		// (deployment not yet created) and should not trigger error alerts.
		metricsSrv := &http.Server{
			Addr:    ":5001",
			Handler: metricsHandler(cfg.Instancer.Metrics.Username, cfg.Instancer.Metrics.Password, promhttp.Handler()),
		}
		go func() {
			zap.S().Info("Starting metrics server on :5001")
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				zap.S().Errorf("Metrics server error: %v", err)
			}
		}()

		challIdx, err := challenge.NewChallengeIndex(cfg.Instancer.ChallengeDir)
		if err != nil {
			zap.S().Fatalf("Failed to initialize challenge index: %v", err)
		}
		// Seed the challenge index gauge with the initial count.
		{
			challs := challIdx.GetAll()
			counts := make(map[string]int, 8)
			for _, ch := range challs {
				counts[ch.Category]++
			}
			pkgmetrics.SetChallengesIndexed(counts)
		}

		// 6. Initialize Redis job queue and worker pool (if configured)
		var jobQueue *worker.Queue
		var workerPool *worker.Pool

		if cfg.Instancer.Redis.Addr != "" {
			var queueErr error
			jobQueue, queueErr = worker.NewQueue(worker.QueueConfig{
				Addr:     cfg.Instancer.Redis.Addr,
				Password: cfg.Instancer.Redis.Password,
				DB:       cfg.Instancer.Redis.DB,
			}, zap.S().Named("JobQueue"))
			if queueErr != nil {
				zap.S().Fatalf("Failed to connect to Redis: %v", queueErr)
			}

			// Register queue-depth collector now that we have a live queue.
			prometheus.MustRegister(pkgmetrics.NewQueueCollector(jobQueue))

			numWorkers := cfg.Instancer.NumWorkers
			if numWorkers <= 0 {
				numWorkers = 10
			}

			workerPool = worker.NewPool(worker.PoolConfig{
				NumWorkers: numWorkers,
				Queue:      jobQueue,
				DB:         db,
				ChallIdx:   challIdx,
				ConfProv:   confProv,
				Deployer:   &ansible.AnsibleDeployer{},
				Logger:     zap.S().Named("WorkerPool"),
			})
			zap.S().Infof("Redis job queue enabled with %d workers", numWorkers)
		} else {
			zap.S().Info("Redis not configured, using direct goroutines for deployments")
		}

		// Initialize scheduler with job queue (can be nil if Redis not configured)
		expirySched := scheduler.NewExpiryScheduler(db, jobQueue, zap.S().Named("ExpiryScheduler"))

		// 7. Server Init via DI
		srv := server.NewServerWithOpts(server.ServerOpts{
			DB:               db,
			ChallengeIndexer: challIdx,
			ConfigProvider:   confProv,
			Deployer:         &ansible.AnsibleDeployer{},
			ExpiryScheduler:  expirySched,
			JobQueue:         jobQueue,
		})
		api.RegisterHandlers(e, srv)

		// 8. Start background services
		schedCtx, schedCancel := context.WithCancel(context.Background())
		srv.StartScheduler(schedCtx, expirySched)

		// Start worker pool if configured
		if workerPool != nil {
			workerPool.Start(schedCtx)
		}

		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			zap.S().Infof("Starting server on port %s", portStr)
			if err := e.Start(":" + portStr); err != nil && !errors.Is(err, http.ErrServerClosed) {
				zap.S().Fatalf("shutting down the server: %v", err)
			}
		}()
		// Wait for interrupt signal to gracefully shut down the server
		<-ctx.Done()
		zap.S().Info("Shutting down server...")
		schedCancel()

		// Stop worker pool first (it will finish current jobs)
		if workerPool != nil {
			workerPool.Stop()
		}

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		if err := e.Shutdown(shutdownCtx); err != nil {
			zap.S().Fatalf("Failed to shutdown server: %v", err)
		}
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			zap.S().Warnf("Failed to shutdown metrics server: %v", err)
		}
		if err := srv.Wait(shutdownCtx); err != nil {
			zap.S().Fatalf("Failed to wait for server shutdown: %v", err)
		}

		// Close Redis connection
		if jobQueue != nil {
			if err := jobQueue.Close(); err != nil {
				zap.S().Warnf("Failed to close Redis connection: %v", err)
			}
		}
	},
}

// metricsHandler wraps h with HTTP Basic Auth if a password is configured.
// If password is empty the handler is returned as-is (no auth).
func metricsHandler(username, password string, h http.Handler) http.Handler {
	if password == "" {
		return h
	}
	if username == "" {
		username = "prometheus"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok ||
			subtle.ConstantTimeCompare([]byte(u), []byte(username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func validatePort(port string) bool {
	if port == "" {
		return false
	}
	portInt, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	if portInt < 1 || portInt > 65535 {
		return false
	}
	return true
}

func init() {
	serveCmd.Flags().StringP("port", "p", "8080", "Port to listen on")
	rootCmd.AddCommand(serveCmd)
}
