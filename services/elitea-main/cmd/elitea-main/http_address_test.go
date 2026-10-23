package main

import "testing"

func TestConfiguredHTTPAddressDefaultsAndValidatesOverride(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	address, err := configuredHTTPAddress(lookup)
	if err != nil || address != defaultHTTPAddress {
		t.Fatalf("default address = %q, err = %v", address, err)
	}

	lookup = func(key string) (string, bool) {
		if key == "ELITEA_HTTP_ADDRESS" {
			return "127.0.0.1:18080", true
		}
		return "", false
	}
	address, err = configuredHTTPAddress(lookup)
	if err != nil || address != "127.0.0.1:18080" {
		t.Fatalf("override address = %q, err = %v", address, err)
	}

	for _, invalid := range []string{"localhost", ":0", ":65536", ":08080", ":abc", ":8080\nspoof"} {
		lookup = func(string) (string, bool) { return invalid, true }
		if _, err := configuredHTTPAddress(lookup); err == nil {
			t.Fatalf("invalid HTTP address %q was accepted", invalid)
		}
	}
}

func TestHealthcheckURLUsesConfiguredListenerPort(t *testing.T) {
	lookup := func(string) (string, bool) { return ":18080", true }
	endpoint, err := healthcheckURL(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "http://localhost:18080/readyz" {
		t.Fatalf("healthcheck URL = %q", endpoint)
	}
}

// TestHealthcheckURLTakesOneProbePath is the chart's exec probe (#1359 P3):
// `-healthcheck /startupz` reads the startup probe over loopback, which the
// browser edge would refuse to a kubelet httpGet from the node.
func TestHealthcheckURLTakesOneProbePath(t *testing.T) {
	lookup := func(string) (string, bool) { return ":8080", true }
	for _, path := range []string{"/readyz", "/startupz", "/healthz"} {
		endpoint, err := healthcheckURL(lookup, path)
		if err != nil {
			t.Fatalf("-healthcheck %s: %v", path, err)
		}
		if want := "http://localhost:8080" + path; endpoint != want {
			t.Fatalf("-healthcheck %s = %q, want %q", path, endpoint, want)
		}
	}
	for _, args := range [][]string{{"/api/v2/projects"}, {"readyz"}, {"/readyz?x=1"}, {"/readyz", "/healthz"}} {
		if endpoint, err := healthcheckURL(lookup, args...); err == nil {
			t.Fatalf("-healthcheck %v was accepted as %q; only the three probe paths may be read", args, endpoint)
		}
	}
}
