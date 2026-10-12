package run

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestJSONStringLenMatchesMarshal: the import's size check counts the
// escaped document by scanning, and must equal what encoding/json writes.
func TestJSONStringLenMatchesMarshal(t *testing.T) {
	corpus := []string{
		"",
		"plain ascii graph",
		`quote " backslash \ slash /`,
		"\b\f\n\r\t\x00\x01\x1f\x7f",
		"<script>a && b</script>",
		"é ü 中文 😀 \U0010ffff \ufeff",
		"  line   paragraph",
		"\xff\xfe invalid \xc3 truncated \xe2\x82",
		"\xed\xa0\x80 surrogate half",
		"\xf4\x90\x80\x80 above U+10FFFF",
		strings.Repeat(`{"id": "a\"b", "x": [1, 2]}`+"\n", 50),
	}
	for b := 0; b < 256; b++ {
		corpus = append(corpus, string([]byte{byte(b)}), "a"+string([]byte{byte(b)})+"é")
	}
	for _, text := range corpus {
		encoded, err := json.Marshal(text)
		if err != nil {
			t.Fatalf("marshal %q: %v", text, err)
		}
		if got := jsonStringLen([]byte(text)); got != len(encoded) {
			t.Errorf("jsonStringLen(%q) = %d, json.Marshal wrote %d bytes (%s)", text, got, len(encoded), encoded)
		}
	}
}
