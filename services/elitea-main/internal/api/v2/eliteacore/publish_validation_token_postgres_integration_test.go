package eliteacore_test

// Real-PostgreSQL coverage for the approval token the publish route accepts in
// place of running the pre-publish quality gate again.
//
// The route used to check the token's SHAPE and nothing else — sixteen or more
// lowercase hexadecimal characters — so the gate that decides what may reach the
// catalogue could be skipped on any version by anyone who typed one (issue 855).
// The unit tests beside this file pin the token itself; these pin what the
// ROUTE does with it, which is the surface a client meets: a token nobody
// issued, a token issued for another version and a token whose agent changed
// afterwards are each refused with a message that says which, and nothing is
// published by any of them.
//
// The fixtures, pool and router come from
// catalog_mirror_postgres_integration_test.go. p_1 is the public project, so a
// publish here writes no catalogue twin and the case stays about the token.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/eliteacore"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPublishRefusesATokenNobodyIssued(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	fixture := seedCatalogMirrorFixture(t, pool, 1, "agent behind a forged token")

	// WELL FORMED and unsigned: the exact length the platform mints, all
	// lowercase hexadecimal. The old check accepted this, which is why the
	// case is written this way rather than with an obviously foreign string.
	forged := strings.Repeat("ab", 72)
	refused := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-forged",
		"validation_token": forged,
	})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("publish status = %d, want 400; body = %s", refused.Code, refused.Body.String())
	}
	assertTokenRefusal(t, refused.Body.Bytes(), "not issued for this version")
	assertNothingPublished(t, pool, fixture)
}

func TestPublishRefusesATokenIssuedForAnotherVersion(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	handler := eliteacore.NewHandler(pool)
	router := catalogMirrorRouter(handler)
	fixture := seedCatalogMirrorFixture(t, pool, 1, "agent behind a borrowed token")
	other := seedCatalogMirrorFixture(t, pool, 1, "agent that was actually checked")

	// A token the platform really issued — for a DIFFERENT version. It carries
	// a valid signature, so only the binding to the version can refuse it.
	borrowed := catalogMirrorToken(t, pool, other)
	refused := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-borrowed",
		"validation_token": borrowed,
	})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("publish status = %d, want 400; body = %s", refused.Code, refused.Body.String())
	}
	assertTokenRefusal(t, refused.Body.Bytes(), "not issued for this version")
	assertNothingPublished(t, pool, fixture)

	// …and the same token publishes the version it WAS issued for, which is
	// what tells this apart from a route that refuses every token.
	published := catalogMirrorPublish(t, router, other, map[string]any{
		"version_name":     "v-borrowed",
		"validation_token": borrowed,
	})
	if published.Code != http.StatusOK {
		t.Fatalf("publish of the checked version = %d, want 200; body = %s",
			published.Code, published.Body.String())
	}
}

// TestPublishRefusesATokenWhoseAgentChanged pins why the token carries a
// fingerprint of the version: the check that passed describes the agent as it
// was, and an agent edited afterwards has not been checked at all.
func TestPublishRefusesATokenWhoseAgentChanged(t *testing.T) {
	pool := newCatalogMirrorPool(t)
	router := catalogMirrorRouter(eliteacore.NewHandler(pool))
	fixture := seedCatalogMirrorFixture(t, pool, 1, "agent edited after the check")

	token := catalogMirrorToken(t, pool, fixture)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE p_1.application_versions SET instructions = $2 WHERE id = $1`,
		fixture.versionID, "a different agent altogether"); err != nil {
		t.Fatalf("edit the version: %v", err)
	}

	refused := catalogMirrorPublish(t, router, fixture, map[string]any{
		"version_name":     "v-edited",
		"validation_token": token,
	})
	if refused.Code != http.StatusBadRequest {
		t.Fatalf("publish status = %d, want 400; body = %s", refused.Code, refused.Body.String())
	}
	assertTokenRefusal(t, refused.Body.Bytes(), "changed after")
	assertNothingPublished(t, pool, fixture)
}

func assertTokenRefusal(t *testing.T, body []byte, wantMessagePart string) {
	t.Helper()
	var refusal struct {
		Error   string `json:"error"`
		Message string `json:"msg"`
	}
	if err := json.Unmarshal(body, &refusal); err != nil {
		t.Fatalf("decode refusal %q: %v", body, err)
	}
	if refusal.Error != "validation_token_invalid" {
		t.Errorf("error = %q, want validation_token_invalid", refusal.Error)
	}
	if !strings.Contains(refusal.Message, wantMessagePart) {
		t.Errorf("msg = %q, want it to say %q", refusal.Message, wantMessagePart)
	}
}

func assertNothingPublished(t *testing.T, pool *pgxpool.Pool, fixture catalogMirrorFixture) {
	t.Helper()
	if names := publishedVersionNames(t, pool, fixture); len(names) != 0 {
		t.Errorf("the refusal published %v anyway", names)
	}
}
