package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestShutdownTimeoutFromEnv(t *testing.T) {
	lookup := func(value string, set bool) func(string) (string, bool) {
		return func(key string) (string, bool) {
			if key != shutdownTimeoutEnv {
				return "", false
			}
			return value, set
		}
	}
	for _, unset := range []func(string) (string, bool){lookup("", false), lookup("", true)} {
		if got, err := shutdownTimeoutFromEnv(unset); err != nil || got != defaultShutdownTimeout {
			t.Fatalf("unset: %s, %v; want %s", got, err, defaultShutdownTimeout)
		}
	}
	if got, err := shutdownTimeoutFromEnv(lookup("2m", true)); err != nil || got != 2*time.Minute {
		t.Fatalf("2m: %s, %v", got, err)
	}
	for _, bad := range []string{"15", "500ms", "11m", "-5s", "soon"} {
		if _, err := shutdownTimeoutFromEnv(lookup(bad, true)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// An SSE stream that outlives the drain is closed at the deadline: its
// handler sees its context end (so it lets go of what it holds before the
// pools close) and the client reads the end of the stream, which is what
// makes a browser reconnect to another replica.
func TestShutdownPublicClosesAStreamThatOutlivesTheDrain(t *testing.T) {
	handlerDone := make(chan struct{})
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer close(handlerDone)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, ": open\n\n")
			_ = http.NewResponseController(w).Flush()
			<-r.Context().Done()
		}),
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+listener.Addr().String()+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	body := bufio.NewReader(response.Body)
	if line, err := body.ReadString('\n'); err != nil || line != ": open\n" {
		t.Fatalf("first frame: %q, %v", line, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := shutdownPublic(ctx, server); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdownPublic: %v, want the drain deadline", err)
	}
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream's handler still runs after the drain deadline")
	}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, body)
		readDone <- err
	}()
	select {
	case <-readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the client still reads an open stream after the drain deadline")
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve: %v", err)
	}
}

// A drain that finishes in time closes nothing early.
func TestShutdownPublicWithinTheDeadlineIsAPlainShutdown(t *testing.T) {
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.NotFoundHandler()}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdownPublic(ctx, server); err != nil {
		t.Fatalf("shutdownPublic: %v", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve: %v", err)
	}
}
