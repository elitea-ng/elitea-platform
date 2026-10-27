package eliteanative_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
)

const validDocument = `{
  "origin": "https://elitea.example.com",
  "keys": [{"kid": "es-2026-10", "alg": "ES256", "file": "/etc/elitea/agent-assertion/es.pem"}],
  "relying_parties": [{"id": "aws-prod", "audience": "sts.amazonaws.com", "claims_profile": "aws"}]
}`

func TestParseConfigAppliesTheDocumentedDefaults(t *testing.T) {
	cfg, err := eliteanative.ParseConfig([]byte(validDocument), "")
	if err != nil {
		t.Fatal(err)
	}
	rp := cfg.RelyingParties[0]
	if rp.Algorithm != eliteanative.ES256 || rp.MaxTTL != 10*time.Minute || cfg.RotationOverlap != 24*time.Hour ||
		cfg.Tenant != "elitea.example.com" || cfg.Issuer() != "https://elitea.example.com/agent-issuer" ||
		cfg.JWKSURI() != "https://elitea.example.com/.well-known/elitea-agent-jwks.json" {
		t.Fatalf("cfg = %+v", cfg)
	}
	// The deployment's public origin is the default origin.
	noOrigin := strings.Replace(validDocument, `"origin": "https://elitea.example.com",`, "", 1)
	cfg, err = eliteanative.ParseConfig([]byte(noOrigin), "https://deploy.example.com")
	if err != nil || cfg.Issuer() != "https://deploy.example.com/agent-issuer" {
		t.Fatalf("cfg = %+v, %v", cfg, err)
	}
}

func TestParseConfigRefusesWhatTheIssueForbids(t *testing.T) {
	replace := func(old, repl string) string { return strings.Replace(validDocument, old, repl, 1) }
	for name, doc := range map[string]string{
		"EdDSA relying party":      replace(`"claims_profile": "aws"`, `"claims_profile": "aws", "algorithm": "EdDSA"`),
		"EdDSA key":                replace(`"alg": "ES256"`, `"alg": "EdDSA"`),
		"max_ttl above 15 minutes": replace(`"claims_profile": "aws"`, `"claims_profile": "aws", "max_ttl": "16m"`),
		"max_ttl below the floor":  replace(`"claims_profile": "aws"`, `"claims_profile": "aws", "max_ttl": "5s"`),
		"unknown claims profile":   replace(`"aws"}`, `"azure"}`),
		"RS256 without an RS key":  replace(`"claims_profile": "aws"`, `"claims_profile": "aws", "algorithm": "RS256"`),
		"unknown field":            replace(`"origin"`, `"orign": "x", "origin"`),
		"http origin":              replace(`https://elitea.example.com`, `http://elitea.example.com`),
		"origin with a path":       replace(`https://elitea.example.com`, `https://elitea.example.com/x`),
		"no origin":                replace(`"origin": "https://elitea.example.com",`, ""),
		"no relying party":         replace(`[{"id": "aws-prod", "audience": "sts.amazonaws.com", "claims_profile": "aws"}]`, `[]`),
		"duplicate audience": replace(`"claims_profile": "aws"}]`,
			`"claims_profile": "aws"}, {"id": "aws-2", "audience": "sts.amazonaws.com", "claims_profile": "aws"}]`),
		"two sources": replace(`"file": "/etc/elitea/agent-assertion/es.pem"`,
			`"file": "/etc/elitea/agent-assertion/es.pem", "vault": {"mount": "kv", "path": "p", "version": 1}`),
		"vault without version": replace(`"file": "/etc/elitea/agent-assertion/es.pem"`, `"vault": {"mount": "kv", "path": "p"}`),
		"relative key file":     replace(`/etc/elitea/agent-assertion/es.pem`, `es.pem`),
		"two established keys": replace(`"file": "/etc/elitea/agent-assertion/es.pem"}`,
			`"file": "/etc/elitea/agent-assertion/es.pem"}, {"kid": "es-2", "alg": "ES256", "file": "/k/es2.pem"}`),
		"overlap too short": replace(`"keys"`, `"rotation_overlap": "10s", "keys"`),
		"two documents":     validDocument + validDocument,
	} {
		if _, err := eliteanative.ParseConfig([]byte(doc), ""); !errors.Is(err, eliteanative.ErrInvalidConfig) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// The overlap must be strictly longer than the JWKS cache max-age: a
	// relying party that cached the set just before publication must have
	// refreshed before the new key signs.
	for overlap, ok := range map[time.Duration]bool{
		eliteanative.CacheMaxAge - time.Second: false, eliteanative.CacheMaxAge: false,
		eliteanative.CacheMaxAge + time.Second: true,
	} {
		doc := replace(`"keys"`, `"rotation_overlap": "`+overlap.String()+`", "keys"`)
		if _, err := eliteanative.ParseConfig([]byte(doc), ""); (err == nil) != ok {
			t.Errorf("rotation_overlap %s: err = %v, want accepted = %v", overlap, err, ok)
		}
	}
	// A rotation: a second key with not_before is accepted.
	rotation := replace(`"file": "/etc/elitea/agent-assertion/es.pem"}`,
		`"file": "/etc/elitea/agent-assertion/es.pem"}, {"kid": "es-2", "alg": "ES256", "file": "/k/es2.pem", "not_before": "2026-11-01T00:00:00Z"}`)
	if _, err := eliteanative.ParseConfig([]byte(rotation), ""); err != nil {
		t.Fatal(err)
	}
}
