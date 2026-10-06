package storage

import (
	"context"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

// CodeCallOriginalIntent is an owning journal relation, not an output proof.
// The caller must independently match it to the immutable Code intent and visit.
type CodeCallOriginalIntent struct {
	OriginalVisit       code.OriginalVisitRef
	Binding             code.Binding
	BindingSHA256       string
	PreparedFingerprint string
	Broker              CodePreparedPlatformClient
	BrokerSHA256        string
	CompiledExecute     *OriginalCompiledCodeExecute
	CallEffectID        string
	Sequence            uint64
	State               string
}

// The caller supplies its already-authorized short recovery transaction.
// No method dispatches an operation, returns secret bytes, or changes graph identity.
type CodeCallEffectResolver interface {
	ResolveOriginalCodeCallEffect(context.Context, CodeTransaction, string, uint64, string) (CodeCallOriginalIntent, error)
}

// CodeBrokerEffectFacts is defined solely by the whole-Code intent owner.
// This predicate describes journal observations and grants no owner proof.
func (f CodeBrokerEffectFacts) NoObservedEffect() bool {
	return f.Registered && !f.HasObservedCalls && !f.HasDispatchedEffects && !f.HasUncertainEffects && !f.HasPendingToolkitChildren
}

type CodeBrokerEffectReader = OriginalCodeBrokerEffectFactsReader

// RegisterCodePlatformJob pins an actual authenticated retained runtime before
// the first frame is read. Registration is required for a later no-effect check.
type CodePlatformJobRegistrar interface {
	RegisterCodePlatformJob(context.Context, CodeTransaction, domain.Admission) error
}
