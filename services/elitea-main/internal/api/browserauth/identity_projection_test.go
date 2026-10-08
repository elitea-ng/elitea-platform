package browserauth

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var identityProjectionTestClock = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func newSigningTestResolver(t *testing.T, secret []byte, now time.Time) *TrustedProxyResolver {
	t.Helper()
	resolver, err := NewTrustedProxyResolver(TrustedProxyConfig{
		TrustedProxyCIDRs:        []string{"10.0.0.0/8"},
		PublicOrigin:             "https://elitea.example.test",
		IdentityProjectionSecret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolver.now = func() time.Time { return now }
	return resolver
}

func identityProjectionTestSecret() []byte {
	return bytes.Repeat([]byte{0x5a}, 32)
}

// projectedRequest builds the request elitea-main receives after the edge has
// copied the EdgeAuth response headers onto it.
func projectedRequest(method, uri string, projection http.Header) *http.Request {
	request := httptest.NewRequest(method, uri, nil)
	request.RemoteAddr = "10.1.2.3:4567"
	for name, values := range projection {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	return request
}

func signedUserProjection(t *testing.T, resolver *TrustedProxyResolver, method, uri string) http.Header {
	t.Helper()
	header := http.Header{}
	header.Set("X-Auth-Type", "user")
	header.Set("X-Auth-ID", "7")
	header.Set("X-Auth-User-ID", "7")
	if err := resolver.SignIdentityProjection(header, method, uri); err != nil {
		t.Fatal(err)
	}
	return header
}

func TestSignedIdentityProjectionIsAccepted(t *testing.T) {
	resolver := newSigningTestResolver(t, identityProjectionTestSecret(), identityProjectionTestClock)
	header := signedUserProjection(t, resolver, http.MethodGet, "/api/v2/projects?limit=5")

	request := projectedRequest(http.MethodGet, "/api/v2/projects?limit=5", header)
	if err := resolver.VerifyForwardedIdentityPeer(request); err != nil {
		t.Fatalf("VerifyForwardedIdentityPeer() = %v, want nil", err)
	}
}

func TestTokenIdentityProjectionIsAccepted(t *testing.T) {
	resolver := newSigningTestResolver(t, identityProjectionTestSecret(), identityProjectionTestClock)
	header := http.Header{}
	header.Set("X-Auth-Type", "token")
	header.Set("X-Auth-ID", "41")
	header.Set("X-Auth-User-ID", "7")
	if err := resolver.SignIdentityProjection(header, http.MethodPost, "/llm/v1/chat/completions"); err != nil {
		t.Fatal(err)
	}

	request := projectedRequest(http.MethodPost, "/llm/v1/chat/completions", header)
	if err := resolver.VerifyForwardedIdentityPeer(request); err != nil {
		t.Fatalf("VerifyForwardedIdentityPeer() = %v, want nil", err)
	}
}

// A peer inside trusted_proxy_cidrs is not proof of identity: unsigned
// X-Auth-* headers never verify, whatever the source address.
func TestTrustedPeerWithUnsignedIdentityIsRefused(t *testing.T) {
	resolver := newSigningTestResolver(t, identityProjectionTestSecret(), identityProjectionTestClock)
	for _, authType := range []string{"user", "token"} {
		header := http.Header{}
		header.Set("X-Auth-Type", authType)
		header.Set("X-Auth-ID", "1")
		header.Set("X-Auth-User-ID", "1")

		request := projectedRequest(http.MethodGet, "/api/v2/admin/users", header)
		if err := resolver.VerifyForwardedIdentityPeer(request); !errors.Is(err, ErrInvalidForwardedRequest) {
			t.Fatalf("%s: VerifyForwardedIdentityPeer() = %v, want ErrInvalidForwardedRequest", authType, err)
		}
	}
}

func TestIdentityProjectionLifetimeBoundary(t *testing.T) {
	signer := newSigningTestResolver(t, identityProjectionTestSecret(), identityProjectionTestClock)
	header := signedUserProjection(t, signer, http.MethodGet, "/api/v2/projects")

	cases := []struct {
		name    string
		elapsed time.Duration
		valid   bool
	}{
		{name: "fresh", elapsed: 0, valid: true},
		{name: "at expiry", elapsed: IdentityProjectionLifetime, valid: true},
		{name: "one second past expiry", elapsed: IdentityProjectionLifetime + time.Second, valid: false},
		{name: "verifier clock behind by the tolerated skew", elapsed: -identityProjectionClockSkew, valid: true},
		{name: "verifier clock behind by more than the skew", elapsed: -identityProjectionClockSkew - time.Second, valid: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verifier := newSigningTestResolver(t, identityProjectionTestSecret(), identityProjectionTestClock.Add(testCase.elapsed))
			err := verifier.VerifyForwardedIdentityPeer(projectedRequest(http.MethodGet, "/api/v2/projects", header))
			if testCase.valid && err != nil {
				t.Fatalf("VerifyForwardedIdentityPeer() = %v, want nil", err)
			}
			if !testCase.valid && !errors.Is(err, ErrInvalidForwardedRequest) {
				t.Fatalf("VerifyForwardedIdentityPeer() = %v, want ErrInvalidForwardedRequest", err)
			}
		})
	}
}

func TestIdentityProjectionRefusesEveryTamperedField(t *testing.T) {
	resolver := newSigningTestResolver(t, identityProjectionTestSecret(), identityProjectionTestClock)
	cases := map[string]func(*http.Request){
		"another user id":      func(r *http.Request) { r.Header.Set("X-Auth-ID", "1") },
		"another owner id":     func(r *http.Request) { r.Header.Set("X-Auth-User-ID", "1") },
		"another type":         func(r *http.Request) { r.Header.Set("X-Auth-Type", "token") },
		"removed owner id":     func(r *http.Request) { r.Header.Del("X-Auth-User-ID") },
		"another method":       func(r *http.Request) { r.Method = http.MethodDelete },
		"another route":        func(r *http.Request) { r.RequestURI = "/api/v2/admin/users" },
		"another query":        func(r *http.Request) { r.RequestURI = "/api/v2/projects?limit=6" },
		"no request target":    func(r *http.Request) { r.RequestURI = "" },
		"removed signature":    func(r *http.Request) { r.Header.Del(IdentitySignatureHeader) },
		"duplicated signature": func(r *http.Request) { r.Header.Add(IdentitySignatureHeader, r.Header.Get(IdentitySignatureHeader)) },
		"placeholder":          func(r *http.Request) { r.Header.Set(IdentitySignatureHeader, "-") },
		"unknown version": func(r *http.Request) {
			r.Header.Set(IdentitySignatureHeader, "v9"+strings.TrimPrefix(r.Header.Get(IdentitySignatureHeader), "v1"))
		},
		"extended expiry": func(r *http.Request) {
			parts := strings.Split(r.Header.Get(IdentitySignatureHeader), ".")
			expiry, _ := strconv.ParseInt(parts[1], 10, 64)
			parts[1] = strconv.FormatInt(expiry+3600, 10)
			r.Header.Set(IdentitySignatureHeader, strings.Join(parts, "."))
		},
		"truncated mac": func(r *http.Request) {
			value := r.Header.Get(IdentitySignatureHeader)
			r.Header.Set(IdentitySignatureHeader, value[:len(value)-4])
		},
		"untrusted peer": func(r *http.Request) { r.RemoteAddr = "198.51.100.9:443" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			header := signedUserProjection(t, resolver, http.MethodGet, "/api/v2/projects?limit=5")
			request := projectedRequest(http.MethodGet, "/api/v2/projects?limit=5", header)
			mutate(request)
			if err := resolver.VerifyForwardedIdentityPeer(request); !errors.Is(err, ErrInvalidForwardedRequest) {
				t.Fatalf("VerifyForwardedIdentityPeer() = %v, want ErrInvalidForwardedRequest", err)
			}
		})
	}
}

// A signature from a deployment with another key (another install, or the key
// before a rotation) never names a principal here.
func TestIdentityProjectionFromAnotherKeyIsRefused(t *testing.T) {
	other := newSigningTestResolver(t, bytes.Repeat([]byte{0x11}, 32), identityProjectionTestClock)
	header := signedUserProjection(t, other, http.MethodGet, "/api/v2/projects")

	resolver := newSigningTestResolver(t, identityProjectionTestSecret(), identityProjectionTestClock)
	err := resolver.VerifyForwardedIdentityPeer(projectedRequest(http.MethodGet, "/api/v2/projects", header))
	if !errors.Is(err, ErrInvalidForwardedRequest) {
		t.Fatalf("VerifyForwardedIdentityPeer() = %v, want ErrInvalidForwardedRequest", err)
	}
}

func TestTrustedProxyResolverRequiresAnIdentityProjectionSecret(t *testing.T) {
	for name, secret := range map[string][]byte{
		"absent": nil,
		"short":  bytes.Repeat([]byte{1}, minIdentityProjectionSecretBytes-1),
	} {
		_, err := NewTrustedProxyResolver(TrustedProxyConfig{
			TrustedProxyCIDRs:        []string{"10.0.0.0/8"},
			PublicOrigin:             "https://elitea.example.test",
			IdentityProjectionSecret: secret,
		})
		if !errors.Is(err, ErrInvalidForwardedRequest) {
			t.Fatalf("%s secret: NewTrustedProxyResolver() error = %v, want ErrInvalidForwardedRequest", name, err)
		}
	}
}

// The resolver keeps a derived key, never the caller's slice: zeroing the
// source after construction must not change what verifies.
func TestTrustedProxyResolverDoesNotAliasTheSecret(t *testing.T) {
	secret := identityProjectionTestSecret()
	resolver := newSigningTestResolver(t, secret, identityProjectionTestClock)
	header := signedUserProjection(t, resolver, http.MethodGet, "/api/v2/projects")
	for index := range secret {
		secret[index] = 0
	}
	if err := resolver.VerifyForwardedIdentityPeer(projectedRequest(http.MethodGet, "/api/v2/projects", header)); err != nil {
		t.Fatalf("VerifyForwardedIdentityPeer() = %v, want nil", err)
	}
}

func TestSignIdentityProjectionRefusesUnsignableValues(t *testing.T) {
	resolver := newSigningTestResolver(t, identityProjectionTestSecret(), identityProjectionTestClock)
	cases := map[string]struct {
		mutate      func(http.Header)
		method, uri string
	}{
		"newline in id":     {mutate: func(h http.Header) { h["X-Auth-Id"] = []string{"7\n1"} }, method: http.MethodGet, uri: "/x"},
		"duplicate id":      {mutate: func(h http.Header) { h.Add("X-Auth-ID", "1") }, method: http.MethodGet, uri: "/x"},
		"missing type":      {mutate: func(h http.Header) { h.Del("X-Auth-Type") }, method: http.MethodGet, uri: "/x"},
		"newline in uri":    {mutate: func(http.Header) {}, method: http.MethodGet, uri: "/x\n/y"},
		"empty uri":         {mutate: func(http.Header) {}, method: http.MethodGet, uri: ""},
		"newline in method": {mutate: func(http.Header) {}, method: "GET\n", uri: "/x"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			header := http.Header{}
			header.Set("X-Auth-Type", "user")
			header.Set("X-Auth-ID", "7")
			header.Set("X-Auth-User-ID", "7")
			testCase.mutate(header)
			if err := resolver.SignIdentityProjection(header, testCase.method, testCase.uri); err == nil {
				t.Fatal("SignIdentityProjection() = nil, want an error")
			}
			if header.Get(IdentitySignatureHeader) != "" {
				t.Fatal("a refused projection must not carry a signature")
			}
		})
	}
}
