package configurations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/llmproxy"
)

// provider_voice_listing.go is the elitea-main half of the TTS voice listing
// (issue 323).
//
// # What was missing, and what is here now
//
// `GET /configurations/tts_voices/{projectID}` answered 501 for every project,
// with a reason that named the two things that did not exist: no route anywhere
// could ask a provider which voices it has, and nothing ever wrote the
// `meta.voices` cache the reference falls back on. Both now exist. The gateway
// serves `POST /llm/v1/list_provider_voices`, and this file is the client for
// it; `TTSVoices` fills the cache from the answer.
//
// # Why this rides the connection checker
//
// The same reason ProviderModelLister does: one hop, one mTLS transport, one
// identity secret, one operator configuration. A second option would be a
// second thing to forget, and forgetting it would give a deployment a voice
// picker that is empty for a reason nothing states.

// maxProviderVoiceListingBytes bounds the gateway's reply. A voice catalogue is
// tens of entries; this is three orders of magnitude of headroom and still a
// bound, so a broken gateway cannot stream into this process.
const maxProviderVoiceListingBytes = 1 << 18 // 256 KiB

// maxProviderVoices bounds how many voices reach a browser. The reference's
// largest catalogue is thirty.
const maxProviderVoices = 500

// maxProviderVoiceFieldLength bounds one id or display name.
const maxProviderVoiceFieldLength = 200

// ProviderVoice is one selectable voice, in the reference's own shape
// (`{"id": ..., "name": ...}`) because the web voice picker was ported from
// the client that read it.
type ProviderVoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ProviderVoiceListing is one provider's answer to "which voices does this
// model have".
//
// Success true with an EMPTY Voices is a real answer and the common one: it
// means the provider publishes no catalogue this build knows, and the caller
// keeps its own default voice. It is not a failure and must never be reported
// as one.
type ProviderVoiceListing struct {
	Success bool
	Voices  []ProviderVoice
}

// ProviderVoiceLister reads the voices a TTS model can be asked for.
//
// Implementations MUST NOT dial a provider directly: any round trip belongs to
// the gateway, which owns the egress allowlist.
type ProviderVoiceLister interface {
	ListProviderVoices(ctx context.Context, providerType, model string) (ProviderVoiceListing, error)
}

// providerVoicesResponseBody mirrors elitea-llm-gateway's
// internal/llmproxy.listProviderVoicesResponse.
type providerVoicesResponseBody struct {
	Success bool            `json:"success"`
	Reason  string          `json:"reason"`
	Voices  []ProviderVoice `json:"voices"`
}

// ListProviderVoices implements ProviderVoiceLister on the SAME client the
// stored connection check and the model listing already use.
//
// A transport-level failure is returned as an ERROR. The caller must not map
// that to an empty catalogue: "the gateway did not answer" and "this provider
// has no voices" are different states, and only one of them is worth telling an
// operator about.
func (c *GatewayConnectionChecker) ListProviderVoices(
	ctx context.Context, providerType, model string,
) (ProviderVoiceListing, error) {
	if c == nil {
		// A typed nil boxed into the interface makes a caller's `lister == nil`
		// test FALSE — the trap recorded on WithProviderAdmission. The guard
		// belongs here as well as at the composition root.
		return ProviderVoiceListing{}, errors.New("list provider voices: the gateway client is not composed")
	}

	raw, err := json.Marshal(map[string]string{"type": providerType, "model": model})
	if err != nil {
		return ProviderVoiceListing{}, fmt.Errorf("list provider voices: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/llm/v1/list_provider_voices", strings.NewReader(string(raw)))
	if err != nil {
		return ProviderVoiceListing{}, fmt.Errorf("list provider voices: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// No execution id: a voice listing is a read on behalf of a signed-in user
	// looking at a picker, not a runtime execution.
	llmproxy.SignIdentityHeaders(req.Header, c.identitySecret, connectionCheckProjectIDFrom(ctx), "", "", "")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ProviderVoiceListing{}, fmt.Errorf("list provider voices: call gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The route answers 200 for every provider verdict, so a non-200 is
		// this hop failing — an unmounted route on an older gateway, a refused
		// signature, a proxy error. It is never a verdict about the model.
		return ProviderVoiceListing{}, fmt.Errorf(
			"list provider voices: gateway responded with status %d", resp.StatusCode)
	}

	var out providerVoicesResponseBody
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxProviderVoiceListingBytes)).Decode(&out); err != nil {
		return ProviderVoiceListing{}, fmt.Errorf("list provider voices: decode gateway response: %w", err)
	}
	return ProviderVoiceListing{Success: out.Success, Voices: boundProviderVoices(out.Voices)}, nil
}

// boundProviderVoices applies this process's own bounds to what the gateway
// sent, drops duplicates and keeps the provider's order.
//
// The order matters: a provider lists its catalogue in the order it wants a
// user to see, and the picker offers the FIRST entry as the default. Sorting
// here would silently change which voice a user gets by doing nothing.
func boundProviderVoices(voices []ProviderVoice) []ProviderVoice {
	bounded := make([]ProviderVoice, 0, len(voices))
	seen := make(map[string]struct{}, len(voices))
	for _, voice := range voices {
		id := strings.TrimSpace(voice.ID)
		if id == "" || len(id) > maxProviderVoiceFieldLength {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		name := strings.TrimSpace(voice.Name)
		if name == "" || len(name) > maxProviderVoiceFieldLength {
			// A voice with no display name would render as a blank option.
			// Falling back to the id is what the reference's own picker does.
			name = id
		}
		seen[id] = struct{}{}
		bounded = append(bounded, ProviderVoice{ID: id, Name: name})
		if len(bounded) == maxProviderVoices {
			break
		}
	}
	return bounded
}

// providerVoiceLister reports the voice-listing capability of the composed
// gateway client, or nil when this deployment has none.
//
// Read off the connection checker for the reason providerModelLister is; see
// that function's doc.
func (h *Handler) providerVoiceLister() ProviderVoiceLister {
	if h.connectionChecker == nil {
		return nil
	}
	lister, ok := h.connectionChecker.(ProviderVoiceLister)
	if !ok {
		return nil
	}
	return lister
}

// compile-time assertion that the composed gateway client really carries the
// voice-listing capability — the dead-wiring failure in the one shape a
// compiler can catch.
var _ ProviderVoiceLister = (*GatewayConnectionChecker)(nil)

// BoundProviderVoicesForTest exposes boundProviderVoices to the package's
// external test file. The bounds are the contract between the gateway and a
// browser — order, duplicates, blank labels, catalogue size — and they are worth
// asserting directly rather than only through a handler that would need a
// database to reach them.
func BoundProviderVoicesForTest(voices []ProviderVoice) []ProviderVoice {
	return boundProviderVoices(voices)
}
