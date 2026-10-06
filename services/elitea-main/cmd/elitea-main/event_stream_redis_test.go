package main

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestEventStreamRedisClientIsAbsentWithoutRedisURL(t *testing.T) {
	t.Parallel()

	for name, env := range map[string]map[string]string{
		"unset": {},
		"empty": {"REDIS_URL": ""},
		"blank": {"REDIS_URL": "   "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			client, err := newEventStreamRedisClient(context.Background(), envLookup(env))
			if err != nil {
				t.Fatalf("newEventStreamRedisClient() error = %v, want nil", err)
			}
			if client != nil {
				t.Fatal("newEventStreamRedisClient() returned a client without REDIS_URL; " +
					"deployments with no Redis must keep starting")
			}
		})
	}
}

func TestEventStreamRedisClientRequiresLookup(t *testing.T) {
	t.Parallel()

	if _, err := newEventStreamRedisClient(context.Background(), nil); err == nil {
		t.Fatal("newEventStreamRedisClient(nil) error = nil, want an error")
	}
}

func TestEventStreamRedisClientRejectsInvalidRedisURL(t *testing.T) {
	t.Parallel()

	_, err := newEventStreamRedisClient(
		context.Background(),
		envLookup(map[string]string{"REDIS_URL": "redis://%zz"}),
	)
	if err == nil {
		t.Fatal("newEventStreamRedisClient() error = nil, want an error for an invalid REDIS_URL")
	}
}

// A configured-but-unreachable Redis must fail startup rather than silently
// leave canvas presence on a per-replica roster.
func TestEventStreamRedisClientFailsWhenRedisIsUnreachable(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	client, err := newEventStreamRedisClient(
		context.Background(),
		envLookup(map[string]string{"REDIS_URL": address}),
	)
	if client != nil {
		t.Error("newEventStreamRedisClient() returned a client for an unreachable Redis")
	}
	if err == nil {
		t.Fatal("newEventStreamRedisClient() error = nil, want a reachability error")
	}
	if !strings.Contains(err.Error(), "REDIS_URL") {
		t.Errorf("newEventStreamRedisClient() error = %q, want it to name REDIS_URL", err)
	}
}
