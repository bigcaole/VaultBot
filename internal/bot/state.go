package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"vaultbot/internal/store"
)

type addState struct {
	Step              int    `json:"step"`
	Platform          string `json:"platform"`
	Category          string `json:"category"`
	Username          string `json:"username"`
	EncryptedPassword string `json:"encrypted_password"`
	Nonce             string `json:"nonce"`
	Email             string `json:"email"`
	Phone             string `json:"phone"`
	Notes             string `json:"notes"`
}

type searchState struct {
	Field string `json:"field"`
}

type editState struct {
	AccountID string `json:"account_id"`
	Field     string `json:"field"`
}

type categoryState struct {
	Categories []string `json:"categories"`
}

type categoryEditState struct {
	Old string `json:"old"`
}

const (
	stepPlatform = 1
	stepCategory = 2
	stepUsername = 3
	stepPassword = 4
	stepEmail    = 5
	stepPhone    = 6
	stepNotes    = 7
	stepDone     = 8
)

func stateKey(prefix string, userID string) string {
	return fmt.Sprintf("%s:%s", prefix, userID)
}

func loadState(ctx context.Context, s *store.RedisStore, key string) (*addState, error) {
	return loadJSON[addState](ctx, s, key)
}

func saveState(ctx context.Context, s *store.RedisStore, key string, st *addState, ttl time.Duration) error {
	return saveJSON(ctx, s, key, st, ttl)
}

func loadSearchState(ctx context.Context, s *store.RedisStore, key string) (*searchState, error) {
	return loadJSON[searchState](ctx, s, key)
}

func saveSearchState(ctx context.Context, s *store.RedisStore, key string, st *searchState, ttl time.Duration) error {
	return saveJSON(ctx, s, key, st, ttl)
}

func loadEditState(ctx context.Context, s *store.RedisStore, key string) (*editState, error) {
	return loadJSON[editState](ctx, s, key)
}

func saveEditState(ctx context.Context, s *store.RedisStore, key string, st *editState, ttl time.Duration) error {
	return saveJSON(ctx, s, key, st, ttl)
}

func loadCategoryState(ctx context.Context, s *store.RedisStore, key string) (*categoryState, error) {
	return loadJSON[categoryState](ctx, s, key)
}

func saveCategoryState(ctx context.Context, s *store.RedisStore, key string, st *categoryState, ttl time.Duration) error {
	return saveJSON(ctx, s, key, st, ttl)
}

func loadCategoryEditState(ctx context.Context, s *store.RedisStore, key string) (*categoryEditState, error) {
	return loadJSON[categoryEditState](ctx, s, key)
}

func saveCategoryEditState(ctx context.Context, s *store.RedisStore, key string, st *categoryEditState, ttl time.Duration) error {
	return saveJSON(ctx, s, key, st, ttl)
}

func loadJSON[T any](ctx context.Context, s *store.RedisStore, key string) (*T, error) {
	val, err := s.Get(ctx, key)
	if err != nil || val == "" {
		return nil, err
	}
	var st T
	if err := json.Unmarshal([]byte(val), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func saveJSON(ctx context.Context, s *store.RedisStore, key string, st any, ttl time.Duration) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.Set(ctx, key, string(b), ttl)
}
