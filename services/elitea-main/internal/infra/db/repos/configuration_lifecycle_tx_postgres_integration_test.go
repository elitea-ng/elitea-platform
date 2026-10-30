package repos

import (
	"crypto/sha256"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// A worker_egress row written outside the Configuration route (connector
// approval, permission trimming) leaves the lifecycle event that route
// leaves: revision, operation, actor, a sanitized before/after snapshot and
// the digest of the stored payload. CI-only: skipped without the
// integration database.

type egressLifecycleEvent struct {
	Revision int64
	Op       string
	Actor    int32
	Before   []string
	After    []string
}

func workerEgressLifecycleEvents(t *testing.T, pool *pgxpool.Pool) []egressLifecycleEvent {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT o.revision, o.operation, o.actor_id, o.sanitized_snapshot::text, o.snapshot_digest
FROM elitea_runtime.configuration_lifecycle_outbox AS o
JOIN p_1.configuration AS c ON c.uuid::text = o.configuration_uuid::text
WHERE o.resource_project_id = 1 AND c.type = 'worker_egress'
ORDER BY o.revision`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []egressLifecycleEvent
	for rows.Next() {
		var event egressLifecycleEvent
		var snapshot string
		var digest []byte
		if err := rows.Scan(&event.Revision, &event.Op, &event.Actor, &snapshot, &digest); err != nil {
			t.Fatal(err)
		}
		if sum := sha256.Sum256([]byte(snapshot)); !slices.Equal(sum[:], digest) {
			t.Fatalf("revision %d: the digest does not authenticate the stored snapshot", event.Revision)
		}
		var envelope struct {
			Before *struct {
				Data map[string]any `json:"data"`
			} `json:"before"`
			After *struct {
				Data map[string]any `json:"data"`
			} `json:"after"`
		}
		if err := json.Unmarshal([]byte(snapshot), &envelope); err != nil {
			t.Fatal(err)
		}
		list := func(data map[string]any) []string {
			var hosts []string
			for key, value := range data {
				if key != "allowlist" {
					t.Fatalf("revision %d: the snapshot carries %q beside the allowlist", event.Revision, key)
				}
				for _, item := range value.([]any) {
					hosts = append(hosts, item.(string))
				}
			}
			return hosts
		}
		if envelope.Before != nil {
			event.Before = list(envelope.Before.Data)
		}
		if envelope.After != nil {
			event.After = list(envelope.After.Data)
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestConnectorApproval_EgressWriteAppendsTheLifecycleEvent: the first
// approval creates the project's worker_egress row (configuration_created),
// the next adds to it (configuration_updated), both by the approver.
func TestConnectorApproval_EgressWriteAppendsTheLifecycleEvent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newConnectorTestStore(t, pool, false)

	first := approvedToolkitRequest(t, store, "jira", "jira.example.com")
	if _, err := store.Approve(t.Context(), first.ID, connectorApprover, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	second := approvedToolkitRequest(t, store, "confluence", "confluence.example.com")
	if _, err := store.Approve(t.Context(), second.ID, connectorApprover, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	events := workerEgressLifecycleEvents(t, pool)
	if len(events) != 2 {
		t.Fatalf("events %+v, want created then updated", events)
	}
	if e := events[0]; e.Revision != 1 || e.Op != "configuration_created" || e.Actor != int32(connectorApprover) ||
		e.Before != nil || !slices.Equal(e.After, []string{"jira.example.com"}) {
		t.Fatalf("created event %+v", e)
	}
	if e := events[1]; e.Revision != 2 || e.Op != "configuration_updated" || e.Actor != int32(connectorApprover) ||
		!slices.Equal(e.Before, []string{"jira.example.com"}) ||
		!slices.Equal(e.After, []string{"jira.example.com", "confluence.example.com"}) {
		t.Fatalf("updated event %+v", e)
	}
}

// TestPermissionTrimming_EgressRemovalAppendsTheLifecycleEvent: removing a
// project egress entry appends configuration_updated by the remover.
func TestPermissionTrimming_EgressRemovalAppendsTheLifecycleEvent(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newConnectorTestStore(t, pool, false)
	request := approvedToolkitRequest(t, store, "jira", "jira.example.com", "api.atlassian.com")
	if _, err := store.Approve(t.Context(), request.ID, connectorApprover, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	review := newCredentialReviewTestStore(t, pool, time.Now().UTC(), platformconfig.DefaultCredentialReviewPolicy())
	trimming, err := NewPermissionTrimming(review)
	if err != nil {
		t.Fatal(err)
	}
	const remover int64 = 13
	// The entry has been listed longer than the window: a candidate.
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_lifecycle.permission_trimming_observed
    (tenant_project_id, application_id, item_kind, item_key, first_seen_at, last_seen_at)
VALUES (1, 0, 'egress_host', 'project:jira.example.com', now() - interval '40 days', now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := trimming.Remove(t.Context(), RemoveRequest{ProjectID: 1, Kind: TrimEgressHost,
		Key: "project:jira.example.com", ActorID: remover}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	events := workerEgressLifecycleEvents(t, pool)
	if len(events) != 2 {
		t.Fatalf("events %+v, want the approval's then the removal's", events)
	}
	if e := events[1]; e.Revision != 2 || e.Op != "configuration_updated" || e.Actor != int32(remover) ||
		!slices.Equal(e.Before, []string{"api.atlassian.com", "jira.example.com"}) ||
		!slices.Equal(e.After, []string{"api.atlassian.com"}) {
		t.Fatalf("removal event %+v", e)
	}
}

// A direct writer that takes an entry off a project's egress allowlist (here
// the trimming's row rewrite on its own, without the trimming's bookkeeping)
// forgets the entry's sighting in its transaction through the lifecycle
// event hook, so an entry added back starts its trimming window over. The
// entry kept keeps its sighting.
func TestEgressEntryRemovalForgetsItsSighting(t *testing.T) {
	pool := newMigratedPostgresIntegrationPool(t)
	_, store := newConnectorTestStore(t, pool, false)
	request := approvedToolkitRequest(t, store, "jira", "jira.example.com", "api.atlassian.com")
	if _, err := store.Approve(t.Context(), request.ID, connectorApprover, ""); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `
INSERT INTO elitea_lifecycle.permission_trimming_observed
    (tenant_project_id, application_id, item_kind, item_key, first_seen_at, last_seen_at)
VALUES (1, 0, 'egress_host', 'project:jira.example.com', now() - interval '40 days', now()),
       (1, 0, 'egress_host', 'project:api.atlassian.com', now() - interval '40 days', now())`); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if err := removeProjectEgressEntry(t.Context(), tx, 1, "jira.example.com", 13); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var keys []string
	rows, err := pool.Query(t.Context(), `SELECT item_key FROM elitea_lifecycle.permission_trimming_observed ORDER BY item_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if !slices.Equal(keys, []string{"project:api.atlassian.com"}) {
		t.Fatalf("sightings after the removal = %v, want only the kept entry", keys)
	}
}
