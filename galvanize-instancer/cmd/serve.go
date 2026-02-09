package cmd

import (
	"context"
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
	"github.com/28Pollux28/galvanize/pkg/scheduler"
	"github.com/28Pollux28/galvanize/pkg/utils"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo-contrib/echoprometheus"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
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

		challIdx, err := challenge.NewChallengeIndex(cfg.Instancer.ChallengeDir)
		if err != nil {
			zap.S().Fatalf("Failed to initialize challenge index: %v", err)
		}

		expirySched := scheduler.NewExpiryScheduler(db, challIdx, &ansible.AnsibleDeployer{}, confProv, zap.S().Named("ExpiryScheduler"))

		// 6. Server Init via DI
		srv := server.NewServerWithOpts(server.ServerOpts{
			DB:               db,
			ChallengeIndexer: challIdx,
			ConfigProvider:   confProv,
			Deployer:         &ansible.AnsibleDeployer{},
			ExpiryScheduler:  expirySched,
		})
		api.RegisterHandlers(e, srv)

		// 7. Start background services
		schedCtx, schedCancel := context.WithCancel(context.Background())
		srv.StartScheduler(schedCtx, expirySched)

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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		if err := e.Shutdown(shutdownCtx); err != nil {
			zap.S().Fatalf("Failed to shutdown server: %v", err)
		}
		if err := srv.Wait(shutdownCtx); err != nil {
			zap.S().Fatalf("Failed to wait for server shutdown: %v", err)
		}
	},
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
