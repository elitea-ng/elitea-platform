package guardrail

import (
	"context"
	"strings"
	"testing"
)

func personalLabels(t *testing.T, rules []any, content string) []string {
	t.Helper()
	obj := map[string]any{}
	if rules != nil {
		obj["rules"] = rules
	}
	d, err := newPersonalDataDetector(obj)
	if err != nil {
		t.Fatal(err)
	}
	found, err := d.detect(context.Background(), Env{}, StageInput, content)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range found {
		if f.Start < 0 || f.End > len(content) || f.End <= f.Start {
			t.Fatalf("bad span %+v", f)
		}
		out = append(out, f.Label+"="+content[f.Start:f.End])
	}
	return out
}

func TestPersonalDataHits(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"email", "write to jane.doe+hr@corp-mail.co.uk today", "email=jane.doe+hr@corp-mail.co.uk"},
		{"email end of sentence", "mail bob@acme.io.", "email=bob@acme.io"},
		{"phone international", "call +44 20 7946 0958 now", "phone=+44 20 7946 0958"},
		{"phone international dots", "tel +1.415.555.2671", "phone=+1.415.555.2671"},
		{"phone NANP dashes", "call 415-555-2671 please", "phone=415-555-2671"},
		{"phone NANP parens", "call (415) 555-2671 please", "phone=(415) 555-2671"},
		{"visa", "card 4111 1111 1111 1111 expires", "payment-card=4111 1111 1111 1111"},
		{"visa dashes", "card 4111-1111-1111-1111.", "payment-card=4111-1111-1111-1111"},
		{"visa bare", "4111111111111111", "payment-card=4111111111111111"},
		{"amex", "amex 3782 822463 10005 ok", "payment-card=3782 822463 10005"},
		{"mastercard", "5555555555554444", "payment-card=5555555555554444"},
		{"mastercard 2-series", "2223003122003222", "payment-card=2223003122003222"},
		{"card with trailing digits group", "4111 1111 1111 1111 12", "payment-card=4111 1111 1111 1111"},
		{"iban DE", "pay DE89 3704 0044 0532 0130 00 by friday", "iban=DE89 3704 0044 0532 0130 00"},
		{"iban GB compact", "GB82WEST12345698765432", "iban=GB82WEST12345698765432"},
		{"iban FR", "FR14 2004 1010 0505 0001 3M02 606", "iban=FR14 2004 1010 0505 0001 3M02 606"},
		{"iban followed by word", "DE89 3704 0044 0532 0130 00 ABC", "iban=DE89 3704 0044 0532 0130 00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := personalLabels(t, nil, tc.text)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("got %q, want [%q]", got, tc.want)
			}
		})
	}
}

// TestPersonalDataNearMisses pins the false positives the pack must not
// raise: each text looks like a value and is not one.
func TestPersonalDataNearMisses(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"asset name", "see icon@2x.png and logo@3x.webp"},
		{"documentation address", "mail someone@example.com"},
		{"no domain dot", "user@localhost"},
		{"at mention", "thanks @alice and @bob_smith for the review"},
		{"git ref", "pkg@1.2.3 and v2@latest"},
		{"consecutive dots", "a..b@acme.io"},
		{"ISO date", "released 2024-11-13 and 2024-01-15"},
		{"timestamp", "1731456000 1731456000123"},
		{"version", "version 10.4.2.1 build 20241113"},
		{"phone too short", "+1234567"},
		{"phone unseparated short", "+123456789"},
		{"NANP area code starts 1", "123-456-7890"},
		{"NANP area code starts 0", "012-345-6789"},
		{"phone inside longer digits", "ref 9415-555-26711"},
		{"luhn fails", "4111 1111 1111 1112"},
		{"luhn ok no issuer", "1234 5678 9012 3452"},
		{"all one digit", "4444 4444 4444 4444"},
		{"mixed separators", "4111-1111 1111-1111"},
		{"twelve digits", "411111111111"},
		{"twenty digits", "41111111111111111111"},
		{"card glued to letters", "id4111111111111111x"},
		{"iban bad check digit", "DE88 3704 0044 0532 0130 00"},
		{"iban wrong length", "DE89 3704 0044 0532 0130 0"},
		{"unknown country", "ZZ89 3704 0044 0532 0130 00"},
		{"iban lowercase", "de89 3704 0044 0532 0130 00"},
		{"iban inside a token", "XDE89370400440532013000"},
		{"plain prose", "The quick brown fox jumps over 13 lazy dogs at 5pm."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := personalLabels(t, nil, tc.text); len(got) != 0 {
				t.Fatalf("false positive: %q", got)
			}
		})
	}
}

func TestPersonalDataRuleSelection(t *testing.T) {
	text := "jane@acme.io 4111111111111111"
	if got := personalLabels(t, []any{"payment-card"}, text); len(got) != 1 || !strings.HasPrefix(got[0], "payment-card=") {
		t.Fatalf("got %q", got)
	}
	if _, err := newPersonalDataDetector(map[string]any{"rules": []any{"ssn"}}); err == nil {
		t.Fatal("an unknown rule id was accepted")
	}
}

// TestPersonalDataMaskNeverEchoesTheValue runs the detector in a pipeline: a
// masked message holds a marker, the verdict has the label and no value.
func TestPersonalDataMaskNeverEchoesTheValue(t *testing.T) {
	row, err := ParseRow("pii", map[string]any{"guardrail": map[string]any{
		"stage": "input", "detectors": []any{map[string]any{"type": "personal_data", "action": "mask"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	col := NewCollector()
	p := &Pipeline{Stage: StageInput, Rows: []*Row{row}, Collector: col}
	out, blocked := p.Check(context.Background(), "mail jane@acme.io card 4111 1111 1111 1111 done")
	if blocked != nil {
		t.Fatalf("blocked: %+v", blocked)
	}
	if want := "mail [REDACTED:email] card [REDACTED:payment-card] done"; out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	v := col.Verdicts()
	if len(v) != 1 || v[0].Outcome != OutcomeMasked || v[0].Findings != 2 {
		t.Fatalf("verdicts = %+v", v)
	}
	if strings.Contains(strings.Join(v[0].Labels, ","), "acme") {
		t.Fatal("a label carries content")
	}
}

func TestIbanAndLuhnPrimitives(t *testing.T) {
	if !ibanMod97("GB82WEST12345698765432") || ibanMod97("GB83WEST12345698765432") {
		t.Fatal("mod 97 is wrong")
	}
	if !luhnValid("79927398713") || luhnValid("79927398710") {
		t.Fatal("luhn is wrong")
	}
}
