package logger

import (
	"sync"

	"go.uber.org/zap"
)

var once sync.Once

func Init(dev bool) {
	once.Do(func() {
		var err error
		var zapLogger *zap.Logger
		if dev {
			zapLogger, err = zap.NewDevelopment()
			if err != nil {
				panic(err)
			}
			zapLogger = zapLogger.Named("GalvanizeDev")
			zapLogger.Sugar().Info("Logger initialized in development mode")
		} else {
			zapLogger, err = zap.NewProduction()
			if err != nil {
				panic(err)
			}
			zapLogger = zapLogger.Named("Galvanize")
			zapLogger.Sugar().Info("Logger initialized in production mode")
		}
		zap.ReplaceGlobals(zapLogger)
	})
}
