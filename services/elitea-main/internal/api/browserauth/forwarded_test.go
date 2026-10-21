package browserauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTrustedProxyResolverResolvesClientBehindTrustedHops(t *testing.T) {
	resolver := newTestTrustedProxyResolver(t)
	request := forwardedTestRequest("/auth/login")
	request.RemoteAddr = "10.20.30.40:43120"
	request.Header.Set("X-Forwarded-For", "198.51.100.8, 10.9.8.7")

	clientKey, err := resolver.ResolveClientKey(request)
	if err != nil || clientKey != "198.51.100.8" {
		t.Fatalf("ResolveClientKey() = %q, %v; want 198.51.100.8", clientKey, err)
	}
}

func TestTrustedProxyResolverUsesNearestUntrustedForwardedHop(t *testing.T) {
	resolver := newTestTrustedProxyResolver(t)
	request := forwardedTestRequest("/auth/login")
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
		request := forwardedTestRequest("/auth/login")
		request.Header.Set("X-Forwarded-For", test.forwarded)
		clientKey, err := resolver.ResolveClientKey(request)
		if err != nil || clientKey != test.want {
			t.Fatalf("ResolveClientKey(%s) = %q, %v; want %q", test.forwarded, clientKey, err, test.want)
		}
	}
}

func TestTrustedProxyResolverRejectsUntrustedOrAmbiguousInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "untrusted peer", mutate: func(request *http.Request) { request.RemoteAddr = "192.0.2.10:443" }},
		{name: "unparseable peer", mutate: func(request *http.Request) { request.RemoteAddr = "not-an-address" }},
		{name: "missing client chain", mutate: func(request *http.Request) { request.Header.Del("X-Forwarded-For") }},
		{name: "malformed client chain", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-For", "not-an-ip") }},
		{name: "empty client chain hop", mutate: func(request *http.Request) { request.Header.Set("X-Forwarded-For", "198.51.100.8,") }},
		{name: "duplicate differently-cased header", mutate: func(request *http.Request) {
			request.Header["x-forwarded-for"] = []string{"198.51.100.9"}
		}},
		{name: "oversized forwarded chain", mutate: func(request *http.Request) {
			request.Header.Set("X-Forwarded-For", strings.Repeat("1", maxForwardedForBytes+1))
		}},
		{name: "too many forwarded hops", mutate: func(request *http.Request) {
			request.Header.Set("X-Forwarded-For", strings.TrimSuffix(strings.Repeat("10.0.0.1,", maxForwardedHops+1), ","))
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := newTestTrustedProxyResolver(t)
			request := forwardedTestRequest("/auth/login")
			test.mutate(request)
			if _, err := resolver.ResolveClientKey(request); !errors.Is(err, ErrInvalidForwardedRequest) {
				t.Fatalf("ResolveClientKey() error = %v, want %v", err, ErrInvalidForwardedRequest)
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

	if _, err := NewTrustedProxyResolver(TrustedProxyConfig{
		TrustedProxyCIDRs: []string{"127.0.0.0/8"},
		PublicOrigin:      "http://localhost:8080",
		Development:       true,
	}); err != nil {
		t.Fatalf("development resolver: %v", err)
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
	request := httptest.NewRequest(http.MethodPost, target, nil)
	request.RemoteAddr = "10.20.30.40:43120"
	request.Header.Set("X-Forwarded-For", "198.51.100.8")
	return request
}
