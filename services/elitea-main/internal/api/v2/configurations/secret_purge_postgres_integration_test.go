package configurations_test

// #1204: deleting a configuration must take the hidden vault secret its row
// referenced with it, in the same transaction, unless another row still
// references that secret. An update that replaces a sealed value must not
// orphan the previous one either.
//
// Real PostgreSQL and the real vault: the credential is saved through the real
// Create route (so the row holds the {{secret.NAME}} reference and the value is
// sealed exactly as production seals it), and the vault is read back through
// the production loader after each write.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

func purgeVaultHolds(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	reader, err := storage.NewPostgresSecretVaultLoader(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Destroy()
	vault, err := reader.LoadProjectVault(context.Background(), storedCheckProject)
	if err != nil {
		t.Fatalf("load the project vault: %v", err)
	}
	_, err = vault.Lookup(name)
	if err != nil && !errors.Is(err, centrysecrets.ErrSecretNotFound) {
		t.Fatalf("look up %s: %v", name, err)
	}
	return err == nil
}

// purgeStoredSecretName returns the hidden-secret name the row's api_key
// references.
func purgeStoredSecretName(t *testing.T, pool *pgxpool.Pool, id int) string {
	t.Helper()
	var reference string
	if err := pool.QueryRow(context.Background(), fmt.Sprintf(
		`SELECT data->>'api_key' FROM p_%d.configuration WHERE id = $1`, storedCheckProject), id).
		Scan(&reference); err != nil {
		t.Fatalf("read the stored reference: %v", err)
	}
	name := strings.TrimSuffix(strings.TrimPrefix(reference, "{{secret."), "}}")
	if len(name) != 32 {
		t.Fatalf("the row holds no hidden-secret reference: %q", reference)
	}
	return name
}

func purgeDo(t *testing.T, router http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func purgeConfigurationPath(id int) string {
	return fmt.Sprintf("/api/v2/configurations/configuration/%d/%d", storedCheckProject, id)
}

func TestDeletingAConfigurationPurgesItsHiddenSecret(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})
	id := saveCredential(t, pool, router, "OpenAI", "sk-purged-on-delete")
	name := purgeStoredSecretName(t, pool, id)
	if !purgeVaultHolds(t, pool, name) {
		t.Fatalf("the seed sealed nothing under %s", name)
	}

	recorder := purgeDo(t, router, http.MethodDelete, purgeConfigurationPath(id), nil)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if purgeVaultHolds(t, pool, name) {
		t.Fatalf("the hidden secret %s outlived the configuration that owned it", name)
	}
}

// A secret another row still references survives — another configuration
// row, a toolkit row, and any copy elsewhere in the tenant schema: a
// conversation participant's settings, and a table this code has never heard
// of (the scan reads the catalog, so a copy in a later table counts too).
func TestDeletingAConfigurationKeepsAStillReferencedSecret(t *testing.T) {
	for _, holder := range []string{"configuration", "toolkit", "participant", "unknown table"} {
		t.Run(holder, func(t *testing.T) {
			pool := newStoredCheckPool(t)
			router := storedCheckRouter(t, pool, &fakeConnectionChecker{})
			id := saveCredential(t, pool, router, "OpenAI", "sk-shared-by-two-rows")
			name := purgeStoredSecretName(t, pool, id)
			reference := "{{secret." + name + "}}"

			ctx := context.Background()
			switch holder {
			case "configuration":
				seedModel(t, pool, "copy", map[string]any{"token": reference}, false)
			case "toolkit":
				// 001_initial.sql creates p_N.elitea_tools.
				if _, err := pool.Exec(ctx, fmt.Sprintf(`
					INSERT INTO p_%d.elitea_tools (name, type, owner_id, author_id, created_at, settings)
					VALUES ('gh', 'github', 1, 1, (now() AT TIME ZONE 'UTC'),
						jsonb_build_object('access_token', $1::text))`,
					storedCheckProject), reference); err != nil {
					t.Fatalf("seed the toolkit row: %v", err)
				}
			case "participant":
				if _, err := pool.Exec(ctx, fmt.Sprintf(`
					INSERT INTO p_%d.chat_participants (uuid, entity_name, entity_meta)
					VALUES (gen_random_uuid(), 'toolkit',
						jsonb_build_object('settings', jsonb_build_object('token', $1::text)))`,
					storedCheckProject), reference); err != nil {
					t.Fatalf("seed the participant row: %v", err)
				}
			case "unknown table":
				if _, err := pool.Exec(ctx, fmt.Sprintf(`
					CREATE TABLE p_%d.some_later_snapshot (id serial PRIMARY KEY, body json NOT NULL)`,
					storedCheckProject)); err != nil {
					t.Fatalf("create the table: %v", err)
				}
				if _, err := pool.Exec(ctx, fmt.Sprintf(`
					INSERT INTO p_%d.some_later_snapshot (body) VALUES (json_build_object('nested', json_build_array($1::text)))`,
					storedCheckProject), "Bearer "+reference); err != nil {
					t.Fatalf("seed the snapshot row: %v", err)
				}
			}

			recorder := purgeDo(t, router, http.MethodDelete, purgeConfigurationPath(id), nil)
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("delete answered %d: %s", recorder.Code, recorder.Body.String())
			}
			if !purgeVaultHolds(t, pool, name) {
				t.Fatalf("the hidden secret %s was purged while a %s row still references it", name, holder)
			}
		})
	}
}

// An update that sends a new plaintext replaces the stored reference; the
// previous hidden secret goes, the new one is sealed. An update that sends the
// reference back keeps it.
func TestUpdatingASealedValuePurgesTheReplacedSecret(t *testing.T) {
	pool := newStoredCheckPool(t)
	router := storedCheckRouter(t, pool, &fakeConnectionChecker{})
	id := saveCredential(t, pool, router, "OpenAI", "sk-first-value")
	first := purgeStoredSecretName(t, pool, id)

	// Unchanged reference: nothing is purged.
	recorder := purgeDo(t, router, http.MethodPut, purgeConfigurationPath(id), map[string]any{
		"data": map[string]any{"api_key": "{{secret." + first + "}}", "api_base": "https://api.openai.com/v1"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("update answered %d: %s", recorder.Code, recorder.Body.String())
	}
	if !purgeVaultHolds(t, pool, first) {
		t.Fatalf("an update that kept the reference purged %s", first)
	}

	recorder = purgeDo(t, router, http.MethodPut, purgeConfigurationPath(id), map[string]any{
		"data": map[string]any{"api_key": "sk-second-value", "api_base": "https://api.openai.com/v1"},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("update answered %d: %s", recorder.Code, recorder.Body.String())
	}
	second := purgeStoredSecretName(t, pool, id)
	if second == first {
		t.Fatalf("the new value was not sealed under a new name")
	}
	if !purgeVaultHolds(t, pool, second) {
		t.Fatalf("the new value %s is not in the vault", second)
	}
	if purgeVaultHolds(t, pool, first) {
		t.Fatalf("the replaced hidden secret %s is still in the vault", first)
	}
}
