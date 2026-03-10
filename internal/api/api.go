package api

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"vaultbot/internal/crypto"
	"vaultbot/internal/model"
	"vaultbot/internal/store"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// RegisterRoutes registers API routes with API key auth.
func RegisterRoutes(r *gin.Engine, db *gorm.DB, masterKey []byte, apiKey string, store *store.RedisStore, pwdTokenTTL time.Duration) {
	api := r.Group("/api", apiKeyMiddleware(apiKey), RateLimitMiddleware(store, 120, time.Minute))
	api.GET("/accounts", listAccounts(db))
	api.POST("/accounts", createAccount(db, masterKey))
	api.GET("/accounts/:id", getAccount(db, masterKey, store))
	api.PUT("/accounts/:id", updateAccount(db, masterKey))
	api.DELETE("/accounts/:id", deleteAccount(db))
	api.POST("/password-token", createPasswordToken(store, pwdTokenTTL))
}

type accountInput struct {
	Platform string `json:"platform"`
	Category string `json:"category"`
	Username string `json:"username"`
	Password string `json:"password"`
	Email    string `json:"email"`
	Phone    string `json:"phone"`
	Notes    string `json:"notes"`
}

type accountResponse struct {
	ID        string `json:"id"`
	Platform  string `json:"platform"`
	Category  string `json:"category"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	Notes     string `json:"notes"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func toAccountResponse(a model.Account) accountResponse {
	return accountResponse{
		ID:        a.ID.String(),
		Platform:  a.Platform,
		Category:  a.Category,
		Username:  a.Username,
		Email:     a.Email,
		Phone:     a.Phone,
		Notes:     a.Notes,
		CreatedAt: a.CreatedAt.Format(time.RFC3339),
		UpdatedAt: a.UpdatedAt.Format(time.RFC3339),
	}
}

func apiKeyMiddleware(apiKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if apiKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "API_KEY not configured"})
			return
		}
		if c.GetHeader("X-API-Key") != apiKey {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
			return
		}
		c.Next()
	}
}

func listAccounts(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var accounts []model.Account
		query := db.Model(&model.Account{})
		if platform := strings.TrimSpace(c.Query("platform")); platform != "" {
			query = query.Where("platform ILIKE ?", "%"+platform+"%")
		}
		if category := strings.TrimSpace(c.Query("category")); category != "" {
			query = query.Where("category ILIKE ?", "%"+category+"%")
		}
		if err := query.Order("category").Order("platform").Find(&accounts).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query failed"})
			return
		}
		resp := make([]accountResponse, 0, len(accounts))
		for _, acc := range accounts {
			resp = append(resp, toAccountResponse(acc))
		}
		c.JSON(http.StatusOK, resp)
	}
}

func createAccount(db *gorm.DB, masterKey []byte) gin.HandlerFunc {
	return func(c *gin.Context) {
		var input accountInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
			return
		}
		ciphertext, nonce, err := crypto.Encrypt(input.Password, masterKey)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt failed"})
			return
		}
		account := &model.Account{
			Platform:          input.Platform,
			Category:          input.Category,
			Username:          input.Username,
			EncryptedPassword: ciphertext,
			Email:             input.Email,
			Phone:             input.Phone,
			Notes:             input.Notes,
			Nonce:             nonce,
		}
		if err := db.Create(account).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "create failed"})
			return
		}
		c.JSON(http.StatusCreated, toAccountResponse(*account))
	}
}

func getAccount(db *gorm.DB, masterKey []byte, store *store.RedisStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var account model.Account
		if err := db.First(&account, "id = ?", c.Param("id")).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if c.Query("include_password") == "1" {
			if !isTLS(c) {
				c.JSON(http.StatusForbidden, gin.H{"error": "https required"})
				return
			}
			if store == nil {
				c.JSON(http.StatusForbidden, gin.H{"error": "password token unavailable"})
				return
			}
			token := strings.TrimSpace(c.GetHeader("X-Password-Token"))
			if token == "" || !consumePasswordToken(c, store, token) {
				c.JSON(http.StatusForbidden, gin.H{"error": "invalid password token"})
				return
			}
			pwd, err := crypto.Decrypt(account.EncryptedPassword, account.Nonce, masterKey)
			if err == nil {
				c.JSON(http.StatusOK, gin.H{
					"account":  toAccountResponse(account),
					"password": pwd,
				})
				return
			}
		}
		c.JSON(http.StatusOK, toAccountResponse(account))
	}
}

func createPasswordToken(store *store.RedisStore, ttl time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		if store == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "redis not available"})
			return
		}
		token, err := generateToken(32)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "token generation failed"})
			return
		}
		key := "pwdtoken:" + token
		if err := store.Set(c.Request.Context(), key, "1", ttl); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "token store failed"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"token": token, "expires_in": int(ttl.Seconds())})
	}
}

func consumePasswordToken(c *gin.Context, store *store.RedisStore, token string) bool {
	key := "pwdtoken:" + token
	val, err := store.Get(c.Request.Context(), key)
	if err != nil || val == "" {
		return false
	}
	_ = store.Del(c.Request.Context(), key)
	return true
}

func generateToken(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isTLS(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	return strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https")
}

func updateAccount(db *gorm.DB, masterKey []byte) gin.HandlerFunc {
	return func(c *gin.Context) {
		var account model.Account
		if err := db.First(&account, "id = ?", c.Param("id")).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		var input accountInput
		if err := c.ShouldBindJSON(&input); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
			return
		}

		if strings.TrimSpace(input.Password) != "" {
			ciphertext, nonce, err := crypto.Encrypt(input.Password, masterKey)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "encrypt failed"})
				return
			}
			account.EncryptedPassword = ciphertext
			account.Nonce = nonce
		}
		if input.Platform != "" {
			account.Platform = input.Platform
		}
		if input.Category != "" {
			account.Category = input.Category
		}
		if input.Username != "" {
			account.Username = input.Username
		}
		account.Email = input.Email
		account.Phone = input.Phone
		account.Notes = input.Notes

		if err := db.Save(&account).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
		c.JSON(http.StatusOK, toAccountResponse(account))
	}
}

func deleteAccount(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := db.Delete(&model.Account{}, "id = ?", c.Param("id")).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "delete failed"})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
