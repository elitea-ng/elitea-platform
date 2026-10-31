package agentexecution

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
)

type preambleSourceStub struct {
	snapshot *runtimev1.PlatformPreambleSnapshotV1
	err      error
}

func (s preambleSourceStub) PlatformPreamble(context.Context) (*runtimev1.PlatformPreambleSnapshotV1, error) {
	return s.snapshot, s.err
}

func preambleSnapshot(text string) *runtimev1.PlatformPreambleSnapshotV1 {
	return &runtimev1.PlatformPreambleSnapshotV1{Revision: 2, Sha256: PlatformPreambleDigest(text), Text: text}
}

// ADR-0043 decision 3: admission freezes the newest preamble into the turn's
// input (the data plane), and does not change the caller's input.
func TestAdmissionFreezesThePlatformPreambleIntoTheInput(t *testing.T) {
	t.Parallel()
	input := &runtimev1.AgentExecutionInputV1{SchemaRevision: agentInputSchemaRevision}
	service := (&AdmissionService{}).WithPlatformPreamble(preambleSourceStub{snapshot: preambleSnapshot("Answer in English.")})
	stamped, err := service.stampPlatformPreamble(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if stamped.GetPlatformPreamble().GetText() != "Answer in English." || input.GetPlatformPreamble() != nil {
		t.Fatalf("stamped = %v, caller's input = %v", stamped.GetPlatformPreamble(), input.GetPlatformPreamble())
	}
	// No preamble: the input is unchanged byte for byte.
	none := (&AdmissionService{}).WithPlatformPreamble(preambleSourceStub{})
	if same, err := none.stampPlatformPreamble(context.Background(), input); err != nil || same != input {
		t.Fatalf("same = %v, err = %v", same, err)
	}
	// Without a source admission is as before.
	if same, err := (&AdmissionService{}).stampPlatformPreamble(context.Background(), input); err != nil || same != input {
		t.Fatalf("same = %v, err = %v", same, err)
	}
}

// A preamble that cannot be read refuses the turn: running agents without
// the organisation's rules is not a safe default.
func TestAdmissionRefusesWithoutThePlatformPreamble(t *testing.T) {
	t.Parallel()
	input := &runtimev1.AgentExecutionInputV1{SchemaRevision: agentInputSchemaRevision}
	for name, source := range map[string]preambleSourceStub{
		"read error": {err: errors.New("down")},
		"bad digest": {snapshot: &runtimev1.PlatformPreambleSnapshotV1{Revision: 1, Sha256: strings.Repeat("0", 64), Text: "x"}},
	} {
		service := (&AdmissionService{}).WithPlatformPreamble(source)
		if _, err := service.stampPlatformPreamble(context.Background(), input); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
}

func TestValidPlatformPreambleSnapshot(t *testing.T) {
	t.Parallel()
	if !ValidPlatformPreambleSnapshot(preambleSnapshot("Rules.")) {
		t.Fatal("a well-formed snapshot is refused")
	}
	for name, snapshot := range map[string]*runtimev1.PlatformPreambleSnapshotV1{
		"no revision": {Sha256: PlatformPreambleDigest("x"), Text: "x"},
		"empty":       {Revision: 1, Sha256: PlatformPreambleDigest(""), Text: ""},
		"too long":    preambleSnapshot(strings.Repeat("a", MaxPlatformPreambleBytes+1)),
		"NUL":         preambleSnapshot("a\x00b"),
		"upper hex":   {Revision: 1, Sha256: strings.ToUpper(PlatformPreambleDigest("x")), Text: "x"},
	} {
		if ValidPlatformPreambleSnapshot(snapshot) {
			t.Errorf("%s: accepted", name)
		}
	}
}
