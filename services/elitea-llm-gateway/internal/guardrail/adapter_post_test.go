package guardrail

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestEgressPostIsTheOneEnvelope: the http, cloud and self-hosted adapters
// share egressPost and secretHeader. This test pins the envelope once.
func TestEgressPostIsTheOneEnvelope(t *testing.T) {
	var gotAuth, gotType string
	status := http.StatusOK
	body := `{"ok":true}`
	srv := cloudServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	env := cloudEnv(srv)
	ctx := context.Background()
	prep := secretHeader(ctx, env, "auth_secret", "{{secret.TOKEN}}", "Authorization", "Bearer ")
	raw, err := egressPost(ctx, env, srv.URL, []byte(`{}`), prep)
	if err != nil || string(raw) != body || gotAuth != "Bearer value-TOKEN" || gotType != "application/json" {
		t.Fatalf("raw %q err %v auth %q type %q", raw, err, gotAuth, gotType)
	}
	// A status other than 200 is an error that does not quote the body.
	status, body = http.StatusBadRequest, `{"error":"the text quotes the prompt"}`
	if _, err := egressPost(ctx, env, srv.URL, []byte(`{}`), nil); err == nil || strings.Contains(err.Error(), "prompt") {
		t.Fatalf("err = %v", err)
	}
	// A response over the bound is an error.
	status, body = http.StatusOK, strings.Repeat("a", maxAdapterResponseBytes+1)
	if _, err := egressPost(ctx, env, srv.URL, []byte(`{}`), nil); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("err = %v", err)
	}
	// A host off the allowlist gets no request.
	gotAuth = ""
	env.EgressAllows = func(string) bool { return false }
	if _, err := egressPost(ctx, env, srv.URL, []byte(`{}`), prep); err == nil || gotAuth != "" {
		t.Fatalf("a refused host was called: %v", err)
	}
	// A secret that does not resolve is an error that names the field.
	env = cloudEnv(srv)
	env.ResolveSecret = nil
	if _, err := egressPost(ctx, env, srv.URL, []byte(`{}`), secretHeader(ctx, env, "auth_secret", "{{secret.X}}", "Authorization", "")); err == nil || !strings.Contains(err.Error(), "auth_secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestCodePointOffsets(t *testing.T) {
	got := codePointOffsets("aé😀")
	want := []int{0, 1, 3, 7}
	if len(got) != len(want) {
		t.Fatalf("offsets = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("offsets = %v", got)
		}
	}
}
