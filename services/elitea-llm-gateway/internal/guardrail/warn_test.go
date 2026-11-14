package guardrail

import (
	"context"
	"strings"
	"testing"
)

func warnRow(t *testing.T, stage string) *Row {
	t.Helper()
	row, err := ParseRow("heads-up", map[string]any{"guardrail": map[string]any{
		"stage": stage, "detectors": []any{map[string]any{"type": "personal_data", "action": "warn", "rules": []any{"email"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// TestWarnLeavesContentAndRecordsWarned: a warn detector changes nothing, blocks
// nothing, and its verdict has the outcome `warned`, which the chat turns into
// a notice. A flag detector with the same match stays `flagged`.
func TestWarnLeavesContentAndRecordsWarned(t *testing.T) {
	col := NewCollector()
	p := &Pipeline{Stage: StageInput, Rows: []*Row{warnRow(t, StageInput)}, Collector: col}
	text := "mail jane@acme.io please"
	out, blocked := p.Check(context.Background(), text)
	if blocked != nil || out != text {
		t.Fatalf("out = %q, blocked = %+v", out, blocked)
	}
	v := col.Verdicts()
	if len(v) != 1 || v[0].Outcome != OutcomeWarned || v[0].Action != ActionWarn || v[0].Row != "heads-up" {
		t.Fatalf("verdicts = %+v", v)
	}
	if strings.Contains(strings.Join(v[0].Labels, " "), "acme") {
		t.Fatal("a label carries content")
	}
	if p.Holds() {
		t.Fatal("a warn-only pipeline must not hold streamed output back")
	}
}

func TestWarnPassesWhenNothingMatches(t *testing.T) {
	col := NewCollector()
	p := &Pipeline{Stage: StageInput, Rows: []*Row{warnRow(t, StageInput)}, Collector: col}
	if _, blocked := p.Check(context.Background(), "nothing here"); blocked != nil {
		t.Fatal("blocked")
	}
	if v := col.Verdicts(); len(v) != 1 || v[0].Outcome != OutcomePass {
		t.Fatalf("verdicts = %+v", v)
	}
}

// TestWarnStreamsWithoutBuffering: on a streamed answer a warn detector reads
// the whole text at the end, as a flag detector does, and releases every delta
// at once.
func TestWarnStreamsWithoutBuffering(t *testing.T) {
	col := NewCollector()
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{warnRow(t, StageOutput)}, Collector: col}
	f := p.NewStream()
	if f.Buffers() {
		t.Fatal("warn must not buffer")
	}
	got := ""
	for _, d := range []string{"write to jane@ac", "me.io now"} {
		out, blocked := f.Push(context.Background(), d)
		if blocked != nil {
			t.Fatal("blocked")
		}
		got += out
	}
	tail, blocked := f.Finish(context.Background())
	if blocked != nil {
		t.Fatal("blocked")
	}
	if got+tail != "write to jane@acme.io now" {
		t.Fatalf("released %q", got+tail)
	}
	if v := col.Verdicts(); len(v) != 1 || v[0].Outcome != OutcomeWarned {
		t.Fatalf("verdicts = %+v", v)
	}
}

func TestWarnOnlyRowFailsOpen(t *testing.T) {
	if got := warnRow(t, StageInput).FailMode; got != FailOpen {
		t.Fatalf("fail mode = %q: a warn row changes nothing, so a failure must not refuse", got)
	}
}

func TestStrongestActionRanksWarnAboveFlag(t *testing.T) {
	row := &Row{Detectors: []*Detector{{Action: ActionFlag}, {Action: ActionWarn}}}
	if got := strongestAction(row); got != ActionWarn {
		t.Fatalf("got %q", got)
	}
	row.Detectors = append(row.Detectors, &Detector{Action: ActionMask})
	if got := strongestAction(row); got != ActionMask {
		t.Fatalf("got %q", got)
	}
}
