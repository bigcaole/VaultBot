package bot

import (
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
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
	"golang.org/x/crypto/argon2"
	"gorm.io/gorm"
)

const backupMagic = "VBK2"

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

	if warn := WarnIfPgDumpMismatch(ctx, cfg.DBURL); warn != "" {
		log.Printf("backup warning: %s", warn)
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
	if warn := WarnIfPgDumpMismatch(ctx, cfg.DBURL); warn != "" {
		log.Printf("backup warning: %s", warn)
		return fmt.Errorf("%s", warn)
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
	if warn := WarnIfPgDumpMismatch(ctx, cfg.DBURL); warn != "" {
		return fmt.Errorf("%s", warn)
	}
	_, cleanup, err := createBackupFile(ctx, cfg, true)
	if err != nil {
		return err
	}
	cleanup()
	return nil
}

// RestoreBackup restores an encrypted backup file into the database.
func RestoreBackup(ctx context.Context, cfg *config.Config, encPath string) error {
	if strings.TrimSpace(cfg.DBURL) == "" {
		return fmt.Errorf("DB_URL is required for restore")
	}
	if strings.TrimSpace(cfg.BackupPassword) == "" {
		return fmt.Errorf("BACKUP_PASSWORD not configured")
	}
	if warn := WarnIfPgDumpMismatch(ctx, cfg.DBURL); warn != "" {
		return fmt.Errorf("%s", warn)
	}
	backupCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()

	tmpDir := os.TempDir()
	sqlPath := filepath.Join(tmpDir, fmt.Sprintf("vaultbot_restore_%d.sql", time.Now().UnixNano()))
	if err := decryptBackupToSQL(encPath, sqlPath, cfg.BackupPassword); err != nil {
		_ = os.Remove(sqlPath)
		return err
	}
	defer func() { _ = os.Remove(sqlPath) }()

	if err := execCmd(backupCtx, "psql", []string{"-d", cfg.DBURL, "-f", sqlPath}, nil); err != nil {
		return err
	}
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
	if err := encryptFileWithPassword(gzFile, encFile, cfg.BackupPassword); err != nil {
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

// WarnIfPgDumpMismatch checks pg_dump/server major version mismatch.
func WarnIfPgDumpMismatch(ctx context.Context, dbURL string) string {
	if strings.TrimSpace(dbURL) == "" {
		return ""
	}
	serverMajor, err := fetchServerMajor(ctx, dbURL)
	if err != nil {
		return fmt.Sprintf("无法获取数据库版本: %v", err)
	}
	dumpMajor, err := fetchPgDumpMajor(ctx)
	if err != nil {
		return fmt.Sprintf("无法获取 pg_dump 版本: %v", err)
	}
	if dumpMajor < serverMajor {
		return fmt.Sprintf("pg_dump 版本过低（pg_dump=%d, server=%d），请将 PG_MAJOR 设置为 %d+ 并重建镜像", dumpMajor, serverMajor, serverMajor)
	}
	return ""
}

func fetchServerMajor(ctx context.Context, dbURL string) (int, error) {
	backupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := execCmdOutput(backupCtx, "psql", []string{"-d", dbURL, "-At", "-c", "SHOW server_version_num;"}, nil)
	if err != nil {
		return 0, err
	}
	val := strings.TrimSpace(out)
	if len(val) < 2 {
		return 0, fmt.Errorf("unexpected server_version_num: %s", val)
	}
	num, err := strconv.Atoi(val)
	if err != nil {
		return 0, err
	}
	return num / 10000, nil
}

func fetchPgDumpMajor(ctx context.Context) (int, error) {
	backupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := execCmdOutput(backupCtx, "pg_dump", []string{"--version"}, nil)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(out)
	if len(fields) < 3 {
		return 0, fmt.Errorf("unexpected pg_dump version: %s", strings.TrimSpace(out))
	}
	ver := fields[2]
	parts := strings.SplitN(ver, ".", 2)
	if len(parts) < 1 {
		return 0, fmt.Errorf("unexpected pg_dump version: %s", ver)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	return major, nil
}

func execCmdOutput(ctx context.Context, name string, args []string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func encryptFileWithPassword(inPath string, outPath string, password string) error {
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("BACKUP_PASSWORD not configured")
	}
	in, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
	}()

	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	iv := make([]byte, 16)
	if _, err := rand.Read(iv); err != nil {
		return err
	}

	passBytes := []byte(password)
	keyMaterial := argon2.IDKey(passBytes, salt, 3, 64*1024, 4, 64)
	zeroize(passBytes)
	encKey := keyMaterial[:32]
	macKey := keyMaterial[32:]

	block, err := aes.NewCipher(encKey)
	if err != nil {
		zeroize(keyMaterial)
		return err
	}
	stream := cipher.NewCTR(block, iv)
	h := hmac.New(sha256.New, macKey)

	if _, err := out.Write([]byte(backupMagic)); err != nil {
		zeroize(keyMaterial)
		return err
	}
	if _, err := out.Write(salt); err != nil {
		zeroize(keyMaterial)
		return err
	}
	if _, err := out.Write(iv); err != nil {
		zeroize(keyMaterial)
		return err
	}
	if _, err := h.Write([]byte(backupMagic)); err != nil {
		zeroize(keyMaterial)
		return err
	}
	if _, err := h.Write(salt); err != nil {
		zeroize(keyMaterial)
		return err
	}
	if _, err := h.Write(iv); err != nil {
		zeroize(keyMaterial)
		return err
	}

	buf := make([]byte, 32*1024)
	for {
		n, readErr := in.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			enc := make([]byte, n)
			stream.XORKeyStream(enc, chunk)
			if _, err := out.Write(enc); err != nil {
				zeroize(keyMaterial)
				return err
			}
			if _, err := h.Write(enc); err != nil {
				zeroize(keyMaterial)
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			zeroize(keyMaterial)
			return readErr
		}
	}

	tag := h.Sum(nil)
	if _, err := out.Write(tag); err != nil {
		zeroize(keyMaterial)
		return err
	}
	zeroize(keyMaterial)
	return nil
}

func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func decryptBackupToSQL(encPath string, sqlPath string, password string) error {
	if strings.TrimSpace(password) == "" {
		return fmt.Errorf("BACKUP_PASSWORD not configured")
	}
	gzPath := sqlPath + ".gz"
	if err := decryptFileWithPassword(encPath, gzPath, password); err != nil {
		return err
	}
	defer func() { _ = os.Remove(gzPath) }()

	gzFile, err := os.Open(gzPath)
	if err != nil {
		return err
	}
	defer gzFile.Close()

	gzr, err := gzip.NewReader(gzFile)
	if err != nil {
		return err
	}
	defer gzr.Close()

	out, err := os.Create(sqlPath)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, gzr); err != nil {
		return err
	}
	return nil
}

func decryptFileWithPassword(inPath string, outPath string, password string) error {
	in, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 4+16+16+32 {
		return fmt.Errorf("invalid backup file")
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(in, header); err != nil {
		return err
	}
	if string(header) != backupMagic {
		return fmt.Errorf("invalid backup header")
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(in, salt); err != nil {
		return err
	}
	iv := make([]byte, 16)
	if _, err := io.ReadFull(in, iv); err != nil {
		return err
	}

	passBytes := []byte(password)
	keyMaterial := argon2.IDKey(passBytes, salt, 3, 64*1024, 4, 64)
	zeroize(passBytes)
	encKey := keyMaterial[:32]
	macKey := keyMaterial[32:]
	defer zeroize(keyMaterial)

	block, err := aes.NewCipher(encKey)
	if err != nil {
		return err
	}
	stream := cipher.NewCTR(block, iv)
	h := hmac.New(sha256.New, macKey)

	if _, err := h.Write([]byte(backupMagic)); err != nil {
		return err
	}
	if _, err := h.Write(salt); err != nil {
		return err
	}
	if _, err := h.Write(iv); err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()

	cipherLen := info.Size() - (4 + 16 + 16 + 32)
	if cipherLen < 0 {
		return fmt.Errorf("invalid backup file size")
	}
	lr := io.LimitReader(in, cipherLen)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := lr.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, err := h.Write(chunk); err != nil {
				return err
			}
			dec := make([]byte, n)
			stream.XORKeyStream(dec, chunk)
			if _, err := out.Write(dec); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}

	tag := make([]byte, 32)
	if _, err := io.ReadFull(in, tag); err != nil {
		return err
	}
	if !hmac.Equal(tag, h.Sum(nil)) {
		return fmt.Errorf("backup integrity check failed")
	}
	return nil
}
