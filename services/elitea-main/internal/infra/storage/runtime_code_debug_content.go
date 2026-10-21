package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	runtimev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"
	"google.golang.org/grpc/codes"
)

// codeDebugDeadline bounds one Code debug call. No source or binary payload
// passes through the command bus or a public control route.
const codeDebugDeadline = 5 * time.Second

// AdmitCodeDebug stages one Code debug visit under the live claim.
func (s *ContentServer) AdmitCodeDebug(ctx context.Context, request *runtimev1.AdmitCodeDebugRequestV1) (*runtimev1.AdmitCodeDebugResponseV1, error) {
	if s.codeDebug == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	ctx, release, err := s.codeDebug.admitCall(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, codeDebugStatus(err)
	}
	raw := request.GetAdmissionJson()
	if len(raw) > MaxCodeDebugAdmissionBytes {
		return nil, codeDebugStatus(ErrContentRejected)
	}
	var admission CodeDebugAdmission
	if strictCodeDebugJSON(raw, &admission) != nil {
		return nil, codeDebugStatus(ErrContentRejected)
	}
	if err = s.codeDebug.Admit(ctx, claim, admission); err != nil {
		return nil, codeDebugStatus(err)
	}
	return &runtimev1.AdmitCodeDebugResponseV1{}, nil
}

// CommitCodeDebug stores the snapshot of one admitted visit.
func (s *ContentServer) CommitCodeDebug(ctx context.Context, request *runtimev1.CommitCodeDebugRequestV1) (*runtimev1.CommitCodeDebugResponseV1, error) {
	if s.codeDebug == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	ctx, release, err := s.codeDebug.admitCall(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, codeDebugStatus(err)
	}
	raw := request.GetSnapshotJson()
	if len(raw) < 1 || len(raw) > MaxCodeDebugSnapshotBytes {
		return nil, codeDebugStatus(ErrContentRejected)
	}
	upload, err := s.codeDebug.prepareCommit(ctx, claim, request.GetVisitId())
	if err != nil {
		return nil, codeDebugStatus(err)
	}
	ref, err := s.codeDebug.commitAuthorized(ctx, claim, upload, raw)
	if err != nil {
		return nil, codeDebugStatus(err)
	}
	body, err := json.Marshal(ref)
	if err != nil || len(body) > 4096 {
		return nil, codeDebugStatus(ErrContentUnavailable)
	}
	return &runtimev1.CommitCodeDebugResponseV1{ReferenceJson: body}, nil
}

// admitCall: four calls share a fixed memory budget and none waits in a
// queue; each runs under its own short deadline.
func (s *RuntimeCodeDebugArtifactService) admitCall(ctx context.Context) (context.Context, func(), error) {
	select {
	case s.slots <- struct{}{}:
	default:
		return nil, nil, contentStatus(codes.Unavailable)
	}
	ctx, cancel := context.WithTimeout(ctx, codeDebugDeadline)
	return ctx, func() { cancel(); <-s.slots }, nil
}

func codeDebugStatus(err error) error {
	switch {
	case errors.Is(err, ErrContentUnauthorized):
		return contentStatus(codes.PermissionDenied)
	case errors.Is(err, ErrContentNotFound):
		return contentStatus(codes.NotFound)
	case errors.Is(err, ErrContentRejected):
		return contentStatus(codes.FailedPrecondition)
	}
	return contentStatus(codes.Unavailable)
}
