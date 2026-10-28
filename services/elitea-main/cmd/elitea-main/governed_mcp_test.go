package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

func governedMCPEnv(values map[string]string, read *[]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		*read = append(*read, name)
		value, ok := values[name]
		return value, ok
	}
}

func testMasterKey() ([]byte, error) { return bytes.Repeat([]byte{7}, 32), nil }

// Flag off: not one governed MCP variable is read and nothing is built,
// even when every variable is set.
func TestGovernedMCPBuildsNothingWithTheFlagOff(t *testing.T) {
	var read []string
	got, err := governedMCPAtBoot(profile.LegacyAdmission(true), governedMCPEnv(map[string]string{
		governedMCPInboundIssuerEnv: "https://idp.example.com", governedMCPOAuthEnabledEnv: "true",
	}, &read), nil, testMasterKey, "https://elitea.example.com", slog.Default())
	if err != nil || got.Inbound != nil || got.Outbound != nil || got.References != nil || got.authorizations() != nil {
		t.Fatalf("flag off built %+v, %v", got, err)
	}
	if len(read) != 0 {
		t.Fatalf("flag off read %v", read)
	}
}

func TestGovernedMCPRefusesAPartialConfiguration(t *testing.T) {
	governed := profile.GovernedAdmission(profile.PolicyRevision{ID: "2026-10-r1", Digest: [32]byte{1}})
	for name, tc := range map[string]struct {
		env    map[string]string
		origin string
		key    func() ([]byte, error)
		want   string
	}{
		"issuer without JWKS": {
			env: map[string]string{governedMCPInboundIssuerEnv: "https://idp.example.com"}, origin: "https://elitea.example.com",
			key: testMasterKey, want: governedMCPInboundJWKSURLEnv,
		},
		"outbound over http": {
			env: map[string]string{governedMCPOAuthEnabledEnv: "true"}, origin: "http://elitea.example.com",
			key: testMasterKey, want: "https DEPLOYMENT_URL",
		},
		"outbound without the master key": {
			env: map[string]string{governedMCPOAuthEnabledEnv: "true"}, origin: "https://elitea.example.com",
			key: func() ([]byte, error) { return nil, nil }, want: "master key",
		},
		"malformed flag": {
			env: map[string]string{governedMCPOAuthEnabledEnv: "yes please"}, origin: "https://elitea.example.com",
			key: testMasterKey, want: governedMCPOAuthEnabledEnv,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var read []string
			_, err := governedMCPAtBoot(governed, governedMCPEnv(tc.env, &read), nil, tc.key, tc.origin, slog.Default())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// Governed with outbound OAuth on: the service and its reference minter
// are built; inbound stays off without an issuer.
func TestGovernedMCPBuildsOutboundOAuth(t *testing.T) {
	governed := profile.GovernedAdmission(profile.PolicyRevision{ID: "2026-10-r1", Digest: [32]byte{1}})
	var read []string
	got, err := governedMCPAtBoot(governed, governedMCPEnv(map[string]string{governedMCPOAuthEnabledEnv: "true"}, &read),
		nil, testMasterKey, "https://elitea.example.com/", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got.Inbound != nil || got.Outbound == nil || got.authorizations() == nil {
		t.Fatalf("built %+v", got)
	}
}
