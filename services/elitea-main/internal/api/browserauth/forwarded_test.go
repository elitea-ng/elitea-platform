package browserauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTrustedProxyResolverNormalizesEdgeAuthSource(t *testing.T) {
	resolver := newTestTrustedProxyResolver(t)
	request := forwardedTestRequest("/auth/check?target=rpc&scope=admin")
	request.RemoteAddr = "10.20.30.40:43120"
	request.Header.Set("X-Forwarded-For", "198.51.100.8, 10.9.8.7")

	got, err := resolver.Resolve(request)
	if err != nil {
		t.Fatal(err)
	}
	want := ForwardedRequest{
		Method:        http.MethodPost,
		Proto:         "https",
		Host:          "elitea.example.test",
		URI:           "/api/v2/configurations?project=7",
		ClientIP:      "198.51.100.8",
		Target:        "rpc",
		TargetPresent: true,
		Scope:         "admin",
		ScopePresent:  true,
	}
	if got != want {
		t.Fatalf("Resolve() = %+v, want %+v", got, want)
	}
	clientKey, err := resolver.ResolveClientKey(request)
	if err != nil || clientKey != want.ClientIP {
		t.Fatalf("ResolveClientKey() = %q, %v; want %q", clientKey, err, want.ClientIP)
	}
}

func TestTrustedProxyResolverPreservesAbsentAndEmptyTargetDistinction(t *testing.T) {
	resolver := newTestTrustedProxyResolver(t)

	absent, err := resolver.Resolve(forwardedTestRequest("/auth/check"))
	if err != nil {
		t.Fatal(err)
	}
	if absent.TargetPresent || absent.Target != "" {
		t.Fatalf("absent target = %q present=%v", absent.Target, absent.TargetPresent)
	}

	empty, err := resolver.Resolve(forwardedTestRequest("/auth/check?target="))
	if err != nil {
		t.Fatal(err)
	}
	if !empty.TargetPresent || empty.Target != "" {
		t.Fatalf("empty target = %q present=%v", empty.Target, empty.TargetPresent)
	}
}

func TestTrustedProxyResolverUsesNearestUntrustedForwardedHop(t *testing.T) {
	resolver := newTestTrustedProxyResolver(t)
	request := forwardedTestRequest("/auth/check")
	// The left-most value is attacker supplied. Because the next hop is not in
	// the trusted proxy set, it is the effective client boundary.
	request.Header.Set("X-Forwarded-For", "203.0.113.99, 198.51.100.44")

	clientKey, err := resolver.ResolveClientKey(request)
	if err != nil || clientKey != "198.51.100.44" {
		t.Fatalf("ResolveClientKey() = %q, %v; want nearest untrusted hop", clientKey, err)
	}
}

// One IPv6 subscriber holds at least a /64 and can rotate through it, so the
// attempt limiter keys IPv6 clients by /64. IPv4 keys the address; an
// IPv4-mapped IPv6 address keys as its IPv4 address.
func TestTrustedProxyResolverGroupsIPv6ClientKeysByPrefix64(t *testing.T) {
	resolver := newTestTrustedProxyResolver(t)
	for _, test := range []struct {
		forwarded string
		want      string
	}{
		{forwarded: "2001:db8:1:2:aaaa:bbbb:cccc:dddd", want: "2001:db8:1:2::/64"},
		{forwarded: "2001:db8:1:2::1", want: "2001:db8:1:2::/64"},
		{forwarded: "2001:db8:1:2:ffff:ffff:ffff:ffff", want: "2001:db8:1:2::/64"},
		{forwarded: "2001:db8:1:3::1", want: "2001:db8:1:3::/64"},
		{forwarded: "198.51.100.44", want: "198.51.100.44"},
		{forwarded: "::ffff:198.51.100.44", want: "198.51.100.44"},
	} {
		request := forwardedTestRequest("/auth/check")
		request.Header.Set("X-Forwarded-For", test.forwarded)
		clientKey, err := resolver.ResolveClientKey(request)
		if err != nil || clientKey != test.want {
			t.Fatalf("ResolveClientKey(%s) = %q, %v; want %q", test.forwarded, clientKey, err, test.want)
		}
		// The forwarded identity keeps the full address; only the limiter
		// key is grouped.
		forwarded, err := resolver.Resolve(request)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(test.forwarded, ":") && !strings.HasPrefix(test.forwarded, "::ffff:") &&
			forwarded.ClientIP == test.want {
			t.Fatalf("Resolve(%s).ClientIP = %q, want the full address", test.forwarded, forwarded.ClientIP)
		}
	}
}

func TestTrustedProxyResolverVerifiesOnlyConfiguredImmediatePeerForIdentity(t *testing.T) {
	resolver := newTestTrustedProxyResolver(t)
	trusted := httptest.NewRequest(http.MethodGet, "/projects/project/default/1", nil)
	trusted.RemoteAddr = "10.20.30.40:43120"
	trusted.Header.Set("X-Forwarded-For", "not-part-of-identity-peer-proof")
	if err := resolver.VerifyForwardedIdentityPeer(trusted); err != nil {
		t.Fatalf("trusted peer error = %v", err)
	}

	for _, request := range []*http.Request{
		nil,
		func() *http.Request {
			request := trusted.Clone(trusted.Context())
			request.RemoteAddr = "192.0.2.10:443"
			return request
		}(),
		func() *http.Request {
			request := trusted.Clone(trusted.Context())
			request.RemoteAddr = "not-an-address"
			return request
		}(),
	} {
		if err := resolver.VerifyForwardedIdentityPeer(request); !errors.Is(err, ErrInvalidForwardedRequest) {
			t.Fatalf("untrusted peer error = %v, want %v", err, ErrInvalidForwardedRequest)
		}
	}
}

func TestTrustedProxyResolverRejectsUntrustedOrAmbiguousInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "untrusted peer", mutate: func(request *http.Request) { request.RemoteAddr = "192.0.2.10:443" }},
		{name: "missing forwarded method", mutate: func(request *http.Request) { request.Header.Del("X-Forwarded-Method") }},
		{name: "empty forwarded method", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-Method", "") }},
		{name: "invalid forwarded method", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-Method", "GET POST") }},
		{name: "scheme mismatch", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-Proto", "http") }},
		{name: "host mismatch", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-Host", "attacker.test") }},
		{name: "absolute uri", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-Uri", "https://attacker.test/") }},
		{name: "network path uri", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-Uri", "//attacker.test/") }},
		{name: "malformed client chain", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-For", "not-an-ip") }},
		{name: "empty client chain hop", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-For", "198.51.100.8,") }},
		{name: "duplicate target", mutate: func(request *http.Request) { request.URL.RawQuery = "target=rpc&target=json" }},
		{name: "malformed query", mutate: func(request *http.Request) { request.URL.RawQuery = "target=%zz" }},
		{name: "oversized query", mutate: func(request *http.Request) {
			request.URL.RawQuery = "ignored=" + strings.Repeat("a", maxAuthQueryBytes)
		}},
		{name: "too many query values", mutate: func(request *http.Request) {
			request.URL.RawQuery = strings.Repeat("ignored=value&", maxAuthQueryValues) + "ignored=value"
		}},
		{name: "duplicate differently-cased header", mutate: func(request *http.Request) {
			request.Header["x-forwarded-host"] = []string{"elitea.example.test"}
		}},
		{name: "oversized forwarded chain", mutate: func(request *http.Request) {
			request.Header.Set("X-Forwarded-For", strings.Repeat("1", maxForwardedForBytes+1))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := newTestTrustedProxyResolver(t)
			request := forwardedTestRequest("/auth/check")
			test.mutate(request)
			if _, err := resolver.Resolve(request); !errors.Is(err, ErrInvalidForwardedRequest) {
				t.Fatalf("Resolve() error = %v, want %v", err, ErrInvalidForwardedRequest)
			}
		})
	}
}

func TestTrustedProxyResolverRejectsUnsafeConfiguration(t *testing.T) {
	tests := []TrustedProxyConfig{
		{PublicOrigin: "https://elitea.example.test"},
		{TrustedProxyCIDRs: []string{"not-a-cidr"}, PublicOrigin: "https://elitea.example.test"},
		{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, PublicOrigin: "http://elitea.example.test"},
		{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, PublicOrigin: "https://elitea.example.test/path"},
		{TrustedProxyCIDRs: []string{"10.0.0.0/8"}, PublicOrigin: "https://user@elitea.example.test"},
	}
	for _, config := range tests {
		if _, err := NewTrustedProxyResolver(config); !errors.Is(err, ErrInvalidForwardedRequest) {
			t.Fatalf("NewTrustedProxyResolver(%+v) error = %v", config, err)
		}
	}

	development, err := NewTrustedProxyResolver(TrustedProxyConfig{
		TrustedProxyCIDRs: []string{"127.0.0.0/8"},
		PublicOrigin:      "http://localhost:8080",
		Development:       true,
	})
	if err != nil || development.publicScheme != "http" {
		t.Fatalf("development resolver = %+v, %v", development, err)
	}
}

func newTestTrustedProxyResolver(t *testing.T) *TrustedProxyResolver {
	t.Helper()
	resolver, err := NewTrustedProxyResolver(TrustedProxyConfig{
		TrustedProxyCIDRs: []string{"10.0.0.0/8"},
		PublicOrigin:      "https://elitea.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func forwardedTestRequest(target string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.RemoteAddr = "10.20.30.40:43120"
	request.Header.Set("X-Forwarded-Method", http.MethodPost)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("X-Forwarded-Host", "elitea.example.test")
	request.Header.Set("X-Forwarded-Uri", "/api/v2/configurations?project=7")
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	return request
}
