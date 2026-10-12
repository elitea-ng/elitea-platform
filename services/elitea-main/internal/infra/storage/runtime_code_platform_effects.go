package storage

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codeplatform"
	code "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/codesandbox"
)

// CodePlatformEffectsBodyLimit equals the step route's bound: the request is
// the same closed four-field selector.
const CodePlatformEffectsBodyLimit = CodePlatformStepBodyLimit

// The only two response dispositions. Neither carries an effect id, state or count.
const (
	CodePlatformEffectsNone     = "none"
	CodePlatformEffectsPossible = "possible"
)

// CodePlatformUnresolvedEffectReader is a plain read of Main's durable platform
// call journal inside the caller's already-authorized transaction. It reports
// whether any call of this exact job holds an outcome that is not known. It
// writes nothing and an error is never a "none" answer.
type CodePlatformUnresolvedEffectReader interface {
	ReadOriginalCodePlatformUnresolvedEffects(ctx context.Context, tx CodeTransaction, execution string, generation uint64, dispatch, fingerprint string) (bool, error)
}

// These structs implement code_platform.proto's explicit JSON mapping.
type CodePlatformEffectsRequest struct {
	Schema                     string `json:"schema"`
	Revision                   uint8  `json:"revision"`
	DispatchActivation         string `json:"dispatch_activation"`
	PreparedRequestFingerprint string `json:"prepared_request_fingerprint"`
}
type CodePlatformEffectsResponse struct {
	Schema      string `json:"schema"`
	Revision    uint8  `json:"revision"`
	Disposition string `json:"disposition"`
}

func ParseCodePlatformEffects(raw []byte) (CodePlatformEffectsRequest, error) {
	var request CodePlatformEffectsRequest
	if code.Decode(raw, &request, CodePlatformEffectsBodyLimit) != nil || request.Schema != "elitea.runtime.code-platform-effects-request.v1" || request.Revision != 1 || !code.NonzeroDigest(request.DispatchActivation) || !code.NonzeroDigest(request.PreparedRequestFingerprint) {
		return CodePlatformEffectsRequest{}, domain.ErrFrame
	}
	return request, nil
}

// WithEffectJournal attaches the journal reader from trusted Main composition.
// Without it Effects fails closed; it never answers "none" from nothing.
func (p *CodePlatformPump) WithEffectJournal(reader CodePlatformUnresolvedEffectReader) *CodePlatformPump {
	if p != nil {
		p.effects = reader
	}
	return p
}

// Effects answers whether the journal of this dispatch activation holds a call
// whose outcome is not known. The read runs in the same transaction that checks
// the claim fence and lease, the RUNNING execution, the registered intent and
// prepared-fingerprint binding, and execution/project scoping, so authorization
// fails closed together with the read. It performs no owner IO and no write.
func (p *CodePlatformPump) Effects(ctx context.Context, claim ContentClaim, dispatch, fingerprint string) (bool, error) {
	if p == nil || ctx == nil || claim.PeerCertificate == nil || !code.NonzeroDigest(dispatch) || !code.NonzeroDigest(fingerprint) {
		return false, domain.ErrUnauthorized
	}
	if p.effects == nil || p.intents == nil {
		return false, domain.ErrUnavailable
	}
	var possible bool
	err := p.intents.WithRegisteredCodePlatformIntent(ctx, claim, dispatch, fingerprint, func(ctx context.Context, tx CodeTransaction, intent RegisteredCodeIntent) error {
		// The registered broker binding must exist and match the selector; a job
		// without it was never admitted for platform calls.
		broker, err := ReadRegisteredCodeBroker(ctx, tx, intent)
		if err != nil {
			return err
		}
		o := intent.Original
		if o.ExecutionID != claim.ExecutionID || o.OriginalGeneration != claim.Generation || intent.Binding.DispatchActivation != dispatch || broker.PreparedFingerprint != fingerprint {
			return code.ErrRejected
		}
		possible, err = p.effects.ReadOriginalCodePlatformUnresolvedEffects(ctx, tx, o.ExecutionID, o.OriginalGeneration, dispatch, broker.PreparedFingerprint)
		return err
	})
	if err != nil {
		return false, err
	}
	return possible, nil
}

// PostCodePlatformEffects serves the read-only journal question. Claim parsing,
// admission, body bound and status mapping are identical to the step route.
func (s *ContentServer) PostCodePlatformEffects(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.codePlatformPump == nil || r.Method != http.MethodPost || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	claim, err := parseExecutionClaim(r)
	if err != nil {
		http.Error(w, "Code platform claim denied", http.StatusForbidden)
		return
	}
	if !s.acquire(w) {
		return
	}
	defer s.release()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, CodePlatformEffectsBodyLimit))
	if err != nil {
		http.Error(w, "Invalid Code platform effects request", http.StatusBadRequest)
		return
	}
	request, err := ParseCodePlatformEffects(raw)
	if err != nil {
		http.Error(w, "Invalid Code platform effects request", http.StatusBadRequest)
		return
	}
	possible, err := s.codePlatformPump.Effects(r.Context(), claim, request.DispatchActivation, request.PreparedRequestFingerprint)
	if err != nil {
		writeCodePlatformError(w, err, "Invalid Code platform effects request")
		return
	}
	response := CodePlatformEffectsResponse{Schema: "elitea.runtime.code-platform-effects-response.v1", Revision: 1, Disposition: CodePlatformEffectsNone}
	if possible {
		response.Disposition = CodePlatformEffectsPossible
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(response)
}
