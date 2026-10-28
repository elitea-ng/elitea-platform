package credentialbroker

import (
	"context"
	"errors"
	"strconv"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/authzcompose"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// securitycontextView is the verified stored context, as plain fields.
type securitycontextView struct {
	policyRevisionID   string
	policyDigest       [32]byte
	tenantProjectID    int64
	subjectPrincipal   string
	agentBindingID     string
	agentBindingEpoch  int64
	mandateID          string
	mandateEpoch       int64
	delegationID       string
	delegationEpoch    int64
	authSource         string
	callingApplication string
	definitionDigest   [32]byte
	actorChain         []string
	// applicationID and applicationVersionID are the version admission ran
	// (#1119); frozen holds the governed-credential-references entry by
	// toolkit id.
	applicationID        int64
	applicationVersionID int64
	frozen               map[int64]securitycontext.CredentialReference
}

func viewOf(m *runtimev1.ExecutionSecurityContextV1) *securitycontextView {
	v := &securitycontextView{
		policyRevisionID: m.GetPolicyRevisionId(), tenantProjectID: m.GetTenantProjectId(),
		subjectPrincipal: m.GetSubjectPrincipalRef(), agentBindingID: m.GetAgentBindingId(),
		agentBindingEpoch: m.GetAgentBindingEpoch(), mandateID: m.GetMandateId(), mandateEpoch: m.GetMandateEpoch(),
		delegationID: m.GetDelegationId(), delegationEpoch: m.GetDelegationEpoch(),
		authSource: m.GetCaller().GetAuthenticationSource(), callingApplication: m.GetCaller().GetCallingApplicationId(),
		applicationID: m.GetApplicationId(), applicationVersionID: m.GetApplicationVersionId(),
		frozen: map[int64]securitycontext.CredentialReference{},
	}
	for _, ref := range m.GetCredentialReferences() {
		if frozen, ok := securitycontext.CredentialReferenceFor(m, ref.GetToolkitId()); ok {
			v.frozen[frozen.ToolkitID] = frozen
		}
	}
	copy(v.policyDigest[:], m.GetPolicyDigest().GetValue())
	copy(v.definitionDigest[:], m.GetDefinitionDigest().GetValue())
	// The context lists the chain outermost last; the audit record outermost
	// first.
	links := m.GetActorChain()
	for i := len(links) - 1; i >= 0; i-- {
		v.actorChain = append(v.actorChain, links[i].GetKind()+":"+links[i].GetRef())
	}
	return v
}

// requestFacts is the decision request and the records it was built from.
type requestFacts struct {
	request    authzcompose.OperationRequest
	reference  credentialgrant.CredentialReference
	target     credentialgrant.TargetBinding
	mandate    *mandate.Mandate
	delegation *mandate.Delegation
	actorChain []string
	// externalIdentityID is the binding's external agent identity; empty
	// when it has none or the binding was not read.
	externalIdentityID string
	// runSubjectUser is the stored context's own subject user, before the
	// autonomous-mandate rule replaces the decision subject with the
	// approver. 0 when the run has none.
	runSubjectUser int64
}

// buildRequest builds the operation request from the stored context and the
// invocation. A record that does not exist leaves its fields empty, and the
// decider denies the incomplete request; only a read failure is an error.
//
// Subject rule (owner decision, #1118):
//   - an interactive or delegated run: the context's subject user;
//   - a delegated_background mandate: the context's subject user, which the
//     authority gate requires to be the mandate's user;
//   - an autonomous mandate: the mandate's approver, read at decision time.
//     The mandate names no user, so the person who approved the agent's
//     standing authority is the one whose RBAC and entity access bound it.
//
// A subject no request authenticated (no stored authentication source)
// under a mandate is attributed, never verified.
func (b *Broker) buildRequest(ctx context.Context, sc *securitycontextView, req RedeemRequest) (requestFacts, error) {
	facts := requestFacts{actorChain: sc.actorChain}
	tenant := sc.tenantProjectID
	inv := req.Invocation

	reference, err := b.cfg.References.CredentialReference(ctx, tenant, inv.CredentialReferenceID)
	switch {
	case errors.Is(err, credentialgrant.ErrReferenceNotFound):
	case err != nil:
		return requestFacts{}, err
	default:
		facts.reference = reference
		target, err := b.cfg.Targets.TargetBinding(ctx, tenant, reference.TargetBindingID)
		switch {
		case errors.Is(err, credentialgrant.ErrTargetNotFound):
		case err != nil:
			return requestFacts{}, err
		default:
			facts.target = target
		}
	}

	agent := authzcompose.AgentRef{
		BindingID: sc.agentBindingID, Epoch: sc.agentBindingEpoch, DefinitionDigest: sc.definitionDigest,
	}
	// The application and version are the ones admission ran, from the
	// stored context, and the toolkit digest is the one admission froze
	// (#1119): the agent binding gate compares the binding and its toolkit
	// binding with them, so a binding moved to another version, or a toolkit
	// re-bound at another digest, after admission denies. A toolkit admission
	// froze nothing for keeps the zero digest, which no binding has. The
	// definition digest is the context's copy of the binding's; no canonical
	// digest of the executed version exists to compare it with (#1118, note
	// (d)).
	agent.ApplicationID, agent.ApplicationVersionID = sc.applicationID, sc.applicationVersionID
	toolkitDigest := sc.frozen[inv.ToolkitID].ToolkitDigest
	if sc.agentBindingID != "" {
		binding, err := b.cfg.Bindings.GetUsable(ctx, sc.agentBindingID)
		switch {
		case errors.Is(err, agentidentity.ErrBindingNotFound):
		case err != nil:
			return requestFacts{}, err
		default:
			facts.externalIdentityID = binding.ExternalAgentIdentityID
		}
	}

	subjectUser := parseUserID(sc.subjectPrincipal)
	facts.runSubjectUser = subjectUser
	source := authSource(sc.authSource)
	var authority authzcompose.AuthorityRef
	var externalSubject string
	switch {
	case sc.mandateID != "":
		authority = authzcompose.AuthorityRef{Kind: authzcompose.AuthorityMandate, ID: sc.mandateID, Epoch: sc.mandateEpoch}
		m, err := b.cfg.Mandates.Get(ctx, sc.mandateID)
		switch {
		case errors.Is(err, mandate.ErrMandateNotFound):
		case err != nil:
			return requestFacts{}, err
		default:
			facts.mandate = &m
			if m.Kind == mandate.KindAutonomous {
				subjectUser = parseUserID(m.ApprovedBy)
			}
		}
		// A delegated_background mandate may carry the user's delegation;
		// admission recorded it so an oauth_delegated use can exchange it.
		if sc.delegationID != "" && facts.mandate != nil && facts.mandate.Kind == mandate.KindDelegatedBackground &&
			facts.mandate.DelegationID != nil && *facts.mandate.DelegationID == sc.delegationID {
			d, err := b.cfg.Delegations.Delegation(ctx, sc.delegationID)
			switch {
			case errors.Is(err, mandate.ErrDelegationNotUsable):
			case err != nil:
				return requestFacts{}, err
			default:
				facts.delegation = &d
				externalSubject = d.SubjectObjectID
			}
		}
	case sc.delegationID != "":
		authority = authzcompose.AuthorityRef{Kind: authzcompose.AuthorityDelegation, ID: sc.delegationID, Epoch: sc.delegationEpoch}
		d, err := b.cfg.Delegations.Delegation(ctx, sc.delegationID)
		switch {
		case errors.Is(err, mandate.ErrDelegationNotUsable):
		case err != nil:
			return requestFacts{}, err
		default:
			facts.delegation = &d
			// The custody record binds the delegation to the user who
			// consented; its subject is that user's IdP object id.
			externalSubject = d.SubjectObjectID
		}
	}

	subject := authzcompose.Subject{
		UserID: subjectUser, AuthSource: source,
		CallingApplicationID: sc.callingApplication, WorkloadID: req.Run.WorkloadID,
		ExternalSubjectID: externalSubject,
		Attributed:        authority.Kind == authzcompose.AuthorityMandate && source == auth.AuthenticationSourceUnknown,
	}
	if subjectUser > 0 {
		subject.PrincipalRef = "user:" + strconv.FormatInt(subjectUser, 10)
	}
	facts.request = authzcompose.OperationRequest{
		Subject:   subject,
		Agent:     agent,
		Authority: authority,
		Action: authzcompose.Action{
			OperationID: credentialgrant.OperationID(inv.OperationID), Capability: credentialgrant.CapUse,
		},
		Resource: authzcompose.Resource{
			ToolkitID: inv.ToolkitID, ToolkitDigest: toolkitDigest,
			CredentialReferenceID: inv.CredentialReferenceID, TargetBindingID: facts.reference.TargetBindingID,
			ResourceRef: inv.ResourceRef,
		},
		Context: authzcompose.DecisionContext{
			TenantProjectID: tenant, PolicyRevisionID: sc.policyRevisionID, PolicyDigest: sc.policyDigest,
			ExecutionID: req.Run.ExecutionID, InvocationID: inv.InvocationID,
		},
	}
	return facts, nil
}

// parseUserID reads a positive user id, "7" or "user:7"; anything else is 0.
func parseUserID(ref string) int64 {
	if len(ref) > 5 && ref[:5] == "user:" {
		ref = ref[5:]
	}
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != ref {
		return 0
	}
	return id
}

// authSource maps the context's stored source name back to its value.
func authSource(name string) auth.AuthenticationSource {
	switch name {
	case "session":
		return auth.AuthenticationSourceSession
	case "token":
		return auth.AuthenticationSourceToken
	case "api_key":
		return auth.AuthenticationSourceAPIKey
	case "development":
		return auth.AuthenticationSourceDevelopment
	}
	return auth.AuthenticationSourceUnknown
}
