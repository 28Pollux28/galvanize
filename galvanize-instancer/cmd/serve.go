package cmd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/28Pollux28/galvanize/internal/auth"
	server "github.com/28Pollux28/galvanize/pkg"
	"github.com/28Pollux28/galvanize/pkg/api"
	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo-contrib/echoprometheus"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

var serveCmd = &cobra.Command{
	Use:   "serve [port]",
	Short: "Start the Poly Instancer server",
	Long:  "Starts the Poly Instancer server to handle requests from CTFd for deploying challenges.",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		portStr := args[0]
		if !validatePort(portStr) {
			fmt.Fprintf(os.Stderr, "Invalid port: %s\n", portStr)
			os.Exit(1)
		}

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
		cfg := config.Get()

		// JWT secret strictly from env (for security); allow POLY_JWT_SECRET env var
		jwtSecret := os.Getenv("JWT_SECRET")
		if jwtSecret == "" {
			jwtSecret = cfg.Auth.JWTSecret
		}
		if jwtSecret == "" {
			zap.S().Fatal("JWT_SECRET (or JWT_SECRET) is required")
		}
		zap.S().Debugf("Using JWT secret: %s", jwtSecret)

		ansiblePath := os.Getenv("ANSIBLE_PATH")
		if ansiblePath == "" {
			ansiblePath = cfg.Instancer.AnsibleDir
		}
		if ansiblePath == "" {
			log.Fatal("ANSIBLE_PATH is required")
		}

		// 5. Auth
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
		err := registerSSHHosts()
		if err != nil {
			zap.S().Fatalf("Failed to register SSH hosts: %v", err)
		}

		// 6. Server Init (Includes Metric Rehydration)
		srv := server.NewServer(cfg.Instancer.DBPath)
		api.RegisterHandlers(e, srv)

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
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		if err := e.Shutdown(ctx); err != nil {
			zap.S().Fatalf("Failed to shutdown server: %v", err)
		}
		err = srv.Wait(ctx)
		if err != nil {
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
	//rootCmd.AddCommand(serveCmd)
}

func registerSSHHosts() error {
	// Parse SSH hosts from config and register them using ssh package
	cfg := config.Get()
	hosts := strings.Split(strings.Trim(cfg.Instancer.Ansible.Inventory, ","), ",")
	home, _ := os.UserHomeDir()
	knownHostsPath := filepath.Join(home, ".ssh/known_hosts")

	for _, host := range hosts {
		if host == "" {
			continue
		}
		cmd := exec.Command("ssh-keygen", "-F", host, "-f", knownHostsPath)
		err := cmd.Run()
		if err == nil {
			// Host already exists in known_hosts
			zap.S().Infof("SSH host %s already registered", host)
			continue
		}

		zap.S().Infof("Registering SSH host %s", host)
		cmd = exec.Command("ssh-keyscan", "-H", host)
		output, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("keyscan %s: %w", host, err)
		}

		f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		f.Write(output)
		f.Close()
	}

	return nil
}
