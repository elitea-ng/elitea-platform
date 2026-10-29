package auditexport

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

func runStepRecord(t *testing.T, mutate func(*runevidence.Record)) runevidence.Record {
	t.Helper()
	at := time.Date(2026, 10, 28, 9, 30, 0, 123456000, time.UTC)
	app, release := int64(11), int64(12)
	r := runevidence.Record{
		ID: 301, RunID: "exec-1", Seq: 2, StepID: "0192f0c1-7b6e-7c3a-8f00-0000000000a2", Generation: 1,
		ProjectID: 42, ApplicationID: &app, ReleaseID: &release, DefinitionDigest: bytes.Repeat([]byte{0xab}, 32),
		Kind: runevidence.KindToolCall, Actor: "agent:0192f0c1-7b6e-7c3a-8f00-0000000000b1",
		NodeID: "node-1", Name: "search", ToolkitID: "17", StartedAt: at, EndedAt: at.Add(time.Second),
		Summary: []byte(`{"status":"ok"}`),
		Inputs: runevidence.ContentMeta{
			Digest: bytes.Repeat([]byte{1}, 32), Size: 10, OriginalSize: 10, MediaType: runevidence.MediaJSON,
			ObjectKey: "42/runs/exec-1/0192f0c1-7b6e-7c3a-8f00-0000000000a2/inputs",
		},
		Outputs: runevidence.ContentMeta{
			Digest: bytes.Repeat([]byte{2}, 32), Size: 5, OriginalSize: 900, Truncated: true, Filtered: true,
			MediaType: runevidence.MediaJSON,
		},
		PrevHash: bytes.Repeat([]byte{3}, 32), Hash: bytes.Repeat([]byte{4}, 32),
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func TestRunStepMapperOutputConformsToSchema(t *testing.T) {
	schema := compileSchema(t)
	records := map[string]runevidence.Record{
		"tool call": runStepRecord(t, nil),
		"retrieval": runStepRecord(t, func(r *runevidence.Record) {
			r.Kind, r.Name, r.ToolkitID = runevidence.KindRetrieval, "", ""
			r.Retrieval = []byte(`{"index_name":"docs","query":"q","filters":{},"top_k":3,"documents":[{"document_id":"d1","source":"s","score":0.91,"rank":1}]}`)
			r.CitedDocumentIDs = []string{"d1"}
		}),
		"error by a user, no release": runStepRecord(t, func(r *runevidence.Record) {
			r.Kind, r.Actor, r.ApplicationID, r.ReleaseID, r.DefinitionDigest = runevidence.KindError, "user:7", nil, nil, nil
			r.Inputs, r.Outputs = runevidence.ContentMeta{}, runevidence.ContentMeta{}
			r.Summary = nil
		}),
		"first step": runStepRecord(t, func(r *runevidence.Record) { r.Seq, r.PrevHash = 1, runevidence.ZeroHash[:] }),
	}
	for name, record := range records {
		t.Run(name, func(t *testing.T) {
			if err := validate(t, schema, FromRunStep(record)); err != nil {
				t.Fatalf("run step does not conform to elitea.audit.v1: %v", err)
			}
		})
	}
}

func TestRunStepMapperCarriesNoContent(t *testing.T) {
	event := FromRunStep(runStepRecord(t, nil))
	if event.Source != SourceRunStep || event.EventType != "run_step.tool_call" || event.Ref() != "run_step:301" {
		t.Fatalf("identity: %+v", event)
	}
	if event.Chain == nil || event.Chain.Partition != "run:exec-1" || event.Chain.Sequence != 2 {
		t.Fatalf("chain = %+v", event.Chain)
	}
	if event.Actor.Type != "agent" || event.OCSF == nil || event.OCSF.ClassUID != securityaudit.OCSFClassAPIActivity {
		t.Fatalf("actor/ocsf: %+v %+v", event.Actor, event.OCSF)
	}
	if event.EventID != RunStepEventID("exec-1", "0192f0c1-7b6e-7c3a-8f00-0000000000a2") {
		t.Fatal("event id is not stable")
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"content"`) {
		t.Fatalf("a run step event carries content: %s", raw)
	}
	details := event.Details.(RunStepDetails)
	if details.Inputs.Ref != "run-evidence/42/runs/exec-1/0192f0c1-7b6e-7c3a-8f00-0000000000a2/inputs" || details.Outputs.Ref != "" {
		t.Fatalf("refs: %+v %+v", details.Inputs, details.Outputs)
	}
}

func TestSchemaRefusesRunStepWithoutChain(t *testing.T) {
	schema := compileSchema(t)
	event := FromRunStep(runStepRecord(t, nil))
	event.Chain = nil
	if err := validate(t, schema, event); err == nil {
		t.Fatal("schema accepted a run_step event without its chain")
	}
}
