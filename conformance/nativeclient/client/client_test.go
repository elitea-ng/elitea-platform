package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// RFC 7636 Appendix B.
func TestS256ChallengeMatchesTheRFCVector(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	if got := S256Challenge(verifier); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("S256Challenge = %q", got)
	}
}

func TestNewVerifierIsAFreshFortyThreeCharacterToken(t *testing.T) {
	first, second := NewVerifier(), NewVerifier()
	if len(first) != 43 || first == second || strings.ContainsAny(first, "+/=") {
		t.Fatalf("verifiers %q %q", first, second)
	}
}

func TestCallbackVerifyChecksStateIssuerAndError(t *testing.T) {
	parse := func(raw string) Callback {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return ParseCallback(u)
	}
	good := parse("app:/cb?code=c&state=s&iss=https%3A%2F%2Fa.example")
	if err := good.Verify("s", "https://a.example"); err != nil {
		t.Fatalf("good callback: %v", err)
	}
	for name, check := range map[string]error{
		"wrong state":  good.Verify("other", "https://a.example"),
		"mix-up":       good.Verify("s", "https://b.example"),
		"refused":      parse("app:/cb?error=access_denied&state=s&iss=https%3A%2F%2Fa.example").Verify("s", "https://a.example"),
		"missing code": parse("app:/cb?state=s&iss=https%3A%2F%2Fa.example").Verify("s", "https://a.example"),
	} {
		if check == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEventReaderFollowsTheSSEFieldRules(t *testing.T) {
	stream := ": connected\n\n" +
		"id: 7\nevent: execution.node_event\ndata: {\"type\":\"a\"}\n\n" +
		"id:8\r\ndata: line one\r\ndata: line two\r\n\r\n" +
		"event: ignored-without-data\n\n" +
		"data:no-space\n\n"
	reader := NewEventReader(strings.NewReader(stream))
	var got []Event
	for {
		event, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, event)
	}
	want := []Event{
		{ID: "7", Event: "execution.node_event", Data: `{"type":"a"}`},
		{ID: "8", Data: "line one\nline two"},
		{Data: "no-space"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("events\n got %q\nwant %q", got, want)
	}
}

func TestDecodeFrameRequiresAnIntegerCursor(t *testing.T) {
	if _, err := DecodeFrame(Event{ID: "abc", Data: "{}"}); err == nil {
		t.Fatal("a non-integer id was accepted")
	}
	frame, err := DecodeFrame(Event{ID: "12", Event: "execution.failed", Data: `{"code":"x"}`})
	if err != nil || frame.Cursor != 12 || frame.Type != "execution.failed" || !IsTerminal(frame) || !IsFailure(frame) {
		t.Fatalf("frame %+v err %v", frame, err)
	}
}

func TestIsTerminalAppliesTheContractRules(t *testing.T) {
	frame := func(data string) Frame {
		decoded, err := DecodeFrame(Event{ID: "1", Event: "execution.node_event", Data: data})
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	cases := map[string]bool{
		`{"type":"pipeline_finish"}`:                                                                                       true,
		`{"type":"agent_llm_chunk","content":"x"}`:                                                                         false,
		`{"type":"agent_response","response_metadata":{}}`:                                                                 false,
		`{"type":"agent_response","response_metadata":{"finish_reason":"stop"}}`:                                           true,
		`{"type":"mcp_authorization_required","response_metadata":{}}`:                                                     false,
		`{"type":"mcp_authorization_required","response_metadata":{"authorization_requests":[]}}`:                          true,
		`{"type":"agent_hitl_interrupt","response_metadata":{"metadata":{"parent_agent_name":"p","child_thread_id":"c"}}}`: false,
		`{"type":"agent_hitl_interrupt","response_metadata":{"metadata":{"parent_agent_name":"p"}}}`:                       true,
		`{"type":"llm_error"}`: true,
	}
	for data, want := range cases {
		if got := IsTerminal(frame(data)); got != want {
			t.Errorf("%s: IsTerminal = %v, want %v", data, got, want)
		}
	}
}

func TestSyncDrainsPagesAndUpsertsByID(t *testing.T) {
	var cursors []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("changes_since")
		cursors = append(cursors, cursor)
		w.Header().Set("Content-Type", "application/json")
		switch cursor {
		case "0":
			_, _ = io.WriteString(w, `{"total":2,"rows":[{"id":1,"v":"a"}],"next_cursor":"c1","has_more":true}`)
		case "c1":
			_, _ = io.WriteString(w, `{"total":2,"items":[{"id":"1","v":"b"},{"id":2}],"tombstones":[{"id":3,"uuid":null,"reason":"deleted","deleted_at":"2026-01-01T00:00:00Z"}],"next_cursor":"c2","has_more":false}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_sync_cursor","message":"no"}`)
		}
	}))
	defer server.Close()
	api := New(server.URL)
	result, err := api.Sync(context.Background(), "/list?x=1", "0", 0)
	if err != nil {
		t.Fatal(err)
	}
	rows := RowsByID(result.Rows)
	if result.Cursor != "c2" || result.Pages != 2 || len(rows) != 2 || rows["1"]["v"] != "b" {
		t.Fatalf("result %+v rows %v", result, rows)
	}
	if tombstone, ok := TombstoneFor(result.Tombstones, "3"); !ok || tombstone.Reason != "deleted" {
		t.Fatalf("tombstones %+v", result.Tombstones)
	}
	_, err = api.Sync(context.Background(), "/list", "bogus", 0)
	var refused *SyncError
	if !errors.As(err, &refused) || refused.Code != "invalid_sync_cursor" {
		t.Fatalf("bogus cursor: %v", err)
	}
	if fmt.Sprint(cursors) != "[0 c1 bogus]" {
		t.Fatalf("cursors sent %v", cursors)
	}
}

func TestSyncRefusesACursorThatDoesNotMove(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"total":0,"rows":[],"next_cursor":"same","has_more":true}`)
	}))
	defer server.Close()
	if _, err := New(server.URL).Sync(context.Background(), "/list", "same", 0); err == nil {
		t.Fatal("an endless has_more was accepted")
	}
}

func TestDeviceRevokedNeedsTheStringErrorOn401(t *testing.T) {
	for body, want := range map[string]bool{
		`{"error":"device_revoked"}`:          true,
		`{"error":{"code":"device_revoked"}}`: false,
		`{"error":"token_rejected"}`:          false,
		`not json`:                            false,
	} {
		if got := IsDeviceRevoked(&Response{Status: http.StatusUnauthorized, Body: []byte(body)}); got != want {
			t.Errorf("%s: %v, want %v", body, got, want)
		}
	}
	if IsDeviceRevoked(&Response{Status: http.StatusBadRequest, Body: []byte(`{"error":"device_revoked"}`)}) {
		t.Error("a 400 read as device_revoked")
	}
}

func TestRequestsCarryVersionAndBearer(t *testing.T) {
	var header http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	api := New(server.URL).WithVersion("1.2.3").WithToken("tok")
	if _, err := api.Get(context.Background(), "/x"); err != nil {
		t.Fatal(err)
	}
	if header.Get(ClientVersionHeader) != "1.2.3" || header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("headers %v", header)
	}
	// The token endpoint is a public client's: no Authorization header.
	_, _ = api.Refresh(context.Background(), server.URL+"/token", "c", "r")
	if header.Get("Authorization") != "" || header.Get(ClientVersionHeader) != "1.2.3" {
		t.Fatalf("token request headers %v", header)
	}
}
