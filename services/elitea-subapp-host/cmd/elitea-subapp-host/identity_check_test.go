package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-subapp-host/internal/spi"
)

// The start-up rule for the identity secret (#2958, #2959). Each case reads
// the settings the way run() does and asks identityStartupCheck.
func TestIdentityStartupCheck(t *testing.T) {
	const strong = "dGhpcy1pcy1hLXRlc3Qtb25seS1pZGVudGl0eS1zZWNyZXQ="
	cases := []struct {
		name    string
		pairs   map[string]string
		wantErr string
	}{
		{"empty secret, no flag", map[string]string{spi.InsecureIdentityEnvVar: "false"}, "ELITEA_DEEPWIKI_IDENTITY_SECRET"},
		{"short secret", map[string]string{"ELITEA_DEEPWIKI_IDENTITY_SECRET": "e2e-deepwiki-identity"}, "ELITEA_DEEPWIKI_IDENTITY_SECRET"},
		{"placeholder secret", map[string]string{"ELITEA_DEEPWIKI_IDENTITY_SECRET": "changeme"}, "ELITEA_DEEPWIKI_IDENTITY_SECRET"},
		{"blank secret", map[string]string{"ELITEA_DEEPWIKI_IDENTITY_SECRET": "   "}, "ELITEA_DEEPWIKI_IDENTITY_SECRET"},
		{"insecure flag next to OIDC_ISSUER_URL", map[string]string{spi.InsecureIdentityEnvVar: "true", "OIDC_ISSUER_URL": "https://issuer.example.test"}, "OIDC_ISSUER_URL"},
		{"insecure flag next to ELITEA_AUTH_CONFIG_FILE", map[string]string{spi.InsecureIdentityEnvVar: "true", "ELITEA_AUTH_CONFIG_FILE": "/etc/elitea/auth.yaml"}, "ELITEA_AUTH_CONFIG_FILE"},
		{"insecure flag alone", map[string]string{spi.InsecureIdentityEnvVar: "true"}, ""},
		{"strong secret", map[string]string{"ELITEA_DEEPWIKI_IDENTITY_SECRET": strong, spi.InsecureIdentityEnvVar: "false"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := lookup(tc.pairs)
			settings, err := spi.SettingsFromEnv("ELITEA_DEEPWIKI_", env)
			if err != nil {
				t.Fatal(err)
			}
			err = identityStartupCheck(settings, env)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("start was not refused")
			}
			if !errors.Is(err, spi.ErrConfig) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want an ErrConfig naming %s", err, tc.wantErr)
			}
			if v := tc.pairs["ELITEA_DEEPWIKI_IDENTITY_SECRET"]; len(strings.TrimSpace(v)) >= 10 && strings.Contains(err.Error(), v) {
				t.Fatalf("the error repeats the secret: %v", err)
			}
		})
	}
}
