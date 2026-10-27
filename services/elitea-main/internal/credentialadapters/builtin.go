package credentialadapters

import "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"

// Short names for the classes the reference providers use.
const (
	staticSecret   = credentialgrant.ClassStaticSecret
	oauthDelegated = credentialgrant.ClassOAuthDelegated
)

// Builtin returns fresh copies of the five reference provider descriptors:
// Jira, Confluence, Aha!, SharePoint and qTest. Their data equals the shared
// contract testdata/proto/runtime/v1/governed-operations/catalogue.json,
// which the Rust governed tool mapping is tested against as well.
//
// Every provider × class is "fixture" at most: no class is qualified
// against a real tenant until gate D6 and a qualification run (#1124).
func Builtin() []AdapterDescriptor {
	return []AdapterDescriptor{Jira(), Confluence(), Aha(), SharePoint(), QTest()}
}

// NewBuiltinRegistry is the registry of the reference providers.
func NewBuiltinRegistry() (*Registry, error) { return NewRegistry(Builtin()...) }

func op(id, class, resource string, cost int) OperationSpec {
	return OperationSpec{ID: credentialgrant.OperationID(id), Class: class, ResourceKind: resource, CostHint: cost}
}

func probe(id string) *ProbeSpec { return &ProbeSpec{Operation: credentialgrant.OperationID(id)} }
