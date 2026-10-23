package runtimecomposition

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
)

func TestCurrentConfigurationLifecyclePublisherUsesBoundedProductionPolicy(t *testing.T) {
	store := &currentConfigurationLifecycleStoreProbe{}
	reconciler := currentConfigurationLifecycleReconcilerProbe{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	publisher, err := newCurrentConfigurationLifecyclePublisherWithStore(store, reconciler, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.limit != currentConfigurationLifecycleBatchSize ||
		store.leaseTTL != currentConfigurationLifecycleLeaseTTL ||
		!strings.HasPrefix(store.leaseToken, "elitea-main-") ||
		len(store.leaseToken) != len("elitea-main-")+(16*2) {
		t.Fatalf(
			"claim token=%q limit=%d lease=%s",
			store.leaseToken,
			store.limit,
			store.leaseTTL,
		)
	}
	for _, character := range store.leaseToken {
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '-' {
			continue
		}
		t.Fatalf("lease token contains an unsupported character: %q", character)
	}
}

func TestCurrentConfigurationLifecyclePublisherRejectsIncompleteGraph(t *testing.T) {
	store := &currentConfigurationLifecycleStoreProbe{}
	reconciler := currentConfigurationLifecycleReconcilerProbe{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		store      configurationapp.CurrentConfigurationLifecycleStore
		reconciler configurationapp.CurrentConfigurationLifecycleReconciler
		logger     *slog.Logger
	}{
		{reconciler: reconciler, logger: logger},
		{store: store, logger: logger},
		{store: store, reconciler: reconciler},
	}
	for _, test := range tests {
		if publisher, err := newCurrentConfigurationLifecyclePublisherWithStore(
			test.store,
			test.reconciler,
			test.logger,
		); err == nil || publisher != nil {
			t.Fatalf("publisher=%#v error=%v", publisher, err)
		}
	}
}

func TestCurrentConfigurationLifecycleLeaseTokensAreFresh(t *testing.T) {
	first, err := newCurrentConfigurationLifecycleLeaseToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newCurrentConfigurationLifecycleLeaseToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("lease token was reused: %q", first)
	}
}

type currentConfigurationLifecycleStoreProbe struct {
	leaseToken string
	limit      int
	leaseTTL   time.Duration
}

func (store *currentConfigurationLifecycleStoreProbe) ClaimCurrentConfigurationLifecycle(
	_ context.Context,
	leaseToken string,
	limit int,
	leaseTTL time.Duration,
) ([]configurationapp.CurrentConfigurationLifecycleEvent, error) {
	store.leaseToken = leaseToken
	store.limit = limit
	store.leaseTTL = leaseTTL
	return []configurationapp.CurrentConfigurationLifecycleEvent{}, nil
}

func (*currentConfigurationLifecycleStoreProbe) MarkCurrentConfigurationLifecycleDelivered(
	context.Context,
	string,
	string,
) error {
	return nil
}

func (*currentConfigurationLifecycleStoreProbe) MarkCurrentConfigurationLifecycleRetry(
	context.Context,
	string,
	string,
	string,
	time.Duration,
) error {
	return nil
}

func (*currentConfigurationLifecycleStoreProbe) MarkCurrentConfigurationLifecycleDead(
	context.Context,
	string,
	string,
	string,
) error {
	return nil
}

type currentConfigurationLifecycleReconcilerProbe struct{}

func (currentConfigurationLifecycleReconcilerProbe) ReconcileCurrentConfigurationLifecycle(
	context.Context,
	configurationapp.CurrentConfigurationLifecycleEvent,
	configurationapp.CurrentConfigurationLifecycleIntent,
) (configurationapp.CurrentConfigurationLifecycleReconcileResult, error) {
	return configurationapp.CurrentConfigurationLifecycleReconcileResult{
		Disposition: configurationapp.CurrentConfigurationLifecycleReconciled,
	}, nil
}
