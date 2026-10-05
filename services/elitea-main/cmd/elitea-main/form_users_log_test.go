package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	v2auth "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

func TestLogFormUserConfigurationNamesTheRefusedLogins(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))

	logFormUserConfiguration(logger, authcomposition.FormUserReport{Configured: 3})
	if buffer.Len() != 0 {
		t.Fatalf("a clean configuration logged %q", buffer.String())
	}

	logFormUserConfiguration(logger, authcomposition.FormUserReport{
		Configured: 3, MisconfiguredLogins: []string{"alice", "bob"},
	})
	logged := buffer.String()
	for _, want := range []string{"level=WARN", "will be refused at sign-in", "logins=\"[alice bob]\"", "misconfigured=2"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log %q lacks %q", logged, want)
		}
	}
}

func TestLogReservedDomainSignInAccountsWarnsOnlyWhenFound(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))

	logReservedDomainSignInAccounts(logger, identityrepo.ReservedDomainAccounts{})
	if buffer.Len() != 0 {
		t.Fatalf("nothing found, yet logged %q", buffer.String())
	}
	logReservedDomainSignInAccounts(logger, identityrepo.ReservedDomainAccounts{
		Count: 2, ProviderReferences: []string{"alice", "carol"},
	})
	logged := buffer.String()
	for _, want := range []string{"level=WARN", "reserved @centry.user domain", "accounts=2", "UPGRADING.md"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log %q lacks %q", logged, want)
		}
	}
}

// Form sign-in is OFF by default: unset and empty both read false, and only
// an explicit boolean turns it on. A value that is not a boolean stops the
// boot instead of being guessed at.
func TestFormSignInEnabledFromEnvDefaultsOff(t *testing.T) {
	lookup := func(value string, present bool) func(string) (string, bool) {
		return func(name string) (string, bool) {
			if name != "ELITEA_FORM_LOGIN_ENABLED" {
				t.Fatalf("read %s", name)
			}
			return value, present
		}
	}
	for _, test := range []struct {
		value   string
		present bool
		want    bool
	}{
		{"", false, false},
		{"", true, false},
		{"  ", true, false},
		{"false", true, false},
		{"FALSE", true, false},
		{"0", true, false},
		{"true", true, true},
		{"TRUE", true, true},
		{"True", true, true},
		{" true ", true, true},
		{"1", true, true},
	} {
		got, err := formSignInEnabledFromEnv(lookup(test.value, test.present))
		if err != nil || got != test.want {
			t.Fatalf("value %q present %v: got %v, %v; want %v", test.value, test.present, got, err, test.want)
		}
	}
	// Exactly the documented spellings (true/false/1/0, any letter case):
	// strconv.ParseBool's extra t/T/f/F must stop the boot like any typo.
	for _, bad := range []string{"yes", "on", "enabled", "t", "T", "f", "F", "tru", "01"} {
		if _, err := formSignInEnabledFromEnv(lookup(bad, true)); err == nil ||
			!strings.Contains(err.Error(), "ELITEA_FORM_LOGIN_ENABLED") {
			t.Fatalf("value %q: error = %v, want a boot error naming the variable", bad, err)
		}
	}
}

func TestLogFormSignInDisabledSaysTheUsersAreIgnored(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))
	logFormSignInDisabled(logger, authcomposition.FormUserReport{Configured: 3})
	logged := buffer.String()
	for _, want := range []string{"level=WARN", "Form sign-in is disabled", "ignored_users=3"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log %q lacks %q", logged, want)
		}
	}
}

// A fresh install with Form sign-in off (the default) makes its first
// administrator through OIDC or SAML, from the document's list or, without
// one, the environment's. Nothing here depends on Form sign-in.
func TestSingleSignOnFirstLoginPolicyBootstrapsWithoutFormSignIn(t *testing.T) {
	environment := func() []string { return []string{"email:first-admin@example.com"} }

	fromEnvironment := singleSignOnFirstLoginPolicy(v2auth.FirstLoginPolicy{}, environment)
	if len(fromEnvironment.InitialGlobalAdmins) != 1 || fromEnvironment.InitialGlobalAdmins[0] != "email:first-admin@example.com" {
		t.Fatalf("an install with no document list must take the environment's: %+v", fromEnvironment)
	}
	fromDocument := singleSignOnFirstLoginPolicy(
		v2auth.FirstLoginPolicy{InitialGlobalAdmins: []string{"oidc:subject-1"}}, environment)
	if len(fromDocument.InitialGlobalAdmins) != 1 || fromDocument.InitialGlobalAdmins[0] != "oidc:subject-1" {
		t.Fatalf("the document's list must win: %+v", fromDocument)
	}
}
