package proxy

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/runtoken"
)

// tlsDestination starts an HTTPS server and routes public 203.0.113.10:443
// to it. httptest's certificate names example.com, so the client verifies the
// destination end to end through the tunnel.
func tlsDestination(t *testing.T, h *harness) *x509.CertPool {
	t.Helper()
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello through the tunnel")
	}))
	t.Cleanup(ts.Close)
	h.resolver.set("example.com", []string{"203.0.113.10"})
	h.dialer.route("203.0.113.10:443", ts.Listener.Addr().String())
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	return pool
}

func clientVia(h *harness, token string, roots *x509.CertPool) *http.Client {
	proxyURL := &url.URL{Scheme: "http", User: url.UserPassword(TokenUser, token), Host: h.addr}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       10 * time.Second,
	}
}

func TestConnectToAnAllowedHostSucceedsEndToEnd(t *testing.T) {
	h := newHarness(t)
	roots := tlsDestination(t, h)
	tok := h.token("exec-ok", []string{"example.com"}, time.Hour)

	client := clientVia(h, tok, roots)
	resp, err := client.Get("https://example.com/")
	if err != nil {
		t.Fatalf("GET through the proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	client.CloseIdleConnections()
	if resp.StatusCode != 200 || string(body) != "hello through the tunnel" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	// The proxy dialled the checked literal, never a name.
	for _, a := range h.dialer.dialedAddrs() {
		if a != "203.0.113.10:443" {
			t.Fatalf("dialled %s", a)
		}
	}
	waitFor(t, 2*time.Second, "the connection log line", func() bool {
		return strings.Contains(h.logs.String(), `"outcome":"ok"`)
	})
	logs := h.logs.String()
	for _, want := range []string{`"msg":"egress_proxy.connection"`, `"execution_id":"exec-ok"`, `"project_id":9`, `"host":"example.com"`, `"port":443`, `"mode":"CONNECT"`, `"bytes_down":`} {
		if !strings.Contains(logs, want) {
			t.Errorf("the connection log lacks %s:\n%s", want, logs)
		}
	}
}

func TestConnectRefusals(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.DenyCIDRs = []*net.IPNet{mustCIDR(t, "203.0.113.96/28")}
	})
	tlsDestination(t, h)
	allow := []string{"example.com", "private.example", "mixed.example", "metadata.example", "cluster.example", "nxdomain.example", "pinned.example:8443"}
	tok := h.token("exec-refusals", allow, time.Hour)
	auth := basic(tok)
	h.resolver.set("private.example", []string{"10.0.0.5"})
	h.resolver.set("mixed.example", []string{"203.0.113.11", "192.168.1.1"})
	h.resolver.set("metadata.example", []string{"169.254.169.254"})
	h.resolver.set("cluster.example", []string{"203.0.113.99"})
	h.resolver.set("pinned.example", []string{"203.0.113.12"})

	h.expectRefused("not-allowed.example:443", auth, 403, ReasonHostNotAllowed)
	h.expectRefused("example.com:8443", auth, 403, ReasonPortNotAllowed)
	h.expectRefused("pinned.example:443", auth, 403, ReasonPortNotAllowed)
	if body := h.expectRefused("github.com:22", auth, 403, ReasonHostNotAllowed); !strings.Contains(body, "SSH") {
		t.Errorf("a port-22 refusal must say SSH is not available: %q", body)
	}
	if body := h.expectRefused("example.com:22", auth, 403, ReasonPortNotAllowed); !strings.Contains(body, "SSH") {
		t.Errorf("a port-22 refusal must say SSH is not available: %q", body)
	}
	h.expectRefused("203.0.113.10:443", auth, 403, ReasonIPLiteralNotAllowed)
	h.expectRefused("10.0.0.1:443", auth, 403, ReasonIPLiteralNotAllowed)
	h.expectRefused("[::1]:443", auth, 403, ReasonIPLiteralNotAllowed)
	h.expectRefused("169.254.169.254:80", auth, 403, ReasonIPLiteralNotAllowed)
	h.expectRefused("private.example:443", auth, 403, ReasonAddressNotPublic)
	h.expectRefused("mixed.example:443", auth, 403, ReasonAddressNotPublic)
	h.expectRefused("metadata.example:443", auth, 403, ReasonAddressNotPublic)
	h.expectRefused("cluster.example:443", auth, 403, ReasonAddressNotPublic)
	h.expectRefused("nxdomain.example:443", auth, 502, ReasonResolveFailed)
	h.expectRefused("bad-.example:443", auth, 400, ReasonBadTarget)
	h.expectRefused("example.com:0", auth, 400, ReasonBadTarget)
	h.expectRefused("example.com", auth, 400, ReasonBadTarget)

	// No refusal reached a destination.
	if d := h.dialer.dialedAddrs(); len(d) != 0 {
		t.Fatalf("a refused request dialled %v", d)
	}
	if h.metrics.RefusalCount(ReasonAddressNotPublic) != 4 {
		t.Errorf("address-not-public counted %d, want 4", h.metrics.RefusalCount(ReasonAddressNotPublic))
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTokenRefusals(t *testing.T) {
	h := newHarness(t)
	tlsDestination(t, h)
	allow := []string{"example.com"}
	now := time.Now().Truncate(time.Second)

	expired := h.tokenAt("exec-expired", allow, now.Add(-2*time.Hour), now.Add(-time.Hour))
	h.expectRefused("example.com:443", basic(expired), 407, ReasonTokenExpired)

	notYet := h.tokenAt("exec-future", allow, now.Add(time.Hour), now.Add(2*time.Hour))
	h.expectRefused("example.com:443", basic(notYet), 407, ReasonTokenInvalid)

	h.expectRefused("example.com:443", "", 407, ReasonTokenMissing)
	h.expectRefused("example.com:443", basic("ert1.garbage"), 407, ReasonTokenInvalid)
	h.expectRefused("example.com:443", "Bearer "+h.token("exec-bearer", allow, time.Hour), 407, ReasonTokenInvalid)
	h.expectRefused("example.com:443", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:"+h.token("e", allow, time.Hour))), 407, ReasonTokenInvalid)
	h.expectRefused("example.com:443", "Basic !!!", 407, ReasonTokenInvalid)

	// Signed by a key outside the set.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := runtoken.NewSigner(otherPriv)
	nonce, _ := runtoken.NewNonce()
	foreign, err := other.Sign(runtoken.Claims{ExecutionID: "exec-foreign", ProjectID: 1, Allowlist: allow,
		IssuedAt: now, NotBefore: now, ExpiresAt: now.Add(time.Hour), Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	h.expectRefused("example.com:443", basic(foreign), 407, ReasonTokenInvalid)

	// Tampered: the allowlist widened after signing.
	good := h.token("exec-tamper", allow, time.Hour)
	parts := strings.Split(good, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[2])
	widened := strings.Replace(string(payload), `"allow":["example.com"]`, `"allow":["example.com","evil.example"]`, 1)
	parts[2] = base64.RawURLEncoding.EncodeToString([]byte(widened))
	h.expectRefused("example.com:443", basic(strings.Join(parts, ".")), 407, ReasonTokenInvalid)

	resp, c, _ := h.rawConnect("example.com:443", "")
	_ = c.Close()
	if !strings.HasPrefix(resp.Header.Get("Proxy-Authenticate"), "Basic") {
		t.Error("a 407 must carry Proxy-Authenticate")
	}
	if d := h.dialer.dialedAddrs(); len(d) != 0 {
		t.Fatalf("a refused token reached a destination: %v", d)
	}
}

func TestDNSRebindingIsRefusedAndTheCheckedAddressIsDialled(t *testing.T) {
	h := newHarness(t)
	echo := echoServer(t)
	// Public on the first lookup, then the metadata address, then private.
	h.resolver.set("rebind.example", []string{"203.0.113.30"}, []string{"169.254.169.254"}, []string{"10.0.0.7"})
	h.dialer.route("203.0.113.30:8443", echo)
	auth := basic(h.token("exec-rebind", []string{"rebind.example:8443"}, time.Hour))

	resp, c, br := h.rawConnect("rebind.example:8443", auth)
	if resp.StatusCode != 200 {
		t.Fatalf("first CONNECT: %d", resp.StatusCode)
	}
	_, _ = io.WriteString(c, "ping")
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("tunnel echo: %q %v", buf, err)
	}
	_ = c.Close()

	h.expectRefused("rebind.example:8443", auth, 403, ReasonAddressNotPublic)
	h.expectRefused("rebind.example:8443", auth, 403, ReasonAddressNotPublic)

	// One resolution per request, and only the checked public literal dialled.
	if got := h.dialer.dialedAddrs(); len(got) != 1 || got[0] != "203.0.113.30:8443" {
		t.Fatalf("dialled %v, want only the checked literal", got)
	}
	if n := h.resolver.callCount("rebind.example"); n != 3 {
		t.Fatalf("resolved %d times, want once per request (3)", n)
	}
}

func TestAbsoluteFormForwardStripsHopByHopAndNeverFollowsRedirects(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var seen http.Header
	var seenHost string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen, seenHost = r.Header.Clone(), r.Host
		mu.Unlock()
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://elsewhere.example/", http.StatusFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		// net/http rewrites a handler's Connection header, so the hop-by-hop
		// response is written raw.
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		out := "echo:" + string(body)
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nConnection: close, X-Resp-Hop\r\nX-Resp-Hop: drop me\r\n" +
			"Keep-Alive: timeout=5\r\nX-Resp-End: kept\r\nContent-Length: " + strconv.Itoa(len(out)) + "\r\n\r\n" + out)
		_ = buf.Flush()
	}))
	t.Cleanup(upstream.Close)
	h.resolver.set("plain.example", []string{"203.0.113.20"})
	h.resolver.set("elsewhere.example", []string{"203.0.113.21"})
	h.dialer.route("203.0.113.20:80", upstream.Listener.Addr().String())
	tok := h.token("exec-forward", []string{"plain.example"}, time.Hour)

	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	req := "POST http://plain.example/echo HTTP/1.1\r\n" +
		"Host: plain.example\r\n" +
		"Proxy-Authorization: " + basic(tok) + "\r\n" +
		"Proxy-Connection: keep-alive\r\n" +
		"Connection: keep-alive, X-Custom-Hop\r\n" +
		"X-Custom-Hop: drop me\r\n" +
		"Keep-Alive: timeout=5\r\n" +
		"Te: trailers\r\n" +
		"Upgrade: websocket\r\n" +
		"X-End: kept\r\n" +
		"Content-Length: 5\r\n\r\nhello"
	if _, err := io.WriteString(c, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "echo:hello" {
		t.Fatalf("forward: %d %q", resp.StatusCode, body)
	}
	mu.Lock()
	for _, hop := range []string{"Proxy-Authorization", "Proxy-Connection", "X-Custom-Hop", "Keep-Alive", "Te", "Upgrade"} {
		if seen.Get(hop) != "" {
			t.Errorf("the destination received hop-by-hop %s: %q", hop, seen.Get(hop))
		}
	}
	if seen.Get("X-End") != "kept" || seenHost != "plain.example" {
		t.Errorf("an end-to-end header or the host was lost: %v %q", seen, seenHost)
	}
	mu.Unlock()
	if resp.Header.Get("X-Resp-Hop") != "" || resp.Header.Get("Keep-Alive") != "" {
		t.Errorf("the client received a hop-by-hop response header: %v", resp.Header)
	}
	if resp.Header.Get("X-Resp-End") != "kept" {
		t.Errorf("an end-to-end response header was lost: %v", resp.Header)
	}

	// A redirect is relayed, never followed: elsewhere.example is never dialled.
	redirect := "GET http://plain.example/redirect HTTP/1.1\r\nHost: plain.example\r\nProxy-Authorization: " + basic(tok) + "\r\n\r\n"
	if _, err := io.WriteString(c, redirect); err != nil {
		t.Fatal(err)
	}
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "http://elsewhere.example/" {
		t.Fatalf("redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for _, a := range h.dialer.dialedAddrs() {
		if a != "203.0.113.20:80" {
			t.Fatalf("the proxy dialled %s", a)
		}
	}

	// https in absolute form is refused: TLS goes through CONNECT.
	https := "GET https://plain.example/ HTTP/1.1\r\nHost: plain.example\r\nProxy-Authorization: " + basic(tok) + "\r\n\r\n"
	_, _ = io.WriteString(c, https)
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 403 || resp.Header.Get(RefusalHeader) != ReasonUnsupported {
		t.Fatalf("absolute https: %d %q", resp.StatusCode, resp.Header.Get(RefusalHeader))
	}

	// An origin-form request is not a proxy request.
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+h.addr+"\r\n\r\n")
	resp, err = http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 400 || resp.Header.Get(RefusalHeader) != ReasonUnsupported {
		t.Fatalf("origin form: %d %q", resp.StatusCode, resp.Header.Get(RefusalHeader))
	}

	// The same flow through net/http's own proxy client, with a body.
	cl := clientVia(h, tok, nil)
	r2, err := cl.Post("http://plain.example/echo", "text/plain", strings.NewReader("via transport"))
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(r2.Body)
	_ = r2.Body.Close()
	if string(b2) != "echo:via transport" {
		t.Fatalf("transport forward: %q", b2)
	}
}

func openTunnel(t *testing.T, h *harness, auth string) (net.Conn, *bufio.Reader) {
	t.Helper()
	resp, c, br := h.rawConnect("echo.example:8443", auth)
	if resp.StatusCode != 200 {
		_ = c.Close()
		t.Fatalf("CONNECT echo.example: %d %q", resp.StatusCode, resp.Header.Get(RefusalHeader))
	}
	return c, br
}

// echoRoute routes echo.example:8443 to an echo server. The echo tunnels
// carry plain bytes, which the ClientHello peek passes only off port 443.
func echoRoute(t *testing.T, h *harness) {
	t.Helper()
	h.resolver.set("echo.example", []string{"203.0.113.40"})
	h.dialer.route("203.0.113.40:8443", echoServer(t))
}

func TestRunConnectionLimit(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RunMaxConnections = 2 })
	echoRoute(t, h)
	auth := basic(h.token("exec-conns", []string{"echo.example:8443"}, time.Hour))
	c1, _ := openTunnel(t, h, auth)
	c2, _ := openTunnel(t, h, auth)
	h.expectRefused("echo.example:8443", auth, 429, ReasonRunConnectionLimit)

	// Another run is not affected.
	other, _ := openTunnel(t, h, basic(h.token("exec-other", []string{"echo.example:8443"}, time.Hour)))
	_ = other.Close()

	_ = c1.Close()
	waitFor(t, 2*time.Second, "the slot to free", func() bool {
		resp, c, _ := h.rawConnect("echo.example:8443", auth)
		defer func() { _ = c.Close() }()
		return resp.StatusCode == 200
	})
	_ = c2.Close()
}

func TestRunByteLimit(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.RunMaxBytes = 1000 })
	echoRoute(t, h)
	auth := basic(h.token("exec-bytes", []string{"echo.example:8443"}, time.Hour))
	c, br := openTunnel(t, h, auth)
	defer func() { _ = c.Close() }()
	_, _ = c.Write(make([]byte, 400))
	if _, err := io.ReadFull(br, make([]byte, 400)); err != nil {
		t.Fatalf("400 bytes up and 400 down fit the budget: %v", err)
	}
	_, _ = c.Write(make([]byte, 400))
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadAll(br); err != nil && !isClosedErr(err) {
		t.Fatalf("read after the budget: %v", err)
	}
	h.expectRefused("echo.example:8443", auth, 403, ReasonRunByteLimit)
	waitFor(t, 2*time.Second, "the byte-limit outcome", func() bool {
		return strings.Contains(h.logs.String(), `"outcome":"byte-limit"`)
	})
}

func isClosedErr(err error) bool {
	return err == nil || strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "reset")
}

func TestIdleTimeoutAndLifetime(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.IdleTimeout = 150 * time.Millisecond })
		echoRoute(t, h)
		c, br := openTunnel(t, h, basic(h.token("exec-idle", []string{"echo.example:8443"}, time.Hour)))
		defer func() { _ = c.Close() }()
		start := time.Now()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := br.ReadByte()
		if err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("the idle tunnel was not closed: %v after %s", err, time.Since(start))
		}
		waitFor(t, 2*time.Second, "the idle outcome", func() bool {
			return strings.Contains(h.logs.String(), `"outcome":"idle-timeout"`)
		})
	})
	t.Run("lifetime", func(t *testing.T) {
		h := newHarness(t, func(o *Options) { o.MaxConnectionLifetime = 300 * time.Millisecond })
		echoRoute(t, h)
		c, br := openTunnel(t, h, basic(h.token("exec-life", []string{"echo.example:8443"}, time.Hour)))
		defer func() { _ = c.Close() }()
		start := time.Now()
		closed := false
		for time.Since(start) < 3*time.Second {
			if _, err := c.Write([]byte("x")); err != nil {
				closed = true
				break
			}
			_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			if _, err := br.ReadByte(); err != nil && !strings.Contains(err.Error(), "timeout") {
				closed = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !closed || time.Since(start) < 250*time.Millisecond {
			t.Fatalf("an active tunnel must close at its lifetime: closed=%v after %s", closed, time.Since(start))
		}
		waitFor(t, 2*time.Second, "the lifetime outcome", func() bool {
			return strings.Contains(h.logs.String(), `"outcome":"lifetime"`)
		})
	})
	t.Run("token expiry", func(t *testing.T) {
		h := newHarness(t)
		echoRoute(t, h)
		now := time.Now()
		exp := now.Truncate(time.Second).Add(2 * time.Second)
		c, br := openTunnel(t, h, basic(h.tokenAt("exec-exp", []string{"echo.example:8443"}, now.Truncate(time.Second), exp)))
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err := br.ReadByte()
		if err == nil || time.Now().Before(exp) {
			t.Fatalf("the tunnel must close at the token expiry: %v at %s, expiry %s", err, time.Now(), exp)
		}
		waitFor(t, 2*time.Second, "the token-expired outcome", func() bool {
			return strings.Contains(h.logs.String(), `"outcome":"token-expired"`)
		})
	})
}

func TestGlobalLimits(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxConnections = 2; o.MaxHeaderBytes = 16 << 10 })
	a, _ := net.Dial("tcp", h.addr)
	b, _ := net.Dial("tcp", h.addr)
	defer func() { _ = a.Close(); _ = b.Close() }()
	waitFor(t, 2*time.Second, "two accepted connections", func() bool {
		return h.metrics.activeConnections.Load() == 2
	})
	c, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	_ = c.Close()
	if err != nil || resp.StatusCode != 503 || resp.Header.Get(RefusalHeader) != ReasonProxyConnectionLimit {
		t.Fatalf("over the connection limit: %v %v", resp, err)
	}
	_ = a.Close()
	_ = b.Close()
	waitFor(t, 2*time.Second, "the slots to free", func() bool { return h.metrics.activeConnections.Load() == 0 })

	big, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = big.Close() }()
	_, _ = io.WriteString(big, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nX-Big: "+strings.Repeat("a", 64<<10)+"\r\n\r\n")
	resp, err = http.ReadResponse(bufio.NewReader(big), nil)
	if err != nil || resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("oversize headers: %v %v", resp, err)
	}
}

func TestTheTokenNeverReachesTheLogs(t *testing.T) {
	h := newHarness(t)
	roots := tlsDestination(t, h)
	echoRoute(t, h)
	allow := []string{"example.com", "echo.example:8443"}
	tok := h.token("exec-secret", allow, time.Hour)
	now := time.Now().Truncate(time.Second)
	expired := h.tokenAt("exec-secret", allow, now.Add(-2*time.Hour), now.Add(-time.Hour))
	tampered := tok[:len(tok)-4] + "AAAA"

	resp, err := clientVia(h, tok, roots).Get("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	h.expectRefused("not-allowed.example:443", basic(tok), 403, ReasonHostNotAllowed)
	h.expectRefused("example.com:443", basic(expired), 407, ReasonTokenExpired)
	h.expectRefused("example.com:443", basic(tampered), 407, ReasonTokenInvalid)
	h.expectRefused("example.com:443", "Basic "+base64.StdEncoding.EncodeToString([]byte("wrong:"+tok)), 407, ReasonTokenInvalid)
	c, _ := openTunnel(t, h, basic(tok))
	_ = c.Close()
	h.srv.Runs().Sweep(time.Now().Add(2*time.Hour), time.Minute, false)

	waitFor(t, 2*time.Second, "the run summary", func() bool {
		return strings.Contains(h.logs.String(), SummaryEvent)
	})
	logs := h.logs.String()
	for _, secret := range []string{tok, expired, tampered, basic(tok), strings.Split(tok, ".")[2], strings.Split(tok, ".")[3]} {
		if strings.Contains(logs, secret) {
			t.Fatalf("the logs hold token material %.20s...", secret)
		}
	}
	if !strings.Contains(logs, `"execution_id":"exec-secret"`) {
		t.Fatal("the logs must still name the run")
	}
}

func TestRunSummary(t *testing.T) {
	h := newHarness(t)
	roots := tlsDestination(t, h)
	tok := h.token("exec-summary", []string{"example.com"}, time.Hour)
	for i := 0; i < 2; i++ {
		resp, err := clientVia(h, tok, roots).Get("https://example.com/")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	h.expectRefused("blocked.example:443", basic(tok), 403, ReasonHostNotAllowed)
	h.expectRefused("blocked.example:443", basic(tok), 403, ReasonHostNotAllowed)
	waitFor(t, 2*time.Second, "both connections to end", func() bool { return h.metrics.activeTunnels.Load() == 0 })

	// A periodic summary while the token is valid, then a final one.
	h.srv.Runs().Sweep(time.Now(), 0, false)
	h.srv.Runs().Sweep(time.Now().Add(2*time.Hour), time.Minute, false)
	var summaries []map[string]any
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if !strings.Contains(line, SummaryEvent) {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		summaries = append(summaries, m)
	}
	if len(summaries) != 2 || summaries[0]["final"] != false || summaries[1]["final"] != true {
		t.Fatalf("want one periodic and one final summary, got %v", summaries)
	}
	final := summaries[1]
	if final["execution_id"] != "exec-summary" || final["project_id"] != float64(9) || final["connections"] != float64(2) {
		t.Fatalf("final summary: %v", final)
	}
	var reached []HostReached
	var refused []HostRefused
	rawReached, _ := json.Marshal(final["hosts_reached"])
	rawRefused, _ := json.Marshal(final["hosts_refused"])
	_ = json.Unmarshal(rawReached, &reached)
	_ = json.Unmarshal(rawRefused, &refused)
	if len(reached) != 1 || reached[0].Host != "example.com" || reached[0].Port != 443 || reached[0].Connections != 2 ||
		reached[0].BytesUp == 0 || reached[0].BytesDown == 0 {
		t.Errorf("hosts_reached: %s", rawReached)
	}
	if len(refused) != 1 || refused[0] != (HostRefused{Host: "blocked.example", Port: 443, Reason: ReasonHostNotAllowed, Count: 2}) {
		t.Errorf("hosts_refused: %s", rawRefused)
	}
	if h.srv.Runs().Active() != 0 {
		t.Error("a run with a final summary must be dropped")
	}
}

func TestGracefulDrain(t *testing.T) {
	h := newHarness(t)
	echoRoute(t, h)
	auth := basic(h.token("exec-drain", []string{"echo.example:8443"}, time.Hour))
	c, br := openTunnel(t, h, auth)

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- h.srv.Shutdown(ctx)
	}()
	waitFor(t, 2*time.Second, "draining", h.srv.Draining)
	// New connections are refused; the open tunnel still works.
	waitFor(t, 2*time.Second, "the listener to close", func() bool {
		nc, err := net.DialTimeout("tcp", h.addr, 200*time.Millisecond)
		if err != nil {
			return true
		}
		_ = nc.Close()
		return false
	})
	_, _ = io.WriteString(c, "still")
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "still" {
		t.Fatalf("an open tunnel must keep working while the replica drains: %q %v", buf, err)
	}
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned with a tunnel open")
	case <-time.After(100 * time.Millisecond):
	}
	_ = c.Close()
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not return after the last tunnel closed")
	}
}

func TestForcedShutdownClosesTunnels(t *testing.T) {
	h := newHarness(t)
	echoRoute(t, h)
	c, br := openTunnel(t, h, basic(h.token("exec-force", []string{"echo.example:8443"}, time.Hour)))
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := br.ReadByte(); err == nil {
		t.Fatal("a forced shutdown must close open tunnels")
	}
	waitFor(t, 2*time.Second, "the shutdown outcome", func() bool {
		return strings.Contains(h.logs.String(), `"outcome":"shutdown"`)
	})
}

func TestAdminHandler(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)
	m := NewMetrics()
	srv := httptest.NewServer(AdminHandler(ready.Load, m))
	defer srv.Close()
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Fatal("healthz")
	}
	if code, _ := get("/readyz"); code != 200 {
		t.Fatal("readyz while ready")
	}
	ready.Store(false)
	if code, _ := get("/readyz"); code != 503 {
		t.Fatal("readyz while draining")
	}
	m.refusal(ReasonHostNotAllowed)
	code, body := get("/metrics")
	if code != 200 || !strings.Contains(body, `egress_proxy_refusals_total{reason="host-not-allowed"} 1`) ||
		!strings.Contains(body, `egress_proxy_connections_total{outcome="refused"} 1`) ||
		!strings.Contains(body, `egress_proxy_refusals_total{reason="token-expired"} 0`) {
		t.Fatalf("metrics:\n%s", body)
	}
}
