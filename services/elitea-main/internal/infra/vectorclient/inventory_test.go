package vectorclient_test

import (
	"context"
	"errors"
	"testing"

	vectorv1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/vectorclient"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The ids are pinned with the Inventory engine's store::vectors::namespace_id
// (services/elitea-inventory-engine/src/store/vectors.rs) and Python's
// uuid.uuid5(uuid.uuid5(uuid.NAMESPACE_URL,
// "https://elitea.ai/ns/inventory/graph-vectors"), "<project>/<toolkit>"): a
// drift here deletes another namespace than the engine wrote.
func TestInventoryGraphNamespaceIsTheEnginesUUIDv5(t *testing.T) {
	for _, tc := range []struct {
		project, toolkit int64
		want             string
	}{
		{1, 1, "09265e87-a97c-53e6-8062-d685fb6cf4ad"},
		{7, 42, "454412b3-aea7-5ecf-aefe-49264f4557d0"},
		{2147483647, 9223372036854775807, "3111a581-a4af-549c-af63-b217cec52eca"},
	} {
		if got := vectorclient.InventoryGraphNamespace(tc.project, tc.toolkit); got != tc.want {
			t.Errorf("InventoryGraphNamespace(%d, %d) = %s, want %s", tc.project, tc.toolkit, got, tc.want)
		}
	}
}

func TestDeleteInventoryGraphIsAWholeNamespaceDeleteOfTheInventorySource(t *testing.T) {
	p := newPKI(t)
	vector := &fakeVector{}
	client := newClient(t, p, serve(t, p, vector))
	if err := client.DeleteInventoryGraph(context.Background(), 7, 42); err != nil {
		t.Fatalf("DeleteInventoryGraph: %v", err)
	}
	vector.mu.Lock()
	defer vector.mu.Unlock()
	if len(vector.deletes) != 1 {
		t.Fatalf("deletes = %d, want 1", len(vector.deletes))
	}
	request := vector.deletes[0]
	if request.GetProjectId() != 7 ||
		request.GetNamespace().GetSource() != vectorv1.Source_SOURCE_INVENTORY ||
		request.GetNamespace().GetNamespaceId() != "454412b3-aea7-5ecf-aefe-49264f4557d0" ||
		request.GetWholeNamespace() == nil || request.GetSpace() != nil {
		t.Fatalf("request = %v", request)
	}
}

func TestDeleteInventoryGraphClassifiesForTheJournal(t *testing.T) {
	p := newPKI(t)
	for _, tc := range []struct {
		code codes.Code
		want error
	}{
		{codes.Unavailable, platformops.ErrRetryLater},
		{codes.PermissionDenied, platformops.ErrNotAllowed},
		{codes.InvalidArgument, platformops.ErrConfig},
	} {
		vector := &fakeVector{delete: func(*vectorv1.DeleteRequest) (*vectorv1.DeleteResponse, error) {
			return nil, status.Error(tc.code, "refused")
		}}
		client := newClient(t, p, serve(t, p, vector))
		if err := client.DeleteInventoryGraph(context.Background(), 7, 42); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.code, err, tc.want)
		}
	}
	var none *vectorclient.Client
	if err := none.DeleteInventoryGraph(context.Background(), 7, 42); !errors.Is(err, platformops.ErrConfig) {
		t.Errorf("nil client: %v", err)
	}
	client := newClient(t, p, serve(t, p, &fakeVector{}))
	if err := client.DeleteInventoryGraph(context.Background(), 0, 42); !errors.Is(err, platformops.ErrConfig) {
		t.Errorf("project 0: %v", err)
	}
}
