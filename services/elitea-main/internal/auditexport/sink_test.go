package auditexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

type recordingSink struct {
	name string
	got  []string
	err  error
}

func (r *recordingSink) Name() string { return r.name }
func (r *recordingSink) Send(_ context.Context, events []Event) error {
	if r.err != nil {
		return r.err
	}
	for _, e := range events {
		r.got = append(r.got, e.Ref())
	}
	return nil
}

func TestSecurityExporterDeadLettersAnUndecodableRecordAloneAndSendsTheRest(t *testing.T) {
	first := securityRecord(t, securityaudit.TypeGrantChanged, nil)
	first.Seq = 1
	broken := securityRecord(t, securityaudit.TypeGrantChanged, nil)
	broken.Seq, broken.EventID, broken.Event = 2, "evt-broken", []byte(`{"truncated":`)
	last := securityRecord(t, securityaudit.TypeGrantChanged, nil)
	last.Seq = 3

	sink := &recordingSink{name: "webhook"}
	store := &memoryDeadLetters{}
	metrics := NewMetrics()
	metrics.RegisterDecodeFailures()
	exporter := SecurityExporter{Sink: sink, DeadLetters: store, Metrics: metrics, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if err := exporter.Export(context.Background(), []securityaudit.Record{first, broken, last}); err != nil {
		t.Fatalf("an undecodable record must not block the stream: %v", err)
	}
	if strings.Join(sink.got, ",") != "security_audit:1,security_audit:3" {
		t.Fatalf("sent = %v; the decodable neighbours must be sent", sink.got)
	}
	if len(store.records) != 1 || store.records[0] != "decode/security_audit:2/1" {
		t.Fatalf("dead letters = %v", store.records)
	}
	if got := metrics.DeadLettered(DecodeSinkName, SourceSecurityAudit); got != 1 {
		t.Fatalf("decode dead letters metric = %d", got)
	}
	var text strings.Builder
	_ = metrics.WriteText(&text)
	if strings.Contains(text.String(), `elitea_audit_export_delivered_total{sink="decode"`) {
		t.Fatal("the decode label is a dead-letter series only")
	}

	// The dead-letter body keeps the stored bytes for the operator.
	body, err := json.Marshal(undecodableEvent(broken))
	if err != nil || !strings.Contains(string(body), `"stored_event":"{\"truncated\":"`) {
		t.Fatalf("dead-letter body = %s, %v", body, err)
	}
}

func TestSecurityExporterKeepsTheBatchWhenTheDeadLetterCannotBeRecorded(t *testing.T) {
	broken := securityRecord(t, securityaudit.TypeGrantChanged, nil)
	broken.Event = []byte(`nope`)
	sink := &recordingSink{name: "webhook"}
	exporter := SecurityExporter{Sink: sink, DeadLetters: &memoryDeadLetters{fail: true}}
	if err := exporter.Export(context.Background(), []securityaudit.Record{broken}); err == nil {
		t.Fatal("acknowledged a record whose dead letter was not recorded")
	}
	if len(sink.got) != 0 {
		t.Fatal("sent before the decision was recorded")
	}
}

func TestSecurityExporterAllUndecodableSendsNothing(t *testing.T) {
	broken := securityRecord(t, securityaudit.TypeGrantChanged, nil)
	broken.Event = []byte(`{}`)
	sink := &recordingSink{name: "webhook", err: errors.New("must not be called")}
	exporter := SecurityExporter{Sink: sink, DeadLetters: &memoryDeadLetters{}}
	if err := exporter.Export(context.Background(), []securityaudit.Record{broken}); err != nil {
		t.Fatal(err)
	}
}

func TestSettledCount(t *testing.T) {
	base := errors.New("down")
	for _, tc := range []struct {
		err  error
		n    int
		want int
	}{
		{nil, 5, 5},
		{base, 5, 0},
		{withSettled(3, base), 5, 3},
		{fmt.Errorf("wrapped: %w", withSettled(2, base)), 5, 2},
		{withSettled(9, base), 5, 5},
		{withSettled(0, base), 5, 0},
		{withSettled(2, withSettled(4, base)), 5, 2},
	} {
		if got := settledCount(tc.err, tc.n); got != tc.want {
			t.Fatalf("settledCount(%v, %d) = %d, want %d", tc.err, tc.n, got, tc.want)
		}
	}
	if !errors.Is(withSettled(2, base), base) {
		t.Fatal("a partial error must unwrap to its cause")
	}
}

func TestFanoutDoesNotReportAPartialDeliveryAsSettled(t *testing.T) {
	partial := &recordingSink{name: "a", err: withSettled(2, errors.New("down"))}
	err := Fanout{partial}.Send(context.Background(), testEvents(3, 1))
	if err == nil || settledCount(err, 3) != 0 {
		t.Fatalf("err = %v; one sink's partial delivery is no acknowledgement for the fan-out", err)
	}
}

func TestRefMemoForgetsTheOldestFirst(t *testing.T) {
	m := newRefMemo(2)
	m.add("a")
	m.add("b")
	m.add("a") // already present: no slot used
	m.add("c")
	if m.has("a") || !m.has("b") || !m.has("c") {
		t.Fatalf("memo = %v", m.set)
	}
}
