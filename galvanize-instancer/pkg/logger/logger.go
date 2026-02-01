package logger

import (
	"sync"

	"go.uber.org/zap"
)

var log *zap.SugaredLogger
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

//// Info logs at info level
//func Info(args ...interface{}) {
//	log.Info(args...)
//}
//
//// Infof logs at info level with formatting
//func Infof(template string, args ...interface{}) {
//	log.Infof(template, args...)
//}
//
//// Debug logs at debug level
//func Debug(args ...interface{}) {
//	log.Debug(args...)
//}
//
//// Debugf logs at debug level with formatting
//func Debugf(template string, args ...interface{}) {
//	log.Debugf(template, args...)
//}
//
//// Error logs at error level
//func Error(args ...interface{}) {
//	log.Error(args...)
//}
//
//// Errorf logs at error level with formatting
//func Errorf(template string, args ...interface{}) {
//	log.Errorf(template, args...)
//}
//
//// Warn logs at warn level
//func Warn(args ...interface{}) {
//	log.Warn(args...)
//}
//
//// Warnf logs at warn level with formatting
//func Warnf(template string, args ...interface{}) {
//	log.Warnf(template, args...)
//}
//
//// Fatal logs at fatal level and exits
//func Fatal(args ...interface{}) {
//	log.Fatal(args...)
//}
//
//// Fatalf logs at fatal level with formatting and exits
//func Fatalf(template string, args ...interface{}) {
//	log.Fatalf(template, args...)
//}
//
//// Panic logs at panic level and panics
//func Panic(args ...interface{}) {
//	log.Panic(args...)
//}
//
//// Panicf logs at panic level with formatting and panics
//func Panicf(template string, args ...interface{}) {
//	log.Panicf(template, args...)
//}
//
//// Sync flushes buffered logs
//func Sync() error {
//	return log.Sync()
//}
