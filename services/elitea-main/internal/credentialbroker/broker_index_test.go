package credentialbroker

import (
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

// indexRun reshapes the world's run as a governed scheduled index run
// (#1473): an autonomous mandate with no subject, no chat turn, the agent
// the mandate binds, and the toolkit frozen under that binding. The worker
// names no reference and no resource: it reads the whole source.
func indexRun(t *testing.T) (*world, RedeemRequest) {
	t.Helper()
	w := newWorld().withMandate(mandate.KindAutonomous)
	w.context.SubjectPrincipalRef, w.context.ActorChain = "", nil
	req := w.request(t)
	req.Run.ActorID = "41"
	req.Run.ConversationUUID, req.Run.ResponseMessageID = "", ""
	req.Invocation.ToolCallID = "index.ingest"
	req.Invocation.ResourceRef = ""
	req.Invocation.CredentialReferenceID = ""
	return w, req
}

// A governed index run under an active mandate redeems its toolkit's frozen
// credential.
func TestIndexRunRedeemsUnderItsMandate(t *testing.T) {
	w, req := indexRun(t)
	result := redeem(t, w.broker(t), req)
	if result.Envelope == nil {
		t.Fatalf("denied: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if string(result.Envelope.Material["api_token"]) != tSecretValue {
		t.Fatalf("the envelope holds %d keys, not the frozen credential", len(result.Envelope.Material))
	}
}

// A revoked mandate denies the redemption before any secret read.
func TestIndexRunWithARevokedMandateIsDenied(t *testing.T) {
	w, req := indexRun(t)
	m := w.mandates.mandates[tMandate]
	m.State = mandate.StateRevoked
	w.mandates.mandates[tMandate] = m
	reads := w.secrets.readCount()
	requireDenial(t, redeem(t, w.broker(t), req), CodeDenied)
	if w.secrets.readCount() != reads {
		t.Fatal("a denied redemption read a secret")
	}
}
