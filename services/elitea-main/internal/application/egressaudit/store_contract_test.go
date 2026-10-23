package egressaudit_test

import (
	"testing"

	app "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit/storetest"
)

// Two Mergers over one store are two Main replicas over one database.
func TestMemoryStoreMeetsTheContract(t *testing.T) {
	storetest.Run(t, func(*testing.T) app.Store { return app.NewMemoryStore() })
}
