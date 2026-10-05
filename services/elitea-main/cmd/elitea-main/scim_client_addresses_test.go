package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An OIDC-only deployment has no Form authentication document, so no Form
// trusted-proxy resolver. The SCIM token limiter must still see the real caller
// behind the ingress, from ELITEA_TRUSTED_PROXY_CIDRS.
func TestSCIMClientAddressesOnAnOIDCOnlyPlane(t *testing.T) {
	getenv := func(name string) string {
		if name == trustedProxyCIDRsEnv {
			return "10.0.0.0/8, fd00::/8"
		}
		return ""
	}
	resolver, err := scimClientAddressesFromConfig(getenv, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	addresses := map[string]bool{}
	for _, client := range []string{"203.0.113.9", "198.51.100.7"} {
		request := httptest.NewRequest(http.MethodPost, "/api/v2/scim/oauth/token", nil)
		request.RemoteAddr = "10.42.0.17:41234" // the ingress pod
		request.Header.Set("X-Forwarded-For", client)
		address, ok := resolver.Resolve(request)
		if !ok || address != client {
			t.Fatalf("Resolve = (%q, %v), want (%q, true)", address, ok, client)
		}
		addresses[address] = true
	}
	if len(addresses) != 2 {
		t.Fatalf("two callers behind one ingress resolved to %d keys, want 2", len(addresses))
	}
}

func TestSCIMClientAddressesMergeTheAuthDocument(t *testing.T) {
	resolver, err := scimClientAddressesFromConfig(func(string) string { return "" }, []string{"172.16.0.0/12"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !resolver.Configured() {
		t.Fatal("the Form document's trusted_proxy_cidrs were dropped")
	}

	empty, err := scimClientAddressesFromConfig(func(string) string { return "" }, nil, nil)
	if err != nil || empty.Configured() {
		t.Fatalf("no CIDRs = (%v, %v), want an unconfigured resolver", empty.Configured(), err)
	}

	_, err = scimClientAddressesFromConfig(func(string) string { return "not-a-cidr" }, nil, nil)
	if err == nil {
		t.Fatal("an invalid CIDR must stop the boot")
	}
}
