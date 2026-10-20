package proxy

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib/runtoken"
)

// rawForward sends one absolute-form request and returns the response.
func rawForward(t *testing.T, h *harness, rawURL, auth string) *http.Response {
	t.Helper()
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	host := strings.SplitN(strings.TrimPrefix(rawURL, "http://"), "/", 2)[0]
	req := "GET " + rawURL + " HTTP/1.1\r\nHost: " + host + "\r\n"
	if auth != "" {
		req += "Proxy-Authorization: " + auth + "\r\n"
	}
	_, _ = io.WriteString(c, req+"\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestForwardAppliesTheSameChecks(t *testing.T) {
	h := newHarness(t)
	h.resolver.set("private.example", []string{"10.1.2.3"})
	auth := basic(h.token("exec-fwd-checks", []string{"private.example", "plain.example"}, time.Hour))
	cases := []struct {
		url    string
		status int
		reason string
	}{
		{"http://private.example/", 403, ReasonAddressNotPublic},
		{"http://other.example/", 403, ReasonHostNotAllowed},
		{"http://plain.example:8080/", 403, ReasonPortNotAllowed},
		{"http://169.254.169.254/latest/meta-data/", 403, ReasonIPLiteralNotAllowed},
	}
	for _, tc := range cases {
		resp := rawForward(t, h, tc.url, auth)
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || resp.Header.Get(RefusalHeader) != tc.reason {
			t.Errorf("%s: %d %q, want %d %q", tc.url, resp.StatusCode, resp.Header.Get(RefusalHeader), tc.status, tc.reason)
		}
	}
	resp := rawForward(t, h, "http://plain.example/", "")
	_ = resp.Body.Close()
	if resp.StatusCode != 407 || resp.Header.Get(RefusalHeader) != ReasonTokenMissing {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if d := h.dialer.dialedAddrs(); len(d) != 0 {
		t.Fatalf("a refused forward dialled %v", d)
	}
}

func TestForwardRefusesOversizeResponseHeaders(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxHeaderBytes = 16 << 10 })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = bufio.NewReader(c).ReadString('\n')
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nX-Huge: "+strings.Repeat("a", 64<<10)+"\r\n\r\n")
	}()
	h.resolver.set("huge.example", []string{"203.0.113.50"})
	h.dialer.route("203.0.113.50:80", ln.Addr().String())
	resp := rawForward(t, h, "http://huge.example/", basic(h.token("exec-huge", []string{"huge.example"}, time.Hour)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("oversize upstream headers: %d", resp.StatusCode)
	}
}

func TestFileKeysReloadKeepsTheLastGoodSet(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, pub ed25519.PublicKey) {
		pemBytes, _ := runtoken.MarshalPublicKeyPEM(pub)
		if err := os.WriteFile(filepath.Join(dir, name), pemBytes, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	oldPub, _, _ := ed25519.GenerateKey(rand.Reader)
	newPub, _, _ := ed25519.GenerateKey(rand.Reader)
	write("old.pem", oldPub)
	m := NewMetrics()
	keys, err := LoadFileKeys(dir, slog.New(slog.NewTextHandler(io.Discard, nil)), m)
	if err != nil {
		t.Fatal(err)
	}
	if ids := keys.Current().KeyIDs(); len(ids) != 1 || ids[0] != runtoken.KeyID(oldPub) {
		t.Fatalf("initial set: %v", ids)
	}
	// Rotation: the new key is added.
	write("new.pem", newPub)
	keys.Reload()
	if len(keys.Current().KeyIDs()) != 2 {
		t.Fatalf("after adding a key: %v", keys.Current().KeyIDs())
	}
	// A broken write keeps the last good set and counts an error.
	if err := os.WriteFile(filepath.Join(dir, "new.pem"), []byte("-----BEGIN PUBLIC KEY-----\ngarbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys.Reload()
	if len(keys.Current().KeyIDs()) != 2 || m.keyReloadErrors.Load() != 1 {
		t.Fatalf("a failed reload must keep the last good set: %v, errors %d", keys.Current().KeyIDs(), m.keyReloadErrors.Load())
	}
	// The old key is removed.
	_ = os.Remove(filepath.Join(dir, "old.pem"))
	write("new.pem", newPub)
	keys.Reload()
	if ids := keys.Current().KeyIDs(); len(ids) != 1 || ids[0] != runtoken.KeyID(newPub) {
		t.Fatalf("after removing the old key: %v", ids)
	}
	if _, err := LoadFileKeys(filepath.Join(dir, "missing"), slog.Default(), m); err == nil {
		t.Fatal("a missing key set must fail at startup")
	}
}
