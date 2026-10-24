package llmproxy

// The metered-call routes (#1238) differ from the model routes in one rule:
// an execution id the verifier drops is a refusal, not an unattributed call.
// Tool spend exists only to land on a run, and a worker naming a run the edge
// does not admit is either misconfigured or forging attribution.

import (
	"context"
	"io"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy/llmproxytest"
)

func TestProxy_MeteredCallNeedsAnAttributableExecution(t *testing.T) {
	cases := map[string]struct {
		header   string
		verifier *stubExecutionVerifier
		want     int
	}{
		"a verified execution is forwarded":    {"exec-9", &stubExecutionVerifier{ok: true}, http.StatusOK},
		"a dropped execution is refused":       {"exec-9", &stubExecutionVerifier{ok: false}, http.StatusForbidden},
		"no execution at all is refused":       {"", &stubExecutionVerifier{ok: true}, http.StatusForbidden},
		"an evaluation attribution is refused": {"eval:1:case:c", &stubExecutionVerifier{ok: true}, http.StatusForbidden},
	}
	for _, path := range []string{meteredAdmitPath, meteredSettlePath} {
		for name, tc := range cases {
			t.Run(path+"/"+name, func(t *testing.T) {
				var reached bool
				var seen string
				backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					reached = true
					seen = r.Header.Get(HeaderExecutionID)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{}`))
				}))
				proxy, err := New(Config{Gateway: gatewayTo(t, backend, frozenSecret), ExecutionVerifier: tc.verifier})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				req := httptest.NewRequestWithContext(fullCtx(), http.MethodPost, "http://edge"+path, strings.NewReader(`{}`))
				if tc.header != "" {
					req.Header.Set(HeaderExecutionID, tc.header)
				}
				rec := httptest.NewRecorder()
				proxy.ServeHTTP(rec, req)
				if rec.Code != tc.want {
					t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
				}
				if tc.want == http.StatusForbidden {
					if reached {
						t.Fatal("a refused metered call reached the gateway")
					}
					if !strings.Contains(rec.Body.String(), `"code":"execution_not_attributable"`) {
						t.Fatalf("body = %s", rec.Body.String())
					}
					return
				}
				if seen != tc.header {
					t.Fatalf("the gateway received execution id %q, want %q", seen, tc.header)
				}
			})
		}
	}
}

type recordingAudit struct{ events []audit.Event }

func (r *recordingAudit) Record(_ context.Context, event audit.Event) {
	r.events = append(r.events, event)
}

// A settled call writes one row naming the tool, the provider and the target
// the worker disclosed; an admitted call writes none (it is recorded when it
// settles); a refused admission writes one; a target with a control character
// is recorded empty; a settle retry the gateway answers as a duplicate writes
// none, and its answer still reaches the worker unchanged.
func TestProxy_MeteredCallsAreAudited(t *testing.T) {
	settle := `{"admission_id":"a","event_id":"e-1","operation":"web_search","provider":"tavily",` +
		`"units":{"requests":1},"outcome":"ok","audit":{"tool":"web_search","target":"0f3a","status":"ok"}}`
	cases := map[string]struct {
		path       string
		body       string
		status     int
		wantRows   int
		wantAction string
		answer     string
	}{
		"settled":           {meteredSettlePath, settle, http.StatusOK, 1, `web_search via "tavily": ok; target "0f3a"; execution exec-9; event e-1`, ""},
		"admitted":          {meteredAdmitPath, settle, http.StatusOK, 0, "", ""},
		"admission refused": {meteredAdmitPath, settle, http.StatusPaymentRequired, 1, `web_search via "tavily": refused_budget`, ""},
		"no audit object":   {meteredSettlePath, `{"event_id":"e"}`, http.StatusOK, 0, "", ""},
		"control character": {meteredSettlePath, strings.Replace(settle, "0f3a", `a\u0007b`, 1), http.StatusOK, 1, `target ""`, ""},
		"first settle":      {meteredSettlePath, settle, http.StatusOK, 1, `: ok;`, `{"billed_nano_usd":8,"duplicate":false}`},
		"duplicate settle":  {meteredSettlePath, settle, http.StatusOK, 0, "", `{"billed_nano_usd":8,"duplicate":true}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var forwarded string
			backend := llmproxytest.Start(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				forwarded = string(raw)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				answer := tc.answer
				if answer == "" {
					answer = `{}`
				}
				_, _ = w.Write([]byte(answer))
			}))
			recorder := &recordingAudit{}
			proxy, err := New(Config{Gateway: gatewayTo(t, backend, frozenSecret),
				ExecutionVerifier: &stubExecutionVerifier{ok: true}, Audit: recorder})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			req := httptest.NewRequestWithContext(fullCtx(), http.MethodPost, "http://edge"+tc.path, strings.NewReader(tc.body))
			req.Header.Set(HeaderExecutionID, "exec-9")
			out := httptest.NewRecorder()
			proxy.ServeHTTP(out, req)
			if tc.answer != "" && out.Body.String() != tc.answer {
				t.Fatalf("the worker received %q, want the gateway's answer %q", out.Body.String(), tc.answer)
			}
			if forwarded != tc.body {
				t.Fatalf("the gateway received %q, want the body unchanged", forwarded)
			}
			if len(recorder.events) != tc.wantRows {
				t.Fatalf("audit rows = %d, want %d", len(recorder.events), tc.wantRows)
			}
			if tc.wantRows == 1 {
				event := recorder.events[0]
				if event.EventType != "agent" || !strings.Contains(event.Action, tc.wantAction) {
					t.Fatalf("audit row = %+v, want action containing %q", event, tc.wantAction)
				}
			}
		})
	}
}
