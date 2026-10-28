package mcp_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters/mcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
)

// The adapter is a built-in provider, and the registry answers the facts
// the broker and the decider read: listing is a read, a call is a write.
func TestTheMCPAdapterIsABuiltinProvider(t *testing.T) {
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if !registry.Known(mcp.OperationToolsList) || registry.Class(mcp.OperationToolsList) != credentialadapters.OperationRead {
		t.Fatalf("mcp.tools.list known=%v class=%q", registry.Known(mcp.OperationToolsList), registry.Class(mcp.OperationToolsList))
	}
	if !registry.Known(mcp.OperationToolCall) || registry.Class(mcp.OperationToolCall) != credentialadapters.OperationWrite {
		t.Fatalf("mcp.tool.call known=%v class=%q", registry.Known(mcp.OperationToolCall), registry.Class(mcp.OperationToolCall))
	}
	if provider, _ := registry.ProviderFor(mcp.OperationToolCall); provider != mcp.ProviderType {
		t.Fatalf("provider = %q", provider)
	}
	if keys := registry.MaterialKeys(mcp.ProviderType, credentialgrant.ClassOAuthDelegated); len(keys) != 1 || keys[0] != "access_token" {
		t.Fatalf("material keys = %v", keys)
	}
	if registry.MaterialKeys(mcp.ProviderType, credentialgrant.ClassStaticSecret) != nil {
		t.Fatal("a static secret is released for an MCP server")
	}
	if got := registry.OAuthAuthority(mcp.ProviderType, credentialgrant.ClassOAuthDelegated); got != credentialgrant.OAuthAuthorityProvider {
		t.Fatalf("authority = %q", got)
	}
	d, _ := registry.Descriptor(mcp.ProviderType)
	if d.TargetKind != credentialadapters.TargetResourceURI || d.Qualification[credentialgrant.ClassOAuthDelegated] != credentialadapters.QualificationFixture {
		t.Fatalf("target kind %q qualification %v", d.TargetKind, d.Qualification)
	}
}

// The target is the canonical resource URI mcpoauth binds tokens to: two
// spellings of one server are one target, two servers are two.
func TestCanonicalResource(t *testing.T) {
	for input, want := range map[string]string{
		"https://MCP.Example.com:443":       "https://mcp.example.com/",
		" https://mcp.example.com/mcp ":     "https://mcp.example.com/mcp",
		"https://mcp.example.com/mcp/":      "https://mcp.example.com/mcp/",
		"https://mcp.example.com:8443/mcp":  "https://mcp.example.com:8443/mcp",
		"https://other.example.com/mcp":     "https://other.example.com/mcp",
		"https://[::1]:9443/servers/a/mcp":  "https://[::1]:9443/servers/a/mcp",
		"https://mcp.example.com/a%2Fb/mcp": "",
	} {
		got, err := mcp.CanonicalResource(input)
		if want == "" {
			if !errors.Is(err, credentialadapters.ErrInvalidTarget) {
				t.Errorf("CanonicalResource(%q) = %q, %v; want refused", input, got, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("CanonicalResource(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, refused := range []string{
		"", "http://mcp.example.com/mcp", "https://user:pw@mcp.example.com/mcp", "https://mcp.example.com/mcp?x=1",
		"https://mcp.example.com/mcp#f", "https://mcp.example.com/../mcp", "https://mcp.example.com/" + strings.Repeat("a", 600),
	} {
		if _, err := mcp.CanonicalResource(refused); !errors.Is(err, credentialadapters.ErrInvalidTarget) {
			t.Errorf("CanonicalResource(%q) accepted", refused)
		}
	}
}
