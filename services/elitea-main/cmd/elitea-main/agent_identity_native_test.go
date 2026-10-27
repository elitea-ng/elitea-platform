package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
)

func agentAssertionRevision(modes ...agentidentity.IdentityMode) profile.PolicyRevision {
	return profile.PolicyRevision{ID: "2026-10-r1", Document: profile.PolicyDocument{
		Schema: profile.PolicySchemaV1, AllowedIdentityModes: modes,
		MaxMandateTTLSeconds: 3600, AuditBacklogAlertThreshold: 100}}
}

func writeAgentTestKey(t *testing.T, dir, name string, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func agentAssertionEnv(t *testing.T) (map[string]string, string, *ecdsa.PrivateKey) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := writeAgentTestKey(t, dir, "agent.pem", key)
	return map[string]string{
		"DEPLOYMENT_URL": "https://elitea.example.com",
		eliteanative.EnvConfig: fmt.Sprintf(`{"keys":[{"kid":"agent-es-1","alg":"ES256","file":%q}],
			"relying_parties":[{"id":"aws","audience":"sts.amazonaws.com","claims_profile":"aws"}]}`, path),
	}, dir, key
}

// Flag off: nothing is read, not even the configuration variable, and no
// route is mounted.
func TestEliteaNativeIsNotBuiltWithTheFlagOff(t *testing.T) {
	got, err := eliteaNativeAgentIdentityAtBoot(context.Background(), profile.LegacyAdmission(false), profile.PolicyRevision{},
		func(name string) (string, bool) { t.Fatalf("flag off read %s", name); return "", false }, nil, 1, nil)
	if err != nil || got.Provider != nil || got.WellKnown != nil {
		t.Fatalf("flag off built %+v, %v", got, err)
	}
}

// Flag on without a relying party, or with a revision that does not allow
// elitea_native: not built, no key loaded.
func TestEliteaNativeIsNotBuiltWithoutConfigurationOrPolicy(t *testing.T) {
	revision := agentAssertionRevision(agentidentity.IdentityModeEliteaNative)
	got, err := eliteaNativeAgentIdentityAtBoot(context.Background(), profile.GovernedAdmission(revision), revision,
		lookupOf(map[string]string{}), nil, 1, nil)
	if err != nil || got.Provider != nil || got.WellKnown != nil {
		t.Fatalf("unconfigured built %+v, %v", got, err)
	}
	env, _, _ := agentAssertionEnv(t)
	// A key file that does not exist proves no key is loaded.
	env[eliteanative.EnvConfig] = strings.Replace(env[eliteanative.EnvConfig], "agent.pem", "missing.pem", 1)
	denied := agentAssertionRevision(agentidentity.IdentityModeDedicatedAgent)
	got, err = eliteaNativeAgentIdentityAtBoot(context.Background(), profile.GovernedAdmission(denied), denied, lookupOf(env), nil, 1, nil)
	if err != nil || got.Provider != nil || got.WellKnown != nil {
		t.Fatalf("a revision without elitea_native built %+v, %v", got, err)
	}
}

func TestEliteaNativeIsBuiltWhenConfigured(t *testing.T) {
	revision := agentAssertionRevision(agentidentity.IdentityModeEliteaNative)
	env, _, _ := agentAssertionEnv(t)
	got, err := eliteaNativeAgentIdentityAtBoot(context.Background(), profile.GovernedAdmission(revision), revision, lookupOf(env), nil, 1, nil)
	if err != nil || got.Provider == nil || got.WellKnown == nil {
		t.Fatalf("configured: %+v, %v", got, err)
	}
	if got.Provider.Name() != "elitea" {
		t.Fatalf("name = %q", got.Provider.Name())
	}
	// A set but invalid configuration refuses boot.
	env[eliteanative.EnvConfig] = `{"keys":[]}`
	if _, err := eliteaNativeAgentIdentityAtBoot(context.Background(), profile.GovernedAdmission(revision), revision, lookupOf(env), nil, 1, nil); !errors.Is(err, eliteanative.ErrInvalidConfig) {
		t.Fatalf("err = %v", err)
	}
}

// Acceptance (negative): a configuration that reuses the command signing
// key (which also signs code-sandbox owner grants) refuses boot, by key id,
// by file and by key material.
func TestEliteaNativeRefusesBootWhenItReusesAnotherPurposesKey(t *testing.T) {
	revision := agentAssertionRevision(agentidentity.IdentityModeEliteaNative)
	boot := func(env map[string]string) error {
		_, err := eliteaNativeAgentIdentityAtBoot(context.Background(), profile.GovernedAdmission(revision), revision, lookupOf(env), nil, 1, nil)
		return err
	}
	_, commandKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	env, dir, _ := agentAssertionEnv(t)
	env["ELITEA_RUNTIME_SIGNING_KEY_ID"] = "agent-es-1"
	env["ELITEA_RUNTIME_SIGNING_KEY_FILE"] = writeAgentTestKey(t, dir, "command.pem", commandKey)
	if err := boot(env); !errors.Is(err, eliteanative.ErrKeyReuse) {
		t.Fatalf("the command signing key id: err = %v", err)
	}

	env, dir, _ = agentAssertionEnv(t)
	agentFile := filepath.Join(dir, "agent.pem")
	env["ELITEA_RUNTIME_SIGNING_KEY_ID"] = "command-1"
	env["ELITEA_RUNTIME_SIGNING_KEY_FILE"] = agentFile
	if err := boot(env); !errors.Is(err, eliteanative.ErrKeyReuse) {
		t.Fatalf("the command signing key file: err = %v", err)
	}

	// The same EC material in another file, as the code owner TLS identity.
	env, dir, key := agentAssertionEnv(t)
	env["ELITEA_RUNTIME_CODE_OWNER_RECOVERY_CONFIG"] = fmt.Sprintf(`{"private_key_path":%q}`, writeAgentTestKey(t, dir, "tls.pem", key))
	if err := boot(env); !errors.Is(err, eliteanative.ErrKeyReuse) || !strings.Contains(err.Error(), "code owner TLS") {
		t.Fatalf("the code owner TLS key material: err = %v", err)
	}

	// Distinct keys of every other purpose boot.
	env, dir, _ = agentAssertionEnv(t)
	env["ELITEA_RUNTIME_SIGNING_KEY_ID"] = "command-1"
	env["ELITEA_RUNTIME_SIGNING_KEY_FILE"] = writeAgentTestKey(t, dir, "command.pem", commandKey)
	if err := boot(env); err != nil {
		t.Fatalf("distinct keys: %v", err)
	}
	// A configured foreign key that cannot be read refuses boot.
	env["ELITEA_RUNTIME_SIGNING_KEY_FILE"] = filepath.Join(dir, "absent.pem")
	if err := boot(env); err == nil {
		t.Fatal("an unreadable command signing key booted")
	}
}
