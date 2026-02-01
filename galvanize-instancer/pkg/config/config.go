package config

import (
	"sync"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
)

type Config struct {
	Auth      AuthConfig      `mapstructure:"auth"`
	Instancer InstancerConfig `mapstructure:"instancer"`
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
	DBPath                    string                 `mapstructure:"db_path"`                               // Path to the database file
	DeploymentTTL             time.Duration          `mapstructure:"deployment_ttl,omitempty"`              // Time-to-live for deployments
	DeploymentTTLExtension    time.Duration          `mapstructure:"deployment_ttl_extension,omitempty"`    // Duration to extend deployment TTL
	DeploymentMaxExtensions   int                    `mapstructure:"deployment_max_extensions,omitempty"`   // Maximum number of TTL extensions allowed
	DeploymentExtensionWindow time.Duration          `mapstructure:"deployment_extension_window,omitempty"` // Time window before expiration when extension is allowed
}

type AnsibleConfig struct {
	Inventory  string `mapstructure:"inventory"`   // List of hosts or path to inventory file, e.g., "deployer_host,1.2.3.4,"
	PrivateKey string `mapstructure:"private_key"` // Path to the private key for SSH access
	User       string `mapstructure:"user"`        // SSH user
}

var (
	current *Config
	mu      sync.RWMutex
)

func Load() error {
	zap.S().Infof("Loading config from %s", viper.ConfigFileUsed())
	mu.Lock()
	defer mu.Unlock()

	cfg := &Config{}
	if err := viper.Unmarshal(cfg); err != nil {
		return err
	}
	zap.S().Info("Config loaded successfully")
	current = cfg
	return nil
}

func Get() *Config {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

func Reload() error {
	return Load()
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
