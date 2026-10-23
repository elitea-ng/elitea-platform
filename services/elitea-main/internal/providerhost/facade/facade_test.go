package facade_test

import (
	"context"
	"strings"
	"testing"

	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware/authtest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/facade"
)

var names = facade.EnvNames{
	Enabled:        "P_ENABLED",
	Target:         "P_TARGET",
	RetiredBaseURL: "P_BASE_URL",
	ClientCertFile: "P_CERT",
	ClientKeyFile:  "P_KEY",
	CAFile:         "P_CA",
	ServerName:     "P_SERVER_NAME",
	IdentitySecret: "P_SECRET",
	Timeout:        "P_TIMEOUT",
}

func env(pairs map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := pairs[key]
		return value, ok
	}
}

// A DISABLED facade must compose cleanly with nothing else set. It is the
// shipped default, and demanding certificates from a deployment that has not
// turned the provider on would fail every install on a component it does not
// run.
func TestADisabledFacadeNeedsNothingElse(t *testing.T) {
	cfg, err := facade.ConfigFromEnv(names, env(map[string]string{"P_ENABLED": "false"}))
	if err != nil {
		t.Fatalf("a disabled facade was refused: %v", err)
	}
	if cfg.Enabled {
		t.Error("Enabled is true for P_ENABLED=false")
	}
}

// Every missing name is reported TOGETHER. One per restart is how an operator
// configures a new provider five times.
func TestEveryMissingVariableIsNamedAtOnce(t *testing.T) {
	_, err := facade.ConfigFromEnv(names, env(map[string]string{"P_ENABLED": "true"}))
	if err == nil {
		t.Fatal("an enabled facade with nothing configured was accepted")
	}
	for _, want := range []string{"P_TARGET", "P_CERT", "P_KEY", "P_CA", "P_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not name %s: %v", want, err)
		}
	}
}

// The retired HTTPS origin is refused by name, with the variable that
// replaced it, whether the facade is on or off: an operator who set it meant
// it, and reading it as nothing leaves them guessing why nothing answers.
func TestTheRetiredBaseURLIsRefusedByName(t *testing.T) {
	for _, enabled := range []string{"true", "false"} {
		_, err := facade.ConfigFromEnv(names, env(map[string]string{"P_ENABLED": enabled, "P_BASE_URL": "https://provider:8080"}))
		if err == nil || !strings.Contains(err.Error(), "P_BASE_URL") || !strings.Contains(err.Error(), "P_TARGET") {
			t.Errorf("enabled=%s: %v", enabled, err)
		}
	}
}

// The leading-zero rule. "007" parses to 7, so a caller could address one
// project by several spellings — and the value reaches a permission resolver,
// which would then be asked about a string the audit trail records differently
// from the project it resolved.
func TestValidProjectID(t *testing.T) {
	for _, valid := range []string{"1", "42", "999999"} {
		if !facade.ValidProjectID(valid) {
			t.Errorf("%q was refused", valid)
		}
	}
	// "0" is here, not above: the rule is a POSITIVE id, and project 0 does
	// not exist. It survives the leading-zero branch (which exempts the bare
	// string) only to be refused by the positivity check — a detail worth
	// pinning, because a future "simplification" of that branch would let it
	// through.
	for _, invalid := range []string{"", "0", "007", "01", "-1", "1.0", "abc", " 1", "1 "} {
		if facade.ValidProjectID(invalid) {
			t.Errorf("%q was accepted", invalid)
		}
	}
}

// Composable is the nil-validator check every route in this service makes. A
// route serves perfectly well without a validator; it simply does not
// authenticate, and that is invisible at runtime.
func TestComposableRefusesAnIncompleteAuthConfig(t *testing.T) {
	complete := apimw.AuthConfig{
		PrincipalValidator: stubPrincipal{},
		FormSessions:       authtest.FormSessions{},
	}
	if !facade.Composable(complete, stubResolver{}) {
		t.Fatal("a complete configuration was refused")
	}
	if facade.Composable(apimw.AuthConfig{FormSessions: authtest.FormSessions{}}, stubResolver{}) {
		t.Error("a nil PrincipalValidator was accepted")
	}
	if facade.Composable(apimw.AuthConfig{PrincipalValidator: stubPrincipal{}}, stubResolver{}) {
		t.Error("a configuration that can authenticate nobody was accepted")
	}
	// OIDC-only: no forwarded identity, but a token validator for the tokens
	// APPLICATION_SECRET_KEY signs — which is what reads a callback bearer back.
	oidcOnly := apimw.AuthConfig{PrincipalValidator: stubPrincipal{}, Validator: stubTokens{}, SessionSecret: "s"}
	if !facade.Composable(oidcOnly, stubResolver{}) {
		t.Error("the OIDC-only credential set was refused")
	}
	if facade.Composable(complete, nil) {
		t.Error("a nil permission resolver was accepted")
	}
}

/* ── stubs ─────────────────────────────────────────────────────────────── */

type stubPrincipal struct{}

func (stubPrincipal) ValidatePrincipal(_ context.Context, u auth.User) (auth.User, error) {
	return u, nil
}

type stubResolver struct{}

func (stubResolver) ResolvePermissions(
	context.Context, auth.User, string, string,
) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{}, nil
}

type stubTokens struct{}

func (stubTokens) ValidateToken(context.Context, string) (auth.User, error) { return auth.User{}, nil }
