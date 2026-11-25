package challenge

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/28Pollux28/galvanize/internal/pathutil"
	"github.com/28Pollux28/galvanize/pkg/config"
	yaml "github.com/oasdiff/yaml3"
	"go.uber.org/zap"
)

// isChallengeDefFile reports whether name is a challenge definition file:
// challenge.yml / challenge.yaml, or a numbered variant such as
// challenge-1.yml, so one directory can hold several challenges.
func isChallengeDefFile(name string) bool {
	if name == "challenge.yml" || name == "challenge.yaml" {
		return true
	}
	if !strings.HasPrefix(name, "challenge-") {
		return false
	}
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

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
	// Catalog lists every challenge definition the last BuildIndex read,
	// whatever its type (Get and GetAll only know deployable zync
	// challenges), sorted by file path.
	Catalog() []*Challenge
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
	catalog []*Challenge
	skipped []SkippedChallenge
}

type Challenge struct {
	Name             string                 `yaml:"name"`
	Category         string                 `yaml:"category"`
	PlaybookName     string                 `yaml:"playbook_name"`
	Type             string                 `yaml:"type"`
	Unique           bool                   `yaml:"-"`
	InstanceGroup    string                 `yaml:"-"`
	ShowTCPPorts     bool                   `yaml:"-"`
	ResourceLimits   config.ResourceLimits  `yaml:"-"`
	Dir              string                 `yaml:"-"`
	DeployParameters map[string]interface{} `yaml:"deploy_parameters"`
	// File is the challenge definition file the challenge was read from.
	File string `yaml:"-"`
	// ComposeFile is the Compose file of the challenge folder (explicit
	// compose_file, else a standard name), checked to be inside it: the one
	// compose_definition was loaded from or, next to an inline
	// compose_definition (ComposeInline), the one images are built from.
	// Empty when there is none.
	ComposeFile string `yaml:"-"`
	// ComposeInline: compose_definition is written in the definition file,
	// and deployed instead of ComposeFile.
	ComposeInline bool `yaml:"-"`
	// Definition is the whole definition file, as parsed, for clients that
	// need more than the deployment fields (e.g. a CTF platform importing
	// flags and descriptions). Never modified by Galvanize.
	Definition map[string]interface{} `yaml:"-"`
}

// InstanceKey is the name deployments of this challenge are stored and
// deployed under: deploy_parameters.instance_group when set (so several
// challenge entries share one instance), the challenge name otherwise.
func (c *Challenge) InstanceKey() string {
	if c != nil && strings.TrimSpace(c.InstanceGroup) != "" {
		return c.InstanceGroup
	}
	if c == nil {
		return ""
	}
	return c.Name
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
	var catalog []*Challenge
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
			// Hidden folders are skipped too: they hold tooling (.git,
			// .github) and the temporary clones of the repository manager
			if path != baseDir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "example") {
				return filepath.SkipDir
			}
			return nil
		}
		if !isChallengeDefFile(d.Name()) {
			return nil
		}
		chall, err := parseChallenge(path)
		if err != nil {
			skip(path, err.Error())
			// nil, not SkipDir: a directory can hold several challenge-*.yml
			// files and one bad file must not hide its siblings.
			return nil
		}
		catalog = append(catalog, chall)
		if chall.Type != "zync" {
			return nil
		}
		key := chall.Category + "/" + chall.Name
		if first, ok := paths[key]; ok {
			skip(path, fmt.Sprintf("duplicate of %s (both declare %s)", first, key))
			return nil
		}
		challs[key] = chall
		paths[key] = path
		zap.S().Infof("Registered challenge: %s", key)
		return nil
	})
	if err != nil {
		return fmt.Errorf("index %s: %w", baseDir, err)
	}

	idx.mu.Lock()
	idx.challs = challs
	idx.catalog = catalog
	idx.skipped = skipped
	idx.mu.Unlock()
	return nil
}

// Catalog returns every challenge definition read by the last successful
// BuildIndex, in file path order (WalkDir's lexical order).
func (idx *ChallengeIndex) Catalog() []*Challenge {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return append([]*Challenge(nil), idx.catalog...)
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
	if chall, ok := idx.challs[key]; ok {
		return chall, nil
	}
	for _, chall := range idx.challs {
		if !strings.EqualFold(chall.Category, category) {
			continue
		}
		if chall.Name == name || strings.EqualFold(chall.InstanceGroup, name) {
			return chall, nil
		}
	}
	return nil, fmt.Errorf("challenge not found: %s", key)
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
	var definition interface{}
	if err := yaml.Unmarshal(data, &definition); err != nil {
		return nil, fmt.Errorf("failed to parse challenge file: %w", err)
	}
	normalized, err := jsonSafe(definition, "")
	if err != nil {
		return nil, fmt.Errorf("challenge file cannot be served as JSON: %w", err)
	}
	if m, ok := normalized.(map[string]interface{}); ok {
		challenge.Definition = m
	}
	challenge.File = challengeFilePath
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
	if group := toStringField(challenge.DeployParameters, "instance_group"); group != "" {
		challenge.InstanceGroup = group
	}
	if show, ok := challenge.DeployParameters["show_tcp_ports"]; ok {
		if b, ok := show.(bool); ok && b {
			challenge.ShowTCPPorts = true
		}
	}
	if _, hasPorts := challenge.DeployParameters["published_ports"]; !hasPorts {
		if port, ok := challenge.DeployParameters["port"]; ok && port != nil {
			challenge.DeployParameters["published_ports"] = []interface{}{port}
		}
	}
	if challenge.PlaybookName == "http" {
		if _, hasHTTP := challenge.DeployParameters["http_port"]; !hasHTTP {
			if port, ok := challenge.DeployParameters["port"]; ok && port != nil {
				challenge.DeployParameters["http_port"] = port
			}
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
	if err := tagBuiltServices(&challenge); err != nil {
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
// The Compose file is an explicit deploy_parameters.compose_file (path
// relative to the challenge directory), else the first standard Compose file
// name found in the challenge directory. It must stay inside the challenge
// folder, symlinks included. An inline deploy_parameters.compose_definition
// wins over it for the deployment (the file is then only read to build).
//
// When a file is loaded, its contents are placed in compose_definition so the
// existing custom_compose playbook consumes it unchanged, and playbook_name
// defaults to "custom_compose" if the author did not set one.
func loadComposeDefinition(c *Challenge) error {
	var composePath string
	if explicit := toStringField(c.DeployParameters, "compose_file"); explicit != "" {
		if filepath.IsAbs(explicit) || strings.HasPrefix(explicit, "/") {
			return fmt.Errorf("compose_file %q for challenge %q: must be a path inside the challenge folder", explicit, c.Name)
		}
		composePath = filepath.Join(c.Dir, filepath.FromSlash(explicit))
		if err := insideChallengeDir(c.Dir, composePath); err != nil {
			return fmt.Errorf("compose_file %q for challenge %q: %w", explicit, c.Name, err)
		}
		if _, err := os.Stat(composePath); err != nil {
			return fmt.Errorf("compose_file %q for challenge %q not found: %w", explicit, c.Name, err)
		}
		delete(c.DeployParameters, "compose_file")
	} else {
		for _, name := range composeFileNames {
			candidate := filepath.Join(c.Dir, name)
			if _, err := os.Stat(candidate); err == nil {
				if err := insideChallengeDir(c.Dir, candidate); err != nil {
					return fmt.Errorf("%s for challenge %q: %w", name, c.Name, err)
				}
				composePath = candidate
				break
			}
		}
	}
	c.ComposeFile = composePath

	if def, ok := c.DeployParameters["compose_definition"].(string); ok && strings.TrimSpace(def) != "" {
		c.ComposeInline = true
		return nil
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

// insideChallengeDir checks that path, symlinks resolved, is inside dir:
// a challenge reads no file of another challenge or of the host.
func insideChallengeDir(dir, path string) error {
	outside := fmt.Errorf("must be inside the challenge folder")
	if !pathutil.Inside(dir, path) {
		return outside
	}
	_, err := pathutil.Resolve(dir, path)
	switch {
	case errors.Is(err, pathutil.ErrOutside):
		return outside
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return err
	}
	return nil // missing: reported by the caller
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

// BuiltImageTag is the image a Compose service with a build section but no
// image is tagged with: Galvanize builds it under that name on the Ansible
// hosts and deploys it from there. The readable part is made of slugs, which
// several names share ("web/a-b" and "web-a/b", "Web" and "web", non-ASCII
// names); the hash of the exact names makes the tag unique to one service of
// one challenge.
func BuiltImageTag(category, challengeName, service string) string {
	return "galvanize/" + BuildSlug(category, challengeName, service) + ":latest"
}

// BuildSlug joins the slugs of parts and a hash of their exact values, for
// image and Compose project names unique to these parts.
func BuildSlug(parts ...string) string {
	h := sha256.New()
	slugs := make([]string, 0, len(parts)+1)
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
		slug := ImageSlug(p)
		if len(slug) > 40 {
			slug = strings.TrimRight(slug[:40], "-")
		}
		if slug == "" {
			slug = "x"
		}
		slugs = append(slugs, slug)
	}
	return strings.Join(slugs, "-") + "-" + hex.EncodeToString(h.Sum(nil))[:10]
}

// ImageSlug lowercases s and replaces runs of other characters than a-z and
// 0-9 with a dash, for Docker image and project names. Different names can
// share a slug: see BuildSlug for unique names.
func ImageSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// tagBuiltServices gives every service of a string compose_definition that
// has a build section and no image the BuiltImageTag name. Without it, the
// deployment could only try to build from a context that does not exist on
// the host, while the build feature builds exactly this name.
func tagBuiltServices(c *Challenge) error {
	def, ok := c.DeployParameters["compose_definition"].(string)
	if !ok || strings.TrimSpace(def) == "" {
		return nil
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(def), &doc); err != nil {
		// Not YAML as it stands (e.g. Jinja expressions the playbook renders
		// first): deployed as written, nothing to tag
		return nil
	}
	services, _ := doc["services"].(map[string]interface{})
	changed := false
	for name, raw := range services {
		svc, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if _, hasBuild := svc["build"]; !hasBuild {
			continue
		}
		if image, _ := svc["image"].(string); strings.TrimSpace(image) != "" {
			continue
		}
		svc["image"] = BuiltImageTag(c.Category, c.Name, name)
		changed = true
	}
	if !changed {
		return nil
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("failed to tag built services: %w", err)
	}
	c.DeployParameters["compose_definition"] = string(out)
	return nil
}
