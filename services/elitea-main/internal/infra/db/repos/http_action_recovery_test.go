package repos

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	values := []any{r.state, r.activation, r.effect, r.requestDigest, r.bindingDigest, r.policyDigest, r.outputBucket, r.request, r.receiptWire, r.receiptSHA, r.input, r.tenant, r.project, r.projection, r.inputs, r.ruleWire, r.inputsDigest, r.attempt}
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

const httpProofPipeline = `entry_point: fetch
nodes:
  - id: fetch
    type: http
    revision: 2
    operation: tracker.create_item
    request:
      path: {item: {from: item_key}}
      body: {kind: json, value: {a: 2, n: 1}}
    response: {mode: json}
    output: [report]
    transition: END
`

// httpProofFixture is a committed revision-2 effect built through the real
// freeze, render, effect-id and projection contracts.
func httpProofFixture(t *testing.T) (string, uint64, recovery.Receipt, httpRecoveryRecord) {
	t.Helper()
	// Main runtime IDs are hex encodings of 16 random bytes, not UUIDs.
	execution, generation := "0123456789abcdef0123456789abcdef", uint64(2)
	snapshot, err := app.FreezeSnapshot(31, 41, httpProofPipeline)
	if err != nil || snapshot == nil {
		t.Fatal(err)
	}
	application, _ := json.Marshal(map[string]any{"id": 31, "version_id": 41, "version_details": map[string]any{"instructions": httpProofPipeline, "http_action_snapshot": snapshot}})
	input, err := proto.Marshal(&runtimev1.AgentExecutionInputV1{ThreadId: proto.String("root-thread"), Application: application})
	if err != nil {
		t.Fatal(err)
	}
	rule := app.Rule{ID: 5, ProjectID: 7, Revision: 3, Name: "tracker.create_item", Origin: "https://api.example.com", PathTemplate: "/items/{item}", Method: "POST", ConfigurationID: 9, Toolkit: "tracker", Tool: "create_item", OutputBucket: "outputs", Enabled: true}
	rule.Normalize()
	inputs := []byte(`{"path":{"item":"i-1"}}`)
	thread := app.RootGraphThread("tenant", 7, 7, "root-thread")
	node := snapshot.Nodes[0]
	inv := app.Invocation{SchemaVersion: app.Schema, NodeID: "fetch", ThreadID: thread, Step: 3, Attempt: 1, BindingDigest: node.BindingDigest, InputsB64: base64.StdEncoding.EncodeToString(inputs), InputsDigest: app.Digest(inputs)}
	inv.ActivationID = app.VisitID(thread, "fetch", 3, app.BindingBytes(node.BindingDigest))
	inv, err = app.ParseInvocation(inv)
	if err != nil {
		t.Fatal(err)
	}
	template, err := snapshot.Verify(31, 41, httpProofPipeline, inv)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := app.Render(rule, template, inputs)
	if err != nil {
		t.Fatal(err)
	}
	effect := app.EffectID(execution, generation, inv)
	projection, failure := app.Project(rendered.Request, 200, "application/json", []byte(`null`), nil)
	if failure != "" {
		t.Fatal(failure)
	}
	receipt := app.Receipt{SchemaVersion: app.ReceiptSchema, ActivationID: inv.ActivationID, RequestDigest: rendered.Digest, BindingDigest: node.BindingDigest, EffectID: effect, State: "completed", Result: projection}
	wire, _ := json.Marshal(receipt)
	sha := app.Digest(wire)
	inputsDigest := app.Digest(inputs)
	record := httpRecoveryRecord{state: "completed", activation: inv.ActivationID, effect: effect, requestDigest: rendered.Digest, bindingDigest: node.BindingDigest, policyDigest: rule.PolicyDigest(), outputBucket: "outputs", request: rendered.Wire, receiptWire: wire, receiptSHA: &sha, input: input, tenant: "tenant", project: 7, projection: 7, inputs: inputs, ruleWire: rule.PolicyWire(), inputsDigest: &inputsDigest, attempt: 1}
	journal := recovery.Receipt{Schema: recovery.Schema, ActivationID: strings.Repeat("c", 64), JournalRevision: 4, NodeID: "fetch", GraphThread: thread, Step: 3, Attempt: 1, FailureClass: "invalid_result", StopReason: "effect_reconciliation_required", ReplaySafety: recovery.ReplaySafety{Kind: "completed_external_effect", ReceiptID: effect}, AllowedActions: []string{"resume_result"}}
	if journal.Validate() != nil {
		t.Fatal("journal fixture invalid")
	}
	return execution, generation, journal, record
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
		// The stored inputs, the stored rule and the attempt all re-render.
		func(r *httpRecoveryRecord, _ *recovery.Receipt) { r.inputs = []byte(`{"path":{"item":"i-2"}}`) },
		func(r *httpRecoveryRecord, _ *recovery.Receipt) {
			r.ruleWire = []byte(strings.Replace(string(r.ruleWire), `"origin":"https://api.example.com"`, `"origin":"https://evil.example.com"`, 1))
		},
		func(r *httpRecoveryRecord, _ *recovery.Receipt) { r.policyDigest = strings.Repeat("e", 64) },
		func(r *httpRecoveryRecord, _ *recovery.Receipt) { r.attempt = 0 },
		func(r *httpRecoveryRecord, _ *recovery.Receipt) { r.inputsDigest = nil },
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
