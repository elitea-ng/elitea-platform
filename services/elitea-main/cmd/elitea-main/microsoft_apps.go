package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftapps"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// microsoftAppsAtBoot seeds the Microsoft Default app registry from the Helm
// values (ADR-0057 decision 5): `microsoftOAuthApps` renders the
// ELITEA_MICROSOFT_OAUTH_APPS JSON, and each entry's client secret comes from the
// Kubernetes Secret the chart binds to ELITEA_MICROSOFT_OAUTH_APP_SECRET_<KEY>.
//
// Nothing ships enabled: an empty list (the default) reads nothing and writes
// nothing. A malformed entry refuses boot, because a half-applied seed would
// leave an app with the wrong client. A restart that changes nothing writes
// nothing (no revision, no audit event), a row an administrator disabled stays
// disabled, and the secret is only re-sealed when it differs from the sealed one.
// No secret, client id or ciphertext is logged.
func microsoftAppsAtBoot(
	ctx context.Context,
	lookup func(string) (string, bool),
	pool *pgxpool.Pool,
	masterKey func() ([]byte, error),
	logger *slog.Logger,
) error {
	seeds, err := microsoftapps.SeedFromEnv(lookup)
	if err != nil {
		return fmt.Errorf("microsoft oauth apps: %w", err)
	}
	if len(seeds) == 0 {
		return nil
	}
	if pool == nil || masterKey == nil {
		return errors.New("microsoft oauth apps: seeding needs the platform database and the vault master key")
	}
	key, err := masterKey()
	if err != nil {
		return fmt.Errorf("microsoft oauth apps: the vault master key: %w", err)
	}
	defer clear(key)
	if len(key) == 0 {
		return errors.New("microsoft oauth apps: seeding needs the vault master key (SECRETS_MASTER_KEY)")
	}
	audit, err := securityaudit.NewPostgresAppender("elitea-main")
	if err != nil {
		return fmt.Errorf("microsoft oauth apps: the security audit appender: %w", err)
	}
	store, err := microsoftapps.NewStore(pool, key, audit)
	if err != nil {
		return fmt.Errorf("microsoft oauth apps: %w", err)
	}
	report, err := store.Seed(ctx, seeds, logger)
	if err != nil {
		return fmt.Errorf("microsoft oauth apps: %w", err)
	}
	logger.Info("microsoft oauth apps seeded",
		"created", report.Created, "updated", report.Updated, "unchanged", report.Unchanged)
	return nil
}
