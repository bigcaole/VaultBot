package model

import (
	"context"
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Init initializes the database connection and runs migrations with retry.
func Init(dbURL string, retries int, delay time.Duration) (*gorm.DB, error) {
	if dbURL == "" {
		return nil, fmt.Errorf("DB_URL is required")
	}
	if retries < 0 {
		retries = 0
	}
	if delay <= 0 {
		delay = 2 * time.Second
	}

	var lastErr error
	for i := 0; i <= retries; i++ {
		db, err := gorm.Open(postgres.Open(dbURL), &gorm.Config{})
		if err == nil {
			sqlDB, err := db.DB()
			if err == nil {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				pingErr := sqlDB.PingContext(ctx)
				cancel()
				if pingErr == nil {
					if err := db.AutoMigrate(&Account{}); err != nil {
						return nil, err
					}
					return db, nil
				}
				lastErr = pingErr
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		time.Sleep(delay)
	}
	return nil, lastErr
}
