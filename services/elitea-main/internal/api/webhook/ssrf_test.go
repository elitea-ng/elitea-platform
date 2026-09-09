package webhook

// Unit coverage for DestinationGuard: the table-driven validator (Validate,
// used by Create/Update) and the dial-time pinning path (dialContext, used by
// the Dispatcher's guarded Transport). Every case here uses a fakeIPResolver
// (handler_test.go) so no case depends on the test runner's real DNS.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
)

func addr(ip string) net.IPAddr { return net.IPAddr{IP: net.ParseIP(ip)} }

func guardWithHosts(t *testing.T, allowPrivate bool, hosts map[string]string) *DestinationGuard {
	t.Helper()
	ips := make(map[string][]net.IPAddr, len(hosts))
	for host, ip := range hosts {
		ips[host] = []net.IPAddr{addr(ip)}
	}
	var allowlist *egresslib.Allowlist
	if allowPrivate {
		list, err := egresslib.Parse([]string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8"})
		if err != nil {
			t.Fatalf("parse test allowlist: %v", err)
		}
		allowlist = list
	}
	return NewDestinationGuardWithResolver(allowlist, fakeIPResolver{ips: ips})
}

func TestDestinationGuardValidateTable(t *testing.T) {
	cases := []struct {
		name         string
		url          string
		host         string
		ip           string
		allowPrivate bool
		wantErr      bool
	}{
		{name: "ordinary public host is allowed", url: "https://receiver.example/hook", host: "receiver.example", ip: "93.184.216.34", wantErr: false},
		{name: "loopback IP literal is refused by default", url: "http://127.0.0.1:8080/hook", host: "127.0.0.1", ip: "127.0.0.1", wantErr: true},
		{name: "loopback IP literal is allowed when the allowlist permits private egress", url: "http://127.0.0.1:8080/hook", host: "127.0.0.1", ip: "127.0.0.1", allowPrivate: true, wantErr: false},
		{name: "localhost is treated as loopback", url: "http://localhost/hook", host: "localhost", ip: "127.0.0.1", wantErr: true},
		{name: "RFC1918 private IP is refused by default", url: "http://10.0.0.5/hook", host: "10.0.0.5", ip: "10.0.0.5", wantErr: true},
		{name: "RFC1918 private IP is allowed when the allowlist permits private egress", url: "http://10.0.0.5/hook", host: "10.0.0.5", ip: "10.0.0.5", allowPrivate: true, wantErr: false},
		{name: "a name that resolves to a private address is refused by default", url: "http://internal.example/hook", host: "internal.example", ip: "192.168.1.5", wantErr: true},
		{name: "metadata address is refused even when private egress is allowed", url: "http://169.254.169.254/latest/meta-data/", host: "169.254.169.254", ip: "169.254.169.254", allowPrivate: true, wantErr: true},
		{name: "a name that resolves to the metadata address is refused", url: "http://metadata.example/", host: "metadata.example", ip: "169.254.169.254", allowPrivate: true, wantErr: true},
		{name: "IPv6 loopback is refused", url: "http://[::1]:9000/hook", host: "::1", ip: "::1", wantErr: true},
		{name: "IPv6 unique-local is refused by default", url: "http://[fc00::1]/hook", host: "fc00::1", ip: "fc00::1", wantErr: true},
		{name: "multicast is always refused", url: "http://224.0.0.1/hook", host: "224.0.0.1", ip: "224.0.0.1", allowPrivate: true, wantErr: true},
		{name: "unspecified address is always refused", url: "http://0.0.0.0/hook", host: "0.0.0.0", ip: "0.0.0.0", allowPrivate: true, wantErr: true},
		{name: "ftp scheme is refused", url: "ftp://example.com/hook", host: "example.com", ip: "93.184.216.34", wantErr: true},
		{name: "empty URL is refused", url: "", host: "", ip: "", wantErr: true},
		{name: "a host with no DNS answer is refused", url: "http://does-not-resolve.invalid/hook", host: "resolves-to-nothing", ip: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hosts := map[string]string{}
			if tc.host != "" && tc.ip != "" {
				hosts[tc.host] = tc.ip
			}
			g := guardWithHosts(t, tc.allowPrivate, hosts)
			err := g.Validate(context.Background(), tc.url)
			if tc.wantErr && err == nil {
				t.Fatalf("Validate(%q) = nil, want a refusal", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Validate(%q) = %v, want nil", tc.url, err)
			}
			if tc.wantErr && err != nil && !errors.Is(err, ErrDestinationRefused) {
				t.Errorf("Validate(%q) error does not wrap ErrDestinationRefused: %v", tc.url, err)
			}
		})
	}
}

// TestDestinationGuardDialsThePinnedIPNotTheHostname proves the SSRF-relevant
// property: the transport connects to the IP the guard already validated, not
// to a hostname net/http (or anything downstream) could re-resolve.
func TestDestinationGuardDialsThePinnedIPNotTheHostname(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	serverURL := server.Listener.Addr().String() // 127.0.0.1:PORT
	host, port, err := net.SplitHostPort(serverURL)
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}

	g := guardWithHosts(t, true, map[string]string{"receiver.example": host})
	client := &http.Client{Transport: g.Transport()}

	resp, err := client.Get("http://receiver.example:" + port + "/hook")
	if err != nil {
		t.Fatalf("request through the guarded transport failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestDestinationGuardTransportRefusesAtDialTime proves the transport itself
// blocks a disallowed destination — the defence-in-depth path a row that
// bypassed Validate (smuggled directly into the table, or repointed by DNS
// after creation) still has to go through.
func TestDestinationGuardTransportRefusesAtDialTime(t *testing.T) {
	g := guardWithHosts(t, false, map[string]string{"rebound.example": "169.254.169.254"})
	client := &http.Client{Transport: g.Transport()}

	_, err := client.Get("http://rebound.example/latest/meta-data/")
	if err == nil {
		t.Fatal("request to the metadata address through the guarded transport succeeded, want a refusal")
	}
	if !errors.Is(err, ErrDestinationRefused) && !strings.Contains(err.Error(), "refused") {
		t.Errorf("error does not read as an SSRF refusal: %v", err)
	}
}

func TestDestinationGuardNilAllowlistRefusesPrivateByDefault(t *testing.T) {
	g := NewDestinationGuardWithResolver(nil, fakeIPResolver{ips: map[string][]net.IPAddr{
		"internal.example": {addr("10.1.2.3")},
	}})
	if err := g.Validate(context.Background(), "http://internal.example/hook"); err == nil {
		t.Fatal("Validate accepted a private destination with no allowlist configured")
	}
}
