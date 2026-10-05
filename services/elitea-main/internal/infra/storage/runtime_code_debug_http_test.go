package storage

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type debugReadSpy struct {
	reader io.Reader
	reads  int
}

func (s *debugReadSpy) Read(p []byte) (int, error) { s.reads++; return s.reader.Read(p) }
func (s *debugReadSpy) Close() error               { return nil }
func debugHTTPRequest(t *testing.T, path string, raw []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	identity, _ := url.Parse("spiffe://elitea.internal/runtime/worker-debug")
	certificate := certificateWithURI(identity)
	r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{certificate}}}
	r.Header.Set(claimIDHeader, "claim-debug")
	r.Header.Set(fenceHeader, base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)))
	r.Header.Set("Content-Type", "application/json")
	return r
}
func debugHTTPRouter(service *RuntimeCodeDebugArtifactService) http.Handler {
	r := chi.NewRouter()
	r.Mount("/executions/{executionID}/generations/{generation}/code-debug", service.Routes())
	return r
}
func TestCodeDebugHTTPRejectsBinaryBeforeReadingWhenAuthorityIsDenied(t *testing.T) {
	a, raw := debugFixture(t)
	repository := &debugRepositoryStub{admission: a}
	calls := 0
	authority := debugAuthorityFunc(func(context.Context, ContentClaim, CodeDebugAdmission) (CodeDebugAuthorization, error) {
		calls++
		return CodeDebugAuthorization{}, ErrContentUnauthorized
	})
	repository.authority = authority
	service, _ := NewRuntimeCodeDebugArtifactService(repository)
	request := debugHTTPRequest(t, "/executions/execution-debug/generations/1/code-debug/commit/"+a.OriginalVisit.VisitID, raw)
	spy := &debugReadSpy{reader: bytes.NewReader(raw)}
	request.Body = spy
	response := httptest.NewRecorder()
	debugHTTPRouter(service).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || spy.reads != 0 || repository.committed != 0 || calls != 1 {
		t.Fatalf("status=%d reads=%d writes=%d checks=%d", response.Code, spy.reads, repository.committed, calls)
	}
	if strings.Contains(response.Body.String(), a.ConfigurationJSON) {
		t.Fatal("private configuration entered response")
	}
}
func TestCodeDebugHTTPBoundedAdmissionAndStrictUnknownFields(t *testing.T) {
	a, _ := debugFixture(t)
	repository := new(debugRepositoryStub)
	calls := 0
	authority := debugAuthorityFunc(func(context.Context, ContentClaim, CodeDebugAdmission) (CodeDebugAuthorization, error) {
		calls++
		return CodeDebugAuthorization{}, ErrContentUnauthorized
	})
	repository.authority = authority
	service, _ := NewRuntimeCodeDebugArtifactService(repository)
	encoded, _ := json.Marshal(a)
	var extra map[string]any
	_ = json.Unmarshal(encoded, &extra)
	extra["project_id"] = 7
	unknown, _ := json.Marshal(extra)
	for _, raw := range [][]byte{bytes.Repeat([]byte{'x'}, MaxCodeDebugAdmissionBytes+1), unknown} {
		response := httptest.NewRecorder()
		debugHTTPRouter(service).ServeHTTP(response, debugHTTPRequest(t, "/executions/execution-debug/generations/1/code-debug/admit", raw))
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatal(response.Code)
		}
	}
	if calls != 0 || repository.staged != 0 {
		t.Fatal("invalid admission reached authority or staging")
	}
}
func TestCodeDebugHTTPOverloadHasNoQueueOrArtifact(t *testing.T) {
	repository := &debugRepositoryStub{authority: debugAuthorityFunc(func(context.Context, ContentClaim, CodeDebugAdmission) (CodeDebugAuthorization, error) {
		t.Fatal("overload reached authority")
		return CodeDebugAuthorization{}, nil
	})}
	service, _ := NewRuntimeCodeDebugArtifactService(repository)
	for i := 0; i < cap(service.slots); i++ {
		service.slots <- struct{}{}
	}
	response := httptest.NewRecorder()
	debugHTTPRouter(service).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/executions/execution-debug/generations/1/code-debug/admit", nil))
	if response.Code != http.StatusTooManyRequests {
		t.Fatal(response.Code)
	}
}
