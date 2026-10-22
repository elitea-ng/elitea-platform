package agentexecution

// #606: `payload.attachments` on the start route. Before this, the field was
// undeclared on currentApplicationStartBody, so the decoder dropped it
// silently and no attachment ever reached the admission — the reason the
// chat_messages_attachment table had a reader and no writer.

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

func TestCurrentApplicationStartRouteCarriesPayloadAttachmentsToTheUseCase(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-1", CommandID: "command-1", ResponseMessageID: "response-1",
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(
		validCurrentStartBody(),
		`"payload":{"user_input":"hello"}`,
		`"payload":{"user_input":"hello","attachments":[`+
			`{"filepath":"/chat-attachments/8bc66e50-46c4-4e2c-94ec-daec6c596ac0/report.pdf","name":"report.pdf"},`+
			`{"filepath":"/other-bucket/8bc66e50-46c4-4e2c-94ec-daec6c596ac0/shot.png","name":"shot.png"}]}`,
		1,
	)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(body))

	if response.Code != http.StatusOK || useCase.calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.calls, response.Body.String())
	}
	// The conversation-uuid prefix STAYS in the name — that is what makes the
	// name address the stored object, and what DeleteMessage feeds back to the
	// object store. The client's own `name` field ("report.pdf") is a display
	// basename and must NOT win here.
	want := []agentexecutionapp.CurrentTurnAttachmentRef{
		{Bucket: "chat-attachments", Name: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0/report.pdf"},
		{Bucket: "other-bucket", Name: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0/shot.png"},
	}
	if !reflect.DeepEqual(useCase.request.Attachments, want) {
		t.Fatalf("attachments=%+v want=%+v", useCase.request.Attachments, want)
	}
}

func TestCurrentAdhocStartRouteCarriesPayloadAttachmentsToTheUseCase(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-adhoc", CommandID: "command-adhoc",
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(
		validCurrentAdhocStartBody(),
		`"payload":{"user_input":"hello from main chat"}`,
		`"payload":{"user_input":"hello from main chat","attachments":[`+
			`{"filepath":"/chat-attachments/8bc66e50-46c4-4e2c-94ec-daec6c596ac0/notes.md","name":"notes.md"}]}`,
		1,
	)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentAdhocStartRequest(body))

	if response.Code != http.StatusOK || useCase.adhocCalls != 1 {
		t.Fatalf("status=%d adhoc_calls=%d body=%s",
			response.Code, useCase.adhocCalls, response.Body.String())
	}
	want := []agentexecutionapp.CurrentTurnAttachmentRef{
		{Bucket: "chat-attachments", Name: "8bc66e50-46c4-4e2c-94ec-daec6c596ac0/notes.md"},
	}
	if !reflect.DeepEqual(useCase.adhocRequest.Attachments, want) {
		t.Fatalf("attachments=%+v want=%+v", useCase.adhocRequest.Attachments, want)
	}
}

func TestCurrentApplicationStartRouteRejectsMalformedAttachmentFilepathBeforeUseCase(t *testing.T) {
	// Every one of these fails the legacy parse_filepath: after stripping one
	// leading slash there is no further slash, or one side of the split is
	// empty. A start carrying one is refused whole — never admitted with the
	// offending entry dropped, which would produce a turn the user believes
	// carried their file.
	for _, filepath := range []string{
		"",
		"/",
		"//name.pdf",
		"/bucket-only",
		"/bucket/",
		"no-leading-slash-and-no-bucket",
	} {
		t.Run(filepath, func(t *testing.T) {
			useCase := &currentStartUseCaseStub{}
			route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
			body := strings.Replace(
				validCurrentStartBody(),
				`"payload":{"user_input":"hello"}`,
				`"payload":{"user_input":"hello","attachments":[{"filepath":"`+filepath+`","name":"x"}]}`,
				1,
			)

			response := httptest.NewRecorder()
			route.ServeHTTP(response, currentStartRequest(body))

			if response.Code != http.StatusBadRequest || useCase.calls != 0 {
				t.Fatalf("status=%d calls=%d body=%s",
					response.Code, useCase.calls, response.Body.String())
			}
		})
	}
}

func TestCurrentApplicationStartRouteLeavesAttachmentsEmptyWhenTheClientSendsNone(t *testing.T) {
	useCase := &currentStartUseCaseStub{outcome: agentexecutionapp.CurrentApplicationStartOutcome{
		ExecutionID: "execution-1",
	}}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(validCurrentStartBody()))

	if response.Code != http.StatusOK || useCase.calls != 1 ||
		len(useCase.request.Attachments) != 0 {
		t.Fatalf("status=%d calls=%d attachments=%+v",
			response.Code, useCase.calls, useCase.request.Attachments)
	}
}

// The unported `attachments_info` gate is a DIFFERENT field and must keep
// refusing, or this change would quietly widen the parity slice.
func TestCurrentApplicationStartRouteStillRefusesAttachmentsInfo(t *testing.T) {
	useCase := &currentStartUseCaseStub{}
	route := newCurrentStartRoute(t, useCase, allowCurrentStartPermission())
	body := strings.Replace(
		validCurrentStartBody(),
		`"attachments_info":[]`,
		`"attachments_info":[{"filepath":"/chat-attachments/x/y.pdf"}]`,
		1,
	)

	response := httptest.NewRecorder()
	route.ServeHTTP(response, currentStartRequest(body))

	if response.Code != http.StatusUnprocessableEntity || useCase.calls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, useCase.calls, response.Body.String())
	}
}
