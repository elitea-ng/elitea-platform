package ssewriter

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The production interval must stay well under the ~100 second idle timeout
// of a Cloudflare-style proxy in front of the GKE load balancer, including the
// half-interval tick slack.
func TestKeepaliveIntervalFitsAProxyIdleTimeout(t *testing.T) {
	if worst := KeepaliveInterval + KeepaliveInterval/2; worst >= 60*time.Second {
		t.Fatalf("the longest silence is %s; a proxy that idles out at 100s needs a wide margin", worst)
	}
}

func TestKeepaliveWritesACommentWhileIdleAtABoundary(t *testing.T) {
	recorder := httptest.NewRecorder()
	keepalive := StartKeepalive(recorder, 20*time.Millisecond)
	if _, err := keepalive.Write([]byte("data: one\n\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	keepalive.Stop()

	body := recorder.Body.String()
	if !strings.HasPrefix(body, "data: one\n\n: keepalive\n\n") {
		t.Fatalf("body = %q; want the event, then a keepalive comment", body)
	}
	if !recorder.Flushed {
		t.Fatal("the keepalive was not flushed; a buffered comment keeps no proxy awake")
	}
}

// A relayed stream can pause mid-event. A comment spliced in there would land
// inside the event's data and corrupt it.
func TestKeepaliveNeverSplitsAnEvent(t *testing.T) {
	recorder := httptest.NewRecorder()
	keepalive := StartKeepalive(recorder, 20*time.Millisecond)
	if _, err := keepalive.Write([]byte("data: {\"partial\"")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if strings.Contains(recorder.Body.String(), "keepalive") {
		keepalive.Stop()
		t.Fatalf("a comment was written mid-event: %q", recorder.Body.String())
	}
	if _, err := keepalive.Write([]byte(":1}\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	keepalive.Stop()
	if body := recorder.Body.String(); !strings.HasPrefix(body, "data: {\"partial\":1}\r\n\r\n: keepalive\n\n") {
		t.Fatalf("body = %q; want the whole event, then a comment once it ended", body)
	}
}

func TestKeepaliveIsQuietWhileTheStreamIsBusy(t *testing.T) {
	recorder := httptest.NewRecorder()
	keepalive := StartKeepalive(recorder, 80*time.Millisecond)
	for i := 0; i < 10; i++ {
		if _, err := keepalive.Write([]byte("data: x\n\n")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	keepalive.Stop()
	if strings.Contains(recorder.Body.String(), "keepalive") {
		t.Fatalf("a comment was written into a stream that was never idle: %q", recorder.Body.String())
	}
}

func TestKeepaliveWritesNothingAfterStop(t *testing.T) {
	recorder := httptest.NewRecorder()
	keepalive := StartKeepalive(recorder, 10*time.Millisecond)
	keepalive.Stop()
	keepalive.Stop() // idempotent
	time.Sleep(50 * time.Millisecond)
	if recorder.Body.Len() != 0 {
		t.Fatalf("body after Stop = %q", recorder.Body.String())
	}
}
