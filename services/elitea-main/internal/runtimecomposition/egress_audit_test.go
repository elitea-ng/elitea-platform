package runtimecomposition

import (
	"context"
	"testing"
	"time"

	egressauditapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/egressaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
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

// The composition root builds the service over the shared audit recorder and
// the shared store; the Runtime's Close stops the sweep and writes nothing (a
// turn that is not written stays in the store for the next Main).
func TestComposedEgressAuditWritesThroughTheAuditRecorder(t *testing.T) {
	if server, merger, err := newEgressAudit(Config{}, Dependencies{}); server != nil || merger != nil || err != nil {
		t.Fatal("no clients: nothing is composed")
	}
	clients := Config{EgressAuditClients: []string{"dns:elitea-egress-proxy"}}
	if _, _, err := newEgressAudit(clients, Dependencies{}); err == nil {
		t.Fatal("clients without an audit recorder are refused")
	}
	recorder := &egressAuditRecorder{}
	if _, _, err := newEgressAudit(clients, Dependencies{AuditRecorder: recorder}); err == nil {
		t.Fatal("clients without a summary store are refused: the summaries are never held in memory")
	}
	store := egressauditapp.NewMemoryStore()
	server, merger, err := newEgressAudit(clients, Dependencies{AuditRecorder: recorder, EgressAuditStore: store})
	if err != nil || server == nil || merger == nil {
		t.Fatalf("%v %v %v", server, merger, err)
	}
	if egressAuditServerOrNil(nil) != nil {
		t.Fatal("a nil server must stay a nil interface")
	}
	runtime := &Runtime{egressAudit: merger}
	if err := merger.Report(context.Background(), reportFor("exec-9")); err != nil {
		t.Fatal(err)
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if len(recorder.events) != 0 || store.Rows() != 1 {
		t.Fatalf("Close writes nothing and keeps the row: events %+v rows %d", recorder.events, store.Rows())
	}
}

func TestEgressAuditRetentionParses(t *testing.T) {
	lookup := func(value string) LookupEnv {
		return func(name string) (string, bool) { return value, name == EgressAuditRetentionEnv }
	}
	if d, err := egressAuditRetention(lookup("")); d != 0 || err != nil {
		t.Fatalf("default: %v %v", d, err)
	}
	if d, err := egressAuditRetention(lookup("14")); d != 14*24*time.Hour || err != nil {
		t.Fatalf("14 days: %v %v", d, err)
	}
	for _, bad := range []string{"0", "-1", "366", "a week", "1.5"} {
		if _, err := egressAuditRetention(lookup(bad)); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
}

func reportFor(execution string) egressauditapp.Report {
	return egressauditapp.Report{ReplicaID: "r", ExecutionID: execution, ProjectID: 3, Final: true}
}
