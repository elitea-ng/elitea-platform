package nativeauth

import (
	"net/http"
	"testing"
)

// The consent POST's origin check (decisionFromThisPage). The "null" rows are
// the WebKit case: its form POST from the consent page carries Origin "null"
// and Sec-Fetch-Site "same-origin", and refusing it made the page unusable in
// Safari.
func TestDecisionFromThisPage(t *testing.T) {
	const origin = "https://elitea.example"
	cases := []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"no Origin (a non-browser client)", http.Header{}, true},
		{"this origin", http.Header{"Origin": {origin}}, true},
		{"another origin", http.Header{"Origin": {"https://evil.example"}}, false},
		{"another origin claiming same-origin", http.Header{"Origin": {"https://evil.example"}, "Sec-Fetch-Site": {"same-origin"}}, false},
		{"repeated Origin", http.Header{"Origin": {origin, origin}}, false},
		{"null without fetch metadata", http.Header{"Origin": {"null"}}, false},
		{"null from a cross-site page", http.Header{"Origin": {"null"}, "Sec-Fetch-Site": {"cross-site"}}, false},
		{"null from a same-site page", http.Header{"Origin": {"null"}, "Sec-Fetch-Site": {"same-site"}}, false},
		{"null from this origin's page (WebKit)", http.Header{"Origin": {"null"}, "Sec-Fetch-Site": {"same-origin"}}, true},
		{"null with a repeated fetch-site", http.Header{"Origin": {"null"}, "Sec-Fetch-Site": {"same-origin", "same-origin"}}, false},
	}
	for _, c := range cases {
		if got := decisionFromThisPage(c.header, origin); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
