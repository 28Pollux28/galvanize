package pkg

import (
	"fmt"
	"log"
	"time"

	"github.com/28Pollux28/galvanize/pkg/models"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func InitDB(dbPath string) (*gorm.DB, error) {
	// Add busy_timeout and WAL mode for better concurrency
	dsn := fmt.Sprintf("%s?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL", dbPath)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.New(log.Default(), logger.Config{
			LogLevel:      logger.Warn,
			SlowThreshold: 200 * time.Millisecond,
		},
		),
	})
	if err != nil {
		return nil, err
	}

	// Limit connection pool to 1 to avoid SQLite concurrency issues
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)

	return db, db.AutoMigrate(models.Deployment{})
}
