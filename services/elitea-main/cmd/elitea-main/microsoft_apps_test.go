package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
)

func TestMicrosoftAppsSeedIsInertByDefault(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))
	// No values, no database, no key: nothing is read, nothing is written, and
	// boot is not affected. Nothing ships enabled.
	for _, env := range []map[string]string{nil, {microsoftapps.SeedConfigEnv: ""}, {microsoftapps.SeedConfigEnv: "[]"}} {
		if err := microsoftAppsAtBoot(context.Background(), envLookup(env), nil, nil, logger); err != nil {
			t.Errorf("env %v refused boot: %v", env, err)
		}
	}
	if logged.Len() != 0 {
		t.Errorf("an empty seed logged %q", logged.String())
	}
}

func TestMicrosoftAppsSeedRefusesBootWhenItCannotBeApplied(t *testing.T) {
	entry := `[{"key":"corp","name":"Corp","clientId":"7f8d0c1e-0000-4000-8000-000000000001",` +
		`"authority":"https://login.microsoftonline.com/organizations","credentialTypes":["outlook"]}]`
	const secret = "s3cr3t~value.from.Entra"
	env := map[string]string{
		microsoftapps.SeedConfigEnv:             entry,
		microsoftapps.SeedSecretEnv("corp"):     secret,
		microsoftapps.SeedSecretEnvPrefix + "X": "unrelated",
	}
	// Configured, but no database: refuse, and name neither the secret nor the client.
	err := microsoftAppsAtBoot(context.Background(), envLookup(env), nil, nil, slog.Default())
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("seeding without a database: %v", err)
	}
	// A malformed entry refuses boot before anything is written.
	env[microsoftapps.SeedConfigEnv] = `[{"key":"Not A Key"}]`
	err = microsoftAppsAtBoot(context.Background(), envLookup(env), nil, nil, slog.Default())
	if err == nil || !strings.Contains(err.Error(), microsoftapps.SeedConfigEnv) {
		t.Errorf("a malformed seed: %v", err)
	}
}
