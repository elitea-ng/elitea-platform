package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"
)

func debugContentServer(service *RuntimeCodeDebugArtifactService) *ContentServer {
	return (&ContentServer{}).WithCodeDebugArtifacts(service)
}

func debugCallContext(t *testing.T) context.Context {
	t.Helper()
	identity, _ := url.Parse("spiffe://elitea.internal/runtime/worker-debug")
	return contentClaimContext(context.Background(), certificateWithURI(identity), "claim-debug", bytes.Repeat([]byte{5}, 32))
}

func TestCodeDebugCommitChecksAuthorityBeforeStoring(t *testing.T) {
	a, raw := debugFixture(t)
	repository := &debugRepositoryStub{admission: a}
	calls := 0
	repository.authority = debugAuthorityFunc(func(context.Context, ContentClaim, CodeDebugAdmission) (CodeDebugAuthorization, error) {
		calls++
		return CodeDebugAuthorization{}, ErrContentUnauthorized
	})
	service, _ := NewRuntimeCodeDebugArtifactService(repository)
	_, err := debugContentServer(service).CommitCodeDebug(debugCallContext(t), &runtimev1.CommitCodeDebugRequestV1{
		Scope:        contentScope("execution-debug", 1),
		VisitId:      a.OriginalVisit.VisitID,
		SnapshotJson: raw,
	})
	requireContentCode(t, err, codes.PermissionDenied)
	if repository.committed != 0 || calls != 1 {
		t.Fatalf("writes=%d checks=%d", repository.committed, calls)
	}
	if strings.Contains(err.Error(), a.ConfigurationJSON) {
		t.Fatal("private configuration entered response")
	}
}

func TestCodeDebugBoundedAdmissionAndStrictUnknownFields(t *testing.T) {
	a, _ := debugFixture(t)
	repository := new(debugRepositoryStub)
	calls := 0
	repository.authority = debugAuthorityFunc(func(context.Context, ContentClaim, CodeDebugAdmission) (CodeDebugAuthorization, error) {
		calls++
		return CodeDebugAuthorization{}, ErrContentUnauthorized
	})
	service, _ := NewRuntimeCodeDebugArtifactService(repository)
	encoded, _ := json.Marshal(a)
	var extra map[string]any
	_ = json.Unmarshal(encoded, &extra)
	extra["project_id"] = 7
	unknown, _ := json.Marshal(extra)
	for _, raw := range [][]byte{bytes.Repeat([]byte{'x'}, MaxCodeDebugAdmissionBytes+1), unknown} {
		_, err := debugContentServer(service).AdmitCodeDebug(debugCallContext(t), &runtimev1.AdmitCodeDebugRequestV1{
			Scope:         contentScope("execution-debug", 1),
			AdmissionJson: raw,
		})
		requireContentCode(t, err, codes.FailedPrecondition)
	}
	if calls != 0 || repository.staged != 0 {
		t.Fatal("invalid admission reached authority or staging")
	}
}

func TestCodeDebugOverloadHasNoQueueOrArtifact(t *testing.T) {
	repository := &debugRepositoryStub{authority: debugAuthorityFunc(func(context.Context, ContentClaim, CodeDebugAdmission) (CodeDebugAuthorization, error) {
		t.Fatal("overload reached authority")
		return CodeDebugAuthorization{}, nil
	})}
	service, _ := NewRuntimeCodeDebugArtifactService(repository)
	for i := 0; i < cap(service.slots); i++ {
		service.slots <- struct{}{}
	}
	_, err := debugContentServer(service).AdmitCodeDebug(debugCallContext(t), &runtimev1.AdmitCodeDebugRequestV1{Scope: contentScope("execution-debug", 1)})
	requireContentCode(t, err, codes.Unavailable)
}

func TestCodeDebugWithoutServiceIsUnimplemented(t *testing.T) {
	_, err := (&ContentServer{}).AdmitCodeDebug(debugCallContext(t), &runtimev1.AdmitCodeDebugRequestV1{Scope: contentScope("execution-debug", 1)})
	requireContentCode(t, err, codes.Unimplemented)
}
