package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

// eventStreamPingTimeout bounds the startup reachability probe. A configured but
// unreachable Redis is a misconfiguration we want to surface at boot.
const eventStreamPingTimeout = 5 * time.Second

// newEventStreamRedisClient opens the plain Redis at REDIS_URL. Its one
// remaining use is canvas presence's roster store (v2canvaspresence.RedisStore);
// the project SSE stream and every publisher moved to the live-update NATS bus
// (event_stream_nats.go).
//
// Returns (nil, nil) when REDIS_URL is absent or empty; canvas presence then
// stays on its per-replica in-process roster.
func newEventStreamRedisClient(
	ctx context.Context,
	lookup func(string) (string, bool),
) (*goredis.Client, error) {
	if lookup == nil {
		return nil, errors.New("environment lookup is required")
	}
	raw, present := lookup("REDIS_URL")
	if !present || strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	options, err := redisOptionsFromEnv(lookup)
	if err != nil {
		return nil, err
	}

	client := goredis.NewClient(options)
	pingCtx, cancel := context.WithTimeout(ctx, eventStreamPingTimeout)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("verify REDIS_URL reachability: %w", err)
	}
	return client, nil
}
