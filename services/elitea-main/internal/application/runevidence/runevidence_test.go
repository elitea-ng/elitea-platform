package runevidence

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func chained(t *testing.T, records ...Record) []Record {
	t.Helper()
	prev := ZeroHash
	for i := range records {
		records[i].Seq, records[i].PrevHash = int64(i+1), prev
		hash, err := ChainHash(prev, records[i])
		if err != nil {
			t.Fatal(err)
		}
		records[i].Hash, prev = hash, hash
	}
	return records
}

func sample(kind Kind) Record {
	at := time.Date(2026, 10, 28, 9, 0, 0, 123456000, time.UTC)
	return Record{
		RunID: "run-1", StepID: "0192f0c1-7b6e-7c3a-8f00-0000000000a1", Generation: 1, ProjectID: 3, Kind: kind,
		Actor: "user:8", StartedAt: at, EndedAt: at.Add(time.Second), Summary: []byte(`{"b":1e2,"a":[1,2.50]}`),
		Inputs: ContentMeta{Digest: bytes.Repeat([]byte{1}, 32), Size: 4, OriginalSize: 4, MediaType: MediaJSON, ObjectKey: "k"},
	}
}

func TestCanonicalSurvivesTheJSONBRoundTrip(t *testing.T) {
	a := sample(KindModelCall)
	b := a
	b.Summary = []byte(`{"a": [1, 2.5], "b": 100}`) // jsonb's rewrite of the same value
	ca, err := Canonical(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, _ := Canonical(b)
	if !bytes.Equal(ca, cb) {
		t.Fatalf("canonical differs:\n%s\n%s", ca, cb)
	}
	// Expiring the content (object key gone) leaves the hash as it was.
	c := a
	c.Inputs.ObjectKey = ""
	if cc, _ := Canonical(c); !bytes.Equal(ca, cc) {
		t.Fatal("the object key is chained")
	}
	if bytes.Contains(ca, []byte("ObjectKey")) || bytes.Contains(ca, []byte(`"k"`)) {
		t.Fatalf("canonical carries the object key: %s", ca)
	}
}

func TestVerifyChainNamesTheFirstBreak(t *testing.T) {
	records := chained(t, sample(KindModelCall), sample(KindToolCall), sample(KindOutput))
	head := records[2].Hash
	if b := VerifyChain(records, 3, head); b != nil {
		t.Fatalf("intact chain broke: %+v", b)
	}
	tampered := append([]Record(nil), records...)
	tampered[1].Name = "other"
	if b := VerifyChain(tampered, 3, head); b == nil || b.Seq != 2 {
		t.Fatalf("tampered seq 2: %+v", b)
	}
	if b := VerifyChain([]Record{records[0], records[2]}, 3, head); b == nil || b.Seq != 2 {
		t.Fatalf("missing seq 2: %+v", b)
	}
	if b := VerifyChain(records[:2], 3, head); b == nil || b.Seq != 3 {
		t.Fatalf("truncated run: %+v", b)
	}
	if b := VerifyChain(records, 3, ZeroHash); b == nil || b.Seq != 3 {
		t.Fatalf("head hash mismatch: %+v", b)
	}
}

func TestFilterFields(t *testing.T) {
	kept := FilterFields(&ContentInput{Bytes: []byte(`{"id":1,"secret":"x","status":"ok"}`), MediaType: MediaJSON}, []string{"id", "status", "absent"})
	if string(kept.Bytes) != `{"id":1,"status":"ok"}` || !kept.Filtered || kept.OriginalSize != 35 {
		t.Fatalf("kept = %+v %s", kept, kept.Bytes)
	}
	for name, in := range map[string]*ContentInput{
		"text":  {Bytes: []byte("secret"), MediaType: MediaText},
		"array": {Bytes: []byte(`[{"id":1}]`), MediaType: MediaJSON},
		"cut":   {Bytes: []byte(`{"id":1,"sec`), MediaType: MediaJSON, Truncated: true},
	} {
		if out := FilterFields(in, []string{"id"}); out.Bytes != nil || !out.Filtered {
			t.Fatalf("%s: stored %s", name, out.Bytes)
		}
	}
}

type fakeStore struct {
	fields   map[string][]string
	stored   map[string]bool
	appended []Record
	events   *[]string
}

func (f *fakeStore) log(event string) {
	if f.events != nil {
		*f.events = append(*f.events, event)
	}
}

func (f *fakeStore) RunContext(context.Context, string, uint64) (RunContext, error) {
	return RunContext{RunID: "run-1", Generation: 1, ProjectID: 3, Actor: "user:8"}, nil
}

func (f *fakeStore) EvidenceFields(_ context.Context, _ int64, toolkit string) ([]string, bool, error) {
	fields, ok := f.fields[toolkit]
	return fields, ok, nil
}

func (f *fakeStore) StoredStepIDs(context.Context, string, []string) (map[string]bool, error) {
	return f.stored, nil
}

func (f *fakeStore) Append(_ context.Context, _ RunContext, records []Record) (int, error) {
	f.log("append")
	f.appended = append(f.appended, records...)
	return len(records), nil
}

func (f *fakeStore) JournalContent(_ context.Context, _ RunContext, entries []PendingContent) error {
	for _, e := range entries {
		f.log("journal " + e.Key)
	}
	return nil
}

func (f *fakeStore) RequestAnchor(context.Context, string) error { return nil }

type memContent map[string][]byte

func (m memContent) Put(_ context.Context, key string, body []byte, _ string, _ []byte) error {
	m[key] = body
	return nil
}

// orderedContent logs each Put into the store's event list.
type orderedContent struct {
	memContent
	events *[]string
}

func (o orderedContent) Put(ctx context.Context, key string, body []byte, media string, digest []byte) error {
	*o.events = append(*o.events, "put "+key)
	return o.memContent.Put(ctx, key, body, media, digest)
}
func (m memContent) Get(_ context.Context, key string) ([]byte, error) { return m[key], nil }
func (m memContent) Delete(context.Context, []string) error            { return nil }

func step(id string, kind Kind) StepInput {
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	return StepInput{StepID: id, Kind: kind, StartedAt: at, EndedAt: at}
}

func TestReportStoresContentByDigestAndFiltersToolResults(t *testing.T) {
	store := &fakeStore{fields: map[string][]string{"17": {"id"}}, stored: map[string]bool{"0192f0c1-7b6e-7c3a-8f00-0000000000a3": true}}
	content := memContent{}
	service, _ := NewService(store, content, nil)
	tool := step("0192f0c1-7b6e-7c3a-8f00-0000000000a1", KindToolCall)
	tool.ToolkitID = "17"
	tool.Inputs = &ContentInput{Bytes: []byte(`{"q":"x"}`), MediaType: MediaJSON}
	tool.Outputs = &ContentInput{Bytes: []byte(`{"id":1,"secret":"y"}`), MediaType: MediaJSON}
	unfiltered := step("0192f0c1-7b6e-7c3a-8f00-0000000000a2", KindToolCall)
	unfiltered.ToolkitID = "18"
	unfiltered.Outputs = &ContentInput{Bytes: []byte(`{"secret":"z"}`), MediaType: MediaJSON}
	already := step("0192f0c1-7b6e-7c3a-8f00-0000000000a3", KindOutput)

	n, err := service.Report(context.Background(), "run-1", 1, []StepInput{tool, unfiltered, tool, already})
	if err != nil || n != 2 || len(store.appended) != 2 {
		t.Fatalf("n=%d err=%v appended=%d", n, err, len(store.appended))
	}
	first := store.appended[0]
	if !first.Outputs.Filtered || string(content[first.Outputs.ObjectKey]) != `{"id":1}` {
		t.Fatalf("filtered outputs = %+v %s", first.Outputs, content[first.Outputs.ObjectKey])
	}
	if first.Inputs.Filtered || first.Inputs.ObjectKey != "3/runs/run-1/0192f0c1-7b6e-7c3a-8f00-0000000000a1/inputs" {
		t.Fatalf("inputs = %+v", first.Inputs)
	}
	if store.appended[1].Outputs.Filtered || !strings.Contains(string(content[store.appended[1].Outputs.ObjectKey]), "secret") {
		t.Fatal("a toolkit without evidence_fields keeps its whole result")
	}
}

func TestReportRefusesOutOfBounds(t *testing.T) {
	service, _ := NewService(&fakeStore{}, nil, nil)
	big := step("0192f0c1-7b6e-7c3a-8f00-0000000000a1", KindToolCall)
	big.Outputs = &ContentInput{Bytes: make([]byte, MaxContentBytes+1), MediaType: MediaText}
	summary := step("0192f0c1-7b6e-7c3a-8f00-0000000000a1", KindModelCall)
	summary.SummaryJSON = `{"x":"` + strings.Repeat("a", MaxSummaryBytes) + `"}`
	docs := step("0192f0c1-7b6e-7c3a-8f00-0000000000a1", KindRetrieval)
	docs.Retrieval = &Retrieval{Documents: make([]Document, MaxDocuments+1)}
	for name, steps := range map[string][]StepInput{
		"content":   {big},
		"summary":   {summary},
		"documents": {docs},
		"kind":      {step("0192f0c1-7b6e-7c3a-8f00-0000000000a1", "thinking")},
		"step id":   {step("not-a-uuid", KindOutput)},
		"count":     make([]StepInput, MaxStepsPerReport+1),
	} {
		if _, err := service.Report(context.Background(), "run-1", 1, steps); !errors.Is(err, ErrInvalidReport) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

// Every object key is journalled (committed) before any object is written,
// and the rows are appended last: a persist that fails leaves journalled
// keys the step_content routine can find, never an untracked object.
func TestReportJournalsKeysBeforeWritingObjects(t *testing.T) {
	var events []string
	store := &fakeStore{events: &events}
	service, _ := NewService(store, orderedContent{memContent: memContent{}, events: &events}, nil)
	a := step("0192f0c1-7b6e-7c3a-8f00-0000000000a1", KindModelCall)
	a.Inputs = &ContentInput{Bytes: []byte("in"), MediaType: MediaText}
	a.Outputs = &ContentInput{Bytes: []byte("out"), MediaType: MediaText}
	b := step("0192f0c1-7b6e-7c3a-8f00-0000000000a2", KindOutput)
	b.Outputs = &ContentInput{Bytes: []byte("final"), MediaType: MediaText}
	if _, err := service.Report(context.Background(), "run-1", 1, []StepInput{a, b}); err != nil {
		t.Fatal(err)
	}
	prefix := "3/runs/run-1/"
	want := []string{
		"journal " + prefix + "0192f0c1-7b6e-7c3a-8f00-0000000000a1/inputs",
		"journal " + prefix + "0192f0c1-7b6e-7c3a-8f00-0000000000a1/outputs",
		"journal " + prefix + "0192f0c1-7b6e-7c3a-8f00-0000000000a2/outputs",
		"put " + prefix + "0192f0c1-7b6e-7c3a-8f00-0000000000a1/inputs",
		"put " + prefix + "0192f0c1-7b6e-7c3a-8f00-0000000000a1/outputs",
		"put " + prefix + "0192f0c1-7b6e-7c3a-8f00-0000000000a2/outputs",
		"append",
	}
	if strings.Join(events, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s", strings.Join(events, "\n"))
	}
}
