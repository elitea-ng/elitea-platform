package api

// The composition of the application engines into the two cleanup journals
// (#1243, #1244): every wired client becomes one provisioner option and one
// toolkit-journal remover, and the toolkit journal built here enqueues only
// for the applications it was given.

import (
	"context"
	"slices"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
)

type fakeApplicationClient struct {
	app      string
	toolkits [][2]int64
	projects []int64
	err      error
}

func (f *fakeApplicationClient) App() string { return f.app }

func (f *fakeApplicationClient) DeleteProjectData(_ context.Context, projectID int64) (string, error) {
	f.projects = append(f.projects, projectID)
	return "nothing to delete", f.err
}

func (f *fakeApplicationClient) DeleteToolkitData(_ context.Context, projectID, toolkitID int64) (platformops.ToolkitDeletion, error) {
	f.toolkits = append(f.toolkits, [2]int64{projectID, toolkitID})
	return platformops.ToolkitDeletion{Deleted: f.err == nil}, f.err
}

func TestEveryClientBecomesOneProvisionerOption(t *testing.T) {
	t.Parallel()
	clients := []ApplicationDataClient{
		&fakeApplicationClient{app: platformops.AppDeepWiki},
		nil,
		&fakeApplicationClient{app: platformops.AppInventory},
	}
	if got := len(applicationDataOptions(clients)); got != 2 {
		t.Fatalf("%d options for two clients and a nil", got)
	}
	if got := len(toolkitCleanupOptions(clients)); got != 2 {
		t.Fatalf("%d journal options for two clients and a nil", got)
	}
}

// The toolkit journal the router builds knows the wired applications, and
// decides from them (and from which applications keep data per toolkit) which
// toolkit types owe a row: Inventory yes, DeepWiki never, others never.
func TestTheToolkitJournalEnqueuesForTheWiredEnginesOnly(t *testing.T) {
	t.Parallel()
	journal := newToolkitCleanupJournal(RouterConfig{ApplicationData: []ApplicationDataClient{
		&fakeApplicationClient{app: platformops.AppInventory},
		&fakeApplicationClient{app: platformops.AppDeepWiki},
	}})
	if got := journal.AppsWired(); !slices.Equal(got, []string{"deepwiki", "inventory"}) {
		t.Fatalf("AppsWired = %v", got)
	}
	ctx := context.Background()
	if app := journal.AppToEnqueue(ctx, 7, 70, "inventory_inventory"); app != platformops.AppInventory {
		t.Fatalf("inventory toolkit → %q", app)
	}
	if app := journal.AppToEnqueue(ctx, 7, 71, "deepwiki_Deepwiki"); app != "" {
		t.Fatalf("a DeepWiki toolkit owes an engine call: %q", app)
	}
	if app := journal.AppToEnqueue(ctx, 7, 72, "github"); app != "" {
		t.Fatalf("a github toolkit owes an engine call: %q", app)
	}

	unwired := newToolkitCleanupJournal(RouterConfig{})
	if app := unwired.AppToEnqueue(ctx, 7, 70, "inventory_inventory"); app != "" {
		t.Fatalf("a replica without the Inventory client enqueued for it: %q", app)
	}
}
