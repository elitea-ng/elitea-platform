package run

// The tee. What must hold is that watching changes nothing: the caller's
// status and bytes are the provider's, whether or not a recorder is attached
// and whether or not it succeeds.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// hop stands in for providerhost/proxy: it fills the Outcome the way that
// package's ModifyResponse does and writes the same bytes to the caller.
func hop(status int, body string) Forwarder {
	return func(w http.ResponseWriter, r *http.Request, _, _, _ string) {
		if outcome := proxy.OutcomeFrom(r.Context()); outcome != nil {
			outcome.Status = status
			if outcome.CaptureLimit > 0 {
				outcome.Body = []byte(body)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func drain(forward Forwarder) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v2/deepwiki/invocations/7/Wikis/ask/inv-1", nil)
	forward(recorder, request, "/tools/Wikis/ask/invocations/inv-1", "7", "3")
	return recorder
}

func TestTeeRecordsATerminalPollAndChangesNothing(t *testing.T) {
	var seen []Observed
	body := `{"invocation_id":"inv-1","status":"Completed","result":"done"}`
	forward := Tee(hop(http.StatusOK, body), 0, quiet(),
		func(_ context.Context, observed Observed) { seen = append(seen, observed) })

	recorder := drain(forward)

	if recorder.Code != http.StatusOK || recorder.Body.String() != body {
		t.Fatalf("the tee changed the response: %d %q", recorder.Code, recorder.Body.String())
	}
	if len(seen) != 1 {
		t.Fatalf("recorded %d terminals, want 1", len(seen))
	}
	if seen[0].InvocationID != "inv-1" || seen[0].Content != "done" {
		t.Errorf("observed = %+v", seen[0])
	}
	// The ids come from the route table, so an observer never re-parses them.
	if seen[0].ProjectID != "7" || seen[0].UserID != "3" {
		t.Errorf("observed identity = %q/%q, want 7/3", seen[0].ProjectID, seen[0].UserID)
	}
}

func TestTeeRecordsNothingBeforeTheRunEndsOrWhenTheHopFailed(t *testing.T) {
	for name, testCase := range map[string]struct {
		status int
		body   string
	}{
		"still running":  {http.StatusOK, `{"invocation_id":"inv-1","status":"InProgress"}`},
		"the hop failed": {http.StatusBadGateway, `{"invocation_id":"inv-1","status":"Completed","result":"done"}`},
		"an empty body":  {http.StatusOK, ``},
	} {
		t.Run(name, func(t *testing.T) {
			recorded := 0
			drain(Tee(hop(testCase.status, testCase.body), 0, quiet(),
				func(context.Context, Observed) { recorded++ }))
			if recorded != 0 {
				t.Fatalf("recorded %d terminals for %s", recorded, name)
			}
		})
	}
}

// A payload past the limit is reported as unreadable, NOT as a truncated
// answer: half a JSON document is not a shorter one, and a cut answer in a
// transcript is read back as the whole of it.
func TestTeeRefusesATruncatedPayload(t *testing.T) {
	body := `{"invocation_id":"inv-1","status":"Completed","result":"` + strings.Repeat("x", 200) + `"}`
	recorded := 0
	forward := Tee(func(w http.ResponseWriter, r *http.Request, _, _, _ string) {
		if outcome := proxy.OutcomeFrom(r.Context()); outcome != nil {
			outcome.Status = http.StatusOK
			outcome.Body = []byte(body[:10])
			outcome.Truncated = true
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}, 10, quiet(), func(context.Context, Observed) { recorded++ })

	recorder := drain(forward)

	if recorded != 0 {
		t.Fatal("a truncated payload was recorded as an answer")
	}
	if recorder.Body.String() != body {
		t.Fatal("the caller did not receive the whole payload")
	}
}

// A recorder is asked once per terminal poll. Whether that means one WRITE is
// the store's business — the browser polls on an interval and nothing stops
// it draining the terminal payload twice.
func TestTeeAsksTheRecorderOncePerTerminalPoll(t *testing.T) {
	var mutex sync.Mutex
	recorded := 0
	forward := Tee(hop(http.StatusOK, `{"invocation_id":"inv-1","status":"Completed","result":"done"}`),
		0, quiet(), func(context.Context, Observed) {
			mutex.Lock()
			defer mutex.Unlock()
			recorded++
		})

	var waiting sync.WaitGroup
	for range 8 {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			drain(forward)
		}()
	}
	waiting.Wait()

	if recorded != 8 {
		t.Fatalf("eight polls reached the recorder %d times", recorded)
	}
}

// A facade that keeps no record pays nothing — not even the buffering, which
// is what `CaptureLimit` staying zero proves.
func TestTeeWithNoRecorderIsTheHopItself(t *testing.T) {
	var captured int
	inner := Forwarder(func(w http.ResponseWriter, r *http.Request, _, _, _ string) {
		if outcome := proxy.OutcomeFrom(r.Context()); outcome != nil {
			captured = outcome.CaptureLimit
		}
		w.WriteHeader(http.StatusOK)
	})
	if got := Tee(inner, 0, quiet(), nil); got == nil {
		t.Fatal("Tee with no recorder returned nothing")
	}
	drain(Tee(inner, 0, quiet(), nil))
	if captured != 0 {
		t.Fatalf("a hop with no recorder still asked for %d bytes of capture", captured)
	}
}
