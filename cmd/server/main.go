package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"vaultbot/internal/api"
	"vaultbot/internal/bot"
	"vaultbot/internal/config"
	"vaultbot/internal/crypto"
	"vaultbot/internal/model"
	"vaultbot/internal/store"

	"github.com/gin-gonic/gin"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	masterKey, err := crypto.LoadMasterKey(cfg.MasterKey)
	if err != nil {
		log.Fatal(err)
	}

	db, err := model.Init(cfg.DBURL, cfg.DBConnectRetries, cfg.DBConnectDelay)
	if err != nil {
		log.Fatal(err)
	}

	redisStore, err := store.NewRedisStore(cfg.RedisURL)
	if err != nil {
		log.Fatal(err)
	}

	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/healthz", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	api.RegisterRoutes(r, db, masterKey, cfg.APIKey, redisStore, cfg.PasswordTokenTTL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.TelegramBotToken != "" {
		if _, err := bot.StartTelegramBot(ctx, cfg, db, redisStore, masterKey); err != nil {
			log.Fatal(err)
		}
		log.Println("Telegram bot started")
	}

	if cfg.FeishuAppID != "" && cfg.FeishuAppSecret != "" {
		feishuBot, err := bot.NewFeishuBot(cfg, db, redisStore, masterKey)
		if err != nil {
			log.Fatal(err)
		}
		r.POST("/lark/events", feishuBot.HandleEvent)
		log.Println("Feishu bot handler registered")
	}

	server := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: r,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)

	if redisStore != nil {
		_ = redisStore.Close()
	}
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}
