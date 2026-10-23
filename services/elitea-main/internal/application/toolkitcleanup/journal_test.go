package toolkitcleanup

// The journal's decisions that need no database: which toolkit types owe a row,
// and how one engine call's answer is classified. The SQL (enqueue, claim,
// resume, close, backoff, generation) is tested against Postgres in
// journal_postgres_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

type fakeRemover struct {
	calls   [][2]int64
	deleted bool
	err     error
}

func (f *fakeRemover) DeleteToolkitData(_ context.Context, projectID, toolkitID int64) (platformops.ToolkitDeletion, error) {
	f.calls = append(f.calls, [2]int64{projectID, toolkitID})
	return platformops.ToolkitDeletion{Deleted: f.deleted}, f.err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Which types owe a row is a property of the type alone: the Inventory types
// (exact names), never a DeepWiki type (nothing per toolkit), never another.
func TestOnlyInventoryTypesOweARow(t *testing.T) {
	t.Parallel()
	for toolkitType, want := range map[string]string{
		"inventory":        platformops.AppInventory,
		"inventory_search": platformops.AppInventory,
		// DeepWiki keeps nothing per toolkit: no row, no call, in any spelling.
		"deepwiki_Deepwiki": "",
		"wikis_Wikis":       "",
		"wikis":             "",
		"deepwiki":          "",
		// Not a published type: exact matches only.
		"inventory_inventory": "",
		"Inventory_Graph":     "",
		// No engine at all.
		"github": "",
		"":       "",
	} {
		if got := AppToEnqueue(toolkitType); got != want {
			t.Errorf("AppToEnqueue(%q) = %q, want %q", toolkitType, got, want)
		}
	}
	wired := New(nil, quiet(), WithRemover(platformops.AppInventory, &fakeRemover{}), WithRemover(platformops.AppDeepWiki, &fakeRemover{}))
	if got := wired.AppsWired(); !slices.Equal(got, []string{"deepwiki", "inventory"}) {
		t.Fatalf("AppsWired = %v", got)
	}
}

// The obligation does not depend on the replica's wiring: a replica without
// the Inventory client enqueues the row all the same (and so needs the
// transaction), for a replica that has the client to drain.
func TestAnUnwiredReplicaEnqueuesTheObligationAllTheSame(t *testing.T) {
	t.Parallel()
	unwired := New(nil, quiet())
	if _, err := unwired.Enqueue(context.Background(), nil, 7, 70, "inventory"); err == nil {
		t.Fatal("Enqueue on an unwired replica skipped the row (no transaction was needed)")
	}
	if app, err := unwired.Enqueue(context.Background(), nil, 7, 70, "github"); app != "" || err != nil {
		t.Fatalf("Enqueue of a type with no engine = %q, %v", app, err)
	}
}

func TestANilOrTypedNilRemoverIsIgnored(t *testing.T) {
	t.Parallel()
	var typedNil *fakeRemover
	j := New(nil, quiet(), WithRemover("inventory", nil), WithRemover("deepwiki", typedNil), WithRemover("", &fakeRemover{}), nil)
	if got := j.AppsWired(); len(got) != 0 {
		t.Fatalf("AppsWired = %v, want none", got)
	}
}

func TestEnqueueOnAWiredReplicaNeedsTheTransaction(t *testing.T) {
	t.Parallel()
	j := New(nil, quiet(), WithRemover(platformops.AppInventory, &fakeRemover{}))
	if _, err := j.Enqueue(context.Background(), nil, 7, 70, "inventory"); err == nil {
		t.Fatal("Enqueue without a transaction succeeded")
	}
}

func inventoryRow() row {
	return row{id: 1, generation: 1, projectID: 7, toolkitID: 70, app: platformops.AppInventory, toolkitType: "inventory"}
}

func TestTheEngineAnswersAreClassified(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		remover    *fakeRemover
		wantKind   outcomeKind
		wantResult string
		wantNote   string
		wantError  string
	}{
		{"deleted", &fakeRemover{deleted: true}, outcomeDone, OutcomeDeleted, "deleted the toolkit's data", ""},
		{"nothing", &fakeRemover{}, outcomeDone, OutcomeNothingToDelete, "nothing to delete", ""},
		{"unsupported", &fakeRemover{err: fmt.Errorf("%w: inventory delete_toolkit", platformops.ErrNotSupported)},
			outcomeDone, OutcomeUnsupported, NoteUnsupported, ""},
		{"not allowed", &fakeRemover{err: fmt.Errorf("%w: inventory delete_toolkit: CN=elitea-main not in allowlist (PermissionDenied)", platformops.ErrNotAllowed)},
			outcomeDone, OutcomeFailedPermanent, NoteRefused, ""},
		{"bad request", &fakeRemover{err: fmt.Errorf("%w: toolkit id 0 is not a positive int32", platformops.ErrConfig)},
			outcomeDone, OutcomeFailedPermanent, NoteInvalidRequest, ""},
		{"retry later", &fakeRemover{err: fmt.Errorf("%w: inventory delete_toolkit: ingest running (FailedPrecondition)", platformops.ErrRetryLater)},
			outcomeRetry, "", "", lastErrorRetryLater},
		{"engine failure", &fakeRemover{err: errors.New("inventory delete_toolkit failed: boom (Internal)")},
			outcomeRetry, "", "", lastErrorFailed},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			j := New(nil, quiet(), WithRemover(platformops.AppInventory, testCase.remover))
			got := j.deleteInEngine(context.Background(), inventoryRow())
			if got.kind != testCase.wantKind {
				t.Fatalf("kind = %v, want %v (%+v)", got.kind, testCase.wantKind, got)
			}
			if got.result != testCase.wantResult {
				t.Errorf("result = %q, want %q", got.result, testCase.wantResult)
			}
			if got.note != testCase.wantNote {
				t.Errorf("note = %q, want %q", got.note, testCase.wantNote)
			}
			if got.lastError != testCase.wantError {
				t.Errorf("lastError = %q, want %q", got.lastError, testCase.wantError)
			}
			if got.kind == outcomeRetry && got.cause == nil {
				t.Error("a retry carries no cause")
			}
			if len(testCase.remover.calls) != 1 || testCase.remover.calls[0] != [2]int64{7, 70} {
				t.Errorf("remover called with %v", testCase.remover.calls)
			}
		})
	}
}

// The permanent note is the CLASS of the failure, never the engine's message:
// the message (an identity, an address, what the engine refused) is logged
// with the row's ids and stored nowhere.
func TestAPermanentFailureNotesTheClassNotTheReason(t *testing.T) {
	t.Parallel()
	var log strings.Builder
	logger := slog.New(slog.NewTextHandler(&log, nil))
	j := New(nil, logger, WithRemover(platformops.AppInventory,
		&fakeRemover{err: fmt.Errorf("%w: inventory delete_toolkit: CN=elitea-main not in allowlist at 10.0.7.3:9443 (PermissionDenied)", platformops.ErrNotAllowed)}))
	got := j.deleteInEngine(context.Background(), inventoryRow())
	if got.note != NoteRefused {
		t.Fatalf("note = %q, want %q", got.note, NoteRefused)
	}
	for _, leak := range []string{"allowlist", "10.0.7.3", "CN="} {
		if strings.Contains(got.note, leak) {
			t.Fatalf("note %q carries the engine's message (%q)", got.note, leak)
		}
	}
	if !strings.Contains(log.String(), "10.0.7.3:9443") || !strings.Contains(log.String(), "toolkit_id=70") {
		t.Fatalf("the raw error and the row's ids are not in the log: %s", log.String())
	}
}

// A row whose application this replica has no client for is left pending: the
// remover of another application is not consulted for it.
func TestAnUnwiredReplicaLeavesTheRowPending(t *testing.T) {
	t.Parallel()
	other := &fakeRemover{deleted: true}
	j := New(nil, quiet(), WithRemover(platformops.AppDeepWiki, other))
	got := j.deleteInEngine(context.Background(), inventoryRow())
	if got.kind != outcomeNotWired {
		t.Fatalf("kind = %v, want notWired (%+v)", got.kind, got)
	}
	if len(other.calls) != 0 {
		t.Fatalf("another application's remover was called: %v", other.calls)
	}
}

func TestAJournalWithoutADatabaseClaimsAndResumesNothing(t *testing.T) {
	t.Parallel()
	j := New(nil, quiet())
	if _, _, err := j.ClaimNext(context.Background(), 0); err == nil {
		t.Fatal("ClaimNext without a database succeeded")
	}
	if _, err := j.Resume(context.Background(), Claim{ID: 1, Generation: 1}); err == nil {
		t.Fatal("Resume without a database succeeded")
	}
}
