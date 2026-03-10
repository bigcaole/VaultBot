package store

import (
	"context"
	"time"
)

// Allow returns true if request is within rate limit.
func (s *RedisStore) Allow(ctx context.Context, key string, limit int64, window time.Duration) (bool, error) {
	count, err := s.client.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	if count == 1 {
		_ = s.client.Expire(ctx, key, window).Err()
	}
	return count <= limit, nil
}
