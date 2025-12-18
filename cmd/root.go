package cmd

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/28Pollux28/poly-instancer/pkg/config"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var rootCmd = &cobra.Command{
	Use:   "poly-instancer",
	Short: "Poly Instancer",
	Long:  "Poly Instancer is an instancer using Ansible to deploy CTFs challenges for the PolyPWN CTF. CTFd connects to it to deploy challenges",
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
		log.Println("No config file specified, using defaults")
		if err := config.LoadDefaults(); err != nil {
			log.Fatalf("Error loading default config: %v", err)
		}
		return
	}

	viper.SetConfigFile(cfgFile)
	viper.SetConfigType("yaml")

	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	if err := config.Load(); err != nil {
		log.Fatalf("Error loading config: %v", err)
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
	log.Printf("Config file %s changed\n", filename)

	if err := config.Reload(); err != nil {
		log.Printf("Error reloading config: %v", err)
		return
	}
	log.Println("Config reloaded successfully")
}
