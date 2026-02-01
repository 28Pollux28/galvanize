package main

import (
	"os"

	"github.com/28Pollux28/galvanize/cmd"
	"github.com/28Pollux28/galvanize/pkg/logger"
	"go.uber.org/zap"
)

func main() {
	dev := os.Getenv("DEVELOPMENT")
	if dev == "true" {
		logger.Init(true)
	} else {
		logger.Init(false)
	}
	defer zap.L().Sync()
	cmd.Execute()
}
