package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

func testResumeCursor(now time.Time) resumeCursor {
	return resumeCursor{StreamID: "stream-1", ProjectID: 2, ActorID: 7, RequestID: json.RawMessage(`"call-17"`), ExecutionID: "execution-1", ResponseMessageID: "message-1", ApplicationID: 20, ApplicationVersionID: 30, ToolName: "agent", ExpiresAt: now.Add(time.Hour).Unix()}
}

func TestResumeCursorSurvivesReplacementAndRejectsInvalidInput(t *testing.T) {
	now := time.Now()
	key := bytes.Repeat([]byte{42}, 32)
	codec, err := NewResumeCursorCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	cursor := testResumeCursor(now)
	encoded, err := codec.seal(cursor, now)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := NewResumeCursorCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := replacement.open(encoded, now.Add(time.Minute))
	if err != nil || restored.ExecutionID != cursor.ExecutionID || !bytes.Equal(restored.RequestID, cursor.RequestID) {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
	encoded2, err := codec.seal(cursor, now)
	if err != nil || encoded == encoded2 {
		t.Fatal("cursor encryption reused its nonce")
	}
	other, _ := NewResumeCursorCodec(bytes.Repeat([]byte{43}, 32))
	if _, err := other.open(encoded, now); err == nil {
		t.Fatal("another key accepted cursor")
	}
	for _, bad := range []string{"", strings.Repeat("x", maxResumeCursorBytes+1), encoded[:len(encoded)/2], "!" + encoded[1:]} {
		if _, err := codec.open(bad, now); err == nil {
			t.Fatal("invalid cursor accepted")
		}
	}
	if _, err := codec.open(encoded, now.Add(time.Hour)); err == nil {
		t.Fatal("expired cursor accepted")
	}
	cursor.ExpiresAt = now.Add(2 * resumeCursorLifetime).Unix()
	if _, err := codec.seal(cursor, now); err == nil {
		t.Fatal("unbounded lifetime accepted")
	}
	if _, err := NewResumeCursorCodec(nil); err == nil {
		t.Fatal("missing master key accepted")
	}
}

func TestResumeGETChecksCallerScopeAndCurrentExportWithoutStartingWork(t *testing.T) {
	codec, _ := NewResumeCursorCodec(bytes.Repeat([]byte{42}, 32))
	for _, test := range []struct {
		name         string
		actor        int64
		path, accept string
		removed      bool
		want         int
	}{
		{"completed", 7, "/app/2/mcp", "text/event-stream", false, 204},
		{"different actor", 8, "/app/2/mcp", "text/event-stream", false, 403},
		{"different project", 7, "/app/3/mcp", "text/event-stream", false, 404},
		{"different scope", 7, "/app/2/mcp/applications", "text/event-stream", false, 404},
		{"removed export", 7, "/app/2/mcp", "text/event-stream", true, 403},
		{"wrong accept", 7, "/app/2/mcp", "application/json", false, 406},
	} {
		t.Run(test.name, func(t *testing.T) {
			cursor := testResumeCursor(time.Now())
			cursor.Complete = true
			encoded, err := codec.seal(cursor, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			source := &funcSource{fn: func(string, scope) ([]Tool, error) {
				if test.removed {
					return nil, nil
				}
				return []Tool{{Name: "agent", applicationID: 20, applicationVersionID: 30}}, nil
			}}
			start := &recordingStart{}
			permissions := allowRuns()
			permissions.userID = test.actor
			handler := NewHandler(nil, nil, start, permissions, WithResumeCursorCodec(codec))
			handler.source = source
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			request = request.WithContext(auth.ContextWithUser(request.Context(), auth.User{ID: "7", UserID: "7"}))
			request.Header.Set("Accept", test.accept)
			request.Header.Set("Last-Event-ID", encoded)
			response := httptest.NewRecorder()
			newTestRouter(handler).ServeHTTP(response, request)
			if response.Code != test.want || start.calls != 0 {
				t.Fatalf("status=%d want=%d admissions=%d body=%s", response.Code, test.want, start.calls, response.Body.String())
			}
		})
	}
}

func TestResumeStreamFramesKeepRequestIdentity(t *testing.T) {
	codec, _ := NewResumeCursorCodec(bytes.Repeat([]byte{42}, 32))
	cursor := testResumeCursor(time.Now())
	encoded, err := codec.seal(cursor, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	if err := startResumeStream(response, encoded); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{resumeCodec: codec}
	handler.finishResumeStream(response, cursor, newResult(cursor.RequestID, map[string]any{"content": []any{}}))
	body := response.Body.String()
	if response.Header().Get("Content-Type") != "text/event-stream" || !response.Flushed || !strings.Contains(body, "data:\nretry: 1000\n\n") || strings.Count(body, "event: message") != 1 || !strings.Contains(body, `"id":"call-17"`) {
		t.Fatalf("invalid SSE response: %s", body)
	}
	var ids []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "id: ") {
			ids = append(ids, strings.TrimPrefix(line, "id: "))
		}
	}
	if len(ids) != 2 {
		t.Fatalf("ids=%v", ids)
	}
	final, err := codec.open(ids[1], time.Now())
	if err != nil || !final.Complete || final.StreamID != cursor.StreamID {
		t.Fatalf("final cursor=%#v err=%v", final, err)
	}
}
