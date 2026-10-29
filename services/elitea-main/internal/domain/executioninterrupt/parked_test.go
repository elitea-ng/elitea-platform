package executioninterrupt

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParkedFixtures(t *testing.T) {
	valid, invalid := fixtures(t, "fanout-parked")
	for name, raw := range valid {
		parked, err := ParseParked(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !bytes.Equal(parked.Canonical, raw) {
			t.Errorf("%s: canonical bytes differ from the canonical fixture", name)
		}
		if len(parked.Pending) == 0 || !ValidDigest(parked.Pending[0].InterruptKey) || parked.Pending[0].InterruptID == "" {
			t.Errorf("%s: decoded payload is incomplete: %+v", name, parked)
		}
	}
	for name, raw := range invalid {
		if _, err := ParseParked(raw); !errors.Is(err, ErrInvalidParked) {
			t.Errorf("%s: err=%v, want ErrInvalidParked", name, err)
		}
	}
}

func TestParseParkedRefusesMalformedPayloads(t *testing.T) {
	key := strings.Repeat("ab", 32)
	entry := func(id string) string { return `{"interrupt_id":"` + id + `","interrupt_key":"` + key + `"}` }
	seventeen := make([]string, 17)
	for index := range seventeen {
		seventeen[index] = `{"interrupt_id":"ask:` + strings.Repeat("x", index+1) + `","interrupt_key":"` +
			strings.Repeat(string(rune('a'+index%6)), 64) + `"}`
	}
	cases := map[string]string{
		"unknown member":        `{"decision_revision_seen":1,"extra":1,"pending":[` + entry("a") + `],"schema":"` + ParkedSchema + `"}`,
		"wrong schema":          `{"decision_revision_seen":1,"pending":[` + entry("a") + `],"schema":"elitea.pipeline.fanout-parked.v2"}`,
		"negative revision":     `{"decision_revision_seen":-1,"pending":[` + entry("a") + `],"schema":"` + ParkedSchema + `"}`,
		"fractional revision":   `{"decision_revision_seen":1.5,"pending":[` + entry("a") + `],"schema":"` + ParkedSchema + `"}`,
		"same key, two ids":     `{"decision_revision_seen":1,"pending":[` + entry("a") + `,` + entry("b") + `],"schema":"` + ParkedSchema + `"}`,
		"seventeen pending":     `{"decision_revision_seen":1,"pending":[` + strings.Join(seventeen, ",") + `],"schema":"` + ParkedSchema + `"}`,
		"missing pending":       `{"decision_revision_seen":1,"schema":"` + ParkedSchema + `"}`,
		"not an object":         `[]`,
		"trailing value":        `{"decision_revision_seen":1,"pending":[` + entry("a") + `],"schema":"` + ParkedSchema + `"} {}`,
		"duplicate member name": `{"decision_revision_seen":1,"decision_revision_seen":2,"pending":[` + entry("a") + `],"schema":"` + ParkedSchema + `"}`,
		"oversized":             `{"decision_revision_seen":1,"pending":[` + entry(strings.Repeat("x", maxRawParkedBytes)) + `],"schema":"` + ParkedSchema + `"}`,
	}
	for name, raw := range cases {
		if _, err := ParseParked([]byte(raw)); !errors.Is(err, ErrInvalidParked) {
			t.Errorf("%s: err=%v, want ErrInvalidParked", name, err)
		}
	}
}
