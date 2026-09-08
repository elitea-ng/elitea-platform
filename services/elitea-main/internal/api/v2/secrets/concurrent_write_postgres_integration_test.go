package secrets

// The vault is one encrypted blob, and every write re-encrypts the WHOLE of
// it. Two writers that read the same version and each write back their own
// edit therefore do not merge: the second write replaces the first, and every
// secret only the first writer added is gone — silently, with a 2xx to both
// (#858).
//
// Each test here starts many writers against ONE vault at the same instant and
// then asserts, from the stored bytes, that every write survived. Against the
// old unlocked read-modify-write these fail on the first run: with a start
// barrier, the writers all read the same version before any of them writes.
//
// Requires a PostgreSQL to create an isolated database in; skipped otherwise.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/centrysecrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const (
	// A project of its own, apart from every other file's fixtures.
	concurrentProjectID    = "9"
	concurrentProjectInt64 = int64(9)
	concurrentWriters      = 16
)

// runAtOnce starts `n` writers behind one barrier so they all read the vault
// before any of them writes, and returns each writer's error by index.
func runAtOnce(n int, write func(i int) error) []error {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = write(i)
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

// assertVaultHolds reads the vault through the handler and fails on every
// name that is not there, naming all of them at once.
func assertVaultHolds(t *testing.T, handler *Handler, projectID string, want []string) {
	t.Helper()
	vault, err := handler.readVaultCtx(context.Background(), projectID)
	if err != nil {
		t.Fatalf("read the vault after the writes: %v", err)
	}
	var lost []string
	for _, name := range want {
		if _, ok := vault.Secrets[name]; !ok {
			lost = append(lost, name)
		}
	}
	sort.Strings(lost)
	if len(lost) > 0 {
		t.Fatalf("%d of %d writes were lost: %v (vault holds %d secrets)",
			len(lost), len(want), lost, len(vault.Secrets))
	}
}

// Sixteen concurrent creates through the product's POST route, on a vault that
// already holds a marker. Every one of them must be in the vault afterwards,
// and so must the marker.
func TestConcurrentCreatesOnOneVaultAllSurvive(t *testing.T) {
	pool := newSecretsPool(t)
	seedProjectVault(t, pool, concurrentProjectID)
	router := secretsRouter(t, pool, allSecretPermissions())

	names := make([]string, concurrentWriters)
	for i := range names {
		names[i] = fmt.Sprintf("created_%02d", i)
	}
	errs := runAtOnce(concurrentWriters, func(i int) error {
		// Not `do`: it may call t.Fatalf, which a goroutine must not.
		body := fmt.Sprintf(`{"name":%q,"value":%q}`, names[i], "v-"+names[i])
		request := httptest.NewRequest(http.MethodPost, projectSecretsBase+concurrentProjectID,
			strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusCreated {
			return fmt.Errorf("create %s: status %d, body %s", names[i], recorder.Code, recorder.Body.String())
		}
		return nil
	})
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertVaultHolds(t, NewHandler(pool), concurrentProjectID, append(names, projectSecretName))
}

// Half the writers go through this handler, half through the repository that
// already held the row lock. Before the fix the handler's writers read past
// the repository's lock and overwrote its results (and each other's).
func TestHandlerAndRepositoryWritersOnOneVaultKeepEachOthersWrites(t *testing.T) {
	pool := newSecretsPool(t)
	seedProjectVault(t, pool, concurrentProjectID)
	handler := NewHandler(pool)
	repository, err := repos.NewCurrentSecretVaultRepository(pool, handler.masterKey)
	if err != nil {
		t.Fatalf("open the current secret vault repository: %v", err)
	}
	ctx := context.Background()

	names := make([]string, concurrentWriters)
	for i := range names {
		names[i] = fmt.Sprintf("mixed_%02d", i)
	}
	errs := runAtOnce(concurrentWriters, func(i int) error {
		if i%2 == 0 {
			return handler.StoreSecret(ctx, nil, concurrentProjectID, names[i], "v-"+names[i])
		}
		return repository.MutateProject(ctx, concurrentProjectInt64, []centrysecrets.Mutation{{
			Collection: centrysecrets.RegularSecrets,
			Name:       names[i],
			Value:      "v-" + names[i],
		}})
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d (%s): %v", i, names[i], err)
		}
	}
	assertVaultHolds(t, handler, concurrentProjectID, append(names, projectSecretName))
}

// The first writers of a project that has NO vault yet. Nothing can be locked
// before the rows exist, so this is the case the key-row insert serialises:
// exactly one writer mints the key, the rest open the vault it committed, and
// every write survives under ONE key.
func TestConcurrentFirstWritesOnAnAbsentVaultAllSurvive(t *testing.T) {
	pool := newSecretsPool(t)
	handler := NewHandler(pool)
	ctx := context.Background()

	names := make([]string, concurrentWriters)
	for i := range names {
		names[i] = fmt.Sprintf("first_%02d", i)
	}
	errs := runAtOnce(concurrentWriters, func(i int) error {
		return handler.StoreSecret(ctx, nil, concurrentProjectID, names[i], "v-"+names[i])
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d (%s): %v", i, names[i], err)
		}
	}
	assertVaultHolds(t, handler, concurrentProjectID, names)
}
