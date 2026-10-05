package publicorigin

import (
	"crypto/tls"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestNormalize(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		dropped  bool
		wantErr  bool
	}{
		{in: "", want: ""},
		{in: "   ", want: ""},
		{in: "https://elitea.example.com", want: "https://elitea.example.com"},
		{in: "https://elitea.example.com/", want: "https://elitea.example.com"},
		{in: "HTTPS://Elitea.Example.COM:8443", want: "https://elitea.example.com:8443"},
		{in: "http://localhost:8080", want: "http://localhost:8080"},
		// A default port is not part of a browser's serialized Origin, so it
		// must not be part of ours: the consent POST compares them exactly.
		{in: "https://elitea.example.com:443", want: "https://elitea.example.com"},
		{in: "HTTPS://Elitea.Example.COM:443/", want: "https://elitea.example.com"},
		{in: "http://elitea.example.com:80", want: "http://elitea.example.com"},
		{in: "https://elitea.example.com:", want: "https://elitea.example.com"},
		{in: "http://elitea.example.com:443", want: "http://elitea.example.com:443"},
		{in: "https://elitea.example.com:80", want: "https://elitea.example.com:80"},
		{in: "https://[2001:db8::1]:443", want: "https://[2001:db8::1]"},
		{in: "https://[2001:db8::1]:8443", want: "https://[2001:db8::1]:8443"},
		{in: "https://elitea.example.com/base", want: "https://elitea.example.com", dropped: true},
		{in: "elitea.example.com", wantErr: true},
		{in: "ftp://elitea.example.com", wantErr: true},
		{in: "https://", wantErr: true},
		{in: "https://user:pw@elitea.example.com", wantErr: true},
		{in: "https://elitea.example.com/?x=1", wantErr: true},
		{in: "https://elitea.example.com/#frag", wantErr: true},
		{in: "https://elitea.example.com?", wantErr: true},
		{in: "://bad", wantErr: true},
	} {
		got, dropped, err := Normalize(tc.in)
		if tc.wantErr {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Normalize(%q) err = %v, want ErrInvalid", tc.in, err)
			}
			continue
		}
		if err != nil || got != tc.want || dropped != tc.dropped {
			t.Errorf("Normalize(%q) = %q, %v, %v; want %q, %v, nil", tc.in, got, dropped, err, tc.want, tc.dropped)
		}
	}
}

func TestFromRequest(t *testing.T) {
	plain := httptest.NewRequest("GET", "http://Elitea.Local:8080/x", nil)
	if got := FromRequest(plain); got != "http://elitea.local:8080" {
		t.Errorf("plain = %q", got)
	}

	tlsReq := httptest.NewRequest("GET", "https://elitea.local/x", nil)
	tlsReq.TLS = &tls.ConnectionState{}
	if got := FromRequest(tlsReq); got != "https://elitea.local" {
		t.Errorf("tls = %q", got)
	}

	defaultPort := httptest.NewRequest("GET", "https://elitea.local:443/x", nil)
	defaultPort.TLS = &tls.ConnectionState{}
	if got := FromRequest(defaultPort); got != "https://elitea.local" {
		t.Errorf("default port = %q, want it dropped", got)
	}

	proxied := httptest.NewRequest("GET", "http://elitea.local/x", nil)
	proxied.Header.Set("X-Forwarded-Proto", "https")
	// X-Forwarded-Host is a caller-chosen header and must never select the
	// host of an anonymous cacheable body.
	proxied.Header.Set("X-Forwarded-Host", "evil.example")
	if got := FromRequest(proxied); got != "https://elitea.local" {
		t.Errorf("proxied = %q, want the request Host with the forwarded scheme", got)
	}

	ambiguous := httptest.NewRequest("GET", "http://elitea.local/x", nil)
	ambiguous.Header.Add("X-Forwarded-Proto", "https")
	ambiguous.Header.Add("X-Forwarded-Proto", "http")
	if got := FromRequest(ambiguous); got != "http://elitea.local" {
		t.Errorf("ambiguous = %q, want http", got)
	}
}

func TestResolve(t *testing.T) {
	r := httptest.NewRequest("GET", "http://elitea.local/x", nil)
	if got, from := Resolve("https://fixed.example", r); got != "https://fixed.example" || from {
		t.Errorf("configured: %q, %v", got, from)
	}
	if got, from := Resolve("", r); got != "http://elitea.local" || !from {
		t.Errorf("fallback: %q, %v", got, from)
	}
}
