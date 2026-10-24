package conversations_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/conversations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

type fakeNamer struct {
	answer   string
	err      error
	requests []conversations.NameRequest
	// onCall runs during the model call (a person renaming meanwhile).
	onCall func()
}

func (f *fakeNamer) Complete(_ context.Context, request conversations.NameRequest) (string, error) {
	f.requests = append(f.requests, request)
	if f.onCall != nil {
		f.onCall()
	}
	return f.answer, f.err
}

// namingRepo stores one conversation's name and serves two messages.
func namingRepo(name *string, updates *[]string) *mockRepo {
	return &mockRepo{
		getFn: func(context.Context, string, string) (conversations.Conversation, error) {
			return conversations.Conversation{Name: *name}, nil
		},
		updateFn: func(_ context.Context, _, _ string, conv conversations.Conversation) (conversations.Conversation, error) {
			*updates = append(*updates, conv.Name)
			*name = conv.Name
			return conversations.Conversation{Name: conv.Name}, nil
		},
		listMessagesFn: func(_ context.Context, _, _ string, query conversations.MessagesQuery) (conversations.MessagesListResponse, error) {
			if query.SortOrder != "asc" {
				return conversations.MessagesListResponse{}, errors.New("the first exchange is read oldest first")
			}
			return conversations.MessagesListResponse{Items: []conversations.Message{
				{Role: "user", Content: "how do I rotate the gateway's TLS certificate"},
				{Role: "assistant", Content: "Run the rotation job, then restart the pods."},
			}}, nil
		},
	}
}

func generateName(t *testing.T, h *conversations.Handler, body string) (int, map[string]any) {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/conversation_name/{projectID}/{conversationID}", h.GenerateName)
	req := httptest.NewRequest(http.MethodPost, "/conversation_name/7/42", strings.NewReader(body))
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "9", UserID: "9"}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestGenerateNameRenamesAConversationThatKeepsItsFirstName(t *testing.T) {
	name, updates := "how do I rotate the gateway's TLS", []string{}
	namer := &fakeNamer{answer: "\"Rotating the gateway TLS certificate.\"\n"}
	h := conversations.NewHandler(namingRepo(&name, &updates)).WithNamer(namer)

	code, out := generateName(t, h, `{"expected_name":"how do I rotate the gateway's TLS"}`)
	if code != http.StatusOK || out["renamed"] != true || out["name"] != "Rotating the gateway TLS certificate" {
		t.Fatalf("got %d %v", code, out)
	}
	if len(updates) != 1 || updates[0] != "Rotating the gateway TLS certificate" {
		t.Fatalf("updates %v", updates)
	}
	if len(namer.requests) != 1 {
		t.Fatalf("model calls %d", len(namer.requests))
	}
	request := namer.requests[0]
	if request.ProjectID != "7" || request.UserID != "9" {
		t.Fatalf("billed to %q/%q", request.ProjectID, request.UserID)
	}
	if !strings.Contains(request.User, "<question>\nhow do I rotate") || !strings.Contains(request.User, "<answer>\nRun the rotation job") {
		t.Fatalf("exchange %q", request.User)
	}
}

func TestGenerateNameKeepsAPersonsRename(t *testing.T) {
	name, updates := "My TLS notes", []string{}
	namer := &fakeNamer{answer: "Gateway TLS rotation"}
	h := conversations.NewHandler(namingRepo(&name, &updates)).WithNamer(namer)

	code, out := generateName(t, h, `{"expected_name":"how do I rotate the gateway's TLS"}`)
	if code != http.StatusOK || out["renamed"] != false || out["name"] != "My TLS notes" {
		t.Fatalf("got %d %v", code, out)
	}
	if len(namer.requests) != 0 || len(updates) != 0 {
		t.Fatalf("called the model %d times, renamed %v", len(namer.requests), updates)
	}
}

func TestGenerateNameKeepsARenameMadeDuringTheModelCall(t *testing.T) {
	name, updates := "first words", []string{}
	namer := &fakeNamer{answer: "Gateway TLS rotation"}
	namer.onCall = func() { name = "Renamed meanwhile" }
	h := conversations.NewHandler(namingRepo(&name, &updates)).WithNamer(namer)

	code, out := generateName(t, h, `{"expected_name":"first words"}`)
	if code != http.StatusOK || out["renamed"] != false || out["name"] != "Renamed meanwhile" || len(updates) != 0 {
		t.Fatalf("got %d %v, updates %v", code, out, updates)
	}
}

func TestGenerateNameRefusesWithoutAModelOrAnAnswer(t *testing.T) {
	name, updates := "first words", []string{}
	if code, _ := generateName(t, conversations.NewHandler(namingRepo(&name, &updates)), `{"expected_name":"first words"}`); code != http.StatusServiceUnavailable {
		t.Fatalf("no namer: %d", code)
	}
	for _, namer := range []*fakeNamer{{err: errors.New("gateway down")}, {answer: "  \n\"\" "}} {
		h := conversations.NewHandler(namingRepo(&name, &updates)).WithNamer(namer)
		if code, _ := generateName(t, h, `{"expected_name":"first words"}`); code != http.StatusBadGateway {
			t.Fatalf("answer %q err %v: %d", namer.answer, namer.err, code)
		}
	}
	h := conversations.NewHandler(namingRepo(&name, &updates)).WithNamer(&fakeNamer{answer: "x"})
	if code, _ := generateName(t, h, `{}`); code != http.StatusBadRequest {
		t.Fatalf("no expected_name: %d", code)
	}
	if len(updates) != 0 {
		t.Fatalf("renamed %v", updates)
	}
}
