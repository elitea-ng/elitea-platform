package credentialbroker

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"

// OperationCatalog is what the broker and the decider read from the provider
// adapter descriptors (#1120): which operations exist, their class, and
// which material keys an adapter consumes for a provider type and
// credential class. It is a credentialgrant.OperationRegistry, so one value
// feeds both the decider's scope gate and the broker.
type OperationCatalog interface {
	credentialgrant.OperationRegistry
	// MaterialKeys names the keys of the material the adapter of
	// providerType reads for class. Empty means the adapter takes none of
	// this class, and the broker releases nothing.
	MaterialKeys(providerType string, class credentialgrant.CredentialClass) []string
}

// EmptyCatalog knows no operation and no material key: every decision it
// feeds denies with OPERATION_UNKNOWN and the broker releases nothing. It is
// the fail-closed catalogue until the adapter descriptors exist.
type EmptyCatalog struct{}

// Known implements credentialgrant.OperationRegistry.
func (EmptyCatalog) Known(credentialgrant.OperationID) bool { return false }

// Class implements credentialgrant.OperationRegistry.
func (EmptyCatalog) Class(credentialgrant.OperationID) string { return "" }

// MaterialKeys implements OperationCatalog.
func (EmptyCatalog) MaterialKeys(string, credentialgrant.CredentialClass) []string { return nil }

var _ OperationCatalog = EmptyCatalog{}
