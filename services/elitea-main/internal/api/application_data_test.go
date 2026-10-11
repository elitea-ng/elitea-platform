package api

// The toolkit-side adapter over the application engines (#1244): which engine a
// toolkit type is sent to, and how the engine's answers reach the handler.

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/toolkits"
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

func TestToolkitDataGoesToTheEngineOfItsType(t *testing.T) {
	t.Parallel()
	inventory := &fakeApplicationClient{app: platformops.AppInventory}
	deepwiki := &fakeApplicationClient{app: platformops.AppDeepWiki, err: platformops.ErrNotSupported}
	remover := newApplicationToolkitData([]ApplicationDataClient{inventory, deepwiki}, slog.Default())

	if err := remover.DeleteToolkitData(context.Background(), 7, 70, "inventory_inventory"); err != nil {
		t.Fatalf("inventory: %v", err)
	}
	if len(inventory.toolkits) != 1 || inventory.toolkits[0] != [2]int64{7, 70} {
		t.Fatalf("inventory saw %v", inventory.toolkits)
	}
	// DeepWiki keeps nothing per toolkit: UNIMPLEMENTED is a no-op, not a
	// refusal of the row delete.
	if err := remover.DeleteToolkitData(context.Background(), 7, 71, "deepwiki_Deepwiki"); err != nil {
		t.Fatalf("deepwiki: %v", err)
	}
	if len(deepwiki.toolkits) != 1 {
		t.Fatalf("deepwiki was not asked: %v", deepwiki.toolkits)
	}
	// A type no engine covers is nobody's.
	if err := remover.DeleteToolkitData(context.Background(), 7, 72, "github"); err != nil {
		t.Fatalf("github: %v", err)
	}
	if len(inventory.toolkits)+len(deepwiki.toolkits) != 2 {
		t.Fatal("a github toolkit reached an engine")
	}
}

func TestAnUnconfiguredApplicationIsANoOp(t *testing.T) {
	t.Parallel()
	remover := newApplicationToolkitData(nil, nil)
	if err := remover.DeleteToolkitData(context.Background(), 7, 70, "inventory_inventory"); err != nil {
		t.Fatalf("an unconfigured engine refused the delete: %v", err)
	}
}

func TestEngineAnswersAreTranslatedForTheHandler(t *testing.T) {
	t.Parallel()
	busy := &fakeApplicationClient{app: platformops.AppInventory, err: platformops.ErrRetryLater}
	if err := newApplicationToolkitData([]ApplicationDataClient{busy}, nil).
		DeleteToolkitData(context.Background(), 7, 70, "inventory"); !errors.Is(err, toolkits.ErrToolkitDataBusy) {
		t.Fatalf("retry-later = %v, want ErrToolkitDataBusy", err)
	}
	cause := errors.New("internal")
	failing := &fakeApplicationClient{app: platformops.AppInventory, err: cause}
	err := newApplicationToolkitData([]ApplicationDataClient{failing}, nil).
		DeleteToolkitData(context.Background(), 7, 70, "inventory")
	if !errors.Is(err, cause) || errors.Is(err, toolkits.ErrToolkitDataBusy) {
		t.Fatalf("failure = %v, want the cause and not busy", err)
	}
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
}
