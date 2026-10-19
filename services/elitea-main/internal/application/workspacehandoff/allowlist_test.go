package workspacehandoff

import (
	"reflect"
	"testing"
)

func TestHostCovered(t *testing.T) {
	for _, tc := range []struct {
		ceiling, entry string
		want           bool
	}{
		{"pypi.org", "pypi.org", true},
		{"PyPI.org", "pypi.ORG", true},
		{"pypi.org", "files.pypi.org", false},
		{"*.github.com", "api.github.com", true},
		{"*.github.com", "a.b.github.com", true},
		{"*.github.com", "*.api.github.com", true},
		{"*.github.com", "*.github.com", true},
		{"*.github.com", "github.com", false},
		{"*.github.com", "evilgithub.com", false},
		{"*.github.com", "github.com.evil.io", false},
		{"*.b.c", "a.b.c", true},
		{"*.b.c", "b.c", false},
		{"api.github.com", "*.github.com", false},
		// Ports: no port means 443 and 80.
		{"*.github.com", "api.github.com:443", true},
		{"*.github.com", "api.github.com:80", true},
		{"*.github.com", "api.github.com:8443", false},
		{"*.github.com:8443", "api.github.com:8443", true},
		{"*.github.com:8443", "api.github.com", false},
		{"pypi.org:443", "pypi.org", false},
		{"pypi.org:443", "pypi.org:443", true},
		{"pypi.org", "pypi.org:0443", false},
		{"", "pypi.org", false},
	} {
		if got := HostCovered(tc.ceiling, tc.entry); got != tc.want {
			t.Errorf("HostCovered(%q, %q) = %t, want %t", tc.ceiling, tc.entry, got, tc.want)
		}
	}
}

func TestNarrowKeepsProjectHostsUnderAWildcardCeiling(t *testing.T) {
	ceiling := amendedCeiling()
	ceiling.Network = true
	ceiling.NetworkAllowlist = []string{"*.github.com", "pypi.org"}
	requested := []string{"api.github.com", "codeload.github.com", "github.com", "pypi.org", "evil.io"}
	got := Narrow(ceiling, ProjectSetting{NetworkAllowlist: &requested}, true, true)
	if want := []string{"api.github.com", "codeload.github.com", "pypi.org"}; !reflect.DeepEqual(got.NetworkAllowlist, want) || !got.Network {
		t.Fatalf("allowlist = %v (network %t), want %v", got.NetworkAllowlist, got.Network, want)
	}
	none := []string{"github.com"}
	if got := Narrow(ceiling, ProjectSetting{NetworkAllowlist: &none}, true, true); got.Network || len(got.NetworkAllowlist) != 0 {
		t.Fatalf("a request outside the ceiling = %v (network %t), want the network off", got.NetworkAllowlist, got.Network)
	}
}
