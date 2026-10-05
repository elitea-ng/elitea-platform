package llmproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

// waitForArrivals waits until the backend has seen want requests.
func waitForArrivals(t *testing.T, arrived *atomic.Int64, want int64, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for arrived.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: the backend saw %d requests, want %d", what, arrived.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAudio_InFlightCapRefusesTheExtraRequestBeforeForwarding pins the
// per-principal cap. The gateway parses a whole transcription upload before
// its budget and rate checks run, so the edge must stop the N+1th parallel
// upload of one member before any byte of it is forwarded.
func TestAudio_InFlightCapRefusesTheExtraRequestBeforeForwarding(t *testing.T) {
	var arrived atomic.Int64
	unblock := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		<-unblock
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"ok"}`))
	}))
	defer backend.Close()
	defer close(unblock)

	p := proxyTo(t, backend.URL, "sekret")
	member := func(userID string) *http.Request {
		req := audioRequest("/llm/v1/audio/transcriptions", strings.NewReader("audio"))
		return req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: userID, UserID: userID}))
	}
	serve := func(req *http.Request, codes chan<- int) {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		codes <- rec.Code
	}

	done := make(chan int, maxAudioInFlightPerPrincipal+2)
	for range maxAudioInFlightPerPrincipal {
		go serve(member("11"), done)
	}
	waitForArrivals(t, &arrived, maxAudioInFlightPerPrincipal, "the capped uploads")

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, member("11"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if got := errorCode(t, rec.Body.Bytes()); got != "too_many_concurrent_requests" {
		t.Fatalf("code = %q, want too_many_concurrent_requests", got)
	}
	if got := arrived.Load(); got != maxAudioInFlightPerPrincipal {
		t.Fatalf("the backend saw %d requests, want %d: the refused upload was forwarded", got, maxAudioInFlightPerPrincipal)
	}

	// Another member is not affected by the first member's uploads.
	go serve(member("12"), done)
	waitForArrivals(t, &arrived, maxAudioInFlightPerPrincipal+1, "another member's upload")

	// A finished request frees its slot.
	unblock <- struct{}{}
	if code := <-done; code != http.StatusOK {
		t.Fatalf("a finished request answered %d, want 200", code)
	}
	go serve(member("11"), done)
	waitForArrivals(t, &arrived, maxAudioInFlightPerPrincipal+2, "the upload after a slot was released")
}

// TestAudio_InFlightCapLeavesOtherRoutesAlone: the cap counts audio requests
// only. A chat stream of the same member never takes or needs a slot.
func TestAudio_InFlightCapLeavesOtherRoutesAlone(t *testing.T) {
	inFlight := newAudioInFlight(1)
	if !inFlight.acquire("user:1") {
		t.Fatal("the first slot was refused")
	}
	req := audioRequest("/llm/v1/chat/completions", strings.NewReader("{}"))
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "1", UserID: "1"}))
	rec := httptest.NewRecorder()
	if _, release, ok := limitAudioRequest(rec, req, inFlight); !ok {
		t.Fatalf("a chat request was refused by the audio cap; status=%d", rec.Code)
	} else {
		release()
	}
	inFlight.release("user:1")
	if len(inFlight.count) != 0 {
		t.Fatalf("released slots are kept: %v", inFlight.count)
	}
}
