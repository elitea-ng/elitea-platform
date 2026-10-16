package runtimecomposition

import (
	"context"
	"testing"

	egressauditapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/egressaudit"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/audit"
)

type egressAuditRecorder struct{ events []audit.Event }

func (r *egressAuditRecorder) Record(_ context.Context, event audit.Event) {
	r.events = append(r.events, event)
}

func egressAuditLookup(value string) LookupEnv {
	return func(name string) (string, bool) {
		if name == EgressAuditClientsEnv {
			return value, true
		}
		return "", false
	}
}

func TestEgressAuditClientsParseAndRequireTheRuntime(t *testing.T) {
	clients, err := egressAuditClients(egressAuditLookup(" dns:elitea-egress-proxy ,spiffe://elitea/egress-proxy"))
	if err != nil || len(clients) != 2 {
		t.Fatalf("%v %v", clients, err)
	}
	if _, err := egressAuditClients(egressAuditLookup("elitea-egress-proxy")); err == nil {
		t.Fatal("an identity without a scheme is refused")
	}
	if _, err := ConfigFromEnv(egressAuditLookup("dns:elitea-egress-proxy")); err == nil {
		t.Fatal("accepted egress audit clients with the runtime disabled")
	}
}

// The composition root builds the service over the shared audit recorder,
// and the Runtime's Close writes what the merger holds
// (unit-suite-blind-to-composition-root).
func TestComposedEgressAuditWritesThroughTheAuditRecorder(t *testing.T) {
	if server, merger, err := newEgressAudit(Config{}, Dependencies{}); server != nil || merger != nil || err != nil {
		t.Fatal("no clients: nothing is composed")
	}
	if _, _, err := newEgressAudit(Config{EgressAuditClients: []string{"dns:elitea-egress-proxy"}}, Dependencies{}); err == nil {
		t.Fatal("clients without an audit recorder are refused")
	}
	recorder := &egressAuditRecorder{}
	server, merger, err := newEgressAudit(Config{EgressAuditClients: []string{"dns:elitea-egress-proxy"}},
		Dependencies{AuditRecorder: recorder})
	if err != nil || server == nil || merger == nil {
		t.Fatalf("%v %v %v", server, merger, err)
	}
	if egressAuditServerOrNil(nil) != nil {
		t.Fatal("a nil server must stay a nil interface")
	}
	runtime := &Runtime{egressAudit: merger}
	merger.Report(reportFor("exec-9"))
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if len(recorder.events) != 1 || recorder.events[0].EntityName != "exec-9" {
		t.Fatalf("events: %+v", recorder.events)
	}
}

func reportFor(execution string) egressauditapp.Report {
	return egressauditapp.Report{ReplicaID: "r", ExecutionID: execution, ProjectID: 3, Final: true}
}
