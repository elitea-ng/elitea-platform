package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/addrcheck"
)

// A host whose every address is refused gives an *AddressRefusedError, from
// Validate and through the guarded transport alike. It still wraps
// ErrDestinationRefused, its text names no address, and it tells an operator
// log whether an allowlist entry could permit the destination at all.
func TestAddressRefusedErrorCarriesTheClasses(t *testing.T) {
	g := NewWithResolver(nil, fakeIPResolver{ips: map[string][]net.IPAddr{
		"mixed.example":    {addr("10.1.2.3"), addr("169.254.169.254")},
		"metadata.example": {addr("169.254.169.254")},
	}})
	for _, tc := range []struct {
		host          string
		allowlistable bool
		classes       []addrcheck.Class
	}{
		{"mixed.example", true, []addrcheck.Class{addrcheck.Private, addrcheck.Forbidden}},
		{"metadata.example", false, []addrcheck.Class{addrcheck.Forbidden}},
	} {
		t.Run(tc.host, func(t *testing.T) {
			validateErr := g.Validate(context.Background(), "https://"+tc.host)
			_, dialErr := (&http.Client{Transport: g.Transport()}).Get("http://" + tc.host + "/")
			for name, err := range map[string]error{"Validate": validateErr, "dial": dialErr} {
				var refused *AddressRefusedError
				if !errors.As(err, &refused) || !errors.Is(err, ErrDestinationRefused) {
					t.Fatalf("%s: err = %v, want an *AddressRefusedError wrapping ErrDestinationRefused", name, err)
				}
				if refused.Host != tc.host || refused.Allowlistable() != tc.allowlistable || len(refused.Addresses) != len(tc.classes) {
					t.Fatalf("%s: refused = %+v, want host %q, allowlistable %v", name, refused, tc.host, tc.allowlistable)
				}
				for i, class := range tc.classes {
					if refused.Addresses[i].Class != class {
						t.Fatalf("%s: address %d class = %s, want %s", name, i, refused.Addresses[i].Class, class)
					}
				}
				if strings.Contains(refused.Error(), "169.254") || strings.Contains(refused.Error(), "10.1.2.3") {
					t.Fatalf("%s: the error text %q names an address", name, refused.Error())
				}
			}
		})
	}
}
