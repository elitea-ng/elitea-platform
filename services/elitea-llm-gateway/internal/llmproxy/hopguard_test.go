package llmproxy

// hopguard_test.go — the INBOUND half of hop-marker detection (issue #164).
//
// The acceptance question these tests answer is not "does the header compare
// equal" (internal/hopmarker owns that). It is: does a re-entering request get
// contained BEFORE it reaches the provider, and can a marker anybody can
// harvest be turned against anybody else?

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/elitea-ng/elitea-platform/services/elitea-llm-gateway/internal/hopmarker"
)

// hopTestSecret is this test file's hop key material. It is deliberately NOT
// the identity secret any test passes to NewHandler.
var hopTestSecret = []byte("hop-secret-for-tests")

// hopHandlerUnderTest builds a handler with hop detection armed and returns the
// handler, the router it dispatches to, and this deployment's marker value.
func hopHandlerUnderTest(t *testing.T) (http.Handler, *trackingRouter, string) {
	t.Helper()
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	marker := hopmarker.New(hopTestSecret)
	h := NewHandler(router, nil, nil, WithHopMarker(marker))
	// The guard is middleware, exactly as internal/api mounts it.
	return h.HopGuard(http.HandlerFunc(h.Chat)), router, marker.Value()
}

// TestHopGuard_ContainsTheLoopOnFirstReEntry is the acceptance criterion: a
// real circular route is detected by the MARKER, not by a rate, and it is
// stopped the first time round.
//
// The request built here is the one that closes the canonical circle —
// gateway → provider (= platform /llm) → edge → elitea-main → gateway. It is
// well-formed, it carries a project, and it arrives ALONE. No rate-based layer
// can see anything wrong with it; the marker can.
func TestHopGuard_ContainsTheLoopOnFirstReEntry(t *testing.T) {
	guarded, router, marker := hopHandlerUnderTest(t)

	req := chatReqWithProject(t, "42", false)
	req.Header.Set(hopmarker.Header, marker)

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a request carrying this gateway's own hop marker", rec.Code)
	}
	if router.called.Load() {
		t.Error("the provider was called for a re-entering request — the loop was not contained before dispatch")
	}

	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal refusal body: %v", err)
	}
	if out.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q, want invalid_request_error", out.Error.Type)
	}
	if out.Error.Code != hopRefusalCode {
		t.Errorf("error.code = %q, want %q", out.Error.Code, hopRefusalCode)
	}
}

// TestHopGuard_FirstPassIsUnaffected is the other side of the same criterion:
// only the SECOND traversal carries the marker, so ordinary traffic — which is
// every request a client makes — must be untouched. A guard that refused the
// first pass would refuse everything.
func TestHopGuard_FirstPassIsUnaffected(t *testing.T) {
	guarded, router, _ := hopHandlerUnderTest(t)

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, chatReqWithProject(t, "42", false))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a request with no hop marker", rec.Code)
	}
	if !router.called.Load() {
		t.Error("the provider was not called for an ordinary request")
	}
}

// TestHopGuard_ForgedMarkerDeniesOnlyTheForger is the security property the
// issue demands be PROVEN, because the marker cannot be kept secret: every
// upstream the gateway calls receives a copy, and the browser edge forwards
// the header rather than deleting it.
//
// The claim under test is that recognising a marker is a decision about ONE
// request and leaves nothing behind. So after a forged re-entry is refused:
//
//  1. the same project's next request is admitted — no circuit was opened,
//     no counter was incremented, nothing was remembered;
//  2. another project's request is admitted — the refusal cannot cross a
//     tenant boundary;
//  3. the forger can repeat the trick forever and still reach nobody else.
//
// If any of those fails, a value every upstream already holds has become a
// denial-of-service against other tenants, and the guard would have created
// the failure it exists to contain.
func TestHopGuard_ForgedMarkerDeniesOnlyTheForger(t *testing.T) {
	guarded, router, harvested := hopHandlerUnderTest(t)

	// The forger replays a harvested marker, repeatedly.
	for attempt := 1; attempt <= 5; attempt++ {
		req := chatReqWithProject(t, "42", false)
		req.Header.Set(hopmarker.Header, harvested)
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("forged attempt %d: status = %d, want 400", attempt, rec.Code)
		}
	}

	// 1. The SAME project, with no marker, is still served.
	router.called.Store(false)
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Errorf("victim project after the forgery: status = %d, want 200. "+
			"The refusal left state behind, so a harvested marker denies more than its own request.", rec.Code)
	}
	if !router.called.Load() {
		t.Error("the provider was not called for the victim project's own follow-up request")
	}

	// 2. A DIFFERENT project is untouched.
	rec2 := httptest.NewRecorder()
	guarded.ServeHTTP(rec2, chatReqWithProject(t, "43", false))
	if rec2.Code != http.StatusOK {
		t.Errorf("bystander project: status = %d, want 200. A forged marker reached across tenants.", rec2.Code)
	}
}

// TestHopGuard_ForeignMarkerIsNotOurs pins that the guard refuses only THIS
// deployment's marker. A neighbouring Elitea install used as a legitimate
// upstream stamps its own value; treating that as a loop would refuse traffic
// that never touched this gateway.
func TestHopGuard_ForeignMarkerIsNotOurs(t *testing.T) {
	guarded, router, _ := hopHandlerUnderTest(t)

	req := chatReqWithProject(t, "42", false)
	req.Header.Set(hopmarker.Header, hopmarker.New([]byte("some-other-deployment")).Value())

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for another deployment's marker", rec.Code)
	}
	if !router.called.Load() {
		t.Error("the provider was not called for a request carrying a foreign marker")
	}
}

// TestHopGuard_UnarmedAdmitsEverything covers the no-GATEWAY_HOP_SECRET
// deployment. It must behave exactly as it did before hop detection existed —
// including for a request carrying an armed deployment's marker, because an
// unarmed gateway that refused traffic would be a worse failure than one that
// detects nothing. main() logs the mode so the posture is never a surprise.
func TestHopGuard_UnarmedAdmitsEverything(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	h := NewHandler(router, nil, nil) // no WithHopMarker
	guarded := h.HopGuard(http.HandlerFunc(h.Chat))

	req := chatReqWithProject(t, "42", false)
	req.Header.Set(hopmarker.Header, hopmarker.New(hopTestSecret).Value())

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unarmed handler: status = %d, want 200", rec.Code)
	}
	if !router.called.Load() {
		t.Error("unarmed handler did not dispatch")
	}
}

// TestHopGuard_RunsBeforeIdentityVerification pins the ordering.
//
// In the canonical loop the re-entering request carries a VALID signed
// identity: elitea-main signs it, because to elitea-main it is an ordinary
// caller. But a loop can also close through a path that produces no valid
// signature at all. Containment must not depend on either, so the guard sits
// ahead of verifySignature and answers with the loop refusal — not with
// "invalid identity signature", which would send an operator hunting the wrong
// fault.
func TestHopGuard_RunsBeforeIdentityVerification(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	marker := hopmarker.New(hopTestSecret)
	// A non-empty identity secret with NO signature on the request: buildContext
	// would answer 403 permission_error.
	h := NewHandler(router, nil, []byte("identity-secret"), WithHopMarker(marker))
	guarded := h.HopGuard(http.HandlerFunc(h.Chat))

	req := chatReqWithProject(t, "42", false)
	req.Header.Set(hopmarker.Header, marker.Value())

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 — the hop refusal must win over the identity refusal", rec.Code)
	}
	var out openAIError
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal refusal body: %v", err)
	}
	if out.Error.Code != hopRefusalCode {
		t.Errorf("error.code = %q, want %q — a loop must be reported as a loop", out.Error.Code, hopRefusalCode)
	}
}

// TestHopGuard_LoopBreakerIsNotConsulted pins that the two layers stay
// independent. A refused re-entry must not feed the per-(project, model)
// sliding window: the marker is public, so a request that fed the backstop
// would let anybody open another tenant's circuit — the exact escalation the
// stateless design forbids.
func TestHopGuard_LoopBreakerIsNotConsulted(t *testing.T) {
	router := &trackingRouter{}
	router.chatResp = &schemas.BifrostChatResponse{ID: "ok"}
	marker := hopmarker.New(hopTestSecret)
	clk := &testClock{t: time.Unix(1_700_000_000, 0)}
	h := NewHandler(router, nil, nil,
		WithHopMarker(marker),
		WithLoopBreakerClock(testLoopParams(), clk.now),
	)
	guarded := h.HopGuard(http.HandlerFunc(h.Chat))

	// Far more marked requests than the backstop's threshold, all at one
	// instant, all for the same tuple.
	for i := 0; i < testLoopThreshold*3; i++ {
		req := chatReqWithProject(t, "42", false)
		req.Header.Set(hopmarker.Header, marker.Value())
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("marked request %d: status = %d, want 400", i+1, rec.Code)
		}
	}

	// The tuple's circuit must still be closed for genuine traffic.
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, chatReqWithProject(t, "42", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("genuine request after %d refused re-entries: status = %d, want 200. "+
			"The hop refusal fed the amplification backstop, so a public marker can open another project's circuit.",
			testLoopThreshold*3, rec.Code)
	}
}
