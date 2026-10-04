package nativeauth

import (
	"strings"
	"testing"
	"time"
)

func TestCredentialFormatsDoNotCollide(t *testing.T) {
	prefixes := []string{PrefixAccessToken, PrefixRefreshToken, PrefixCode, "scim_", "scimc_", "scimcs_", "scimat_", "eyJ"}
	for i, a := range prefixes[:3] {
		for j, b := range prefixes {
			if i != j && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a)) {
				t.Fatalf("prefix %q collides with %q", a, b)
			}
		}
	}
	for _, prefix := range prefixes[:3] {
		secret, err := newSecret(prefix)
		if err != nil {
			t.Fatal(err)
		}
		if len(secret) != len(prefix)+43 || !WellFormed(secret, prefix) {
			t.Fatalf("%q: %q is not prefix + 43 base64url characters", prefix, secret)
		}
		if WellFormed(secret+"A", prefix) || WellFormed(strings.Replace(secret, prefix, "", 1), prefix) {
			t.Fatalf("%q: malformed variants accepted", prefix)
		}
		if len(HashSecret(secret)) != 64 || HashSecret(secret) == HashSecret(secret+"x") {
			t.Fatal("hash is not a 64-character SHA-256 hex digest")
		}
	}
}

func TestConfigFromEnvDefaultsBoundsAndRefusals(t *testing.T) {
	env := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}
	cfg, err := ConfigFromEnv(env(nil))
	if err != nil || cfg != DefaultConfig() {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	if cfg.RedeliveryWindow != 30*time.Second || cfg.AccessTokenTTL != 15*time.Minute || cfg.SessionMaxLifetime != 0 {
		t.Fatalf("default policy = %+v", cfg)
	}
	cfg, err = ConfigFromEnv(env(map[string]string{RedeliveryWindowEnv: "0", SessionMaxLifetimeEnv: "48h"}))
	if err != nil || cfg.RedeliveryWindow != 0 || cfg.SessionMaxLifetime != 48*time.Hour {
		t.Fatalf("strict window and cap = %+v, %v", cfg, err)
	}
	for name, value := range map[string]string{
		AccessTokenTTLEnv:     "1m",
		RefreshIdleTTLEnv:     "1h",
		SessionMaxLifetimeEnv: "1h",
		RedeliveryWindowEnv:   "10m",
	} {
		if _, err := ConfigFromEnv(env(map[string]string{name: value})); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s=%s: err = %v, want a refusal naming the variable", name, value, err)
		}
	}
	if _, err := ConfigFromEnv(env(map[string]string{AccessTokenTTLEnv: "soon"})); err == nil {
		t.Fatal("an unparsable duration must refuse boot")
	}
}

func TestRedirectURIRules(t *testing.T) {
	accepted := map[string]RedirectKind{
		"ca.zefir.agent:/oauth/callback": RedirectPrivateUse,
		"com.example.app:/cb":            RedirectPrivateUse,
		"dev.elitea.conformance:/":       RedirectPrivateUse,
		"http://127.0.0.1/callback":      RedirectLoopback,
		"http://[::1]/oauth":             RedirectLoopback,
	}
	for uri, kind := range accepted {
		got, err := ValidateRedirectURI(uri)
		if err != nil || got != kind {
			t.Fatalf("%q = (%v, %v), want accepted as %v", uri, got, err, kind)
		}
	}
	for _, uri := range []string{
		"", "elitea:/cb", "agentzefir:/cb", "https://app.example/cb", "http://localhost/cb",
		"http://127.0.0.1:8080/cb", "http://10.0.0.1/cb", "com.example.app://host/cb",
		"com.example.app:/cb?x=1", "com.example.app:/cb#f", "Com.Example.App:/cb",
		"com.example.app:cb", "com.example.app:/c%62", "javascript:alert(1)", "data:text/html,x",
		"com.exämple.app:/cb", "com.example.app:/cb x", "file:///etc/passwd",
		"com.example.app:/" + strings.Repeat("a", 600),
	} {
		if _, err := ValidateRedirectURI(uri); err == nil {
			t.Fatalf("%q was accepted", uri)
		}
	}
}

func TestRedirectMatching(t *testing.T) {
	cases := []struct {
		registered, presented string
		want                  bool
	}{
		{"com.example.app:/cb", "com.example.app:/cb", true},
		{"com.example.app:/cb", "com.example.app:/cb/", false},
		{"com.example.app:/cb", "com.example.app:/CB", false},
		{"http://127.0.0.1/cb", "http://127.0.0.1/cb", true},
		{"http://127.0.0.1/cb", "http://127.0.0.1:53123/cb", true},
		{"http://[::1]/cb", "http://[::1]:8080/cb", true},
		{"http://127.0.0.1/cb", "http://127.0.0.1:0/cb", false},
		{"http://127.0.0.1/cb", "http://127.0.0.1:99999/cb", false},
		{"http://127.0.0.1/cb", "http://127.0.0.1:80/other", false},
		{"http://127.0.0.1/cb", "http://localhost:80/cb", false},
		{"http://127.0.0.1/cb", "http://127.0.0.1:80/cb?x=1", false},
		{"http://127.0.0.1/cb", "https://127.0.0.1:80/cb", false},
	}
	for _, c := range cases {
		if got := RedirectMatches(c.registered, c.presented); got != c.want {
			t.Fatalf("RedirectMatches(%q, %q) = %v, want %v", c.registered, c.presented, got, c.want)
		}
	}
	if FormActionSource("com.example.app:/cb") != "com.example.app:" ||
		FormActionSource("http://127.0.0.1:5000/cb") != "http://127.0.0.1:* http://[::1]:*" {
		t.Fatal("form-action sources")
	}
}

func TestClientsFileParsesAndRefuses(t *testing.T) {
	clients, err := ParseClientsFile([]byte(`
- client_id: dev.elitea.conformance
  display_name: Conformance
  redirect_uris: ["dev.elitea.conformance:/oauth/callback", "http://127.0.0.1/callback"]
- client_id: dev.elitea.off
  display_name: Off
  redirect_uris: ["dev.elitea.off:/cb"]
  enabled: false
`))
	if err != nil || len(clients) != 2 || !clients[0].Enabled || clients[1].Enabled || clients[0].Source != SourceFile {
		t.Fatalf("yaml = %+v, %v", clients, err)
	}
	if _, err := ParseClientsFile([]byte(`[{"client_id":"dev.elitea.json","display_name":"J","redirect_uris":["dev.elitea.json:/cb"]}]`)); err != nil {
		t.Fatalf("json: %v", err)
	}
	for name, content := range map[string]string{
		"unknown key":   `[{"client_id":"dev.a.b","display_name":"x","redirect_uris":["dev.a.b:/cb"],"secret":"s"}]`,
		"duplicate id":  `[{"client_id":"dev.a.b","display_name":"x","redirect_uris":["dev.a.b:/cb"]},{"client_id":"dev.a.b","display_name":"y","redirect_uris":["dev.a.b:/cb"]}]`,
		"bad redirect":  `[{"client_id":"dev.a.b","display_name":"x","redirect_uris":["https://a.b/cb"]}]`,
		"bad client id": `[{"client_id":"A","display_name":"x","redirect_uris":["dev.a.b:/cb"]}]`,
		"not a list":    `client_id: x`,
	} {
		if _, err := ParseClientsFile([]byte(content)); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestRegistryFileLayerAndPublicView(t *testing.T) {
	registry := NewRegistry([]Client{
		{ClientID: "dev.a.on", DisplayName: "On", Enabled: true, RedirectURIs: []string{"dev.a.on:/cb"}, Source: SourceFile},
		{ClientID: "dev.a.off", DisplayName: "Off", RedirectURIs: []string{"dev.a.off:/cb"}, Source: SourceFile},
	}, nil)
	ctx := t.Context()
	if ok, _ := registry.Registered(ctx); !ok {
		t.Fatal("registered")
	}
	if _, active, _ := registry.Active(ctx, "dev.a.off"); active {
		t.Fatal("a disabled client is not active")
	}
	if _, ok, _ := registry.Lookup(ctx, "dev.a.off"); !ok {
		t.Fatal("a disabled client is still registered")
	}
	if enabled, _ := registry.AnyEnabled(ctx); !enabled {
		t.Fatal("any enabled")
	}
	empty := NewRegistry(nil, nil)
	if ok, _ := empty.Registered(ctx); ok {
		t.Fatal("an empty registry is not registered")
	}
}

func TestSealedSuccessorOpensOnlyWithThePresentedToken(t *testing.T) {
	pair := sealedPair{AccessToken: "elnat_a", RefreshToken: "elnrt_b", AccessExpiresAt: 1700000000}
	box, err := sealSuccessor("elnrt_presented", "session-1", 3, pair)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(box), "elnat_a") || strings.Contains(string(box), "elnrt_b") {
		t.Fatal("the sealed box carries plaintext tokens")
	}
	opened, err := openSuccessor("elnrt_presented", "session-1", 3, box)
	if err != nil || opened != pair {
		t.Fatalf("open = %+v, %v", opened, err)
	}
	for name, open := range map[string]func() error{
		"another token":      func() error { _, err := openSuccessor("elnrt_other", "session-1", 3, box); return err },
		"another family":     func() error { _, err := openSuccessor("elnrt_presented", "session-2", 3, box); return err },
		"another generation": func() error { _, err := openSuccessor("elnrt_presented", "session-1", 4, box); return err },
	} {
		if open() == nil {
			t.Fatalf("%s opened the box", name)
		}
	}
}

func TestPKCEAppendixBVector(t *testing.T) {
	// RFC 7636 Appendix B.
	if !pkceMatches("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM") {
		t.Fatal("the RFC 7636 Appendix B vector does not verify")
	}
	if pkceMatches("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXl", "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM") {
		t.Fatal("a different verifier verified")
	}
}
