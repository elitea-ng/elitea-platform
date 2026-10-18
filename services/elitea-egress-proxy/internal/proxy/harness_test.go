package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib/runtoken"
)

// lockedBuffer is a log sink the tests read while the proxy writes.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeResolver answers from a table. A host with several answers returns
// them in turn (the last one repeats), which is how a rebinding resolver
// behaves.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]string
	calls   map[string]int
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{answers: map[string][][]string{}, calls: map[string]int{}}
}

func (f *fakeResolver) set(host string, answers ...[]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[host] = answers
}

func (f *fakeResolver) callCount(host string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[host]
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq, ok := f.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	i := f.calls[host]
	f.calls[host]++
	if i >= len(seq) {
		i = len(seq) - 1
	}
	out := make([]net.IPAddr, 0, len(seq[i]))
	for _, a := range seq[i] {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}
	return out, nil
}

// fakeDialer maps the public test addresses the resolver hands out to the
// local listeners that stand in for the destinations. It refuses anything
// else, and it records every address it was asked to dial.
type fakeDialer struct {
	mu     sync.Mutex
	routes map[string]string
	dialed []string
}

func (d *fakeDialer) route(public, local string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routes[public] = local
}

func (d *fakeDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, address)
	local, ok := d.routes[address]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("test dialer: no route to %s", address)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, local)
}

func (d *fakeDialer) dialedAddrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

type harness struct {
	t        *testing.T
	srv      *Server
	addr     string
	signer   *runtoken.Signer
	resolver *fakeResolver
	dialer   *fakeDialer
	logs     *lockedBuffer
	metrics  *Metrics
}

type harnessOption func(*Options)

func newHarness(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := runtoken.NewSigner(priv)
	if err != nil {
		t.Fatal(err)
	}
	ks, err := runtoken.NewKeySet(pub)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:        t,
		signer:   signer,
		resolver: newFakeResolver(),
		dialer:   &fakeDialer{routes: map[string]string{}},
		logs:     &lockedBuffer{},
		metrics:  NewMetrics(),
	}
	o := Options{
		Keys:                  StaticKeys{Set: ks},
		Resolver:              h.resolver,
		Dialer:                h.dialer,
		MaxConnections:        64,
		MaxHeaderBytes:        32 << 10,
		ReadHeaderTimeout:     5 * time.Second,
		DialTimeout:           2 * time.Second,
		RunMaxConnections:     16,
		RunMaxBytes:           64 << 20,
		IdleTimeout:           10 * time.Second,
		MaxConnectionLifetime: time.Minute,
		Logger:                slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics:               h.metrics,
	}
	for _, opt := range opts {
		opt(&o)
	}
	srv, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.srv, h.addr = srv, ln.Addr().String()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	})
	return h
}

// token mints a token valid from now for ttl.
func (h *harness) token(executionID string, allow []string, ttl time.Duration) string {
	h.t.Helper()
	now := time.Now().Truncate(time.Second)
	return h.tokenAt(executionID, allow, now, now.Add(ttl))
}

func (h *harness) tokenAt(executionID string, allow []string, nbf, exp time.Time) string {
	h.t.Helper()
	nonce, err := runtoken.NewNonce()
	if err != nil {
		h.t.Fatal(err)
	}
	tok, err := h.signer.Sign(runtoken.Claims{
		ExecutionID: executionID, ProjectID: 9, Allowlist: allow,
		IssuedAt: nbf, NotBefore: nbf, ExpiresAt: exp, Nonce: nonce,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return tok
}

func basic(token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(TokenUser+":"+token))
}

// rawConnect sends a CONNECT and returns the response and the open
// connection (a tunnel when the status is 200).
func (h *harness) rawConnect(authority, proxyAuth string) (*http.Response, net.Conn, *bufio.Reader) {
	h.t.Helper()
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		h.t.Fatal(err)
	}
	req := "CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority + "\r\n"
	if proxyAuth != "" {
		req += "Proxy-Authorization: " + proxyAuth + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		h.t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		h.t.Fatalf("CONNECT %s: read response: %v", authority, err)
	}
	return resp, c, br
}

// expectRefused sends a CONNECT and asserts the status and reason.
func (h *harness) expectRefused(authority, proxyAuth string, status int, reason string) string {
	h.t.Helper()
	resp, c, _ := h.rawConnect(authority, proxyAuth)
	defer func() { _ = c.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != status || resp.Header.Get(RefusalHeader) != reason {
		h.t.Errorf("CONNECT %s: got %d %q (%s), want %d %q", authority, resp.StatusCode,
			resp.Header.Get(RefusalHeader), strings.TrimSpace(string(body)), status, reason)
	}
	return string(body)
}

// echoServer is a TCP destination that echoes every byte back.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
