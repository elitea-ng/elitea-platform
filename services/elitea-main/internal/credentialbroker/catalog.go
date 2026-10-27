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
	// providerType reads for class. The first key is required, the rest are
	// optional. Empty means the adapter takes none of this class, and the
	// broker releases nothing.
	MaterialKeys(providerType string, class credentialgrant.CredentialClass) []string
	// OAuthResource is the delegated token's resource and least-privilege
	// scopes the adapter of providerType asks for under class; empty when
	// it names none (the target binding's audience then decides).
	OAuthResource(providerType string, class credentialgrant.CredentialClass) (resource string, scopes []string)
	// OAuthAuthority is who issued the consent of an oauth_delegated
	// credential of providerType: the Entra delegation (on-behalf-of) or
	// the provider itself (provider OAuth grant). Empty denies the use.
	OAuthAuthority(providerType string, class credentialgrant.CredentialClass) credentialgrant.OAuthAuthority
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

// OAuthResource implements OperationCatalog.
func (EmptyCatalog) OAuthResource(string, credentialgrant.CredentialClass) (string, []string) {
	return "", nil
}

// OAuthAuthority implements OperationCatalog.
func (EmptyCatalog) OAuthAuthority(string, credentialgrant.CredentialClass) credentialgrant.OAuthAuthority {
	return ""
}

var _ OperationCatalog = EmptyCatalog{}
