package proxy

// The Outcome's body capture.
//
// What must hold is that capturing changes NOTHING the caller receives. The
// hop is a reverse proxy and the body is a stream; a capture that consumed it,
// or truncated it, or turned a read error into a clean end, would be invisible
// in the captured copy and visible only to the user.

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// respond builds the response ModifyResponse would be handed, with the
// request the hook reads the Outcome off.
func respond(t *testing.T, outcome *Outcome, body io.ReadCloser) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "https://provider.invalid/tools", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Request:    request.WithContext(WithOutcome(request.Context(), outcome)),
	}
}

func TestCaptureLeavesTheBodyReadableInFull(t *testing.T) {
	const payload = `{"invocation_id":"inv-1","status":"Completed","result":"done"}`
	outcome := &Outcome{CaptureLimit: 1 << 10}
	response := respond(t, outcome, io.NopCloser(strings.NewReader(payload)))

	capture(outcome, response)

	if string(outcome.Body) != payload {
		t.Errorf("captured %q, want the whole payload", outcome.Body)
	}
	if outcome.Truncated {
		t.Error("a payload inside the limit was reported as truncated")
	}
	forwarded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(forwarded) != payload {
		t.Fatalf("the caller received %q, want the whole payload", forwarded)
	}
}

// A body over the limit is reported as truncated AND still forwarded whole.
// Reporting it is what lets a consumer refuse to parse half a document.
func TestCaptureReportsTruncationAndStillForwardsEverything(t *testing.T) {
	payload := strings.Repeat("x", 100)
	outcome := &Outcome{CaptureLimit: 10}
	response := respond(t, outcome, io.NopCloser(strings.NewReader(payload)))

	capture(outcome, response)

	if !outcome.Truncated {
		t.Error("a payload past the limit was not reported as truncated")
	}
	if len(outcome.Body) != 10 {
		t.Errorf("captured %d bytes, want the limit of 10", len(outcome.Body))
	}
	forwarded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(forwarded) != payload {
		t.Fatalf("the caller received %d bytes, want all %d", len(forwarded), len(payload))
	}
}

// A read that fails part-way must fail the caller's read too. Swallowing it
// would turn a truncated provider response into a clean short one, which is
// the failure mode nobody notices.
func TestCaptureLetsAReadFailureTravelToTheCaller(t *testing.T) {
	failure := errors.New("connection reset")
	outcome := &Outcome{CaptureLimit: 1 << 10}
	response := respond(t, outcome, io.NopCloser(
		io.MultiReader(strings.NewReader("half"), failingReader{failure})))

	capture(outcome, response)

	if _, err := io.ReadAll(response.Body); !errors.Is(err, failure) {
		t.Fatalf("the caller read err = %v, want the underlying failure", err)
	}
}

// Zero is the default and must cost nothing: every other route on this hop
// shares the proxy, and a hop that buffered every response would put a
// generation's terminal payload in memory for nobody.
func TestCaptureIsOffByDefault(t *testing.T) {
	outcome := &Outcome{}
	body := io.NopCloser(strings.NewReader("streamed"))
	response := respond(t, outcome, body)

	capture(outcome, response)

	if outcome.Body != nil {
		t.Errorf("captured %q with no limit set", outcome.Body)
	}
	if response.Body != body {
		t.Error("the body was replaced although nothing was captured")
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }
