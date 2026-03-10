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
	val, err := s.Get(ctx, key)
	if err != nil || val == "" {
		return nil, err
	}
	var st addState
	if err := json.Unmarshal([]byte(val), &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func saveState(ctx context.Context, s *store.RedisStore, key string, st *addState, ttl time.Duration) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.Set(ctx, key, string(b), ttl)
}
