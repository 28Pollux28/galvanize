package pkg

import (
	"log"
	"time"

	"github.com/28Pollux28/galvanize/pkg/models"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func InitDB(dbPath string) (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		Logger: logger.New(log.Default(), logger.Config{
			LogLevel:      logger.Warn,
			SlowThreshold: 200 * time.Millisecond,
		},
		),
	})
	if err != nil {
		return nil, err
	}

	return db, db.AutoMigrate(models.Deployment{})
}
