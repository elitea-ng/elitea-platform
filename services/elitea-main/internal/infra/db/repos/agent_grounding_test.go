package repos

import (
	"encoding/json"
	"testing"
)

func TestGroundingFromMetadataKeepsAClosedShape(t *testing.T) {
	metadata := json.RawMessage(`{"thread_id":"t","grounding":{"version":1,"sources_available":3,` +
		`"sources_cited":2,"claim_spans":4,"uncited_spans":1,"uncited":[{"start":10,"end":20}],` +
		`"invalid_markers":0,"confidence":0.9}}`)
	got := groundingFromMetadata(metadata)
	want := `{"version":1,"sources_available":3,"sources_cited":2,"claim_spans":4,"uncited_spans":1,` +
		`"uncited":[{"start":10,"end":20}],"invalid_markers":0}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestGroundingFromMetadataDropsAbsentAndMalformed(t *testing.T) {
	for name, metadata := range map[string]string{
		"absent":             `{"thread_id":"t"}`,
		"null":               `{"grounding":null}`,
		"not an object":      `{"grounding":"yes"}`,
		"wrong version":      `{"grounding":{"version":2,"sources_available":1,"uncited":[]}}`,
		"no source":          `{"grounding":{"version":1,"sources_available":0,"uncited":[]}}`,
		"cited above total":  `{"grounding":{"version":1,"sources_available":1,"sources_cited":2,"uncited":[]}}`,
		"uncited above all":  `{"grounding":{"version":1,"sources_available":1,"claim_spans":1,"uncited_spans":2,"uncited":[]}}`,
		"negative":           `{"grounding":{"version":1,"sources_available":1,"invalid_markers":-1,"uncited":[]}}`,
		"empty span":         `{"grounding":{"version":1,"sources_available":1,"claim_spans":1,"uncited_spans":1,"uncited":[{"start":4,"end":4}]}}`,
		"spans out of order": `{"grounding":{"version":1,"sources_available":1,"claim_spans":2,"uncited_spans":2,"uncited":[{"start":9,"end":12},{"start":1,"end":3}]}}`,
		"more spans than count": `{"grounding":{"version":1,"sources_available":1,"claim_spans":1,"uncited_spans":0,` +
			`"uncited":[{"start":1,"end":3}]}}`,
		"metadata not JSON": `{`,
	} {
		if got := groundingFromMetadata(json.RawMessage(metadata)); got != nil {
			t.Errorf("%s: got %s, want nil", name, got)
		}
	}
}

func TestGroundingWithoutSpansStoresAnEmptyList(t *testing.T) {
	got := sanitizeGrounding(json.RawMessage(`{"version":1,"sources_available":2,"sources_cited":2,"claim_spans":3,"uncited_spans":0}`))
	want := `{"version":1,"sources_available":2,"sources_cited":2,"claim_spans":3,"uncited_spans":0,"uncited":[],"invalid_markers":0}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
