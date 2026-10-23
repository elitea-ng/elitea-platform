package runtimecomposition

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	currentConfigurationLifecyclePollInterval = 250 * time.Millisecond
	currentConfigurationLifecycleLeaseTTL     = 5 * time.Minute
	currentConfigurationLifecycleRetryBase    = time.Second
	currentConfigurationLifecycleBatchSize    = 64
	currentConfigurationLifecycleConcurrency  = 8
)

func newCurrentConfigurationLifecyclePublisher(
	pool *pgxpool.Pool,
	reconciler configurationapp.CurrentConfigurationLifecycleReconciler,
	logger *slog.Logger,
) (publisherRunner, error) {
	if pool == nil {
		return nil, errors.New("current configuration lifecycle database is required")
	}
	store, err := repos.NewCurrentConfigurationLifecycleOutboxRepository(pool)
	if err != nil {
		return nil, err
	}
	return newCurrentConfigurationLifecyclePublisherWithStore(store, reconciler, logger)
}

func newCurrentConfigurationLifecyclePublisherWithStore(
	store configurationapp.CurrentConfigurationLifecycleStore,
	reconciler configurationapp.CurrentConfigurationLifecycleReconciler,
	logger *slog.Logger,
) (*configurationapp.CurrentConfigurationLifecycleProcessor, error) {
	if store == nil || reconciler == nil || logger == nil {
		return nil, errors.New("current configuration lifecycle dependencies are required")
	}
	return configurationapp.NewCurrentConfigurationLifecycleProcessor(
		store,
		reconciler,
		newCurrentConfigurationLifecycleLeaseToken,
		configurationapp.CurrentConfigurationLifecycleProcessorConfig{
			PollInterval:  currentConfigurationLifecyclePollInterval,
			LeaseTTL:      currentConfigurationLifecycleLeaseTTL,
			RetryBase:     currentConfigurationLifecycleRetryBase,
			BatchSize:     currentConfigurationLifecycleBatchSize,
			MaxConcurrent: currentConfigurationLifecycleConcurrency,
			MaxAttempts:   configurationapp.MaxCurrentConfigurationLifecycleAttempts,
			ReportFailure: func(err error) {
				// Processor errors contain only event identities and bounded safe
				// codes. Provider errors and configuration payloads are discarded
				// by the application boundary before this callback runs.
				logger.Error("current configuration lifecycle cycle failed", "err", err)
			},
		},
	)
}

func newCurrentConfigurationLifecycleLeaseToken() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "elitea-main-" + hex.EncodeToString(random[:]), nil
}

var _ publisherRunner = (*configurationapp.CurrentConfigurationLifecycleProcessor)(nil)
