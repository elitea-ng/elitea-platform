package storage

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
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
func (s *ContentServer) PostCodePlatformStep(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.codePlatformPump == nil || r.Method != http.MethodPost || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	// This parser binds the exact route execution/generation, verified peer and fence.
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, "Code platform claim denied", http.StatusForbidden)
		return
	}
	if !s.acquire(w) {
		return
	}
	defer s.release()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, CodePlatformStepBodyLimit))
	if err != nil {
		http.Error(w, "Invalid Code platform step", http.StatusBadRequest)
		return
	}
	request, err := ParseCodePlatformStep(raw)
	if err != nil {
		http.Error(w, "Invalid Code platform step", http.StatusBadRequest)
		return
	}
	result, err := s.codePlatformPump.Step(r.Context(), claim, request.DispatchActivation, request.PreparedRequestFingerprint)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrUnauthorized), errors.Is(err, ErrContentUnauthorized):
			http.Error(w, "Code platform authority denied", http.StatusForbidden)
		case errors.Is(err, domain.ErrFrame):
			http.Error(w, "Invalid Code platform step", http.StatusBadRequest)
		case errors.Is(err, domain.ErrConflict), errors.Is(err, code.ErrRejected):
			http.Error(w, "Code platform original binding refused", http.StatusConflict)
		default:
			http.Error(w, "Code platform dependency unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	response := CodePlatformStepResponse{Schema: "elitea.runtime.code-platform-step-response.v1", Revision: 1, Disposition: "idle"}
	switch result.State {
	case "idle":
		if result.EffectID != "" {
			http.Error(w, "Code platform observation refused", http.StatusConflict)
			return
		}
	case "committed":
		response.Disposition = "committed"
		response.EffectID = &result.EffectID
	case "unknown_effect", "reply_delivery_unknown":
		response.Disposition = "unknown"
		response.EffectID = &result.EffectID
	default:
		http.Error(w, "Code platform observation refused", http.StatusConflict)
		return
	}
	if response.EffectID != nil && !code.NonzeroDigest(*response.EffectID) {
		http.Error(w, "Code platform observation refused", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err = json.NewEncoder(w).Encode(response); err != nil {
		return
	}
}
