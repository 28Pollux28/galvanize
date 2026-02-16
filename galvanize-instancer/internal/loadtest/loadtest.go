package loadtest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const maxInt64 = int64(^uint64(0) >> 1)

// Config controls the load test behavior.
type Config struct {
	BaseURL        string
	JWTSecret      string
	Category       string
	ChallengeName  string
	Role           string
	Teams          int
	Concurrency    int
	TeamPrefix     string
	TeamStart      int
	RequestTimeout time.Duration
	InsecureTLS    bool
	PhasesCSV      string
}

type Phase struct {
	Name    string
	Method  string
	Path    string
	HasBody bool
}

var defaultPhases = []Phase{
	{Name: "deploy", Method: http.MethodPost, Path: "/deploy", HasBody: true},
	{Name: "status", Method: http.MethodGet, Path: "/status", HasBody: false},
	{Name: "terminate", Method: http.MethodPost, Path: "/terminate", HasBody: true},
}

func ParsePhases(csv string) ([]Phase, error) {
	if strings.TrimSpace(csv) == "" {
		return defaultPhases, nil
	}
	items := strings.Split(csv, ",")
	phases := make([]Phase, 0, len(items))
	for _, raw := range items {
		name := strings.TrimSpace(strings.ToLower(raw))
		if name == "" {
			continue
		}
		switch name {
		case "deploy":
			phases = append(phases, defaultPhases[0])
		case "status":
			phases = append(phases, defaultPhases[1])
		case "terminate":
			phases = append(phases, defaultPhases[2])
		default:
			return nil, fmt.Errorf("unknown phase: %s", name)
		}
	}
	if len(phases) == 0 {
		return nil, errors.New("no valid phases provided")
	}
	return phases, nil
}

func ValidateConfig(cfg Config) error {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return errors.New("base URL is required")
	}
	if strings.TrimSpace(cfg.JWTSecret) == "" {
		return errors.New("jwt secret is required")
	}
	if strings.TrimSpace(cfg.Category) == "" {
		return errors.New("category is required")
	}
	if strings.TrimSpace(cfg.ChallengeName) == "" {
		return errors.New("challenge name is required")
	}
	if cfg.Teams <= 0 {
		return errors.New("teams must be > 0")
	}
	if cfg.Concurrency <= 0 {
		return errors.New("concurrency must be > 0")
	}
	if cfg.TeamStart < 0 {
		return errors.New("team start must be >= 0")
	}
	if strings.TrimSpace(cfg.TeamPrefix) == "" {
		return errors.New("team prefix is required")
	}
	if cfg.RequestTimeout <= 0 {
		return errors.New("request timeout must be > 0")
	}
	return nil
}

func Run(ctx context.Context, cfg Config, out io.Writer) error {
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	phases, err := ParsePhases(cfg.PhasesCSV)
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	client := &http.Client{Timeout: cfg.RequestTimeout, Transport: newTransport(cfg)}

	tokens := make([]string, cfg.Teams)
	for i := 0; i < cfg.Teams; i++ {
		teamID := fmt.Sprintf("%s%04d", cfg.TeamPrefix, cfg.TeamStart+i)
		token, err := signToken(cfg, teamID)
		if err != nil {
			return fmt.Errorf("sign token for %s: %w", teamID, err)
		}
		tokens[i] = token
	}

	for _, phase := range phases {
		stats := newStats()
		start := time.Now()
		err := runPhase(ctx, client, baseURL, cfg, phase, tokens, stats)
		elapsed := time.Since(start)
		if err != nil {
			return err
		}
		stats.report(out, phase.Name, elapsed)
		time.Sleep(1 * time.Minute)
	}
	return nil
}

func runPhase(ctx context.Context, client *http.Client, baseURL string, cfg Config, phase Phase, tokens []string, stats *stats) error {
	jobs := make(chan int)
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for idx := range jobs {
			select {
			case <-ctx.Done():
				return
			default:
			}

			req, err := newRequest(baseURL, cfg, phase, tokens[idx])
			if err != nil {
				stats.record(0, 0, err.Error())
				continue
			}
			start := time.Now()
			resp, err := client.Do(req)
			lat := time.Since(start)
			if err != nil {
				stats.record(0, lat, err.Error())
				continue
			}
			status := resp.StatusCode
			bodySample := ""
			if status < 200 || status >= 300 {
				bodySample = readSample(resp.Body)
			} else {
				_, _ = io.Copy(io.Discard, resp.Body)
			}
			_ = resp.Body.Close()
			if status < 200 || status >= 300 {
				stats.record(status, lat, fmt.Sprintf("status %d: %s", status, bodySample))
				continue
			}
			stats.record(status, lat, "")
		}
	}

	workers := cfg.Concurrency
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	for i := 0; i < len(tokens); i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return nil
}

func newRequest(baseURL string, cfg Config, phase Phase, token string) (*http.Request, error) {
	url := baseURL + phase.Path
	var body io.Reader
	if phase.HasBody {
		payload := map[string]string{
			"category":       cfg.Category,
			"challenge_name": cfg.ChallengeName,
		}
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(phase.Method, url, body)
	if err != nil {
		return nil, err
	}
	if phase.HasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return req, nil
}

func readSample(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 256))
	return strings.TrimSpace(string(data))
}

func newTransport(cfg Config) *http.Transport {
	tr := &http.Transport{
		MaxIdleConns:        cfg.Concurrency * 2,
		MaxIdleConnsPerHost: cfg.Concurrency * 2,
		IdleConnTimeout:     90 * time.Second,
	}
	if cfg.InsecureTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return tr
}

func signToken(cfg Config, teamID string) (string, error) {
	claims := jwt.MapClaims{
		"team_id":        teamID,
		"challenge_name": cfg.ChallengeName,
		"category":       cfg.Category,
		"role":           cfg.Role,
		"iat":            time.Now().Unix(),
		"exp":            time.Now().Add(1 * time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(cfg.JWTSecret))
}

type stats struct {
	total      int64
	success    int64
	errors     int64
	sumLatency int64
	minLatency int64
	maxLatency int64
	mu         sync.Mutex
	statuses   map[int]int
	errSamples []string
}

func newStats() *stats {
	return &stats{minLatency: maxInt64, statuses: make(map[int]int)}
}

func (s *stats) record(status int, latency time.Duration, errText string) {
	atomic.AddInt64(&s.total, 1)
	atomic.AddInt64(&s.sumLatency, int64(latency))
	updateMin(&s.minLatency, int64(latency))
	updateMax(&s.maxLatency, int64(latency))
	if status > 0 {
		s.mu.Lock()
		s.statuses[status]++
		s.mu.Unlock()
	}
	if errText != "" {
		atomic.AddInt64(&s.errors, 1)
		s.mu.Lock()
		if len(s.errSamples) < 5 {
			s.errSamples = append(s.errSamples, errText)
		}
		s.mu.Unlock()
		return
	}
	atomic.AddInt64(&s.success, 1)
}

func (s *stats) report(out io.Writer, phase string, elapsed time.Duration) {
	total := atomic.LoadInt64(&s.total)
	success := atomic.LoadInt64(&s.success)
	errors := atomic.LoadInt64(&s.errors)
	sumLatency := atomic.LoadInt64(&s.sumLatency)
	minLatency := time.Duration(atomic.LoadInt64(&s.minLatency))
	maxLatency := time.Duration(atomic.LoadInt64(&s.maxLatency))
	avgLatency := time.Duration(0)
	if total > 0 {
		avgLatency = time.Duration(sumLatency / total)
	}
	if minLatency == time.Duration(maxInt64) {
		minLatency = 0
	}

	fmt.Fprintf(out, "\nPhase %s\n", phase)
	fmt.Fprintf(out, "  Duration: %s\n", elapsed)
	fmt.Fprintf(out, "  Total: %d  Success: %d  Errors: %d\n", total, success, errors)
	fmt.Fprintf(out, "  Latency: min %s  avg %s  max %s\n", minLatency, avgLatency, maxLatency)

	s.mu.Lock()
	if len(s.statuses) > 0 {
		fmt.Fprintf(out, "  Status counts:")
		for code, count := range s.statuses {
			fmt.Fprintf(out, " %d=%d", code, count)
		}
		fmt.Fprintln(out, "")
	}
	if len(s.errSamples) > 0 {
		fmt.Fprintln(out, "  Error samples:")
		for _, sample := range s.errSamples {
			fmt.Fprintf(out, "    - %s\n", sample)
		}
	}
	s.mu.Unlock()
}

func updateMin(ptr *int64, val int64) {
	for {
		cur := atomic.LoadInt64(ptr)
		if val >= cur {
			return
		}
		if atomic.CompareAndSwapInt64(ptr, cur, val) {
			return
		}
	}
}

func updateMax(ptr *int64, val int64) {
	for {
		cur := atomic.LoadInt64(ptr)
		if val <= cur {
			return
		}
		if atomic.CompareAndSwapInt64(ptr, cur, val) {
			return
		}
	}
}
