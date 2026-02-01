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

var cfgFile string

var (
	lastReload time.Time
	reloadMu   sync.Mutex
)

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "An error occurred: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "config file path")
	rootCmd.AddCommand(serveCmd)
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

	viper.SetDefault("instancer.deployment_ttl", "1h")
	viper.SetDefault("instancer.deployment_ttl_extension", "30m")
	viper.SetDefault("instancer.deployment_max_extensions", 4)
	viper.SetDefault("instancer.deployment_extension_window", "30m")

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

func handleConfigChange(filename string) {
	reloadMu.Lock()
	defer reloadMu.Unlock()

	if time.Since(lastReload) < 500*time.Millisecond {
		return // ignore duplicate events
	}
	lastReload = time.Now()
	zap.S().Infof("Config file %s changed", filename)

	if err := config.Reload(); err != nil {
		zap.S().Errorf("Error reloading config: %v", err)
		return
	}
	zap.S().Info("Config reloaded successfully")
}
