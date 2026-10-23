package storage

import (
	"context"
	"encoding/json"
	"errors"

	runtimev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/runtime/v1"

	domain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/codesandbox"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

const CodePlatformStepBodyLimit = 512

// These structs implement code_platform.proto's explicit JSON mapping.
type CodePlatformStepRequest struct {
	Schema                     string `json:"schema"`
	Revision                   uint8  `json:"revision"`
	DispatchActivation         string `json:"dispatch_activation"`
	PreparedRequestFingerprint string `json:"prepared_request_fingerprint"`
}
type CodePlatformStepResponse struct {
	Schema      string  `json:"schema"`
	Revision    uint8   `json:"revision"`
	Disposition string  `json:"disposition"`
	EffectID    *string `json:"effect_id"`
}

func ParseCodePlatformStep(raw []byte) (CodePlatformStepRequest, error) {
	var request CodePlatformStepRequest
	if code.Decode(raw, &request, CodePlatformStepBodyLimit) != nil || request.Schema != "elitea.runtime.code-platform-step-request.v1" || request.Revision != 1 || !code.NonzeroDigest(request.DispatchActivation) || !code.NonzeroDigest(request.PreparedRequestFingerprint) {
		return CodePlatformStepRequest{}, domain.ErrFrame
	}
	return request, nil
}
func (s *ContentServer) WithCodePlatformPump(pump *CodePlatformPump) *ContentServer {
	if s != nil {
		s.codePlatformPump = pump
	}
	return s
}

// StepCodePlatform runs one Code platform pump step for the claimed
// execution. The claim binds the exact scope execution/generation, the
// verified peer and the fence.
func (s *ContentServer) StepCodePlatform(ctx context.Context, request *runtimev1.StepCodePlatformRequestV1) (*runtimev1.StepCodePlatformResponseV1, error) {
	if s == nil || s.codePlatformPump == nil {
		return nil, contentStatus(codes.Unimplemented)
	}
	claim, err := parseExecutionClaim(ctx, request.GetScope())
	if err != nil {
		return nil, grpcstatus.Error(codes.PermissionDenied, "Code platform claim denied")
	}
	release, err := acquireContentSlot(s.requests)
	if err != nil {
		return nil, err
	}
	defer release()
	raw := request.GetRequestJson()
	if len(raw) > CodePlatformStepBodyLimit {
		return nil, grpcstatus.Error(codes.InvalidArgument, "Invalid Code platform step")
	}
	parsed, err := ParseCodePlatformStep(raw)
	if err != nil {
		return nil, grpcstatus.Error(codes.InvalidArgument, "Invalid Code platform step")
	}
	result, err := s.codePlatformPump.Step(ctx, claim, parsed.DispatchActivation, parsed.PreparedRequestFingerprint)
	if err != nil {
		return nil, codePlatformStatus(err, "Invalid Code platform step")
	}
	response := CodePlatformStepResponse{Schema: "elitea.runtime.code-platform-step-response.v1", Revision: 1, Disposition: "idle"}
	refused := grpcstatus.Error(codes.Aborted, "Code platform observation refused")
	switch result.State {
	case "idle":
		if result.EffectID != "" {
			return nil, refused
		}
	case "committed":
		response.Disposition = "committed"
		response.EffectID = &result.EffectID
	case "unknown_effect", "reply_delivery_unknown":
		response.Disposition = "unknown"
		response.EffectID = &result.EffectID
	default:
		return nil, refused
	}
	if response.EffectID != nil && !code.NonzeroDigest(*response.EffectID) {
		return nil, refused
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, contentStatus(codes.Unavailable)
	}
	return &runtimev1.StepCodePlatformResponseV1{ResponseJson: encoded}, nil
}

// codePlatformStatus is the single status mapping for the claim-bound Code
// platform RPCs: PermissionDenied authority denied, InvalidArgument invalid
// frame, Aborted binding refused, Unavailable anything else (including a
// missing dependency). It never echoes err.
func codePlatformStatus(err error, invalid string) error {
	switch {
	case errors.Is(err, domain.ErrUnauthorized), errors.Is(err, ErrContentUnauthorized):
		return grpcstatus.Error(codes.PermissionDenied, "Code platform authority denied")
	case errors.Is(err, domain.ErrFrame):
		return grpcstatus.Error(codes.InvalidArgument, invalid)
	case errors.Is(err, domain.ErrConflict), errors.Is(err, code.ErrRejected):
		return grpcstatus.Error(codes.Aborted, "Code platform original binding refused")
	default:
		return grpcstatus.Error(codes.Unavailable, "Code platform dependency unavailable")
	}
}
