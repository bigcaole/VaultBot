package bot

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"vaultbot/internal/config"
	"vaultbot/internal/model"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/robfig/cron/v3"
	"gorm.io/gorm"
)

// StartBackupScheduler starts a daily backup job at 22:00 local time.
func StartBackupScheduler(ctx context.Context, bot *tgbotapi.BotAPI, cfg *config.Config, db *gorm.DB) (*cron.Cron, error) {
	receivers := parseReceiverIDs(cfg.BackupReceiverIDs)
	if len(receivers) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(cfg.BackupPassword) == "" {
		return nil, fmt.Errorf("BACKUP_PASSWORD is required when BACKUP_RECEIVER_IDS is set")
	}
	if strings.TrimSpace(cfg.DBURL) == "" {
		return nil, fmt.Errorf("DB_URL is required for backup")
	}

	c := cron.New(cron.WithLocation(time.Local))
	_, err := c.AddFunc("0 22 * * *", func() {
		if err := runBackup(ctx, bot, cfg, db, receivers, "system", false); err != nil {
			log.Printf("scheduled backup failed err=%v", err)
		}
	})
	if err != nil {
		return nil, err
	}
	c.Start()
	go func() {
		<-ctx.Done()
		c.Stop()
	}()
	return c, nil
}

// RunBackupNow triggers a manual backup.
func RunBackupNow(ctx context.Context, bot *tgbotapi.BotAPI, cfg *config.Config, db *gorm.DB, actor string) error {
	receivers := parseReceiverIDs(cfg.BackupReceiverIDs)
	if len(receivers) == 0 {
		return fmt.Errorf("BACKUP_RECEIVER_IDS not configured")
	}
	if strings.TrimSpace(cfg.BackupPassword) == "" {
		return fmt.Errorf("BACKUP_PASSWORD not configured")
	}
	return runBackup(ctx, bot, cfg, db, receivers, actor, false)
}

// RunBackupTest runs a dry backup test without sending files.
func RunBackupTest(ctx context.Context, cfg *config.Config) error {
	if strings.TrimSpace(cfg.DBURL) == "" {
		return fmt.Errorf("DB_URL is required for backup test")
	}
	if strings.TrimSpace(cfg.BackupPassword) == "" {
		return fmt.Errorf("BACKUP_PASSWORD not configured")
	}
	_, cleanup, err := createBackupFile(ctx, cfg, true)
	if err != nil {
		return err
	}
	cleanup()
	return nil
}

func runBackup(ctx context.Context, bot *tgbotapi.BotAPI, cfg *config.Config, db *gorm.DB, receivers []int64, actor string, schemaOnly bool) error {
	encFile, cleanup, err := createBackupFile(ctx, cfg, schemaOnly)
	if err != nil {
		return err
	}
	defer cleanup()

	var sendErrs []string
	for _, id := range receivers {
		doc := tgbotapi.NewDocument(id, tgbotapi.FilePath(encFile))
		doc.Caption = fmt.Sprintf("VaultBot 备份 %s", time.Now().Format("20060102_150405"))
		if _, err := bot.Send(doc); err != nil {
			sendErrs = append(sendErrs, fmt.Sprintf("receiver=%d err=%v", id, err))
		}
	}
	if db != nil {
		entry := &model.AuditLog{
			UserID:   actor,
			Action:   "backup",
			Platform: "database",
		}
		if err := db.Create(entry).Error; err != nil {
			log.Printf("backup audit failed err=%v", err)
		}
	}
	if len(sendErrs) > 0 {
		return fmt.Errorf("backup send failed: %s", strings.Join(sendErrs, "; "))
	}
	return nil
}

func createBackupFile(ctx context.Context, cfg *config.Config, schemaOnly bool) (string, func(), error) {
	backupCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	ts := time.Now().Format("20060102_150405")
	tmpDir := os.TempDir()
	rawFile := filepath.Join(tmpDir, fmt.Sprintf("vaultbot_%s.sql", ts))
	gzFile := rawFile + ".gz"
	encFile := gzFile + ".enc"

	cleanup := func() {
		cancel()
		_ = os.Remove(rawFile)
		_ = os.Remove(gzFile)
		_ = os.Remove(encFile)
	}

	args := []string{"--dbname", cfg.DBURL, "--file", rawFile}
	if schemaOnly {
		args = append(args, "--schema-only")
	}
	if err := execCmd(backupCtx, "pg_dump", args, nil); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := execCmd(backupCtx, "gzip", []string{"-f", rawFile}, nil); err != nil {
		cleanup()
		return "", func() {}, err
	}
	env := append(os.Environ(), "BACKUP_PASSWORD="+cfg.BackupPassword)
	if err := execCmd(backupCtx, "openssl", []string{"enc", "-aes-256-gcm", "-salt", "-pbkdf2", "-iter", "200000", "-pass", "env:BACKUP_PASSWORD", "-in", gzFile, "-out", encFile}, env); err != nil {
		cleanup()
		return "", func() {}, err
	}
	_ = os.Remove(gzFile)
	return encFile, cleanup, nil
}

func execCmd(ctx context.Context, name string, args []string, env []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func parseReceiverIDs(ids map[string]struct{}) []int64 {
	list := make([]int64, 0, len(ids))
	for id := range ids {
		parsed, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue
		}
		list = append(list, parsed)
	}
	return list
}
