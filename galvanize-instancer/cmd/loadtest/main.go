package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/28Pollux28/galvanize/internal/loadtest"
)

func main() {
	var cfg loadtest.Config

	flag.StringVar(&cfg.BaseURL, "base-url", "http://localhost:8080", "Instancer base URL")
	flag.StringVar(&cfg.JWTSecret, "jwt-secret", os.Getenv("JWT_SECRET"), "JWT secret (defaults to JWT_SECRET env var)")
	flag.StringVar(&cfg.Category, "category", "web", "Challenge category")
	flag.StringVar(&cfg.ChallengeName, "challenge", "http", "Challenge name")
	flag.StringVar(&cfg.Role, "role", "team", "JWT role claim")
	flag.IntVar(&cfg.Teams, "teams", 500, "Number of teams to simulate")
	flag.IntVar(&cfg.Concurrency, "concurrency", 500, "Number of concurrent workers")
	flag.StringVar(&cfg.TeamPrefix, "team-prefix", "team-", "Team ID prefix")
	flag.IntVar(&cfg.TeamStart, "team-start", 1, "First team number")
	flag.DurationVar(&cfg.RequestTimeout, "timeout", 20*time.Second, "HTTP request timeout")
	flag.BoolVar(&cfg.InsecureTLS, "insecure", false, "Skip TLS verification")
	flag.StringVar(&cfg.PhasesCSV, "phases", "deploy" /*"deploy,status,terminate"*/, "Comma-separated phases: deploy,status,terminate")

	flag.Parse()

	if err := loadtest.Run(context.Background(), cfg, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "loadtest error: %v\n", err)
		os.Exit(1)
	}
}
