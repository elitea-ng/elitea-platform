package repos

import "testing"

func TestSameRaisedCardToleratesOnlyAnAddedArgumentDigest(t *testing.T) {
	const digest = `"argument_digest":"sha256:` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `"`
	stored := []byte(`{"interrupt_id":"i1","display":{"tool_name":"send"},"payload_sha256":"ab"}`)
	for _, test := range []struct {
		name   string
		stored []byte
		replay []byte
		same   bool
	}{
		{"identical", stored, stored, true},
		{"digest added by an upgraded worker", stored, []byte(`{"interrupt_id":"i1","display":{"tool_name":"send",` + digest + `},"payload_sha256":"ab"}`), true},
		{"digest added with another change", stored, []byte(`{"interrupt_id":"i1","display":{"tool_name":"other",` + digest + `},"payload_sha256":"ab"}`), false},
		{"digest removed", []byte(`{"interrupt_id":"i1","display":{"tool_name":"send",` + digest + `},"payload_sha256":"ab"}`), stored, false},
		{"digest changed", []byte(`{"display":{` + digest + `}}`), []byte(`{"display":{"argument_digest":"sha256:` + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + `"}}`), false},
		{"other field changed", stored, []byte(`{"interrupt_id":"i2","display":{"tool_name":"send"},"payload_sha256":"ab"}`), false},
		{"not JSON", stored, []byte(`not json`), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := sameRaisedCard(test.stored, test.replay); got != test.same {
				t.Fatalf("sameRaisedCard=%v, want %v", got, test.same)
			}
		})
	}
}
