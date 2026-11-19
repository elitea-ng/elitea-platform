package microsoftapps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// The Helm seed (ADR-0057 decision 5): `microsoftOAuthApps` values name Default
// apps, and Main upserts one row per entry at start, with the client secret read
// from a Kubernetes Secret, never from the values. An empty list seeds nothing:
// nothing ships enabled.
const (
	// SeedConfigEnv holds the JSON array the chart renders from the values. It
	// carries no secret.
	SeedConfigEnv = "ELITEA_MICROSOFT_OAUTH_APPS"
	// SeedSecretEnvPrefix is followed by the entry's key, upper-cased with
	// every character outside A-Z and 0-9 turned into an underscore. The chart
	// binds it to the entry's Secret key with a secretKeyRef.
	SeedSecretEnvPrefix = "ELITEA_MICROSOFT_OAUTH_APP_SECRET_"
)

// SeedApp is one entry of the Helm values.
type SeedApp struct {
	// Key is the stable name of the row; a re-run updates the row it names.
	Key              string              `json:"key"`
	Name             string              `json:"name"`
	Description      string              `json:"description"`
	ClientID         string              `json:"clientId"`
	Authority        string              `json:"authority"`
	CredentialTypes  []string            `json:"credentialTypes"`
	ScopeCeiling     map[string][]string `json:"scopeCeiling"`
	AutoRefreshToken *bool               `json:"autoRefreshToken"`
	ProjectScope     string              `json:"projectScope"`
	ProjectIDs       []int64             `json:"projectIds"`
	// SecretExpiresAt is an RFC 3339 date or date-time.
	SecretExpiresAt string `json:"secretExpiresAt"`
	// Secret is read from the environment, never from the JSON.
	Secret string `json:"-"`
}

// SeedSecretEnv is the environment variable that carries an entry's secret.
func SeedSecretEnv(key string) string {
	var b strings.Builder
	b.WriteString(SeedSecretEnvPrefix)
	for _, r := range strings.ToUpper(key) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// SeedFromEnv reads the seed. An unset or empty variable seeds nothing. A
// malformed entry is an error: the operator must see it, and a half-applied seed
// would leave an app with the wrong client.
func SeedFromEnv(lookup func(string) (string, bool)) ([]SeedApp, error) {
	raw, _ := lookup(SeedConfigEnv)
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil, nil
	}
	var apps []SeedApp
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&apps); err != nil {
		return nil, fmt.Errorf("%s: %w", SeedConfigEnv, err)
	}
	seen := map[string]bool{}
	for i := range apps {
		app := &apps[i]
		if !ValidSeedKey(app.Key) {
			return nil, fmt.Errorf("%s: entry %d: key %q must be lower-case letters, digits and dashes, up to 40 characters", SeedConfigEnv, i, app.Key)
		}
		if seen[app.Key] {
			return nil, fmt.Errorf("%s: key %q is listed twice", SeedConfigEnv, app.Key)
		}
		seen[app.Key] = true
		if value, ok := lookup(SeedSecretEnv(app.Key)); ok {
			app.Secret = strings.TrimSpace(value)
		}
		if _, err := app.input(); err != nil {
			return nil, fmt.Errorf("%s: entry %q: %w", SeedConfigEnv, app.Key, err)
		}
	}
	return apps, nil
}

func (a SeedApp) input() (Input, error) {
	in := Input{
		Name: a.Name, Description: a.Description, ClientID: a.ClientID, Authority: a.Authority,
		CredentialTypes: a.CredentialTypes, ScopeCeiling: a.ScopeCeiling,
		AutoRefreshToken: a.AutoRefreshToken == nil || *a.AutoRefreshToken,
		ProjectScope:     ProjectScope(a.ProjectScope), ProjectIDs: a.ProjectIDs,
	}
	if a.SecretExpiresAt != "" {
		expires, err := ParseExpiry(a.SecretExpiresAt)
		if err != nil {
			return Input{}, invalid("secretExpiresAt", "use a date such as 2027-06-30")
		}
		in.SecretExpiresAt = &expires
	}
	if a.Secret != "" {
		secret := a.Secret
		in.Secret = &secret
	}
	return in.Normalize()
}

// SeedReport counts what a seed did.
type SeedReport struct {
	Created, Updated, Unchanged int
}

var actorHelm = Actor{Ref: "helm-seed"}

// seedLock serializes the seed across replicas: every Main pod runs it at start, and
// two pods racing the check-then-create would otherwise turn a unique violation
// into a refused boot.
const seedLock = int64(0x0255_2267)

// seedDigest is the keyed digest of what an entry asks for: its definition and its
// secret. It is stored with the row (seed_hash), so a restart can tell "the chart
// changed" from "an administrator edited the row in the console".
func (s *Store) seedDigest(in Input) string {
	canonical, _ := json.Marshal(in)
	return s.sealer.digest(canonical)
}

// Seed applies the entries once per change of the chart. The rule that keeps the
// console and the chart from fighting: a row is written only when what the chart
// asks for CHANGED since the last time it was applied (its keyed digest, seed_hash,
// differs). A restart with unchanged values writes nothing, so an administrator's
// console edits (a narrowed project scope, a rotated secret) survive it; a changed
// value in the chart is applied. An administrator's disable always survives, and a
// secret the chart does not supply keeps the sealed one. A quiet restart writes no
// revision and no audit event.
func (s *Store) Seed(ctx context.Context, apps []SeedApp, logger *slog.Logger) (SeedReport, error) {
	var report SeedReport
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return report, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, seedLock); err != nil {
		return report, err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, seedLock) }()

	for _, seed := range apps {
		in, err := seed.input()
		if err != nil {
			return report, fmt.Errorf("microsoft oauth app %q: %w", seed.Key, err)
		}
		hash := s.seedDigest(in)
		current, found, err := s.bySeedKey(ctx, seed.Key)
		if err != nil {
			return report, err
		}
		if !found {
			if in.Secret == nil && logger != nil {
				logger.Warn("microsoft oauth app seeded without a secret; it stays a draft until the Secret is bound",
					"key", seed.Key, "variable", SeedSecretEnv(seed.Key))
			}
			if _, err := s.create(ctx, seed.Key, hash, in, actorHelm); err != nil {
				return report, seedError(seed.Key, err)
			}
			report.Created++
			continue
		}
		if current.SeedHash == hash {
			report.Unchanged++
			continue
		}
		if len(changedFields(current, in)) == 0 && in.Secret == nil {
			// The chart changed in a way the row already has (or only the
			// administrator's edit differs); record that it was seen.
			if err := s.setSeedHash(ctx, current.ID, hash); err != nil {
				return report, err
			}
			report.Unchanged++
			continue
		}
		if _, err := s.Update(ctx, current.ID, in, actorHelm); err != nil {
			return report, seedError(seed.Key, err)
		}
		if err := s.setSeedHash(ctx, current.ID, hash); err != nil {
			return report, err
		}
		report.Updated++
	}
	return report, nil
}

func seedError(key string, err error) error {
	if errors.Is(err, ErrConflict) {
		return fmt.Errorf("microsoft oauth app %q: an app with this name already exists (rename it in the console or in the values)", key)
	}
	return fmt.Errorf("microsoft oauth app %q: %w", key, err)
}

func (s *Store) setSeedHash(ctx context.Context, id, hash string) error {
	_, err := s.pool.Exec(ctx, `UPDATE elitea_identity.microsoft_oauth_apps SET seed_hash = $2 WHERE id = $1::uuid`, id, hash)
	return err
}

func (s *Store) bySeedKey(ctx context.Context, key string) (App, bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id::text FROM elitea_identity.microsoft_oauth_apps WHERE seed_key = $1`, key).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return App{}, false, nil
		}
		return App{}, false, err
	}
	app, err := s.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return App{}, false, nil
	}
	return app, err == nil, err
}
