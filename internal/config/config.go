package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime configuration loaded from environment variables.
type Config struct {
	DBURL             string
	RedisURL          string
	MasterKey         string
	SecretPepper      string
	LegacyMasterKey   string
	LegacyPepper      string
	UnlockPIN         string
	BackupPassword    string
	TelegramBotToken  string
	APIKey            string
	AllowedUserIDs    map[string]struct{}
	BackupReceiverIDs map[string]struct{}
	HTTPAddr          string
	DeleteAfter       time.Duration
	DBConnectRetries  int
	DBConnectDelay    time.Duration
	AllowGroupChat    bool
	PasswordTokenTTL  time.Duration
	UnlockTTL         time.Duration
}

// Load reads configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{}

	cfg.DBURL = strings.TrimSpace(os.Getenv("DB_URL"))
	cfg.RedisURL = strings.TrimSpace(os.Getenv("REDIS_URL"))
	cfg.MasterKey = strings.TrimSpace(os.Getenv("MASTER_KEY"))
	cfg.SecretPepper = strings.TrimSpace(os.Getenv("SECRET_PEPPER"))
	cfg.LegacyMasterKey = strings.TrimSpace(os.Getenv("LEGACY_MASTER_KEY"))
	cfg.LegacyPepper = strings.TrimSpace(os.Getenv("LEGACY_SECRET_PEPPER"))
	cfg.UnlockPIN = strings.TrimSpace(os.Getenv("UNLOCK_PIN"))
	cfg.BackupPassword = strings.TrimSpace(os.Getenv("BACKUP_PASSWORD"))
	cfg.TelegramBotToken = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	cfg.APIKey = strings.TrimSpace(os.Getenv("API_KEY"))
	cfg.HTTPAddr = strings.TrimSpace(os.Getenv("HTTP_ADDR"))
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":8080"
	}

	deleteAfterStr := strings.TrimSpace(os.Getenv("DELETE_AFTER_SECONDS"))
	if deleteAfterStr == "" {
		cfg.DeleteAfter = 60 * time.Second
	} else {
		seconds, err := strconv.Atoi(deleteAfterStr)
		if err != nil || seconds <= 0 {
			return nil, errors.New("invalid DELETE_AFTER_SECONDS")
		}
		cfg.DeleteAfter = time.Duration(seconds) * time.Second
	}

	cfg.AllowedUserIDs = parseAllowedIDs(os.Getenv("ALLOWED_USER_IDS"))
	cfg.BackupReceiverIDs = parseAllowedIDs(os.Getenv("BACKUP_RECEIVER_IDS"))

	cfg.DBConnectRetries = 10
	if retriesStr := strings.TrimSpace(os.Getenv("DB_CONNECT_RETRIES")); retriesStr != "" {
		if retries, err := strconv.Atoi(retriesStr); err == nil {
			cfg.DBConnectRetries = retries
		}
	}
	cfg.DBConnectDelay = 3 * time.Second
	if delayStr := strings.TrimSpace(os.Getenv("DB_CONNECT_DELAY_SECONDS")); delayStr != "" {
		if seconds, err := strconv.Atoi(delayStr); err == nil && seconds > 0 {
			cfg.DBConnectDelay = time.Duration(seconds) * time.Second
		}
	}
	cfg.AllowGroupChat = strings.EqualFold(strings.TrimSpace(os.Getenv("ALLOW_GROUP_CHAT")), "true")
	cfg.PasswordTokenTTL = 60 * time.Second
	if ttlStr := strings.TrimSpace(os.Getenv("PASSWORD_TOKEN_TTL_SECONDS")); ttlStr != "" {
		if seconds, err := strconv.Atoi(ttlStr); err == nil && seconds > 0 {
			cfg.PasswordTokenTTL = time.Duration(seconds) * time.Second
		}
	}
	cfg.UnlockTTL = 15 * time.Minute
	return cfg, nil
}

func parseAllowedIDs(raw string) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ids[part] = struct{}{}
	}
	return ids
}
