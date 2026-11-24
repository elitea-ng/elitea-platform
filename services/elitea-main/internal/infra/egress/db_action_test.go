package egress

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestNewDBActionGuardParsesAndFailsClosed(t *testing.T) {
	t.Parallel()
	if _, err := NewDBActionGuard("10.0.0.0/33"); err == nil {
		t.Fatal("malformed CIDR accepted")
	}
	guard, err := NewDBActionGuard("10.0.0.0/8, 192.168.1.5:5432", WithResolver(fakeIPResolver{ips: map[string][]net.IPAddr{
		"db.public.example":  {{IP: net.ParseIP("93.184.216.34")}},
		"db.private.example": {{IP: net.ParseIP("10.1.2.3")}},
		"db.pinned.example":  {{IP: net.ParseIP("192.168.1.5")}},
		"db.meta.example":    {{IP: net.ParseIP("169.254.169.254")}},
		"db.mapped.example":  {{IP: net.ParseIP("::ffff:127.0.0.1")}},
		"db.nat64.example":   {{IP: net.ParseIP("64:ff9b::169.254.169.254")}},
	}}))
	if err != nil {
		t.Fatalf("valid allowlist rejected: %v", err)
	}

	ctx := context.Background()
	for _, tc := range []struct {
		name string
		host string
		port uint16
		want bool
	}{
		{"public host", "db.public.example", 5432, true},
		{"allowlisted CIDR", "db.private.example", 5432, true},
		{"port-pinned match", "db.pinned.example", 5432, true},
		{"port-pinned wrong port", "db.pinned.example", 3306, false},
		{"metadata never allowed", "db.meta.example", 5432, false},
		{"ipv4-mapped loopback refused", "db.mapped.example", 5432, false},
		{"nat64 metadata refused", "db.nat64.example", 5432, false},
		{"unresolvable host", "db.missing.example", 5432, false},
		{"empty host", "", 5432, false},
		{"empty bracketed host", "[]", 5432, false},
		{"bracketed non-ipv6", "[db.public.example]", 5432, false},
		{"zoned ipv6 literal", "fe80::1%eth0", 5432, false},
		{"bracketed zoned ipv6", "[fe80::1%eth0]", 5432, false},
		{"zero port", "db.public.example", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := guard.ValidateHostPort(ctx, tc.host, tc.port)
			if tc.want && err != nil {
				t.Fatalf("ValidateHostPort(%q, %d) = %v, want nil", tc.host, tc.port, err)
			}
			if !tc.want && !errors.Is(err, ErrDestinationRefused) {
				t.Fatalf("ValidateHostPort(%q, %d) = %v, want ErrDestinationRefused", tc.host, tc.port, err)
			}
		})
	}
}
