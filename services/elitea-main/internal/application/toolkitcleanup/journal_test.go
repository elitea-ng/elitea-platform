package toolkitcleanup

// The journal's decisions that need no database: which toolkit types owe a row
// on this replica, and how one engine call's answer is classified. The SQL
// (enqueue, claim, resume, close, backoff) is tested against Postgres in
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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
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

func TestOnlyInventoryTypesOnAWiredReplicaOweARow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	wired := New(nil, quiet(), WithRemover(platformops.AppInventory, &fakeRemover{}), WithRemover(platformops.AppDeepWiki, &fakeRemover{}))
	for toolkitType, want := range map[string]string{
		"inventory_inventory": platformops.AppInventory,
		"inventory":           platformops.AppInventory,
		"Inventory_Graph":     platformops.AppInventory,
		// DeepWiki keeps nothing per toolkit: no row, no call, in any spelling.
		"deepwiki_Deepwiki": "",
		"wikis":             "",
		"deepwiki":          "",
		// No engine at all.
		"github": "",
		"":       "",
	} {
		if got := wired.AppToEnqueue(ctx, 7, 70, toolkitType); got != want {
			t.Errorf("AppToEnqueue(%q) = %q, want %q", toolkitType, got, want)
		}
	}
	if got := wired.AppsWired(); !slices.Equal(got, []string{"deepwiki", "inventory"}) {
		t.Fatalf("AppsWired = %v", got)
	}

	// A replica without the Inventory client records nothing: there is no
	// engine for it to call, and a row nobody here can drain is not a plan.
	unwired := New(nil, quiet())
	if got := unwired.AppToEnqueue(ctx, 7, 70, "inventory_inventory"); got != "" {
		t.Fatalf("an unwired replica enqueues: %q", got)
	}
	if app, err := unwired.Enqueue(ctx, nil, 7, 70, "inventory_inventory"); app != "" || err != nil {
		t.Fatalf("Enqueue on an unwired replica = %q, %v", app, err)
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
	if _, err := j.Enqueue(context.Background(), nil, 7, 70, "inventory_inventory"); err == nil {
		t.Fatal("Enqueue without a transaction succeeded")
	}
}

func inventoryRow() row {
	return row{id: 1, projectID: 7, toolkitID: 70, app: platformops.AppInventory, toolkitType: "inventory_inventory"}
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
			outcomeDone, OutcomeFailedPermanent, "failed permanently: ", ""},
		{"bad request", &fakeRemover{err: fmt.Errorf("%w: toolkit id 0 is not a positive int32", platformops.ErrConfig)},
			outcomeDone, OutcomeFailedPermanent, "failed permanently: ", ""},
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
			if !strings.HasPrefix(got.note, testCase.wantNote) {
				t.Errorf("note = %q, want prefix %q", got.note, testCase.wantNote)
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

// The permanent note carries the engine's reason, which is what an operator
// needs to fix the allowlist.
func TestAPermanentFailureNotesTheReason(t *testing.T) {
	t.Parallel()
	j := New(nil, quiet(), WithRemover(platformops.AppInventory,
		&fakeRemover{err: fmt.Errorf("%w: inventory delete_toolkit: CN=elitea-main not in allowlist (PermissionDenied)", platformops.ErrNotAllowed)}))
	got := j.deleteInEngine(context.Background(), inventoryRow())
	if !strings.Contains(got.note, "allowlist") {
		t.Fatalf("note = %q, want the engine's reason", got.note)
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
	if _, err := j.Resume(context.Background(), 1); err == nil {
		t.Fatal("Resume without a database succeeded")
	}
}
