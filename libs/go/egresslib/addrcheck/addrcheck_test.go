package addrcheck

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
)

func TestClassifyMatrix(t *testing.T) {
	cases := []struct {
		ip   string
		want Class
	}{
		{"93.184.216.34", Public},
		{"8.8.8.8", Public},
		{"2606:4700::1111", Public},
		{"127.0.0.1", Private},
		{"127.9.9.9", Private},
		{"::1", Private},
		{"10.1.2.3", Private},
		{"172.16.0.1", Private},
		{"192.168.1.1", Private},
		{"fc00::1", Private},
		{"fd12::1", Private},
		{"100.64.0.1", Private},
		{"198.18.0.1", Private},
		{"fec0::1", Private},
		{"0.0.0.0", Forbidden},
		{"::", Forbidden},
		{"0.1.2.3", Forbidden},
		{"169.254.169.254", Forbidden},
		{"fe80::1", Forbidden},
		{"224.0.0.1", Forbidden},
		{"ff02::1", Forbidden},
		{"192.0.0.1", Forbidden},
		{"240.0.0.1", Forbidden},
		{"255.255.255.255", Forbidden},
		{"100.100.100.200", Forbidden},
		{"168.63.129.16", Forbidden},
		{"fd00:ec2::254", Forbidden},
		{"64:ff9b:1::1", Forbidden},
		{"100::1", Forbidden},
		{"2001::1", Forbidden},
		{"::10.0.0.1", Forbidden},
		// IPv4 through an IPv6 form is judged as the IPv4 address it reaches.
		{"::ffff:127.0.0.1", Private},
		{"::ffff:169.254.169.254", Forbidden},
		{"::ffff:93.184.216.34", Public},
		{"64:ff9b::a9fe:a9fe", Forbidden}, // NAT64 169.254.169.254
		{"64:ff9b::a00:1", Private},       // NAT64 10.0.0.1
		{"64:ff9b::5db8:d822", Public},    // NAT64 93.184.216.34
		{"2002:a9fe:a9fe::1", Forbidden},  // 6to4 169.254.169.254
		{"2002:c0a8:0101::1", Private},    // 6to4 192.168.1.1
	}
	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("bad test address %q", tc.ip)
		}
		if got := Classify(ip); got != tc.want {
			t.Errorf("Classify(%s) = %s, want %s", tc.ip, got, tc.want)
		}
	}
}

func TestClassifyDoesNotAllocate(t *testing.T) {
	ip := net.ParseIP("93.184.216.34")
	if allocs := testing.AllocsPerRun(100, func() { _ = Classify(ip) }); allocs != 0 {
		t.Fatalf("Classify allocates %.0f times, want 0", allocs)
	}
}

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"::ffff:10.0.0.1":      "10.0.0.1",
		"64:ff9b::a00:1":       "10.0.0.1",
		"2002:a00:1::1":        "10.0.0.1",
		"2606:4700::1111":      "2606:4700::1111",
		"93.184.216.34":        "93.184.216.34",
		"::ffff:93.184.216.34": "93.184.216.34",
	} {
		got := Canonical(net.ParseIP(in))
		if got.String() != want {
			t.Errorf("Canonical(%s) = %s, want %s", in, got, want)
		}
		if strings.Contains(want, ".") && len(got) != net.IPv4len {
			t.Errorf("Canonical(%s) has %d bytes, want 4", in, len(got))
		}
	}
}

func mustAllowlist(t *testing.T, entries ...string) *egresslib.Allowlist {
	t.Helper()
	list, err := egresslib.Parse(entries)
	if err != nil {
		t.Fatalf("parse allowlist: %v", err)
	}
	return list
}

func TestPrivatePermitsEachEntryOpensOnlyItsRange(t *testing.T) {
	set := PrivatePermits(mustAllowlist(t, "10.0.0.0/8", "192.168.1.5:8080", "localhost:9000", "::ffff:172.16.0.0/108", "*.internal.example", "named.example"))
	cases := []struct {
		ip, port string
		want     bool
	}{
		{"10.9.9.9", "443", true},
		{"::ffff:10.9.9.9", "443", true},
		{"192.168.1.5", "8080", true},
		{"192.168.1.5", "443", false}, // pinned port
		{"192.168.1.6", "8080", false},
		{"127.0.0.1", "9000", true},
		{"::1", "9000", true},
		{"127.0.0.1", "80", false},
		{"172.16.3.4", "443", true}, // IPv4-mapped CIDR reads as IPv4
		{"100.64.0.1", "443", false},
	}
	for _, tc := range cases {
		if got := set.Admits(net.ParseIP(tc.ip), tc.port); got != tc.want {
			t.Errorf("Admits(%s, %s) = %v, want %v", tc.ip, tc.port, got, tc.want)
		}
	}
	// Names and wildcards permit nothing private: only the four literal or
	// CIDR entries above (localhost counts twice, IPv4 and IPv6).
	if len(set) != 5 {
		t.Errorf("got %d permits, want 5", len(set))
	}
}

func TestFilterNeverKeepsForbidden(t *testing.T) {
	// Every private class named, and a metadata address inside CGNAT.
	set := PrivatePermits(mustAllowlist(t, "100.64.0.0/10", "169.254.0.0/16", "0.0.0.0/0", "::/0"))
	ips := []net.IP{
		net.ParseIP("93.184.216.34"),
		net.ParseIP("100.64.1.1"),
		net.ParseIP("100.100.100.200"),
		net.ParseIP("169.254.169.254"),
		net.ParseIP("fe80::1"),
		net.ParseIP("0.0.0.0"),
	}
	got := Filter(ips, "443", set)
	var names []string
	for _, ip := range got {
		names = append(names, ip.String())
	}
	if strings.Join(names, ",") != "93.184.216.34,100.64.1.1" {
		t.Fatalf("Filter kept %v", names)
	}
	if out := Filter(ips, "443", nil); len(out) != 1 {
		t.Fatalf("nil permits kept %v, want the public address only", out)
	}
}

type fakeResolver map[string][]net.IPAddr

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	return addrs, nil
}

func TestResolve(t *testing.T) {
	ctx := context.Background()
	many := make([]net.IPAddr, MaxResolvedAddresses+1)
	for i := range many {
		many[i] = net.IPAddr{IP: net.IPv4(93, 184, 216, byte(i))}
	}
	r := fakeResolver{
		"pub.example":   {{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("fe80::1"), Zone: "eth0"}},
		"many.example":  many,
		"empty.example": {},
		"localhost":     {{IP: net.ParseIP("10.0.0.1")}},
	}
	ips, err := Resolve(ctx, r, "pub.example")
	if err != nil || len(ips) != 1 || ips[0].String() != "93.184.216.34" {
		t.Fatalf("pub.example: %v %v (zoned answers must be dropped)", ips, err)
	}
	if ips, err := Resolve(ctx, r, "localhost"); err != nil || ips[0].String() != "127.0.0.1" {
		t.Fatalf("localhost must be pinned to 127.0.0.1, got %v %v", ips, err)
	}
	if ips, err := Resolve(ctx, r, "10.1.1.1"); err != nil || ips[0].String() != "10.1.1.1" {
		t.Fatalf("an IP literal resolves to itself, got %v %v", ips, err)
	}
	if _, err := Resolve(ctx, r, "many.example"); err == nil {
		t.Fatal("an answer over MaxResolvedAddresses must be refused")
	}
	if _, err := Resolve(ctx, r, "empty.example"); err == nil {
		t.Fatal("an empty answer must be an error")
	}
	if _, err := Resolve(ctx, r, "missing.example"); err == nil {
		t.Fatal("a resolver error must be returned")
	}
}

func TestDialFirstDialsOnlyTheGivenLiterals(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	// A closed port on the first literal; DialFirst falls through to the next.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, closedPort, _ := net.SplitHostPort(closed.Addr().String())
	_ = closed.Close()
	if _, err := DialFirst(context.Background(), nil, "tcp", []net.IP{net.ParseIP("127.0.0.1")}, closedPort); err == nil {
		t.Fatal("dialling a closed port must fail")
	}
	conn, err := DialFirst(context.Background(), nil, "tcp", []net.IP{net.ParseIP("127.0.0.1")}, port)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	if _, err := DialFirst(context.Background(), nil, "tcp", nil, port); err == nil {
		t.Fatal("an empty address list must be an error, never a resolution")
	}
}
