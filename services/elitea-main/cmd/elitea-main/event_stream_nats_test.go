package main

import (
	"net"
	"strings"
	"testing"
)

func envLookup(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, present := pairs[key]
		return value, present
	}
}

// No URL is a declared state — no live-update plane — not an error: the
// deployment must keep starting.
func TestEventsNATSConnIsAbsentWithoutTheURL(t *testing.T) {
	t.Parallel()

	for name, env := range map[string]map[string]string{
		"unset": {},
		"empty": {eventsNATSURLEnv: ""},
		"blank": {eventsNATSURLEnv: "   "},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			conn, err := newEventsNATSConn(envLookup(env), nil)
			if err != nil {
				t.Fatalf("newEventsNATSConn() error = %v, want nil", err)
			}
			if conn != nil {
				t.Fatal("newEventsNATSConn() returned a connection without " + eventsNATSURLEnv)
			}
		})
	}
}

func TestEventsNATSConnRequiresLookup(t *testing.T) {
	t.Parallel()

	if _, err := newEventsNATSConn(nil, nil); err == nil {
		t.Fatal("newEventsNATSConn(nil) error = nil, want an error")
	}
}

// A configured-but-unreachable server must fail startup rather than leave
// RouterConfig.EventSource nil: a nil source silently unregisters
// /api/v2/events/prompt_lib/{projectID}, which is the whole failure mode of
// #152. Better to refuse to boot than to boot without the route.
func TestEventsNATSConnFailsWhenTheServerIsUnreachable(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release the reserved port: %v", err)
	}

	url := "nats://user:s3cret@" + address
	conn, err := newEventsNATSConn(envLookup(map[string]string{eventsNATSURLEnv: url}), nil)
	if conn != nil {
		conn.Close()
		t.Fatal("newEventsNATSConn() returned a connection to an unreachable server")
	}
	if err == nil {
		t.Fatal("newEventsNATSConn() error = nil, want an error for an unreachable server")
	}
	if !strings.Contains(err.Error(), eventsNATSURLEnv) {
		t.Errorf("error %q does not name %s", err, eventsNATSURLEnv)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error %q leaks the credential in the URL", err)
	}
}
