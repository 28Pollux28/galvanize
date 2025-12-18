package config

import (
	"log"
	"sync"

	"github.com/spf13/viper"
)

type Config struct {
	Auth     AuthConfig     `mapstructure:"auth"`
	Deployer DeployerConfig `mapstructure:"deployer"`
}

type AuthConfig struct {
	JWTSecret string `mapstructure:"jwt_secret"`
}

type DeployerConfig struct {
	Ansible      AnsibleConfig `mapstructure:"ansible"`       // Ansible specific configuration
	AnsibleDir   string        `mapstructure:"ansible_dir"`   // Directory where Ansible playbooks are located
	ChallengeDir string        `mapstructure:"challenge_dir"` // Directory where challenge playbooks are stored
	DeployerHost string        `mapstructure:"deployer_host"` // IP or Hostname of the node where challenges will be deployed
	DBPath       string        `mapstructure:"db_path"`
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
	log.Printf("Loading config from %s", viper.ConfigFileUsed())
	mu.Lock()
	defer mu.Unlock()

	cfg := &Config{}
	if err := viper.Unmarshal(cfg); err != nil {
		return err
	}
	log.Println("Config loaded successfully")
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
		Deployer: DeployerConfig{
			AnsibleDir: "/opt/ansible",
		},
	}
	return nil
}
