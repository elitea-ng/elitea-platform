package egress

// SEC-10: a private allowlist entry permits only its own range. Naming one
// private block (a CGNAT receiver, one RFC 1918 subnet) never opens loopback or
// the other private classes for the path, a port-pinned entry permits only its
// port, and forbidden addresses stay refused inside a named block.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGuardPrivateEntryPermitsOnlyItsOwnRange(t *testing.T) {
	cases := []struct {
		name    string
		entries []string
		url     string // host is target.example unless an IP literal
		ip      string
		allowed bool
	}{
		// A CGNAT entry (the Tailscale upgrade note) permits CGNAT only.
		{"CGNAT entry permits CGNAT", []string{"100.64.0.0/10"}, "https://target.example/", "100.64.1.5", true},
		{"CGNAT entry refuses IPv4 loopback", []string{"100.64.0.0/10"}, "https://target.example/", "127.0.0.1", false},
		{"CGNAT entry refuses IPv6 loopback", []string{"100.64.0.0/10"}, "https://target.example/", "::1", false},
		{"CGNAT entry refuses 10/8", []string{"100.64.0.0/10"}, "https://target.example/", "10.0.0.5", false},
		{"CGNAT entry refuses 172.16/12", []string{"100.64.0.0/10"}, "https://target.example/", "172.16.0.5", false},
		{"CGNAT entry refuses 192.168/16", []string{"100.64.0.0/10"}, "https://target.example/", "192.168.1.5", false},
		{"CGNAT entry refuses ULA", []string{"100.64.0.0/10"}, "https://target.example/", "fc00::5", false},
		{"CGNAT entry refuses benchmarking", []string{"100.64.0.0/10"}, "https://target.example/", "198.18.0.5", false},
		{"CGNAT entry refuses loopback literal", []string{"100.64.0.0/10"}, "http://127.0.0.1:8080/", "127.0.0.1", false},
		{"CGNAT entry refuses localhost", []string{"100.64.0.0/10"}, "http://localhost:8080/", "127.0.0.1", false},
		{"CGNAT entry refuses mapped loopback", []string{"100.64.0.0/10"}, "https://target.example/", "::ffff:127.0.0.1", false},
		{"CGNAT entry refuses NAT64 loopback", []string{"100.64.0.0/10"}, "https://target.example/", "64:ff9b::7f00:1", false},
		{"CGNAT entry refuses 6to4 of 10/8", []string{"100.64.0.0/10"}, "https://target.example/", "2002:a00:5::1", false},
		{"CGNAT entry keeps metadata forbidden", []string{"100.64.0.0/10"}, "https://target.example/", "100.100.100.200", false},
		{"CGNAT entry keeps public reachable", []string{"100.64.0.0/10"}, "https://target.example/", "93.184.216.34", true},
		// An RFC 1918 subnet permits that subnet, not the rest of its /8.
		{"subnet entry permits inside", []string{"10.1.2.0/24"}, "https://target.example/", "10.1.2.200", true},
		{"subnet entry refuses the neighbour subnet", []string{"10.1.2.0/24"}, "https://target.example/", "10.1.3.1", false},
		{"subnet entry permits NAT64 form inside", []string{"10.1.2.0/24"}, "https://target.example/", "64:ff9b::a01:205", true},
		{"subnet entry permits mapped form inside", []string{"10.1.2.0/24"}, "https://target.example/", "::ffff:10.1.2.5", true},
		{"mapped CIDR entry permits its IPv4 block", []string{"::ffff:10.1.2.0/120"}, "https://target.example/", "10.1.2.5", true},
		{"mapped CIDR entry refuses outside", []string{"::ffff:10.1.2.0/120"}, "https://target.example/", "10.1.3.5", false},
		// An IP literal permits that address, on its port when pinned.
		{"IP entry permits itself", []string{"192.168.29.60"}, "https://target.example/", "192.168.29.60", true},
		{"IP entry refuses its neighbour", []string{"192.168.29.60"}, "https://target.example/", "192.168.29.61", false},
		{"port-pinned IP entry permits its port", []string{"192.168.29.60:8000"}, "http://target.example:8000/", "192.168.29.60", true},
		{"port-pinned IP entry refuses another port", []string{"192.168.29.60:8000"}, "http://target.example:8001/", "192.168.29.60", false},
		{"port-pinned IP entry refuses the default port", []string{"192.168.29.60:8000"}, "https://target.example/", "192.168.29.60", false},
		{"port-pinned IPv6 entry permits its port", []string{"[fd12::7]:9000"}, "http://target.example:9000/", "fd12::7", true},
		// localhost names loopback only.
		{"localhost entry permits loopback", []string{"localhost"}, "http://localhost:8080/", "127.0.0.1", true},
		{"localhost entry permits IPv6 loopback", []string{"localhost"}, "https://target.example/", "::1", true},
		{"localhost entry refuses RFC 1918", []string{"localhost"}, "https://target.example/", "10.0.0.5", false},
		{"port-pinned localhost refuses another port", []string{"localhost:8080"}, "http://localhost:9090/", "127.0.0.1", false},
		// A name or wildcard says nothing about where it resolves.
		{"host name entry permits no private address", []string{"target.example"}, "https://target.example/", "10.0.0.5", false},
		{"wildcard entry permits no private address", []string{"*.example"}, "https://target.example/", "10.0.0.5", false},
		// A wide block is the operator's explicit range, still not forbidden space.
		{"wide block keeps link-local forbidden", []string{"0.0.0.0/0"}, "https://target.example/", "169.254.169.254", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := singleHostGuard(t, privateAllowlist(t, tc.entries...), "target.example", tc.ip)
			err := g.Validate(context.Background(), tc.url)
			if tc.allowed && err != nil {
				t.Fatalf("Validate(%s -> %s) refused: %v", tc.url, tc.ip, err)
			}
			if !tc.allowed && !errors.Is(err, ErrDestinationRefused) {
				t.Fatalf("Validate(%s -> %s) = %v, want ErrDestinationRefused", tc.url, tc.ip, err)
			}
		})
	}
}

// The dial-time filter applies the same per-entry rule as Validate: with only
// a CGNAT block named, a request whose host resolves to a live loopback
// listener is refused before any connection, and the listener sees nothing.
func TestGuardDialTimeRefusesPrivateOutsideTheNamedEntry(t *testing.T) {
	var reached bool
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer server.Close()
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		entries []string
		allowed bool
	}{
		{[]string{"100.64.0.0/10"}, false},
		{[]string{"10.0.0.0/8", "192.168.0.0/16"}, false},
		{[]string{"127.0.0.1:1"}, false}, // names loopback, on another port
		{[]string{"127.0.0.1:" + port}, true},
		{[]string{"127.0.0.0/8"}, true},
	} {
		t.Run(strings.Join(tc.entries, ","), func(t *testing.T) {
			reached = false
			g := singleHostGuard(t, privateAllowlist(t, tc.entries...), "receiver.example", host)
			client := &http.Client{Transport: g.RoundTripper()}
			resp, err := client.Get("http://receiver.example:" + port + "/hook")
			if resp != nil {
				_ = resp.Body.Close()
			}
			if tc.allowed {
				if err != nil || !reached {
					t.Fatalf("allowed dial failed: err=%v reached=%v", err, reached)
				}
				return
			}
			if !errors.Is(err, ErrDestinationRefused) || reached {
				t.Fatalf("dial = %v reached=%v, want ErrDestinationRefused and no connection", err, reached)
			}
		})
	}
}

// The filter stays allocation-bounded with many entries: one allocation for
// the result slice, none per entry or per address.
func TestGuardPerEntryFilterBudget(t *testing.T) {
	entries := make([]string, 0, 64)
	for i := range 64 {
		entries = append(entries, net.IPv4(10, byte(i), 0, 0).String()+"/16")
	}
	g := New(privateAllowlist(t, entries...))
	ips := make([]net.IP, MaxResolvedAddresses)
	for i := range ips {
		ips[i] = net.IPv4(10, 63, 0, byte(i+1))
	}
	if got := len(g.permittedIPs(ips, "443")); got != len(ips) {
		t.Fatalf("permitted %d of %d named addresses", got, len(ips))
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = g.permittedIPs(ips, "443") }); allocs > 1 {
		t.Fatalf("permittedIPs allocates %.0f times for %d answers over %d entries, want at most 1",
			allocs, len(ips), len(entries))
	}
}
