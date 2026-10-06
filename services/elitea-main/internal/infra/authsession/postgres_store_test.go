package authsession

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	sessionstate "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/session"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authstatetest"
)

func TestNotFoundErrorUsesStorageNeutralSessionContract(t *testing.T) {
	t.Parallel()

	if ErrNotFound != sessionstate.ErrNotFound {
		t.Fatalf("ErrNotFound = %v, want storage-neutral %v", ErrNotFound, sessionstate.ErrNotFound)
	}
}

func TestRandomSessionIDIsCanonicalAndUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 64)
	for range 64 {
		id, err := randomSessionID()
		if err != nil {
			t.Fatal(err)
		}
		if !validSessionID(id) {
			t.Fatalf("generated ID %q is not a canonical 256-bit value", id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate random session ID %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestNewPostgresStoreRejectsIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	pool := authstatetest.ClosedPool(t)
	for name, test := range map[string]struct {
		pool   *pgxpool.Pool
		config Config
		gen    idGenerator
	}{
		"nil pool":                {nil, Config{TTL: time.Hour, PreLoginTTL: time.Minute}, randomSessionID},
		"nil generator":           {pool, Config{TTL: time.Hour, PreLoginTTL: time.Minute}, nil},
		"zero lifetime":           {pool, Config{PreLoginTTL: time.Minute}, randomSessionID},
		"zero pre-login lifetime": {pool, Config{TTL: time.Hour}, randomSessionID},
		"pre-login above full":    {pool, Config{TTL: time.Minute, PreLoginTTL: time.Hour}, randomSessionID},
	} {
		if _, err := newPostgresStore(test.pool, test.config, test.gen); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("%s: error = %v, want %v", name, err, ErrInvalidConfiguration)
		}
	}
}

func TestPostgresStoreCreateReadKeepsSensitiveStateOutOfOpaqueID(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	opaqueID := testSessionID(1)
	store, pool := newTestStore(t, 30*time.Minute, 5*time.Minute, fixedGenerator(opaqueID))
	state := completedState(now.Add(20 * time.Minute))

	id, err := store.Create(context.Background(), state)
	if err != nil {
		t.Fatal(err)
	}
	if id != opaqueID || !validSessionID(id) {
		t.Fatalf("session ID = %q, want generated opaque ID %q", id, opaqueID)
	}
	for _, sensitive := range []string{"subject-42", "provider-session-secret", "42", "oidc"} {
		if strings.Contains(id, sensitive) {
			t.Fatalf("session ID contains sensitive state %q", sensitive)
		}
	}
	if count := rowCount(t, pool); count != 1 {
		t.Fatalf("rows = %d, want 1", count)
	}

	got, err := store.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != sessionstate.CurrentSchemaVersion || !got.Done || got.UserID == nil || *got.UserID != 42 {
		t.Fatalf("read state = %+v", got)
	}
	if string(got.ProviderAttributes) != string(state.ProviderAttributes) {
		t.Fatalf("provider state changed: %+v", got)
	}
	if ttl := remainingLifetime(t, pool, id); ttl <= 29*time.Minute || ttl > 30*time.Minute {
		t.Fatalf("lifetime = %s, want the full server-session lifetime", ttl)
	}
}

// A login begin creates an unauthenticated session. It lives PreLoginTTL,
// not the cookie lifetime: it is useful only for the one five-minute login
// transaction bound to it.
func TestPostgresStoreGivesAPreLoginSessionThePreLoginLifetime(t *testing.T) {
	t.Parallel()

	store, pool := newTestStore(t, 24*time.Hour, 5*time.Minute, fixedGenerator(testSessionID(3)))
	id, err := store.Create(context.Background(), incompleteState())
	if err != nil {
		t.Fatal(err)
	}
	if ttl := remainingLifetime(t, pool, id); ttl <= 4*time.Minute || ttl > 5*time.Minute {
		t.Fatalf("pre-login lifetime = %s, want five minutes", ttl)
	}
}

func TestPostgresStoreKeepsServerLifetimeIndependentFromAuthExpiration(t *testing.T) {
	t.Parallel()

	t.Run("expired row", func(t *testing.T) {
		t.Parallel()
		store, pool := newTestStore(t, time.Hour, 5*time.Minute, fixedGenerator(testSessionID(1)))
		id, err := store.Create(context.Background(), incompleteState())
		if err != nil {
			t.Fatal(err)
		}
		expire(t, pool, id)
		if _, err := store.Read(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("read error = %v, want %v", err, ErrNotFound)
		}
	})

	t.Run("expired auth context remains available to authorization and logout", func(t *testing.T) {
		t.Parallel()
		now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
		store, pool := newTestStore(t, 30*time.Minute, 5*time.Minute, fixedGenerator(testSessionID(2)))
		state := completedState(now.Add(-time.Minute))
		id, err := store.Create(context.Background(), state)
		if err != nil {
			t.Fatal(err)
		}
		got, err := store.Read(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Expiration == nil || !got.Expiration.Equal(*state.Expiration) {
			t.Fatalf("auth expiration changed: got %v, want %v", got.Expiration, state.Expiration)
		}
		if ttl := remainingLifetime(t, pool, id); ttl <= 29*time.Minute || ttl > 30*time.Minute {
			t.Fatalf("lifetime = %s, want the full server-session lifetime", ttl)
		}
	})
}

func TestPostgresStoreRotateAndReplaceCommitsAuthenticatedStateAtomically(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	oldID := testSessionID(12)
	newID := testSessionID(13)
	store, pool := newTestStore(t, 30*time.Minute, 5*time.Minute, sequenceGenerator(oldID, newID))
	createdID, err := store.Create(context.Background(), incompleteState())
	if err != nil {
		t.Fatal(err)
	}
	// Three of the five pre-login minutes are gone.
	authstatetest.Exec(t, pool,
		`UPDATE elitea_auth.form_sessions SET expires_at = now() + interval '2 minutes' WHERE id = $1`, createdID)

	rotatedID, err := store.RotateAndReplace(context.Background(), createdID, completedState(now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if rotatedID != newID {
		t.Fatalf("rotated ID = %q, want %q", rotatedID, newID)
	}
	if _, err := store.Read(context.Background(), createdID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old ID read error = %v, want %v", err, ErrNotFound)
	}
	got, err := store.Read(context.Background(), rotatedID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Done || got.UserID == nil || *got.UserID != 42 {
		t.Fatalf("replacement state = %+v", got)
	}
	if ttl := remainingLifetime(t, pool, rotatedID); ttl <= 29*time.Minute || ttl > 30*time.Minute {
		t.Fatalf("replacement lifetime = %s, want the full server-session lifetime", ttl)
	}
}

func TestPostgresStoreRotateAndReplaceRejectsInvalidStateBeforeMutation(t *testing.T) {
	t.Parallel()

	oldID := testSessionID(14)
	newID := testSessionID(15)
	store, pool := newTestStore(t, time.Hour, 5*time.Minute, sequenceGenerator(oldID, newID))
	createdID, err := store.Create(context.Background(), incompleteState())
	if err != nil {
		t.Fatal(err)
	}
	invalid := incompleteState()
	invalid.SchemaVersion = sessionstate.CurrentSchemaVersion + 1

	if _, err := store.RotateAndReplace(context.Background(), createdID, invalid); !errors.Is(err, sessionstate.ErrInvalidState) {
		t.Fatalf("rotate-and-replace error = %v, want %v", err, sessionstate.ErrInvalidState)
	}
	if !exists(t, pool, createdID) || exists(t, pool, newID) {
		t.Fatal("invalid replacement mutated the table")
	}
}

func TestPostgresStoreRotateInvalidatesOldIDAndPreservesRemainingLifetime(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	oldID := testSessionID(10)
	newID := testSessionID(11)
	store, pool := newTestStore(t, 30*time.Minute, 5*time.Minute, sequenceGenerator(oldID, newID))
	createdID, err := store.Create(context.Background(), completedState(now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	authstatetest.Exec(t, pool,
		`UPDATE elitea_auth.form_sessions SET expires_at = now() + interval '23 minutes' WHERE id = $1`, createdID)
	before := expiresAt(t, pool, createdID)

	rotatedID, err := store.Rotate(context.Background(), createdID)
	if err != nil {
		t.Fatal(err)
	}
	if rotatedID != newID || rotatedID == createdID {
		t.Fatalf("rotated ID = %q, old = %q", rotatedID, createdID)
	}
	if _, err := store.Read(context.Background(), createdID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old ID read error = %v, want %v", err, ErrNotFound)
	}
	if _, err := store.Read(context.Background(), rotatedID); err != nil {
		t.Fatalf("new ID read: %v", err)
	}
	if after := expiresAt(t, pool, rotatedID); !after.Equal(before) {
		t.Fatalf("rotated expiry = %s, want the remaining %s", after, before)
	}
}

func TestPostgresStoreRotationRetriesCollisionWithoutExposingOldID(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	oldID := testSessionID(20)
	collidingID := testSessionID(21)
	newID := testSessionID(22)
	store, pool := newTestStore(t, 30*time.Minute, 5*time.Minute, sequenceGenerator(oldID, collidingID, newID))
	createdID, err := store.Create(context.Background(), completedState(now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	insertRaw(t, pool, collidingID, []byte("unrelated"), time.Hour)

	rotatedID, err := store.Rotate(context.Background(), createdID)
	if err != nil {
		t.Fatal(err)
	}
	if rotatedID != newID {
		t.Fatalf("rotated ID = %q, want %q", rotatedID, newID)
	}
	if record := rawRecord(t, pool, collidingID); string(record) != "unrelated" {
		t.Fatalf("colliding record = %q", record)
	}
	if exists(t, pool, oldID) {
		t.Fatal("old ID remains valid after successful rotation")
	}
}

func TestPostgresStoreCreateCollisionsAreBoundedAndExpiredRowsAreReused(t *testing.T) {
	t.Parallel()

	collidingID := testSessionID(30)
	store, pool := newTestStore(t, time.Hour, 5*time.Minute, fixedGenerator(collidingID))
	insertRaw(t, pool, collidingID, []byte("existing"), time.Hour)

	if _, err := store.Create(context.Background(), incompleteState()); !errors.Is(err, ErrIDCollision) {
		t.Fatalf("create error = %v, want %v", err, ErrIDCollision)
	}
	if record := rawRecord(t, pool, collidingID); string(record) != "existing" {
		t.Fatalf("colliding value = %q", record)
	}

	// An expired row is absent, as an expired Redis key was: the ID is free.
	expire(t, pool, collidingID)
	id, err := store.Create(context.Background(), incompleteState())
	if err != nil || id != collidingID {
		t.Fatalf("create over an expired row = %q, %v", id, err)
	}
	if _, err := store.Read(context.Background(), id); err != nil {
		t.Fatalf("read the replacement: %v", err)
	}
}

func TestPostgresStoreRejectsMalformedAndUnknownRecords(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	store, pool := newTestStore(t, time.Hour, 5*time.Minute, fixedGenerator(testSessionID(40)))

	unknownVersion := completedState(now.Add(time.Hour))
	unknownVersion.SchemaVersion++
	unknownVersionRecord, err := json.Marshal(unknownVersion)
	if err != nil {
		t.Fatal(err)
	}
	validRecord, err := json.Marshal(completedState(now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	unknownFieldRecord := append(append([]byte(nil), validRecord[:len(validRecord)-1]...), []byte(`,"unknown":true}`)...)
	oversizedAttributes := completedState(now.Add(time.Hour))
	oversizedAttributes.ProviderAttributes = json.RawMessage(
		`{"value":"` + strings.Repeat("x", sessionstate.MaxProviderAttributesBytes) + `"}`,
	)
	oversizedAttributesRecord, err := json.Marshal(oversizedAttributes)
	if err != nil {
		t.Fatal(err)
	}

	for index, test := range []struct {
		name   string
		record []byte
	}{
		{name: "malformed JSON", record: []byte("{")},
		{name: "unknown schema", record: unknownVersionRecord},
		{name: "unknown field", record: unknownFieldRecord},
		{name: "trailing data", record: append(append([]byte(nil), validRecord...), []byte(" {}")...)},
		{name: "oversized provider attributes", record: oversizedAttributesRecord},
	} {
		t.Run(test.name, func(t *testing.T) {
			id := testSessionID(byte(50 + index))
			insertRaw(t, pool, id, test.record, time.Hour)
			if _, err := store.Read(context.Background(), id); !errors.Is(err, sessionstate.ErrInvalidState) {
				t.Fatalf("read error = %v, want %v", err, sessionstate.ErrInvalidState)
			}
			if exists(t, pool, id) {
				t.Fatal("invalid stored session was not removed")
			}
		})
	}

	t.Run("the table refuses an oversized or empty record", func(t *testing.T) {
		for _, record := range [][]byte{[]byte(strings.Repeat("x", MaxRecordBytes+1)), {}} {
			_, err := pool.Exec(context.Background(),
				`INSERT INTO elitea_auth.form_sessions (id, record, expires_at) VALUES ($1, $2, now() + interval '1 hour')`,
				testSessionID(69), record)
			if err == nil {
				t.Fatalf("a %d-byte record was stored", len(record))
			}
		}
	})
}

func TestPostgresStoreDeleteIsIdempotentLogoutPrimitive(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	store, _ := newTestStore(t, time.Hour, 5*time.Minute, fixedGenerator(testSessionID(70)))
	id, err := store.Create(context.Background(), completedState(now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(context.Background(), id); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := store.Read(context.Background(), id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("read error = %v, want %v", err, ErrNotFound)
	}
}

func TestPostgresStoreConsumeForLogoutLinearizesAgainstRotation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	t.Run("logout first fences rotation", func(t *testing.T) {
		t.Parallel()
		store, _ := newTestStore(t, time.Hour, 5*time.Minute, sequenceGenerator(testSessionID(71), testSessionID(72)))
		createdID, err := store.Create(context.Background(), completedState(now.Add(time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		consumed, err := store.ConsumeForLogout(context.Background(), createdID)
		if err != nil {
			t.Fatal(err)
		}
		if !consumed.Done || consumed.UserID == nil || *consumed.UserID != 42 {
			t.Fatalf("consumed state = %+v", consumed)
		}
		if _, err := store.RotateAndReplace(context.Background(), createdID, completedState(now.Add(2*time.Hour))); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rotation error = %v, want %v", err, ErrNotFound)
		}
	})

	t.Run("rotation first leaves new ID outside stale logout", func(t *testing.T) {
		t.Parallel()
		store, _ := newTestStore(t, time.Hour, 5*time.Minute, sequenceGenerator(testSessionID(73), testSessionID(74)))
		createdID, err := store.Create(context.Background(), completedState(now.Add(time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		rotatedID, err := store.RotateAndReplace(context.Background(), createdID, completedState(now.Add(2*time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		consumed, err := store.ConsumeForLogout(context.Background(), createdID)
		if err != nil {
			t.Fatal(err)
		}
		if !isZeroState(consumed) {
			t.Fatalf("stale logout state = %+v, want zero", consumed)
		}
		if _, err := store.Read(context.Background(), rotatedID); err != nil {
			t.Fatalf("rotated session was revoked by stale logout: %v", err)
		}
	})
}

// The row lock is the linearization point. Logout and rotation race on one
// ID, many times, on separate connections; exactly one of them wins each round.
func TestPostgresStoreConcurrentLogoutAndRotationHaveOneLinearizedWinner(t *testing.T) {
	t.Parallel()

	type logoutResult struct {
		state sessionstate.State
		err   error
	}
	type rotationResult struct {
		id  string
		err error
	}
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	pool := authstatetest.Pool(t, 8)
	for round := range 32 {
		oldID := testSessionID(byte(100 + round*2))
		newID := testSessionID(byte(101 + round*2))
		store := storeOn(t, pool, time.Hour, 5*time.Minute, sequenceGenerator(oldID, newID))
		createdID, err := store.Create(context.Background(), completedState(now.Add(time.Hour)))
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		logoutDone := make(chan logoutResult, 1)
		rotationDone := make(chan rotationResult, 1)
		go func() {
			<-start
			state, err := store.ConsumeForLogout(context.Background(), createdID)
			logoutDone <- logoutResult{state: state, err: err}
		}()
		go func() {
			<-start
			id, err := store.RotateAndReplace(context.Background(), createdID, completedState(now.Add(2*time.Hour)))
			rotationDone <- rotationResult{id: id, err: err}
		}()
		close(start)
		logout := <-logoutDone
		rotation := <-rotationDone
		if logout.err != nil {
			t.Fatalf("round %d logout: %v", round, logout.err)
		}

		switch {
		case !isZeroState(logout.state):
			if !errors.Is(rotation.err, ErrNotFound) {
				t.Fatalf("round %d logout won but rotation error = %v", round, rotation.err)
			}
			if exists(t, pool, newID) {
				t.Fatalf("round %d logout won but the rotated row exists", round)
			}
		case rotation.err == nil:
			if rotation.id != newID {
				t.Fatalf("round %d rotated ID = %q, want %q", round, rotation.id, newID)
			}
			if _, err := store.Read(context.Background(), rotation.id); err != nil {
				t.Fatalf("round %d rotation winner is unreadable: %v", round, err)
			}
		default:
			t.Fatalf("round %d had no valid winner: logout=%+v rotation=%+v", round, logout, rotation)
		}
	}
}

// Many rotations of one ID at once: the compare-and-swap admits exactly one,
// and exactly one new row exists afterwards.
func TestPostgresStoreConcurrentRotationsOfOneIDHaveOneWinner(t *testing.T) {
	t.Parallel()

	const contenders = 8
	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	pool := authstatetest.Pool(t, contenders+2)
	creator := storeOn(t, pool, time.Hour, 5*time.Minute, fixedGenerator(testSessionID(200)))
	createdID, err := creator.Create(context.Background(), incompleteState())
	if err != nil {
		t.Fatal(err)
	}

	var (
		wait    sync.WaitGroup
		start   = make(chan struct{})
		results = make(chan error, contenders)
	)
	for index := range contenders {
		store := storeOn(t, pool, time.Hour, 5*time.Minute, fixedGenerator(testSessionID(byte(201+index))))
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.RotateAndReplace(context.Background(), createdID, completedState(now.Add(time.Hour)))
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	winners := 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrRotationConflict):
		default:
			t.Fatalf("unexpected rotation error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
	if count := rowCount(t, pool); count != 1 {
		t.Fatalf("rows after the race = %d, want 1", count)
	}
}

func TestPostgresStoreConsumeForLogoutHandlesExpiredMissingAndMalformedState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.UTC)
	store, pool := newTestStore(t, time.Minute, time.Minute, sequenceGenerator(testSessionID(75), testSessionID(76)))

	expiredAuthID, err := store.Create(context.Background(), completedState(now.Add(-time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	expiredAuth, err := store.ConsumeForLogout(context.Background(), expiredAuthID)
	if err != nil {
		t.Fatal(err)
	}
	if !expiredAuth.Done || expiredAuth.Expiration == nil || !expiredAuth.Expiration.Before(now) {
		t.Fatalf("expired authentication logout state = %+v", expiredAuth)
	}

	expiredRowID, err := store.Create(context.Background(), incompleteState())
	if err != nil {
		t.Fatal(err)
	}
	expire(t, pool, expiredRowID)
	for _, id := range []string{expiredRowID, testSessionID(77)} {
		state, err := store.ConsumeForLogout(context.Background(), id)
		if err != nil {
			t.Fatalf("idempotent missing logout %q: %v", id, err)
		}
		if !isZeroState(state) {
			t.Fatalf("missing logout state = %+v, want zero", state)
		}
	}

	for index, record := range [][]byte{[]byte("{"), []byte(`{"schema_version":1,"unknown":true}`)} {
		id := testSessionID(byte(78 + index))
		insertRaw(t, pool, id, record, time.Hour)
		if _, err := store.ConsumeForLogout(context.Background(), id); !errors.Is(err, sessionstate.ErrInvalidState) {
			t.Fatalf("error = %v, want %v", err, sessionstate.ErrInvalidState)
		}
		if exists(t, pool, id) {
			t.Fatal("malformed logout state was not consumed")
		}
	}
}

func TestPostgresStoreFailsClosedWhenPostgreSQLIsUnavailable(t *testing.T) {
	t.Parallel()

	store, err := newPostgresStore(
		authstatetest.ClosedPool(t),
		Config{TTL: time.Hour, PreLoginTTL: 5 * time.Minute},
		fixedGenerator(testSessionID(80)),
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 19, 12, 0, 0, 0, time.UTC)
	id := testSessionID(81)

	if _, err := store.Read(context.Background(), id); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("read error = %v, want %v", err, ErrUnavailable)
	}
	if _, err := store.Create(context.Background(), incompleteState()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("create error = %v, want %v", err, ErrUnavailable)
	}
	if err := store.Delete(context.Background(), id); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("delete error = %v, want %v", err, ErrUnavailable)
	}
	if _, err := store.Rotate(context.Background(), id); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("rotate error = %v, want %v", err, ErrUnavailable)
	}
	if _, err := store.RotateAndReplace(context.Background(), id, completedState(now.Add(time.Hour))); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("rotate-and-replace error = %v, want %v", err, ErrUnavailable)
	}
	if _, err := store.ConsumeForLogout(context.Background(), id); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("consume-for-logout error = %v, want %v", err, ErrUnavailable)
	}
	// The rotation transaction path, past the read: a closed pool cannot
	// begin it either.
	if _, err := store.rotateRecord(context.Background(), id, []byte("{}"), []byte("{}"), 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("rotate transaction error = %v, want %v", err, ErrUnavailable)
	}
}

// A rotation that waits on a row lock past lock_timeout fails closed with
// ErrUnavailable, and does not hold the caller for the whole request.
func TestPostgresStoreRotationLockWaitIsBoundedAndFailsClosed(t *testing.T) {
	t.Parallel()

	pool := authstatetest.Pool(t, 4)
	store := storeOn(t, pool, time.Hour, 5*time.Minute, sequenceGenerator(testSessionID(90), testSessionID(91)))
	id, err := store.Create(context.Background(), incompleteState())
	if err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(context.Background(),
		`SELECT 1 FROM elitea_auth.form_sessions WHERE id = $1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	_, err = store.Rotate(context.Background(), id)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("rotate under a held lock = %v, want %v", err, ErrUnavailable)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("rotation waited %s on the lock", elapsed)
	}
}

func TestPostgresStoreRejectsInvalidIDsBeforeDatabaseAccess(t *testing.T) {
	t.Parallel()

	store, err := newPostgresStore(authstatetest.ClosedPool(t), Config{TTL: time.Hour, PreLoginTTL: time.Minute},
		fixedGenerator(testSessionID(90)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), "../../not-a-session"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("read error = %v, want %v", err, ErrInvalidID)
	}
	if err := store.Delete(context.Background(), "../../not-a-session"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("delete error = %v, want %v", err, ErrInvalidID)
	}
	if _, err := store.ConsumeForLogout(context.Background(), "../../not-a-session"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("consume-for-logout error = %v, want %v", err, ErrInvalidID)
	}
	invalidGenerator, err := newPostgresStore(authstatetest.ClosedPool(t), Config{TTL: time.Hour, PreLoginTTL: time.Minute},
		fixedGenerator("short"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invalidGenerator.Create(context.Background(), incompleteState()); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("create with an invalid generated ID = %v, want %v", err, ErrInvalidID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ConsumeForLogout(ctx, testSessionID(91)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled consume-for-logout error = %v, want %v", err, context.Canceled)
	}
}

func newTestStore(
	t *testing.T,
	ttl time.Duration,
	preLoginTTL time.Duration,
	generate idGenerator,
) (*PostgresStore, *pgxpool.Pool) {
	t.Helper()
	pool := authstatetest.Pool(t, 4)
	return storeOn(t, pool, ttl, preLoginTTL, generate), pool
}

func storeOn(
	t *testing.T,
	pool *pgxpool.Pool,
	ttl time.Duration,
	preLoginTTL time.Duration,
	generate idGenerator,
) *PostgresStore {
	t.Helper()
	store, err := newPostgresStore(pool, Config{TTL: ttl, PreLoginTTL: preLoginTTL}, generate)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func insertRaw(t *testing.T, pool *pgxpool.Pool, id string, record []byte, lifetime time.Duration) {
	t.Helper()
	authstatetest.Exec(t, pool, `
		INSERT INTO elitea_auth.form_sessions (id, record, expires_at)
		VALUES ($1, $2, now() + make_interval(secs => $3))`, id, record, lifetime.Seconds())
}

func expire(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if authstatetest.Exec(t, pool,
		`UPDATE elitea_auth.form_sessions SET expires_at = now() - interval '1 millisecond' WHERE id = $1`, id) != 1 {
		t.Fatalf("no row %q to expire", id)
	}
}

func exists(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var present bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM elitea_auth.form_sessions WHERE id = $1)`, id).Scan(&present); err != nil {
		t.Fatal(err)
	}
	return present
}

func rawRecord(t *testing.T, pool *pgxpool.Pool, id string) []byte {
	t.Helper()
	var record []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT record FROM elitea_auth.form_sessions WHERE id = $1`, id).Scan(&record); err != nil {
		t.Fatal(err)
	}
	return record
}

func rowCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM elitea_auth.form_sessions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func expiresAt(t *testing.T, pool *pgxpool.Pool, id string) time.Time {
	t.Helper()
	var value time.Time
	if err := pool.QueryRow(context.Background(),
		`SELECT expires_at FROM elitea_auth.form_sessions WHERE id = $1`, id).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func remainingLifetime(t *testing.T, pool *pgxpool.Pool, id string) time.Duration {
	t.Helper()
	var seconds float64
	if err := pool.QueryRow(context.Background(),
		`SELECT extract(epoch FROM expires_at - now()) FROM elitea_auth.form_sessions WHERE id = $1`, id).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	return time.Duration(seconds * float64(time.Second))
}

func completedState(expiration time.Time) sessionstate.State {
	userID := int64(42)
	provider := "oidc"
	return sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		Done:               true,
		Expiration:         &expiration,
		Provider:           &provider,
		ProviderAttributes: json.RawMessage(`{"nameid":"subject-42","attributes":{"picture":"avatar"},"sessionindex":"provider-session-secret"}`),
		UserID:             &userID,
	}
}

func incompleteState() sessionstate.State {
	return sessionstate.State{
		SchemaVersion:      sessionstate.CurrentSchemaVersion,
		ProviderAttributes: json.RawMessage("{}"),
	}
}

func isZeroState(state sessionstate.State) bool {
	return state.SchemaVersion == 0 && !state.Done && state.Error == "" && state.Expiration == nil &&
		state.Provider == nil && len(state.ProviderAttributes) == 0 && state.UserID == nil
}

func testSessionID(value byte) string {
	return base64.RawURLEncoding.EncodeToString(bytesOf(value, sessionIDRandomBytes))
}

func bytesOf(value byte, size int) []byte {
	result := make([]byte, size)
	for index := range result {
		result[index] = value
	}
	return result
}

func fixedGenerator(id string) idGenerator {
	return func() (string, error) { return id, nil }
}

func sequenceGenerator(ids ...string) idGenerator {
	var mutex sync.Mutex
	index := 0
	return func() (string, error) {
		mutex.Lock()
		defer mutex.Unlock()
		if index >= len(ids) {
			return ids[len(ids)-1], nil
		}
		id := ids[index]
		index++
		return id, nil
	}
}
