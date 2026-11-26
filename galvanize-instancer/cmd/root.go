package cmd

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/28Pollux28/galvanize/pkg/config"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.uber.org/zap"
)

var rootCmd = &cobra.Command{
	Use:   "galvanize",
	Short: "Galvanize Instancer",
	Long:  "Galvanize Instancer is an instancer using Ansible to deploy CTFs challenges. CTFd connects to it to deploy challenges using the Zync plugin",
}

func SetVersion(version string) {
	rootCmd.Version = version
}

var cfgFile string

// The changes of the config file are applied once it has not changed for
// reloadDelay: a copy writes it in several steps, and only the last one
// must be read.
const reloadDelay = 500 * time.Millisecond

var (
	reloadTimer *time.Timer
	reloadMu    sync.Mutex
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "An error occurred: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file path")
	cobra.OnInitialize(initConfig)
}

func initConfig() {
	if cfgFile == "" {
		zap.S().Error("No config file specified")
		os.Exit(1)
		return
	}

	viper.SetConfigFile(cfgFile)
	viper.SetConfigType("yaml")

	setConfigDefaults(viper.GetViper())

	if err := viper.ReadInConfig(); err != nil {
		zap.S().Fatalf("Error reading config file: %v", err)
	}

	if err := config.Load(); err != nil {
		zap.S().Fatalf("Error loading config: %v", err)
	}

	viper.WatchConfig()
	viper.OnConfigChange(func(e fsnotify.Event) {
		handleConfigChange(e.Name)
	})
}

// setConfigDefaults sets the defaults of the settings a config may omit.
func setConfigDefaults(v *viper.Viper) {
	v.SetDefault("instancer.deployment_ttl", "1h")
	v.SetDefault("instancer.deployment_ttl_extension", "30m")
	v.SetDefault("instancer.deployment_max_extensions", 3)
	v.SetDefault("instancer.deployment_extension_window", "30m")
}

func handleConfigChange(filename string) {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	if reloadTimer != nil {
		reloadTimer.Stop()
	}
	reloadTimer = time.AfterFunc(reloadDelay, func() { reloadConfig(filename) })
}

// reloadConfig reads the config file again, in a viper of its own: the
// global one is read by viper's watcher, possibly in the middle of a write.
func reloadConfig(filename string) {
	zap.S().Infof("Config file %s changed", filename)
	v := viper.New()
	v.SetConfigFile(cfgFile)
	v.SetConfigType("yaml")
	setConfigDefaults(v)
	if err := v.ReadInConfig(); err != nil {
		zap.S().Errorf("Error reading the changed config, the current one is kept: %v", err)
		return
	}
	if err := config.Reload(v); err != nil {
		zap.S().Errorf("Error reloading config: %v", err)
		return
	}
	zap.S().Info("Config reloaded successfully")
}
