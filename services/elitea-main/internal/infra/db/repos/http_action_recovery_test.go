package repos

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	app "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/httpaction"
	recovery "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/noderecovery"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
)

type httpProofRow struct {
	record httpRecoveryRecord
	err    error
}

func (row httpProofRow) Scan(dest ...any) error {
	if row.err != nil {
		return row.err
	}
	r := row.record
	values := []any{r.state, r.activation, r.effect, r.requestDigest, r.bindingDigest, r.policyDigest, r.outputBucket, r.request, r.receiptWire, r.receiptSHA, r.input, r.tenant, r.project, r.projection}
	if len(dest) != len(values) {
		return errors.New("invalid scan")
	}
	for i, value := range values {
		reflect.ValueOf(dest[i]).Elem().Set(reflect.ValueOf(value))
	}
	return nil
}

type httpProofTX struct {
	record httpRecoveryRecord
	err    error
	reads  int
}

func (tx *httpProofTX) QueryRow(_ context.Context, _ string, _ ...any) sqlRow {
	tx.reads++
	return httpProofRow{record: tx.record, err: tx.err}
}
func (*httpProofTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("proof issuer mutated state")
}
func (*httpProofTX) Query(context.Context, string, ...any) (sqlRows, error) {
	panic("proof issuer expanded scope")
}

func httpProofFixture(t *testing.T) (string, uint64, recovery.Receipt, httpRecoveryRecord) {
	t.Helper()
	raw, err := os.ReadFile("../../../../../../libs/proto/elitea/runtime/v1/http_frozen_request.fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Application json.RawMessage `json:"application"`
			Execution   string          `json:"execution_id"`
			Generation  uint64          `json:"generation"`
			Wire        string          `json:"request_wire_utf8"`
		} `json:"cases"`
	}
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("fixture invalid")
	}
	f := fixture.Cases[0]
	// Main runtime IDs are hex encodings of16 random bytes, not UUIDs.
	f.Execution = "0123456789abcdef0123456789abcdef"
	var application struct {
		Version struct {
			Snapshot app.FrozenSnapshot `json:"http_action_snapshot"`
		} `json:"version_details"`
	}
	_ = json.Unmarshal(f.Application, &application)
	node := application.Version.Snapshot.Nodes[0]
	input, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{ThreadId: proto.String("root-thread"), Application: f.Application})
	if err != nil {
		t.Fatal(err)
	}
	thread := app.RootGraphThread("tenant", 7, 7, "root-thread")
	inv := app.Invocation{SchemaVersion: app.Schema, NodeID: "fetch", ThreadID: thread, Step: 3, RequestDigest: node.RequestDigest, BindingDigest: node.BindingDigest, RequestWireB64: node.RequestWireB64, Request: []byte(f.Wire)}
	inv.ActivationID = app.VisitID(thread, "fetch", 3, app.BindingBytes(node.BindingDigest))
	effect := app.EffectID(f.Execution, f.Generation, inv)
	request, err := app.Parse(inv)
	if err != nil {
		t.Fatal(err)
	}
	projection, failure := app.Project(request, 200, "application/json", []byte(`null`), nil)
	if failure != "" {
		t.Fatal(failure)
	}
	receipt := app.Receipt{SchemaVersion: app.ReceiptSchema, ActivationID: inv.ActivationID, RequestDigest: node.RequestDigest, BindingDigest: node.BindingDigest, EffectID: effect, State: "completed", Result: projection}
	wire, _ := json.Marshal(receipt)
	sha := app.Digest(wire)
	record := httpRecoveryRecord{state: "completed", activation: inv.ActivationID, effect: effect, requestDigest: node.RequestDigest, bindingDigest: node.BindingDigest, policyDigest: app.Digest([]byte("operator-policy")), outputBucket: "outputs", request: []byte(f.Wire), receiptWire: wire, receiptSHA: &sha, input: input, tenant: "tenant", project: 7, projection: 7}
	journal := recovery.Receipt{Schema: recovery.Schema, ActivationID: strings.Repeat("c", 64), JournalRevision: 4, NodeID: "fetch", GraphThread: thread, Step: 3, Attempt: 1, FailureClass: "invalid_result", StopReason: "effect_reconciliation_required", ReplaySafety: recovery.ReplaySafety{Kind: "completed_external_effect", ReceiptID: effect}, AllowedActions: []string{"resume_result"}}
	if journal.Validate() != nil {
		t.Fatal("journal fixture invalid")
	}
	return f.Execution, f.Generation, journal, record
}

func TestHTTPRecoveryProofIssuesOnlyExactCommittedOwnerReceipt(t *testing.T) {
	execution, generation, journal, record := httpProofFixture(t)
	tx := &httpProofTX{record: record}
	owner := NewHTTPActionRecoveryProofProvider()
	raw, err := owner.VerifyNodeRecoveryEffect(t.Context(), tx, execution, generation, journal, "resume_result")
	if err != nil {
		t.Fatal(err)
	}
	proof, err := recovery.DecodeOwnerProof(raw, execution, generation, journal, "resume_result")
	if err != nil || proof.Kind != "committed_result" || proof.ActivationID == record.activation || proof.EffectID != record.effect || proof.OwnerReceiptSHA256 != *record.receiptSHA {
		t.Fatalf("wrong owner proof: %v", err)
	}
	returned, err := owner.ReadNodeRecoveryHTTPResult(t.Context(), tx, execution, generation, journal, raw)
	if err != nil || string(returned) != string(record.receiptWire) || tx.reads != 2 {
		t.Fatal("read-only exact receipt redemption failed")
	}
	returned[0] = 'x'
	if record.receiptWire[0] == 'x' {
		t.Fatal("result source aliased immutable receipt bytes")
	}
}

func TestHTTPRecoveryProofNeverTreatsAbsenceFailureOrDispatchAmbiguityAsNoEffect(t *testing.T) {
	execution, generation, journal, record := httpProofFixture(t)
	for _, state := range []string{"dispatching", "uncertain", "failed", "missing"} {
		tx := &httpProofTX{record: record}
		tx.record.state = state
		if state == "missing" {
			tx.err = pgx.ErrNoRows
		}
		raw, err := NewHTTPActionRecoveryProofProvider().VerifyNodeRecoveryEffect(t.Context(), tx, execution, generation, journal, "resume_result")
		if err == nil || raw != nil || tx.reads != 1 {
			t.Fatal("non-committed evidence authorized recovery")
		}
	}
	tx := &httpProofTX{record: record}
	if raw, err := NewHTTPActionRecoveryProofProvider().VerifyNodeRecoveryEffect(t.Context(), tx, execution, generation, journal, "retry"); err == nil || raw != nil || tx.reads != 0 {
		t.Fatal("HTTP owner invented no-effect proof")
	}
}

func TestHTTPRecoveryProofBindsOriginalEffectNodeVisitVersionAndWire(t *testing.T) {
	execution, generation, journal, original := httpProofFixture(t)
	for _, mutate := range []func(*httpRecoveryRecord, *recovery.Receipt){
		func(r *httpRecoveryRecord, _ *recovery.Receipt) {
			r.request = []byte(strings.Replace(string(r.request), `"n":1`, `"n":1.0`, 1))
		},
		func(r *httpRecoveryRecord, _ *recovery.Receipt) { r.bindingDigest = strings.Repeat("a", 64) },
		func(r *httpRecoveryRecord, _ *recovery.Receipt) {
			r.receiptWire = append([]byte(" "), r.receiptWire...)
		},
		func(r *httpRecoveryRecord, _ *recovery.Receipt) {
			var input runtimev1.AgentExecutionInputV1
			_ = proto.Unmarshal(r.input, &input)
			var appFields map[string]json.RawMessage
			_ = json.Unmarshal(input.Application, &appFields)
			appFields["version_id"] = json.RawMessage(`42`)
			input.Application, _ = json.Marshal(appFields)
			r.input, _ = proto.Marshal(&input)
		},
		func(_ *httpRecoveryRecord, j *recovery.Receipt) { j.Step++ },
		func(_ *httpRecoveryRecord, j *recovery.Receipt) { j.NodeID = "other" },
		func(_ *httpRecoveryRecord, j *recovery.Receipt) { j.GraphThread += "/child" },
		func(r *httpRecoveryRecord, _ *recovery.Receipt) {
			var receipt app.Receipt
			_ = json.Unmarshal(r.receiptWire, &receipt)
			receipt.Result.Data.Kind = "text"
			r.receiptWire, _ = json.Marshal(receipt)
			sha := app.Digest(r.receiptWire)
			r.receiptSHA = &sha
		},
	} {
		record := original
		changed := journal
		mutate(&record, &changed)
		tx := &httpProofTX{record: record}
		if raw, err := NewHTTPActionRecoveryProofProvider().VerifyNodeRecoveryEffect(t.Context(), tx, execution, generation, changed, "resume_result"); err == nil || raw != nil {
			t.Fatal("tampered owning relation issued proof")
		}
	}
}

func TestHTTPRecoveryProofRedemptionRequiresByteIdenticalAuthorizedProof(t *testing.T) {
	execution, generation, journal, record := httpProofFixture(t)
	owner := NewHTTPActionRecoveryProofProvider()
	tx := &httpProofTX{record: record}
	raw, err := owner.VerifyNodeRecoveryEffect(t.Context(), tx, execution, generation, journal, "resume_result")
	if err != nil {
		t.Fatal(err)
	}
	for _, changed := range [][]byte{append([]byte(" "), raw...), []byte(strings.Replace(string(raw), `"expected_revision":4`, `"expected_revision":5`, 1))} {
		if _, err := owner.ReadNodeRecoveryHTTPResult(t.Context(), tx, execution, generation, journal, changed); err == nil {
			t.Fatal("unauthorized proof replacement redeemed result")
		}
	}
}
