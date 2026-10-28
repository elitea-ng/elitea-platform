package canary

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureCanary is the value deliberately leaked into testdata/leaked. It is
// test material only: no stack ever seeds it.
var fixtureCanary = Canary{Label: "selftest", Value: "gvcanary-selftest-5f0c1d2e3a4b5c6d7e8f901234567890"}

// TestNegativeSelfTestLeakedFixtureFails is the negative self-test of
// #1124 item 3: a canary deliberately leaked into a fixture, in each encoding
// a real leak is written in, must be found, so a scan that reports zero hits
// on the stack's evidence means the evidence is clean, not that the scanner
// is blind.
func TestNegativeSelfTestLeakedFixtureFails(t *testing.T) {
	scanner, err := NewScanner([]Canary{fixtureCanary})
	if err != nil {
		t.Fatal(err)
	}
	report, err := scanner.ScanDir(filepath.Join("testdata", "leaked"), nil)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, hit := range report.Hits {
		found[hit.File] = hit.Encoding
	}
	for file, encoding := range map[string]string{
		"worker.log":         "raw",
		"pg-dump.sql":        "hex",
		"nats-tap.bin":       "raw",
		"audit-outbox.jsonl": "base64",
	} {
		got, ok := found[file]
		if !ok {
			t.Errorf("the leak in %s was not found (hits %v)", file, report.Hits)
			continue
		}
		if !strings.HasPrefix(got, encoding) {
			t.Errorf("%s: found as %s, want %s", file, got, encoding)
		}
	}
	if report.Files != 4 {
		t.Errorf("scanned %d files, want 4", report.Files)
	}
}

func TestCleanFixturePasses(t *testing.T) {
	scanner, err := NewScanner([]Canary{fixtureCanary})
	if err != nil {
		t.Fatal(err)
	}
	report, err := scanner.ScanDir(filepath.Join("testdata", "clean"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Hits) != 0 || report.Files == 0 {
		t.Fatalf("clean fixture: %d files, hits %v", report.Files, report.Hits)
	}
}

func TestEveryEncodingAndAlignmentIsFound(t *testing.T) {
	c, err := New("alignment")
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := NewScanner([]Canary{c})
	if err != nil {
		t.Fatal(err)
	}
	for prefix := 0; prefix < 6; prefix++ {
		for suffix := 0; suffix < 4; suffix++ {
			payload := append(append(bytes.Repeat([]byte{0x7a}, prefix), c.Value...), bytes.Repeat([]byte{0x01}, suffix)...)
			for name, encoded := range map[string]string{
				"base64":    base64.StdEncoding.EncodeToString(payload),
				"base64url": base64.RawURLEncoding.EncodeToString(payload),
				"hex":       `\x` + hex.EncodeToString(payload),
				"json":      mustJSON(t, map[string]string{"token": string(payload)}),
			} {
				hits, _, err := scanner.ScanReader(name, strings.NewReader("noise "+encoded+" noise"))
				if err != nil {
					t.Fatal(err)
				}
				if len(hits) == 0 {
					t.Errorf("%s with %d leading and %d trailing bytes: not found", name, prefix, suffix)
				}
			}
		}
	}
}

func TestChunkBoundaryIsCovered(t *testing.T) {
	c, err := New("boundary")
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := NewScanner([]Canary{c})
	if err != nil {
		t.Fatal(err)
	}
	// The value straddles the 1 MiB read boundary.
	payload := append(bytes.Repeat([]byte{'.'}, chunkSize-10), c.Value...)
	hits, n, err := scanner.ScanReader("big", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Offset != int64(chunkSize-10) || n != int64(len(payload)) {
		t.Fatalf("hits %v, read %d", hits, n)
	}
}

func TestHitNeverCarriesTheValue(t *testing.T) {
	c, err := New("report")
	if err != nil {
		t.Fatal(err)
	}
	scanner, err := NewScanner([]Canary{c})
	if err != nil {
		t.Fatal(err)
	}
	hits, _, err := scanner.ScanReader("f", strings.NewReader(c.Value))
	if err != nil || len(hits) != 1 {
		t.Fatalf("hits %v, %v", hits, err)
	}
	if raw := mustJSON(t, hits) + hits[0].String(); strings.Contains(raw, c.Value) {
		t.Fatal("a hit report contains the canary value")
	}
}

func TestScannerRefusesNothingToLookFor(t *testing.T) {
	if _, err := NewScanner(nil); err == nil {
		t.Fatal("an empty canary set must be refused: it would find nothing and read as clean")
	}
	if _, err := NewScanner([]Canary{{Label: "short", Value: "abc"}}); err == nil {
		t.Fatal("a short canary must be refused")
	}
	c, _ := New("dup")
	if _, err := NewScanner([]Canary{c, c}); err == nil {
		t.Fatal("a duplicate canary must be refused")
	}
	if _, err := New("Bad Label"); err == nil {
		t.Fatal("a label outside the pattern must be refused")
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestFixturesAreWhatTheSelfTestSays guards the committed fixture: if its
// leak were edited away, the negative self-test would still need to fail.
func TestFixturesAreWhatTheSelfTestSays(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "leaked", "worker.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), fixtureCanary.Value) {
		t.Fatal("testdata/leaked/worker.log no longer carries the deliberate leak")
	}
}
