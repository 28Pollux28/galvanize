package challenge

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/28Pollux28/galvanize/pkg/config"
	yaml "github.com/oasdiff/yaml3"
	"go.uber.org/zap"
)

// composeFileNames lists the standard Docker Compose file names that are
// auto-detected next to a challenge.yml, in the order Docker Compose itself
// resolves them.
var composeFileNames = []string{
	"compose.yaml",
	"compose.yml",
	"docker-compose.yaml",
	"docker-compose.yml",
}

// Exposure types supported by the expose block.
const (
	ExposeHTTP = "http"
	ExposeTCP  = "tcp"
)

// Exposure declares how a single Docker Compose service should be reached by
// players. It lets compose challenges get the same automatic networking wiring
// (Traefik for http, published host ports for tcp) that the http/tcp playbooks
// provide for single-container challenges.
type Exposure struct {
	// Service is the compose service name to expose.
	Service string
	// Port is the container port the service listens on.
	Port int
	// Type is either "http" (routed through Traefik) or "tcp" (published port).
	Type string
	// Scheme overrides the URL scheme used in connection info (e.g. "ssh").
	Scheme string
}

// ParseExposures reads the optional deploy_parameters.expose list. It returns
// nil when no expose block is present. Each entry is validated for structure.
func ParseExposures(params map[string]interface{}) ([]Exposure, error) {
	raw, ok := params["expose"]
	if !ok {
		return nil, nil
	}
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expose must be a list of exposure entries")
	}

	exposures := make([]Exposure, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("expose[%d] must be a mapping", i)
		}
		exp := Exposure{
			Service: toStringField(m, "service"),
			Port:    toIntField(m, "port"),
			Type:    strings.ToLower(toStringField(m, "type")),
			Scheme:  toStringField(m, "scheme"),
		}
		if exp.Service == "" {
			return nil, fmt.Errorf("expose[%d]: missing service", i)
		}
		if exp.Port <= 0 {
			return nil, fmt.Errorf("expose[%d] (%s): port must be a positive integer", i, exp.Service)
		}
		if exp.Type != ExposeHTTP && exp.Type != ExposeTCP {
			return nil, fmt.Errorf("expose[%d] (%s): type must be %q or %q", i, exp.Service, ExposeHTTP, ExposeTCP)
		}
		exposures = append(exposures, exp)
	}
	return exposures, nil
}

func toStringField(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func toIntField(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return 0
}

// ChallengeIndexer is the interface for looking up and managing challenges.
// Consumers should depend on this interface rather than the concrete ChallengeIndex.
type ChallengeIndexer interface {
	Get(category, name string) (*Challenge, error)
	GetAllUnique() []*Challenge
	GetAll() []*Challenge
	BuildIndex(baseDir string) error
	// Skipped lists the challenge files the last BuildIndex left out.
	Skipped() []SkippedChallenge
}

// SkippedChallenge is a challenge file left out of the index, and why.
type SkippedChallenge struct {
	Path   string
	Reason string
}

// Compile-time check that ChallengeIndex implements ChallengeIndexer.
var _ ChallengeIndexer = (*ChallengeIndex)(nil)

type ChallengeIndex struct {
	mu      sync.RWMutex
	challs  map[string]*Challenge
	skipped []SkippedChallenge
}

type Challenge struct {
	Name             string                 `yaml:"name"`
	Category         string                 `yaml:"category"`
	PlaybookName     string                 `yaml:"playbook_name"`
	Type             string                 `yaml:"type"`
	Unique           bool                   `yaml:"-"`
	ResourceLimits   config.ResourceLimits  `yaml:"-"`
	Dir              string                 `yaml:"-"`
	DeployParameters map[string]interface{} `yaml:"deploy_parameters"`
}

func NewChallengeIndex(baseDir string) (*ChallengeIndex, error) {
	idx := &ChallengeIndex{
		challs: make(map[string]*Challenge),
	}
	err := idx.BuildIndex(baseDir)
	if err != nil {
		return nil, err
	}
	return idx, nil
}

// BuildIndex (re)builds the index from the challenge files under baseDir.
//
// A challenge file that cannot be read or parsed, or that declares the same
// category/name as one already indexed, is skipped and reported (Skipped and
// the logs) instead of failing the whole index: one bad challenge.yml would
// otherwise stop Galvanize from starting, or make every reload fail.
// baseDir itself must be readable; otherwise an error is returned and the
// current index is kept as it is.
func (idx *ChallengeIndex) BuildIndex(baseDir string) error {
	challs := make(map[string]*Challenge)
	paths := make(map[string]string)
	var skipped []SkippedChallenge
	skip := func(path, reason string) {
		skipped = append(skipped, SkippedChallenge{Path: path, Reason: reason})
		zap.S().Errorf("Skipping challenge %s: %s", path, reason)
	}

	err := filepath.WalkDir(baseDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == baseDir {
				return err
			}
			skip(path, err.Error())
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "example" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "challenge.yml" && d.Name() != "challenge.yaml" {
			return nil
		}
		// Parse challenge.yml to get category and name
		chall, err := parseChallenge(path)
		if err != nil {
			skip(path, err.Error())
			return filepath.SkipDir
		}
		if chall.Type != "zync" {
			return filepath.SkipDir
		}
		key := chall.Category + "/" + chall.Name
		if first, ok := paths[key]; ok {
			skip(path, fmt.Sprintf("duplicate of %s (both declare %s)", first, key))
			return filepath.SkipDir
		}
		challs[key] = chall
		paths[key] = path
		zap.S().Infof("Registered challenge: %s", key)

		return filepath.SkipDir
	})
	if err != nil {
		return fmt.Errorf("index %s: %w", baseDir, err)
	}

	idx.mu.Lock()
	idx.challs = challs
	idx.skipped = skipped
	idx.mu.Unlock()
	return nil
}

// Skipped returns the challenge files the last successful BuildIndex left out.
func (idx *ChallengeIndex) Skipped() []SkippedChallenge {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return append([]SkippedChallenge(nil), idx.skipped...)
}

func (idx *ChallengeIndex) Get(category, name string) (*Challenge, error) {
	key := category + "/" + name
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	chall, ok := idx.challs[key]
	if !ok {
		return nil, fmt.Errorf("challenge not found: %s", key)
	}
	return chall, nil
}

func (idx *ChallengeIndex) GetAllUnique() []*Challenge {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	var unique []*Challenge
	for _, chall := range idx.challs {
		if chall.Unique {
			unique = append(unique, chall)
		}
	}
	return unique
}

func (idx *ChallengeIndex) GetAll() []*Challenge {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	challs := make([]*Challenge, 0, len(idx.challs))
	for _, ch := range idx.challs {
		challs = append(challs, ch)
	}
	return challs
}

func parseChallenge(challengeFilePath string) (*Challenge, error) {
	data, err := os.ReadFile(challengeFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read challenge file: %w", err)
	}
	var challenge Challenge
	err = yaml.Unmarshal(data, &challenge)
	if err != nil {
		return nil, fmt.Errorf("failed to parse challenge file: %w", err)
	}
	if challenge.Name == "" {
		return nil, fmt.Errorf("missing name in challenge file")
	}
	if challenge.Category == "" {
		return nil, fmt.Errorf("missing category in challenge file")
	}
	if challenge.Type == "" {
		return nil, fmt.Errorf("missing type in challenge file")
	}
	challenge.Dir = filepath.Dir(challengeFilePath)

	if challenge.DeployParameters == nil {
		challenge.DeployParameters = map[string]interface{}{}
	}

	if unique, ok := challenge.DeployParameters["unique"]; ok {
		if b, ok := unique.(bool); ok && b {
			challenge.Unique = true
		}
	}

	if rl, ok := challenge.DeployParameters["resource_limits"].(map[string]interface{}); ok {
		challenge.ResourceLimits = config.ResourceLimits{
			CPUs:      toStringField(rl, "cpus"),
			Memory:    toStringField(rl, "memory"),
			PidsLimit: toIntField(rl, "pids_limit"),
		}
	}

	if err := loadComposeDefinition(&challenge); err != nil {
		return nil, err
	}

	if err := validateExposures(&challenge); err != nil {
		return nil, err
	}

	return &challenge, nil
}

// loadComposeDefinition lets authors keep a standalone Docker Compose file
// (e.g. compose.yaml or docker-compose.yml) next to challenge.yml instead of
// embedding the whole document as a multiline string under
// deploy_parameters.compose_definition.
//
// Resolution order:
//  1. An inline deploy_parameters.compose_definition always wins.
//  2. An explicit deploy_parameters.compose_file (path relative to the
//     challenge directory).
//  3. Auto-detected standard Compose file names in the challenge directory.
//
// When a file is loaded, its contents are placed in compose_definition so the
// existing custom_compose playbook consumes it unchanged, and playbook_name
// defaults to "custom_compose" if the author did not set one.
func loadComposeDefinition(c *Challenge) error {
	if def, ok := c.DeployParameters["compose_definition"].(string); ok && strings.TrimSpace(def) != "" {
		return nil
	}

	var composePath string
	if explicit := toStringField(c.DeployParameters, "compose_file"); explicit != "" {
		composePath = filepath.Join(c.Dir, explicit)
		if _, err := os.Stat(composePath); err != nil {
			return fmt.Errorf("compose_file %q for challenge %q not found: %w", explicit, c.Name, err)
		}
		delete(c.DeployParameters, "compose_file")
	} else {
		for _, name := range composeFileNames {
			candidate := filepath.Join(c.Dir, name)
			if _, err := os.Stat(candidate); err == nil {
				composePath = candidate
				break
			}
		}
	}

	if composePath == "" {
		return nil
	}

	data, err := os.ReadFile(composePath)
	if err != nil {
		return fmt.Errorf("failed to read compose file %s: %w", composePath, err)
	}

	var parsed map[string]interface{}
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		return fmt.Errorf("invalid compose file %s: %w", composePath, err)
	}
	if _, ok := parsed["services"]; !ok {
		return fmt.Errorf("compose file %s has no 'services' section", composePath)
	}

	c.DeployParameters["compose_definition"] = string(data)
	if c.PlaybookName == "" {
		c.PlaybookName = "custom_compose"
	}
	zap.S().Infof("Loaded compose definition for challenge %q from %s", c.Name, filepath.Base(composePath))

	return nil
}

// validateExposures checks the optional expose block: entries must be
// well-formed and reference services that exist in the compose definition.
// It runs after the compose definition has been resolved so authors get clear
// errors at index time rather than at deploy time.
func validateExposures(c *Challenge) error {
	exposures, err := ParseExposures(c.DeployParameters)
	if err != nil {
		return fmt.Errorf("challenge %q: %w", c.Name, err)
	}
	if len(exposures) == 0 {
		return nil
	}

	def, ok := c.DeployParameters["compose_definition"].(string)
	if !ok || strings.TrimSpace(def) == "" {
		return fmt.Errorf("challenge %q: expose requires a compose definition (compose file or compose_definition)", c.Name)
	}

	var parsed struct {
		Services map[string]interface{} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(def), &parsed); err != nil {
		return fmt.Errorf("challenge %q: invalid compose definition: %w", c.Name, err)
	}
	for _, exp := range exposures {
		if _, ok := parsed.Services[exp.Service]; !ok {
			return fmt.Errorf("challenge %q: expose references unknown service %q", c.Name, exp.Service)
		}
	}
	return nil
}
