package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

func providerOAuthEnv(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

var jiraOAuthEnv = map[string]string{
	"ELITEA_PROVIDER_OAUTH_JIRA_CLIENT_ID":     "client",
	"ELITEA_PROVIDER_OAUTH_JIRA_CLIENT_SECRET": "secret",
	"ELITEA_PROVIDER_OAUTH_JIRA_REDIRECT_URI":  "https://elitea.example.com/api/v2/credential_consent/jira:callback",
}

// Flag off: nothing is read and nothing is built, so no consent route
// mounts.
func TestProviderOAuthBuildsNothingWithTheFlagOff(t *testing.T) {
	built, err := providerOAuthAtBoot(profile.LegacyAdmission(true), func(string) (string, bool) {
		t.Fatal("the flag-off composition read the environment")
		return "", false
	}, nil, func() ([]byte, error) {
		t.Fatal("the flag-off composition read the master key")
		return nil, nil
	}, slog.Default())
	if err != nil || built.Consent != nil || built.Tokens != nil {
		t.Fatalf("flag off built %+v err=%v", built, err)
	}
}

func TestProviderOAuthBoot(t *testing.T) {
	governed := profile.GovernedAdmission(profile.PolicyRevision{ID: "2026-10-r1", Digest: [32]byte{1}})
	pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/none?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	key := func() ([]byte, error) { return make([]byte, 32), nil }

	t.Run("no application configured builds nothing", func(t *testing.T) {
		built, err := providerOAuthAtBoot(governed, providerOAuthEnv(nil), pool, key, slog.Default())
		if err != nil || built.Consent != nil || built.Tokens != nil {
			t.Fatalf("built %+v err=%v", built, err)
		}
	})
	t.Run("an application without the master key refuses boot", func(t *testing.T) {
		_, err := providerOAuthAtBoot(governed, providerOAuthEnv(jiraOAuthEnv), pool,
			func() ([]byte, error) { return nil, errors.New("SECRETS_MASTER_KEY is not set") }, slog.Default())
		if err == nil || !strings.Contains(err.Error(), "master key") {
			t.Fatalf("err = %v, want a master key refusal", err)
		}
	})
	t.Run("an unset master key refuses boot", func(t *testing.T) {
		_, err := providerOAuthAtBoot(governed, providerOAuthEnv(jiraOAuthEnv), pool,
			func() ([]byte, error) { return nil, nil }, slog.Default())
		if err == nil || !strings.Contains(err.Error(), "master key") {
			t.Fatalf("err = %v, want a master key refusal", err)
		}
	})
	t.Run("a redirect URI off the callback route refuses boot", func(t *testing.T) {
		env := map[string]string{}
		for k, v := range jiraOAuthEnv {
			env[k] = v
		}
		env["ELITEA_PROVIDER_OAUTH_JIRA_REDIRECT_URI"] = "https://elitea.example.com/oauth/callback"
		if _, err := providerOAuthAtBoot(governed, providerOAuthEnv(env), pool, key, slog.Default()); err == nil {
			t.Fatal("a foreign redirect URI was accepted")
		}
	})
	t.Run("an Aha! application without endpoints refuses boot", func(t *testing.T) {
		env := map[string]string{
			"ELITEA_PROVIDER_OAUTH_AHA_CLIENT_ID":     "client",
			"ELITEA_PROVIDER_OAUTH_AHA_CLIENT_SECRET": "secret",
			"ELITEA_PROVIDER_OAUTH_AHA_REDIRECT_URI":  "https://elitea.example.com/api/v2/credential_consent/aha:callback",
		}
		if _, err := providerOAuthAtBoot(governed, providerOAuthEnv(env), pool, key, slog.Default()); err == nil {
			t.Fatal("Aha! without its account endpoints was accepted")
		}
	})
	t.Run("a configured application builds consent and the token source", func(t *testing.T) {
		built, err := providerOAuthAtBoot(governed, providerOAuthEnv(jiraOAuthEnv), pool, key, slog.Default())
		if err != nil || built.Consent == nil || built.Tokens == nil {
			t.Fatalf("built %+v err=%v", built, err)
		}
	})
}
