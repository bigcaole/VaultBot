package api

import (
	"net/http"
	"time"

	"vaultbot/internal/store"

	"github.com/gin-gonic/gin"
)

// RateLimitMiddleware applies a simple Redis-based rate limit.
func RateLimitMiddleware(store *store.RedisStore, limit int64, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil {
			c.Next()
			return
		}
		key := "rate:api:" + c.ClientIP()
		allowed, err := store.Allow(c.Request.Context(), key, limit, window)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "rate limit failed"})
			return
		}
		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
			return
		}
		c.Next()
	}
}
