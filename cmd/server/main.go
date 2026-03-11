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
	derivedKey, err := crypto.DeriveKey(cfg.MasterKey, cfg.SecretPepper)
	if err != nil {
		log.Fatal(err)
	}
	var legacyKey []byte
	if cfg.LegacyMasterKey != "" || cfg.LegacyPepper != "" {
		legacyKey, err = crypto.DeriveKey(cfg.LegacyMasterKey, cfg.LegacyPepper)
		if err != nil {
			log.Fatal(err)
		}
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
	r.GET("/healthz", func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		status := http.StatusOK
		resp := gin.H{"ok": true}

		if sqlDB, err := db.DB(); err != nil {
			status = http.StatusServiceUnavailable
			resp["postgres"] = "unavailable"
		} else if err := sqlDB.PingContext(ctx); err != nil {
			status = http.StatusServiceUnavailable
			resp["postgres"] = "unavailable"
		} else {
			resp["postgres"] = "ok"
		}

		if redisStore == nil {
			status = http.StatusServiceUnavailable
			resp["redis"] = "unavailable"
		} else if err := redisStore.Client().Ping(ctx).Err(); err != nil {
			status = http.StatusServiceUnavailable
			resp["redis"] = "unavailable"
		} else {
			resp["redis"] = "ok"
		}

		if status != http.StatusOK {
			resp["ok"] = false
		}
		c.JSON(status, resp)
	})
	api.RegisterRoutes(r, db, derivedKey, legacyKey, cfg.APIKey, redisStore, cfg.PasswordTokenTTL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if cfg.TelegramBotToken != "" {
		if _, err := bot.StartTelegramBot(ctx, cfg, db, redisStore, derivedKey, legacyKey); err != nil {
			log.Fatal(err)
		}
		log.Println("Telegram bot started")
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
