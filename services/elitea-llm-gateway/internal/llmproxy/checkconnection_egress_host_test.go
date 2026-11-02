package llmproxy

import (
	"strings"
	"testing"
)

// TestCheckConnection_EgressRefusalNamesOnlyTheHost proves the refusal Detail
// names the refused host, and only the host: no scheme, user info, path or
// query of the api_base gets into the response.
func TestCheckConnection_EgressRefusalNamesOnlyTheHost(t *testing.T) {
	h := newCheckConnectionHandler(fakeEgressPolicy{allow: false})
	rec := doCheckConnection(t, h, checkConnectionRequest{
		Type:    "open_ai",
		APIBase: "https://user:s3cret@LLM.Example.COM:8443/v1?token=abc",
		APIKey:  "sk-whatever",
	})
	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Reason != checkConnectionReasonEgress {
		t.Fatalf("reason = %q, want %q", resp.Reason, checkConnectionReasonEgress)
	}
	if resp.Detail != "llm.example.com:8443" {
		t.Fatalf("detail = %q, want %q", resp.Detail, "llm.example.com:8443")
	}
}

// TestCheckConnection_CloudEgressRefusalNamesTheDerivedHost proves that a
// cloud type, whose endpoint the region selects, names the derived host.
func TestCheckConnection_CloudEgressRefusalNamesTheDerivedHost(t *testing.T) {
	h := newCheckConnectionHandler(fakeEgressPolicy{allow: false, privateNetwork: true})
	rec := doCheckConnection(t, h, checkConnectionRequest{
		Type: "amazon_bedrock", AWSAccessKeyID: "AKIA", AWSSecretAccessKey: "s", AWSRegionName: "us-east-1",
	})
	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Reason != checkConnectionReasonEgress {
		t.Fatalf("reason = %q, want %q", resp.Reason, checkConnectionReasonEgress)
	}
	if !strings.Contains(resp.Detail, "us-east-1") || strings.Contains(resp.Detail, "/") {
		t.Fatalf("detail = %q, want the regional host only", resp.Detail)
	}
}

// TestCheckConnection_EgressFailClosedNamesNoHost proves the fail-closed case
// (no policy wired) names no host: no host was refused.
func TestCheckConnection_EgressFailClosedNamesNoHost(t *testing.T) {
	h := NewHandler(nil, nil, nil)
	rec := doCheckConnection(t, h, checkConnectionRequest{Type: "open_ai", APIBase: "https://llm.example.com", APIKey: "k"})
	resp := decodeCheckConnectionResponse(t, rec)
	if resp.Reason != checkConnectionReasonEgress || resp.Detail != "" {
		t.Fatalf("got reason=%q detail=%q, want %q and no detail", resp.Reason, resp.Detail, checkConnectionReasonEgress)
	}
}

func TestEgressRefusedHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://us-central1-aiplatform.googleapis.com": "us-central1-aiplatform.googleapis.com",
		"http://[::1]:11434/api":                        "[::1]:11434",
		"not a url":                                     "",
		"":                                              "",
	} {
		if got := egressRefusedHost(in); got != want {
			t.Errorf("egressRefusedHost(%q) = %q, want %q", in, got, want)
		}
	}
}
