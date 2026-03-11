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
		runBackup(ctx, bot, cfg, db, receivers)
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

func runBackup(ctx context.Context, bot *tgbotapi.BotAPI, cfg *config.Config, db *gorm.DB, receivers []int64) {
	backupCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	ts := time.Now().Format("20060102_150405")
	tmpDir := os.TempDir()
	rawFile := filepath.Join(tmpDir, fmt.Sprintf("vaultbot_%s.sql", ts))
	gzFile := rawFile + ".gz"
	encFile := gzFile + ".enc"

	cleanup := func() {
		_ = os.Remove(rawFile)
		_ = os.Remove(gzFile)
		_ = os.Remove(encFile)
	}

	if err := execCmd(backupCtx, "pg_dump", []string{"--dbname", cfg.DBURL, "--file", rawFile}, nil); err != nil {
		log.Printf("backup pg_dump failed err=%v", err)
		cleanup()
		return
	}
	if err := execCmd(backupCtx, "gzip", []string{"-f", rawFile}, nil); err != nil {
		log.Printf("backup gzip failed err=%v", err)
		cleanup()
		return
	}
	env := append(os.Environ(), "BACKUP_PASSWORD="+cfg.BackupPassword)
	if err := execCmd(backupCtx, "openssl", []string{"enc", "-aes-256-gcm", "-salt", "-pbkdf2", "-iter", "200000", "-pass", "env:BACKUP_PASSWORD", "-in", gzFile, "-out", encFile}, env); err != nil {
		log.Printf("backup encrypt failed err=%v", err)
		cleanup()
		return
	}
	_ = os.Remove(gzFile)

	for _, id := range receivers {
		doc := tgbotapi.NewDocument(id, tgbotapi.FilePath(encFile))
		doc.Caption = fmt.Sprintf("VaultBot 备份 %s", ts)
		if _, err := bot.Send(doc); err != nil {
			log.Printf("backup send failed receiver=%d err=%v", id, err)
		}
	}
	if db != nil {
		entry := &model.AuditLog{
			UserID:   "system",
			Action:   "backup",
			Platform: "database",
		}
		if err := db.Create(entry).Error; err != nil {
			log.Printf("backup audit failed err=%v", err)
		}
	}
	cleanup()
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
