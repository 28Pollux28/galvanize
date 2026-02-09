package utils

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/28Pollux28/galvanize/pkg/config"
	"go.uber.org/zap"
)

func RegisterSSHHosts(cfg *config.Config) error {
	// Parse SSH hosts from config and register them using ssh package
	hosts := strings.Split(strings.Trim(cfg.Instancer.Ansible.Inventory, ","), ",")
	home, _ := os.UserHomeDir()
	knownHostsPath := filepath.Join(home, ".ssh/known_hosts")

	// Collect hosts that need registration
	var hostsToRegister []string
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
		hostsToRegister = append(hostsToRegister, host)
	}

	// If no hosts to register, return early
	if len(hostsToRegister) == 0 {
		return nil
	}

	// Open the file once for all writes
	f, err := os.OpenFile(knownHostsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()

	// Register all hosts that need it
	for _, host := range hostsToRegister {
		zap.S().Infof("Registering SSH host %s", host)
		cmd := exec.Command("ssh-keyscan", "-H", host)
		output, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("keyscan %s: %w", host, err)
		}

		if _, err = f.Write(output); err != nil {
			return err
		}
	}

	return nil
}
