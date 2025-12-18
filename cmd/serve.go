package cmd

import (
	"fmt"
	"log"
	"os"
	"strconv"

	server "github.com/28Pollux28/poly-instancer/pkg"
	"github.com/28Pollux28/poly-instancer/pkg/api"
	"github.com/28Pollux28/poly-instancer/pkg/config"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo-contrib/echoprometheus"
	echojwt "github.com/labstack/echo-jwt/v4"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/spf13/cobra"
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

		// 2. Middleware
		e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
			Format: "${time_rfc3339} | ${remote_ip} | ${method} ${uri} | ${status} | ${latency_human}\n",
			Output: log.Writer(),
		}))
		e.Use(middleware.Recover())
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
			log.Fatal("JWT_SECRET (or JWT_SECRET) is required")
		}
		log.Printf("Using JWT secret: %s", jwtSecret)

		ansiblePath := os.Getenv("ANSIBLE_PATH")
		if ansiblePath == "" {
			ansiblePath = cfg.Deployer.AnsibleDir
		}
		if ansiblePath == "" {
			log.Fatal("ANSIBLE_PATH is required")
		}

		// 5. Auth
		jwtConfig := echojwt.Config{
			NewClaimsFunc: func(c echo.Context) jwt.Claims {
				return new(server.Claims)
			},
			SigningKey: []byte(jwtSecret),
			Skipper: func(c echo.Context) bool {
				return c.Path() == "/health" || c.Path() == "/metrics"
			},
		}
		e.Use(echojwt.WithConfig(jwtConfig))

		// 6. Server Init (Includes Metric Rehydration)
		srv := server.NewServer(cfg.Deployer.DBPath)
		api.RegisterHandlers(e, srv)

		e.Logger.Fatal(e.Start(":" + portStr))
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
