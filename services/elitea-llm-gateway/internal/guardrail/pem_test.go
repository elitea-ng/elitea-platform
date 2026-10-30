package guardrail

import (
	"context"
	"strings"
	"testing"
)

// testPEM builds a PEM-shaped private key at run time, so this file holds no
// credential-shaped literal. The body is 30 lines of 64 base64 characters,
// longer than the hold of a 64-byte window (16 × 64 bytes).
func testPEM(withEnd bool) string {
	var b strings.Builder
	b.WriteString("-----BEGIN RSA " + "PRIVATE KEY-----\n")
	for i := range 30 {
		line := strings.Repeat(string(rune('A'+i%26)), 60) + "BODY"
		b.WriteString(line + "\n")
	}
	if withEnd {
		b.WriteString("-----END RSA " + "PRIVATE KEY-----")
	}
	return b.String()
}

func pemRow(t *testing.T, window float64) *Row {
	t.Helper()
	return mustRow(t, map[string]any{"guardrail": map[string]any{
		"stage": "output", "stream_window_bytes": window,
		"detectors": []any{map[string]any{"type": "secrets", "action": "mask"}},
	}})
}

// TestCheckMasksAWholePEMBlock is the regression for a private-key rule that
// matched only the BEGIN line: the mask left the base64 body and the END line.
func TestCheckMasksAWholePEMBlock(t *testing.T) {
	p := &Pipeline{Stage: StageInput, Rows: []*Row{pemRow(t, 64)}}
	out, blocked := p.Check(context.Background(), "key:\n"+testPEM(true)+"\nafter the key")
	if blocked != nil {
		t.Fatal(blocked)
	}
	if want := "key:\n" + MaskMarker("private-key") + "\nafter the key"; out != want {
		t.Fatalf("masked = %q, want %q", out, want)
	}
	// No END line: the mask runs through the end of the text.
	out, _ = p.Check(context.Background(), "key:\n"+testPEM(false)[:500])
	if want := "key:\n" + MaskMarker("private-key"); out != want {
		t.Fatalf("masked = %q, want %q", out, want)
	}
}

// TestCheckAllKeepsAnOpenPEMBlockInItsPart: a PEM block without END masks
// through the end of its own message only, not the messages after it.
func TestCheckAllKeepsAnOpenPEMBlockInItsPart(t *testing.T) {
	p := &Pipeline{Stage: StageInput, Rows: []*Row{pemRow(t, 64)}}
	out, blocked := p.CheckAll(context.Background(), []string{"first", "x " + testPEM(false), "next message " + testToken})
	if blocked != nil {
		t.Fatal(blocked)
	}
	want := []string{"first", "x " + MaskMarker("private-key"), "next message " + MaskMarker("github-token")}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("part %d = %q, want %q", i, out[i], want[i])
		}
	}
}

// TestStreamMasksAWholePEMBlock streams a PEM block that is longer than the
// hold, at several chunk sizes and split points. No byte of the body or the
// END line reaches the client, and the result equals the non-streamed mask.
func TestStreamMasksAWholePEMBlock(t *testing.T) {
	for _, withEnd := range []bool{true, false} {
		row := pemRow(t, 64)
		answer := strings.Repeat("intro text ", 12) + testPEM(withEnd)
		if withEnd {
			answer += "\nthe text after the key " + strings.Repeat("goes on ", 20)
		}
		ref := &Pipeline{Stage: StageOutput, Rows: []*Row{row}}
		want, _ := ref.Check(context.Background(), answer)
		if strings.Contains(want, "BODY") || strings.Contains(want, "END RSA") {
			t.Fatalf("the reference mask kept the body: %q", want)
		}
		for _, size := range []int{1, 5, 64, 300} {
			for _, split := range []int{1, 130, 200, 700, 1500} {
				p := &Pipeline{Stage: StageOutput, Rows: []*Row{row}, Collector: NewCollector()}
				f := p.NewStream()
				var got strings.Builder
				chunks := append(chunk(answer[:split], size), chunk(answer[split:], size)...)
				for _, c := range chunks {
					out, blocked := f.Push(context.Background(), c)
					if blocked != nil {
						t.Fatal(blocked)
					}
					got.WriteString(out)
					if strings.Contains(got.String(), "BODY") || strings.Contains(got.String(), "END RSA") {
						t.Fatalf("end %v size %d split %d: key body released: %q", withEnd, size, split, got.String())
					}
				}
				out, _ := f.Finish(context.Background())
				got.WriteString(out)
				if got.String() != want {
					t.Fatalf("end %v size %d split %d: streamed %q, want %q", withEnd, size, split, got.String(), want)
				}
			}
		}
	}
}

// TestStreamMasksTheRestAfterAnOversizedOpenBlock: an open PEM block longer
// than maxOpenContextBytes masks the rest of the channel. No byte of it is
// released raw.
func TestStreamMasksTheRestAfterAnOversizedOpenBlock(t *testing.T) {
	row := pemRow(t, 64)
	p := &Pipeline{Stage: StageOutput, Rows: []*Row{row}, Collector: NewCollector()}
	f := p.NewStream()
	var got strings.Builder
	out, _ := f.Push(context.Background(), "-----BEGIN "+"PRIVATE KEY-----\n")
	got.WriteString(out)
	// Large deltas keep the number of detector runs small.
	body := strings.Repeat(strings.Repeat("QUJD", 16)+"\n", 64)
	for range (maxOpenContextBytes / len(body)) + 4 {
		out, blocked := f.Push(context.Background(), body)
		if blocked != nil {
			t.Fatal(blocked)
		}
		got.WriteString(out)
	}
	out, _ = f.Push(context.Background(), "-----END "+"PRIVATE KEY-----\nplain words")
	got.WriteString(out)
	out, _ = f.Finish(context.Background())
	got.WriteString(out)
	if strings.Contains(got.String(), "QUJD") || strings.Contains(got.String(), "plain") {
		t.Fatalf("text after an oversized open block was released raw (%d bytes)", got.Len())
	}
	if !strings.HasPrefix(got.String(), MaskMarker("private-key")) {
		t.Fatalf("released = %.80q", got.String())
	}
}
