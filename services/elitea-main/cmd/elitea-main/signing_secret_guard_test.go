package main

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	v2secrets "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/secrets"
)

// These pin the start-up refusal of weak signing secrets (#2958) and of the
// unwrapped-vault opt-out next to a real authentication configuration. Like
// auth_dev_mode_guard_test.go they call the pure predicates, never run().

func random32(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestSigningSecretGuardRefusesWeakApplicationSecretKey(t *testing.T) {
	for name, value := range map[string]string{
		"10 bytes":   "0123456789",
		"changeme":   "changeme",
		"whitespace": "    ",
	} {
		t.Run(name, func(t *testing.T) {
			err := requireStrongSigningSecrets(envMap(map[string]string{"APPLICATION_SECRET_KEY": value}))
			if err == nil {
				t.Fatal("start was not refused")
			}
			if !strings.Contains(err.Error(), "APPLICATION_SECRET_KEY") {
				t.Fatalf("the error does not name the variable: %v", err)
			}
			if strings.TrimSpace(value) != "" && strings.Contains(err.Error(), value) {
				t.Fatalf("the error repeats the value: %v", err)
			}
		})
	}
}

func TestSigningSecretGuardAcceptsRandom32Bytes(t *testing.T) {
	env := map[string]string{
		"APPLICATION_SECRET_KEY":           random32(t),
		"GATEWAY_IDENTITY_SECRET":          random32(t),
		"ELITEA_DEEPWIKI_ENABLED":          "true",
		"ELITEA_DEEPWIKI_IDENTITY_SECRET":  random32(t),
		"ELITEA_INVENTORY_ENABLED":         "true",
		"ELITEA_INVENTORY_IDENTITY_SECRET": random32(t),
	}
	if err := requireStrongSigningSecrets(envMap(env)); err != nil {
		t.Fatalf("strong secrets were refused: %v", err)
	}
	// Nothing set: no facade, no tokens. run() warns about the missing key;
	// the guard has nothing to refuse.
	if err := requireStrongSigningSecrets(envMap(nil)); err != nil {
		t.Fatalf("an environment with no signing secret was refused: %v", err)
	}
}

func TestSigningSecretGuardIdentitySecrets(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"weak gateway identity", map[string]string{"GATEWAY_IDENTITY_SECRET": "short"}, "GATEWAY_IDENTITY_SECRET"},
		{"deepwiki enabled, no secret", map[string]string{"ELITEA_DEEPWIKI_ENABLED": "true"}, "ELITEA_DEEPWIKI_IDENTITY_SECRET"},
		{"deepwiki disabled, weak secret", map[string]string{"ELITEA_DEEPWIKI_IDENTITY_SECRET": "changeme"}, "ELITEA_DEEPWIKI_IDENTITY_SECRET"},
		{"inventory enabled, no secret", map[string]string{"ELITEA_INVENTORY_ENABLED": "1"}, "ELITEA_INVENTORY_IDENTITY_SECRET"},
		{"inventory enabled, short secret", map[string]string{"ELITEA_INVENTORY_ENABLED": "true", "ELITEA_INVENTORY_IDENTITY_SECRET": "e2e-inventory-identity"}, "ELITEA_INVENTORY_IDENTITY_SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireStrongSigningSecrets(envMap(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.wantVar) {
				t.Fatalf("err = %v, want a refusal naming %s", err, tc.wantVar)
			}
		})
	}
}

func TestUnwrappedOptOutRefusedNextToRealAuthentication(t *testing.T) {
	for _, configured := range []string{"OIDC_ISSUER_URL", "ELITEA_AUTH_CONFIG_FILE"} {
		t.Run(configured, func(t *testing.T) {
			_, err := requireVaultMasterKey(envMap(map[string]string{
				v2secrets.AllowUnwrappedEnvVar: "true",
				configured:                     "https://issuer.example.test",
			}))
			if err == nil {
				t.Fatalf("the opt-out was honoured next to %s", configured)
			}
			if !strings.Contains(err.Error(), configured) || !strings.Contains(err.Error(), v2secrets.AllowUnwrappedEnvVar) {
				t.Fatalf("the error does not name both variables: %v", err)
			}
		})
	}
}

func TestUnwrappedOptOutAloneStillWarns(t *testing.T) {
	warning, err := requireVaultMasterKey(envMap(map[string]string{v2secrets.AllowUnwrappedEnvVar: "true"}))
	if err != nil {
		t.Fatalf("the development opt-out alone was refused: %v", err)
	}
	if !strings.Contains(warning, "UNWRAPPED") {
		t.Fatalf("warning = %q, want the unwrapped-storage warning", warning)
	}
}
