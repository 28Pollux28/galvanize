package config

import (
	"fmt"
	"sync"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
)

// Provider is the interface for obtaining configuration.
// Consumers should depend on this interface rather than calling the global Get() directly.
type Provider interface {
	GetConfig() *Config
}

// GlobalProvider implements Provider using the package-level singleton.
type GlobalProvider struct{}

func (GlobalProvider) GetConfig() *Config { return Get() }

// StaticProvider implements Provider with a fixed config value, useful for testing.
type StaticProvider struct {
	Cfg *Config
}

func (p *StaticProvider) GetConfig() *Config { return p.Cfg }

type Config struct {
	Auth      AuthConfig      `mapstructure:"auth"`
	Instancer InstancerConfig `mapstructure:"instancer"`
}

type MetricsConfig struct {
	Username string `mapstructure:"username"` // Basic-auth username for the metrics endpoint (default: "prometheus")
	Password string `mapstructure:"password"` // Basic-auth password; leave empty to disable auth
}

type AuthConfig struct {
	JWTSecret string `mapstructure:"jwt_secret"`
}

type InstancerConfig struct {
	Ansible                   AnsibleConfig          `mapstructure:"ansible"`                               // Ansible specific configuration
	ExtraDeploymentParameters map[string]interface{} `mapstructure:"extra_deployment_parameters"`           // Extra parameters for deployment
	AnsibleDir                string                 `mapstructure:"ansible_dir"`                           // Directory where Ansible playbooks are located
	ChallengeDir              string                 `mapstructure:"challenge_dir"`                         // Directory where challenge playbooks are stored
	InstancerHost             string                 `mapstructure:"instancer_host"`                        // Hostname of the node where challenges will be deployed
	ComposeProjectPrefix      string                 `mapstructure:"compose_project_prefix,omitempty"`      // Prefix for compose project / instance subdomains (default: "ctf-event")
	DBPath                    string                 `mapstructure:"db_path"`                               // Path to the database file
	DeploymentTTL             time.Duration          `mapstructure:"deployment_ttl,omitempty"`              // Time-to-live for deployments
	DeploymentTTLExtension    time.Duration          `mapstructure:"deployment_ttl_extension,omitempty"`    // Duration to extend deployment TTL
	DeploymentMaxExtensions   int                    `mapstructure:"deployment_max_extensions,omitempty"`   // Maximum number of TTL extensions allowed
	DeploymentExtensionWindow time.Duration          `mapstructure:"deployment_extension_window,omitempty"` // Time window before expiration when extension is allowed
	RandomizePublishedPorts   bool                   `mapstructure:"randomize_published_ports,omitempty"`   // Randomize host ports for non-fixed TCP published_ports
	RandomizedPortMin         int                    `mapstructure:"randomized_port_min,omitempty"`         // Lower bound for randomized host ports, included (default: 20000)
	RandomizedPortMax         int                    `mapstructure:"randomized_port_max,omitempty"`         // Upper bound for randomized host ports, included (default: 60999)
	MaxConcurrentAnsible      int                    `mapstructure:"max_concurrent_ansible,omitempty"`      // Without Redis: maximum concurrent Ansible runs for team requests and expiries (default: 5); admin actions are not limited
	Redis                     RedisConfig            `mapstructure:"redis"`                                 // Redis configuration for job queue
	NumWorkers                int                    `mapstructure:"num_workers,omitempty"`                 // Number of Ansible workers (default: 10)
	Metrics                   MetricsConfig          `mapstructure:"metrics"`                               // Metrics endpoint configuration
	DefaultResourceLimits     ResourceLimits         `mapstructure:"default_resource_limits"`               // Default container resource limits applied to all deployments
	Repositories              RepositoriesConfig     `mapstructure:"repositories"`                          // Challenge repositories managed through the admin API
	Build                     BuildConfig            `mapstructure:"build"`                                 // Image builds on the Ansible hosts
}

// RepositoriesConfig lists where challenge repositories may be cloned from.
// Cloning is disabled while Sources is empty; repositories already in
// challenge_dir can still be listed, updated and removed.
type RepositoriesConfig struct {
	Sources    []RepositorySource `mapstructure:"sources"`
	GitTimeout time.Duration      `mapstructure:"git_timeout,omitempty"` // Time limit of one git command (default: 10m)
}

// RepositorySource allows clones of the repositories below URLPrefix, a URL
// ending with "/" (same scheme, host and port, path inside its folder; see
// Allows). Token, when
// set, is sent as HTTP basic auth (Username, default "x-access-token") to
// fetch private repositories; it is never written to the clone.
type RepositorySource struct {
	URLPrefix string `mapstructure:"url_prefix"`
	Username  string `mapstructure:"username,omitempty"`
	Token     string `mapstructure:"token,omitempty"`
}

// BuildConfig controls image builds, run with Docker on every Ansible host.
type BuildConfig struct {
	Concurrency int           `mapstructure:"concurrency,omitempty"` // Challenges built at the same time (default: 2)
	Timeout     time.Duration `mapstructure:"timeout,omitempty"`     // Time limit of one challenge build (default: 30m)
	// ProtectedImages are images challenges may never build (an image
	// repository, any tag, or a prefix ending with "/"): the platform's own.
	// Unset: Galvanize's defaults (traefik, mariadb, redis...); [] disables.
	ProtectedImages []string `mapstructure:"protected_images"`
}

const (
	DefaultGitTimeout       = 10 * time.Minute
	DefaultBuildConcurrency = 2
	DefaultBuildTimeout     = 30 * time.Minute
)

// GitTimeoutOrDefault returns git_timeout, or its default when unset.
func (r RepositoriesConfig) GitTimeoutOrDefault() time.Duration {
	if r.GitTimeout > 0 {
		return r.GitTimeout
	}
	return DefaultGitTimeout
}

// ConcurrencyOrDefault returns build.concurrency, or its default when unset.
func (b BuildConfig) ConcurrencyOrDefault() int {
	if b.Concurrency > 0 {
		return b.Concurrency
	}
	return DefaultBuildConcurrency
}

// TimeoutOrDefault returns build.timeout, or its default when unset.
func (b BuildConfig) TimeoutOrDefault() time.Duration {
	if b.Timeout > 0 {
		return b.Timeout
	}
	return DefaultBuildTimeout
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`     // Redis address (e.g., "localhost:6379")
	Password string `mapstructure:"password"` // Redis password (optional)
	DB       int    `mapstructure:"db"`       // Redis database number (default: 0)
}

type AnsibleConfig struct {
	Inventory  string `mapstructure:"inventory"`   // List of hosts or path to inventory file, e.g., "deployer_host,1.2.3.4,"
	PrivateKey string `mapstructure:"private_key"` // Path to the private key for SSH access
	User       string `mapstructure:"user"`        // SSH user
}

// ResourceLimits defines container resource constraints applied via Docker Compose.
// All three map to deploy.resources.limits (cpus, memory, pids) in Docker Compose.
type ResourceLimits struct {
	CPUs      string `mapstructure:"cpus" yaml:"cpus,omitempty"`             // e.g. "0.5", "1", "2"
	Memory    string `mapstructure:"memory" yaml:"memory,omitempty"`         // e.g. "256M", "512M", "1G"
	PidsLimit int    `mapstructure:"pids_limit" yaml:"pids_limit,omitempty"` // e.g. 256, 512
}

// MergeResourceLimits returns a ResourceLimits where non-zero fields in overrides
// take precedence over defaults.
func MergeResourceLimits(defaults, overrides ResourceLimits) ResourceLimits {
	result := defaults
	if overrides.CPUs != "" {
		result.CPUs = overrides.CPUs
	}
	if overrides.Memory != "" {
		result.Memory = overrides.Memory
	}
	if overrides.PidsLimit > 0 {
		result.PidsLimit = overrides.PidsLimit
	}
	return result
}

// DefaultMaxConcurrentAnsible is the default of max_concurrent_ansible
const DefaultMaxConcurrentAnsible = 5

// MaxConcurrentAnsibleLimit returns max_concurrent_ansible, or its default
// when unset or not positive.
func (ic InstancerConfig) MaxConcurrentAnsibleLimit() int {
	if ic.MaxConcurrentAnsible > 0 {
		return ic.MaxConcurrentAnsible
	}
	return DefaultMaxConcurrentAnsible
}

// Default range of randomized host ports, bounds included
const (
	DefaultRandomizedPortMin = 20000
	DefaultRandomizedPortMax = 60999
)

// RandomizedPortRange returns the range, bounds included, randomized host
// ports are picked from: randomized_port_min and randomized_port_max, each
// replaced by its default when unset.
func (ic InstancerConfig) RandomizedPortRange() (lo, hi int) {
	lo, hi = ic.RandomizedPortMin, ic.RandomizedPortMax
	if lo == 0 {
		lo = DefaultRandomizedPortMin
	}
	if hi == 0 {
		hi = DefaultRandomizedPortMax
	}
	return lo, hi
}

// Validate checks the instancer settings that have no safe fallback.
func (ic InstancerConfig) Validate() error {
	for i, src := range ic.Repositories.Sources {
		if src.URLPrefix == "" {
			return fmt.Errorf("repositories.sources[%d]: url_prefix is required", i)
		}
		if err := src.ValidatePrefix(); err != nil {
			return fmt.Errorf("repositories.sources[%d]: url_prefix %w", i, err)
		}
	}
	lo, hi := ic.RandomizedPortRange()
	if lo < 1 || hi > 65535 || lo > hi {
		return fmt.Errorf("invalid randomized port range %d-%d: randomized_port_min and randomized_port_max must be between 1 and 65535, the minimum not above the maximum (defaults: %d-%d)",
			lo, hi, DefaultRandomizedPortMin, DefaultRandomizedPortMax)
	}
	return nil
}

var (
	current *Config
	mu      sync.RWMutex
)

// Load replaces the current config with the one of the global viper.
func Load() error {
	return load(viper.GetViper())
}

func load(v *viper.Viper) error {
	zap.S().Infof("Loading config from %s", v.ConfigFileUsed())
	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return err
	}
	if err := cfg.Instancer.Validate(); err != nil {
		return fmt.Errorf("instancer: %w", err)
	}
	mu.Lock()
	current = cfg
	mu.Unlock()
	zap.S().Info("Config loaded successfully")
	return nil
}

func Get() *Config {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// Reload replaces the current config with the one v read from the changed
// file. The file may have been read while it was being written (cp
// truncates it first): a config without challenge_dir or ansible_dir, which
// every config has, is refused and the current one kept.
func Reload(v *viper.Viper) error {
	for _, key := range []string{"instancer.challenge_dir", "instancer.ansible_dir"} {
		if v.GetString(key) == "" {
			return fmt.Errorf("%s is missing: the file is empty or incomplete, the current config is kept", key)
		}
	}
	return load(v)
}

func LoadDefaults() error {
	mu.Lock()
	defer mu.Unlock()

	current = &Config{
		Auth: AuthConfig{
			JWTSecret: "defaultsecret",
		},
		Instancer: InstancerConfig{
			AnsibleDir: "/opt/ansible",
		},
	}
	return nil
}
