package egress

// Hardening coverage (Point 5 Track M1): the address classes, proxy,
// redirect, header-size, HTTPS-policy, TLS and resolution bounds that make
// this guard the only egress path future HTTP actions may use.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
)

func privateAllowlist(t *testing.T, entries ...string) *egresslib.Allowlist {
	t.Helper()
	if len(entries) == 0 {
		entries = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8"}
	}
	list, err := egresslib.Parse(entries)
	if err != nil {
		t.Fatalf("parse test allowlist: %v", err)
	}
	return list
}

func singleHostGuard(t *testing.T, allowlist *egresslib.Allowlist, host, ip string, opts ...Option) *Guard {
	t.Helper()
	resolver := fakeIPResolver{ips: map[string][]net.IPAddr{host: {{IP: net.ParseIP(ip)}}}}
	return New(allowlist, append([]Option{WithResolver(resolver)}, opts...)...)
}

// TestGuardAddressClasses is the boundary table for every special-purpose
// class. Each class is checked as an IPv4 literal, as its IPv4-mapped IPv6
// form and, where it embeds IPv4, through NAT64 64:ff9b::/96, with the first
// and last address of each range and the neighbours just outside it.
func TestGuardAddressClasses(t *testing.T) {
	const (
		public    = "public"
		liftable  = "liftable" // refused unless the allowlist declares private egress
		alwaysOff = "always"   // refused whatever the allowlist says
	)
	cases := []struct {
		ip    string
		class string
	}{
		// 0.0.0.0/8 "this network".
		{"0.0.0.1", alwaysOff},
		{"0.255.255.255", alwaysOff},
		{"::ffff:0.1.2.3", alwaysOff},
		{"64:ff9b::0.1.2.3", alwaysOff},
		{"1.0.0.0", public},
		// 100.64.0.0/10 CGNAT, and the metadata endpoint inside it.
		{"100.63.255.255", public},
		{"100.64.0.0", liftable},
		{"100.127.255.255", liftable},
		{"100.128.0.0", public},
		{"::ffff:100.64.0.1", liftable},
		{"64:ff9b::100.64.0.1", liftable},
		{"100.100.100.200", alwaysOff},
		{"::ffff:100.100.100.200", alwaysOff},
		{"64:ff9b::100.100.100.200", alwaysOff},
		// 192.0.0.0/24 IETF protocol assignments.
		{"191.255.255.255", public},
		{"192.0.0.0", alwaysOff},
		{"192.0.0.255", alwaysOff},
		{"192.0.1.0", public},
		{"::ffff:192.0.0.8", alwaysOff},
		{"64:ff9b::192.0.0.170", alwaysOff},
		// 198.18.0.0/15 benchmarking.
		{"198.17.255.255", public},
		{"198.18.0.0", liftable},
		{"198.19.255.255", liftable},
		{"198.20.0.0", public},
		{"::ffff:198.18.0.1", liftable},
		{"64:ff9b::198.19.0.1", liftable},
		// 240.0.0.0/4 reserved, broadcast included.
		{"223.255.255.255", public},
		{"240.0.0.0", alwaysOff},
		{"255.255.255.255", alwaysOff},
		{"::ffff:240.0.0.1", alwaysOff},
		{"64:ff9b::250.1.2.3", alwaysOff},
		// NAT64 embedding the classes the guard already refused.
		{"64:ff9b::127.0.0.1", liftable},
		{"64:ff9b::10.0.0.5", liftable},
		{"64:ff9b::169.254.169.254", alwaysOff},
		{"64:ff9b::224.0.0.1", alwaysOff},
		{"64:ff9b::93.184.216.34", public},
		{"64:ff9b:1::a00:5", alwaysOff}, // local-use NAT64 (RFC 8215)
		// IPv6 forms that reach an embedded or deprecated IPv4 address.
		{"::169.254.169.254", alwaysOff}, // IPv4-compatible (deprecated)
		{"2002:a9fe:a9fe::1", alwaysOff}, // 6to4 of 169.254.169.254
		{"2002:a00:5::1", liftable},      // 6to4 of 10.0.0.5
		{"2002:5db8:d822::1", public},    // 6to4 of 93.184.216.34
		{"2001::1", alwaysOff},           // Teredo
		{"100::1", alwaysOff},            // discard-only
		{"fd00:ec2::254", alwaysOff},     // AWS IMDS over IPv6, inside fc00::/7
		{"fec0::1", liftable},            // deprecated site-local
		{"168.63.129.16", alwaysOff},     // Azure platform endpoint
		{"2606:4700:4700::1111", public},
		{"93.184.216.34", public},
		{"::ffff:93.184.216.34", public},
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			if net.ParseIP(tc.ip) == nil {
				t.Fatalf("bad fixture %q", tc.ip)
			}
			for _, allowPrivate := range []bool{false, true} {
				var allowlist *egresslib.Allowlist
				if allowPrivate {
					allowlist = privateAllowlist(t)
				}
				g := singleHostGuard(t, allowlist, "target.example", tc.ip)
				err := g.Validate(context.Background(), "https://target.example/hook")
				wantAllowed := tc.class == public || tc.class == liftable && allowPrivate
				if wantAllowed && err != nil {
					t.Fatalf("allowPrivate=%v: Validate refused %s: %v", allowPrivate, tc.ip, err)
				}
				if !wantAllowed && !errors.Is(err, ErrDestinationRefused) {
					t.Fatalf("allowPrivate=%v: Validate(%s, %s class) = %v, want ErrDestinationRefused",
						allowPrivate, tc.ip, tc.class, err)
				}
			}
		})
	}
}

// TestGuardAllowlistNamingANewPrivateClassDeclaresPrivateEgress keeps the
// operator escape hatch honest for the classes this change starts refusing:
// an entry naming a CGNAT or benchmarking address declares that private
// egress is intended, exactly like an RFC 1918 entry. Metadata and reserved
// addresses stay refused even when an entry names them.
func TestGuardAllowlistNamingANewPrivateClassDeclaresPrivateEgress(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		ip      string
		allowed bool
	}{
		{"CGNAT CIDR entry", []string{"100.64.0.0/10"}, "100.64.1.5", true},
		{"CGNAT IP entry with port", []string{"100.64.1.5:8080"}, "100.64.1.5", true},
		{"benchmarking CIDR entry", []string{"198.18.0.0/15"}, "198.18.0.9", true},
		{"public-only allowlist", []string{"api.example.com"}, "100.64.1.5", false},
		{"entry naming the metadata address", []string{"100.100.100.200"}, "100.100.100.200", false},
		{"entry naming reserved space", []string{"240.0.0.0/4"}, "240.0.0.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := singleHostGuard(t, privateAllowlist(t, tc.entries...), "target.example", tc.ip)
			err := g.Validate(context.Background(), "http://target.example/")
			if tc.allowed != (err == nil) {
				t.Fatalf("Validate = %v, want allowed=%v", err, tc.allowed)
			}
		})
	}
}

func loopbackTarget(t *testing.T, server *httptest.Server, opts ...Option) (*Guard, string) {
	t.Helper()
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	return singleHostGuard(t, privateAllowlist(t), "receiver.example", host, opts...), port
}

// TestGuardTransportIgnoresProxyEnvironment proves the proxy bypass is
// closed: with HTTP_PROXY set, a guarded request still dials the checked
// target directly and the proxy never sees it. With ProxyFromEnvironment the
// guard would check and dial the proxy's address instead of the target's.
func TestGuardTransportIgnoresProxyEnvironment(t *testing.T) {
	var proxyHits, targetHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	g, port := loopbackTarget(t, target)
	if g.Transport().Proxy != nil {
		t.Fatal("guarded transport has a Proxy function; it must dial the checked target directly")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://receiver.example:"+port+"/", nil)
	resp, err := g.RoundTripper().RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	_ = resp.Body.Close()
	if proxyHits.Load() != 0 || targetHits.Load() != 1 {
		t.Fatalf("proxy hits=%d target hits=%d, want 0 and 1", proxyHits.Load(), targetHits.Load())
	}
}

// TestGuardRoundTripperReturnsRedirectUnfollowed proves the transport-only
// constructor cannot follow a redirect: the 3xx goes back to the caller and
// the Location is never dialled.
func TestGuardRoundTripperReturnsRedirectUnfollowed(t *testing.T) {
	var followed atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/latest/meta-data/", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	g, port := loopbackTarget(t, redirector)
	rt := g.RoundTripper()
	req, _ := http.NewRequest(http.MethodPost, "http://receiver.example:"+port+"/hook", strings.NewReader("{}"))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want the 307 returned to the caller", resp.StatusCode)
	}
	if followed.Load() != 0 {
		t.Fatal("the redirect target was dialled")
	}
}

// TestGuardTransportCapsResponseHeaders proves the 64 KiB response-header
// bound: a header under the cap is read, one over it is refused.
func TestGuardTransportCapsResponseHeaders(t *testing.T) {
	const want = 64 << 10
	if got := New(nil).Transport().MaxResponseHeaderBytes; got != want {
		t.Fatalf("MaxResponseHeaderBytes = %d, want %d", got, want)
	}
	for _, tc := range []struct {
		size    int
		allowed bool
	}{{32 << 10, true}, {128 << 10, false}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Big", strings.Repeat("a", tc.size))
			w.WriteHeader(http.StatusOK)
		}))
		g, port := loopbackTarget(t, server)
		req, _ := http.NewRequest(http.MethodGet, "http://receiver.example:"+port+"/", nil)
		resp, err := g.RoundTripper().RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		server.Close()
		if tc.allowed != (err == nil) {
			t.Fatalf("header of %d bytes: err=%v, want allowed=%v", tc.size, err, tc.allowed)
		}
	}
}

// TestGuardRequireHTTPSRefusesPlainHTTP proves the per-caller HTTPS policy at
// both layers: Validate refuses an http URL, and the round tripper refuses an
// http request before any connection is opened.
func TestGuardRequireHTTPSRefusesPlainHTTP(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	g, port := loopbackTarget(t, server, RequireHTTPS())

	if err := g.Validate(context.Background(), "http://receiver.example/hook"); !errors.Is(err, ErrDestinationRefused) {
		t.Fatalf("Validate(http) = %v, want ErrDestinationRefused", err)
	}
	if err := g.Validate(context.Background(), "https://receiver.example/hook"); err != nil {
		t.Fatalf("Validate(https) = %v, want nil", err)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://receiver.example:"+port+"/", nil)
	if _, err := g.RoundTripper().RoundTrip(req); !errors.Is(err, ErrDestinationRefused) {
		t.Fatalf("RoundTrip(http) = %v, want ErrDestinationRefused", err)
	}
	if hits.Load() != 0 {
		t.Fatal("an http request was sent under an HTTPS-only policy")
	}

	// Without the policy http stays allowed: no behaviour change for the
	// callers (webhooks, MCP) that accept http destinations today.
	plain, _ := loopbackTarget(t, server)
	if err := plain.Validate(context.Background(), "http://receiver.example/hook"); err != nil {
		t.Fatalf("default policy refused http: %v", err)
	}
}

// TestGuardVerifiesTLSAgainstTheURLHost proves that dialling a pinned IP
// literal does not weaken certificate checks: the certificate is verified
// against the URL's host name, so a name the certificate does not cover fails
// even though the TCP connection reached the right server.
func TestGuardVerifiesTLSAgainstTheURLHost(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	host, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	g := New(privateAllowlist(t), WithResolver(fakeIPResolver{ips: map[string][]net.IPAddr{
		"example.com":   {{IP: net.ParseIP(host)}}, // covered by httptest's certificate
		"wrong.example": {{IP: net.ParseIP(host)}}, // not covered
	}}))
	transport := g.Transport()
	if transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("guarded transport skips TLS verification")
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}

	ok, _ := http.NewRequest(http.MethodGet, "https://example.com:"+port+"/", nil)
	resp, err := transport.RoundTrip(ok)
	if err != nil {
		t.Fatalf("matching host: %v", err)
	}
	_ = resp.Body.Close()

	bad, _ := http.NewRequest(http.MethodGet, "https://wrong.example:"+port+"/", nil)
	var hostErr x509.HostnameError
	if _, err := transport.RoundTrip(bad); !errors.As(err, &hostErr) {
		t.Fatalf("mismatched host: err=%v, want x509.HostnameError", err)
	}
}

// TestGuardBoundsResolution proves resolution is bounded: at most
// MaxResolvedAddresses answers are accepted (limit passes, limit+1 is
// refused), zoned addresses are never dialled, and only TCP is dialled.
func TestGuardBoundsResolution(t *testing.T) {
	answers := func(n int) []net.IPAddr {
		out := make([]net.IPAddr, n)
		for i := range out {
			out[i] = net.IPAddr{IP: net.IPv4(93, 184, 216, byte(i+1))}
		}
		return out
	}
	g := New(nil, WithResolver(fakeIPResolver{ips: map[string][]net.IPAddr{
		"limit.example": answers(MaxResolvedAddresses),
		"over.example":  answers(MaxResolvedAddresses + 1),
		"zoned.example": {{IP: net.ParseIP("2606:4700:4700::1111"), Zone: "eth0"}},
	}}))
	if err := g.Validate(context.Background(), "https://limit.example/"); err != nil {
		t.Fatalf("%d answers refused: %v", MaxResolvedAddresses, err)
	}
	if err := g.Validate(context.Background(), "https://over.example/"); !errors.Is(err, ErrDestinationRefused) {
		t.Fatalf("%d answers = %v, want ErrDestinationRefused", MaxResolvedAddresses+1, err)
	}
	if err := g.Validate(context.Background(), "https://zoned.example/"); !errors.Is(err, ErrDestinationRefused) {
		t.Fatalf("zoned answer = %v, want ErrDestinationRefused", err)
	}
	if _, err := g.DialContext(context.Background(), "udp", "limit.example:53"); !errors.Is(err, ErrDestinationRefused) {
		t.Fatalf("udp dial = %v, want ErrDestinationRefused", err)
	}
}

// TestGuardRoundTripperReleasesIdleConnections proves an owner can release
// the guarded transport's pooled connections through http.Client.
func TestGuardRoundTripperReleasesIdleConnections(t *testing.T) {
	if _, ok := New(nil).RoundTripper().(interface{ CloseIdleConnections() }); !ok {
		t.Fatal("RoundTripper does not expose CloseIdleConnections")
	}
}

// TestGuardFilterBudget pins the per-dial cost of the filter: classifying an
// address allocates nothing, and filtering a full MaxResolvedAddresses answer
// allocates only the result slice.
func TestGuardFilterBudget(t *testing.T) {
	public := net.ParseIP("93.184.216.34")
	if allocs := testing.AllocsPerRun(100, func() { _ = classify(public) }); allocs != 0 {
		t.Fatalf("classify allocates %.0f times per call, want 0", allocs)
	}
	ips := make([]net.IP, MaxResolvedAddresses)
	for i := range ips {
		ips[i] = net.IPv4(93, 184, 216, byte(i+1))
	}
	g := New(privateAllowlist(t))
	if allocs := testing.AllocsPerRun(100, func() { _ = g.permittedIPs(ips) }); allocs > 1 {
		t.Fatalf("permittedIPs allocates %.0f times for %d answers, want at most 1", allocs, MaxResolvedAddresses)
	}
}

func BenchmarkGuardPermittedIPs(b *testing.B) {
	ips := make([]net.IP, MaxResolvedAddresses)
	for i := range ips {
		ips[i] = net.IPv4(93, 184, 216, byte(i+1))
	}
	g := New(nil)
	b.ReportAllocs()
	for b.Loop() {
		_ = g.permittedIPs(ips)
	}
}

// TestGuardRefusalsNeverEchoURLSecrets proves a refusal carries at most the
// host: the MCP proxies log it and the webhook delivery log stores it, so
// userinfo, path and query (where tokens live) must never reach the message.
func TestGuardRefusalsNeverEchoURLSecrets(t *testing.T) {
	const secret = "s3cr3t-token"
	g := singleHostGuard(t, nil, "internal.example", "10.0.0.5", RequireHTTPS())
	for _, raw := range []string{
		"http://user:" + secret + "@[::1/hook?token=" + secret,           // unparseable
		"https://user:" + secret + "@internal.example/p?token=" + secret, // private
		"http://user:" + secret + "@internal.example/?token=" + secret,   // https-only policy
		"ftp://user:" + secret + "@internal.example/" + secret,           // scheme
		"https://user:" + secret + "@unknown.example/?token=" + secret,   // no DNS answer
	} {
		err := g.Validate(context.Background(), raw)
		if !errors.Is(err, ErrDestinationRefused) {
			t.Fatalf("Validate = %v, want a refusal", err)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("refusal echoes URL secrets: %q", err.Error())
		}
	}
}

// TestGuardRequireHTTPSHoldsOnTheRawTransport proves the HTTPS-only policy is
// enforced by the *http.Transport itself, so a caller that needs the concrete
// transport cannot bypass it, and that the policy still never uses a proxy.
func TestGuardRequireHTTPSHoldsOnTheRawTransport(t *testing.T) {
	var proxyHits, hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	g, port := loopbackTarget(t, server, RequireHTTPS())

	req, _ := http.NewRequest(http.MethodGet, "http://receiver.example:"+port+"/", nil)
	if _, err := g.Transport().RoundTrip(req); !errors.Is(err, ErrDestinationRefused) {
		t.Fatalf("Transport().RoundTrip(http) = %v, want ErrDestinationRefused", err)
	}
	if hits.Load() != 0 || proxyHits.Load() != 0 {
		t.Fatalf("target hits=%d proxy hits=%d, want 0 and 0", hits.Load(), proxyHits.Load())
	}
}

// TestGuardRoundTripperRefusesANilURL proves a malformed request is a typed
// error, not a panic on the request path.
func TestGuardRoundTripperRefusesANilURL(t *testing.T) {
	for _, g := range []*Guard{New(nil), New(nil, RequireHTTPS())} {
		if _, err := g.RoundTripper().RoundTrip(&http.Request{Method: http.MethodGet}); err == nil {
			t.Fatal("RoundTrip(nil URL) = nil error, want an error")
		}
	}
}

// TestGuardTransportSurvivesAReplacedDefaultTransport proves building a guard
// transport never panics when another package has wrapped
// http.DefaultTransport (instrumentation does this), and stays hardened.
func TestGuardTransportSurvivesAReplacedDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })
	defer func() { http.DefaultTransport = original }()

	transport := New(nil).Transport()
	if transport.Proxy != nil || transport.MaxResponseHeaderBytes != MaxResponseHeaderBytes || transport.DialContext == nil {
		t.Fatal("guard transport built from a replaced DefaultTransport is not hardened")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
