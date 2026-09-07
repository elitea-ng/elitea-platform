package llmproxy

import (
	"net/http"
	"strings"
)

// listprovidervoices.go implements POST /llm/v1/list_provider_voices — the
// source the TTS voice listing never had (issue 323).
//
// # The decision this route records
//
// The reference resolved a voice list from two places
// (legacy/plugins/configurations/utils_tts_voices.py's `fetch_tts_voices`): a
// STATIC table for every provider LiteLLM managed, and a live API call for the
// vendors that publish a voice catalogue of their own. The split is not an
// implementation detail — it is what the providers are. OpenAI has no voices
// endpoint at all; its set is a fixed literal in the API documentation. Azure
// OpenAI and Vertex accept the same identifiers because the proxy translates
// them. Gemini accepts only its own native names, and nothing translates those.
//
// So the answer to "which voices does this model have" is, for every provider
// this gateway speaks today, a TABLE LOOKUP and not a request. That is the
// decision: the voice set is resolved here, beside the provider knowledge that
// determines it, and served over the same signed internal contract as
// list_provider_models — so the day a provider that DOES enumerate is added
// (ElevenLabs, Deepgram, Azure Cognitive Services), it becomes a lister
// function in this file and no caller changes.
//
// # Why there is no cache in this file
//
// A TTL cache over a table lookup measures nothing and would read as a cache
// that does work it does not do. The cache the reference has is `meta.voices`
// on the project's tts configuration row, and elitea-main owns it — that is
// where a per-model cache belongs, because that is the layer a future LIVE
// lister would be expensive at.
//
// # Why an unknown provider is an empty list and not an error
//
// A provider whose voice set this build does not know is a provider whose
// caller must keep its own default voice. That is a normal state, not a
// failure, and `success: true` with an empty list is how a caller tells it
// apart from a refusal — the `reason` says which. Nothing here answers 501:
// the capability exists, and "this provider publishes no catalogue" is an
// answer.

// providerVoice is one selectable voice. The shape is the reference's own
// (`{"id": ..., "name": ...}`), because the web voice picker was ported from
// the client that read it.
type providerVoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// listProviderVoicesRequest is the check request plus the MODEL, which the
// voice set depends on for two provider families.
type listProviderVoicesRequest struct {
	checkConnectionRequest
	// Model is the model name as the project configured it. Realtime OpenAI
	// models expose two voices the unary ones do not, and a Gemini model on a
	// Google credential takes native Gemini voice names rather than the
	// OpenAI-compatible six.
	Model string `json:"model"`
}

// listProviderVoicesResponse is the gateway to elitea-main wire contract.
//
// Voices is always present and never null, for the reason
// listProviderModelsResponse.Models is: a caller must not have to read a
// missing field as "not implemented".
type listProviderVoicesResponse struct {
	Success bool            `json:"success"`
	Reason  string          `json:"reason,omitempty"`
	Voices  []providerVoice `json:"voices"`
}

// listProviderVoicesMaxBody bounds the request body. It is the check request's
// body plus one short model name.
const listProviderVoicesMaxBody = 8 << 10 // 8 KiB

// openAITTSVoices is OpenAI's published set, and the set Azure OpenAI accepts
// unchanged because the proxy passes the identifiers through natively.
var openAITTSVoices = []providerVoice{
	{ID: "alloy", Name: "Alloy"},
	{ID: "ash", Name: "Ash"},
	{ID: "coral", Name: "Coral"},
	{ID: "echo", Name: "Echo"},
	{ID: "fable", Name: "Fable"},
	{ID: "nova", Name: "Nova"},
	{ID: "onyx", Name: "Onyx"},
	{ID: "sage", Name: "Sage"},
	{ID: "shimmer", Name: "Shimmer"},
}

// openAIRealtimeExtraVoices are the two a realtime model adds on top.
var openAIRealtimeExtraVoices = []providerVoice{
	{ID: "ballad", Name: "Ballad"},
	{ID: "verse", Name: "Verse"},
}

// vertexTTSVoices is SIX of OpenAI's nine, and the omission is the point.
//
// The classic Google Cloud TTS path maps only these six; `ash`, `coral` and
// `sage` are passed through literally and rejected by the Google API with a
// 400. Offering them would be offering an operator three voices that cannot
// work, which is worse than a shorter list.
var vertexTTSVoices = []providerVoice{
	{ID: "alloy", Name: "Alloy"},
	{ID: "echo", Name: "Echo"},
	{ID: "fable", Name: "Fable"},
	{ID: "onyx", Name: "Onyx"},
	{ID: "nova", Name: "Nova"},
	{ID: "shimmer", Name: "Shimmer"},
}

// geminiTTSVoices are Gemini's NATIVE names. There is no mapping table on the
// Gemini code path, so an OpenAI voice name fails there; these are the only
// identifiers that work.
var geminiTTSVoices = []providerVoice{
	{ID: "Zephyr", Name: "Zephyr (Bright)"},
	{ID: "Puck", Name: "Puck (Upbeat)"},
	{ID: "Charon", Name: "Charon (Informational)"},
	{ID: "Kore", Name: "Kore (Firm)"},
	{ID: "Fenrir", Name: "Fenrir (Excitable)"},
	{ID: "Leda", Name: "Leda (Youthful)"},
	{ID: "Orus", Name: "Orus (Firm)"},
	{ID: "Aoede", Name: "Aoede (Breezy)"},
	{ID: "Callirrhoe", Name: "Callirrhoe (Easy-going)"},
	{ID: "Autonoe", Name: "Autonoe (Bright)"},
	{ID: "Enceladus", Name: "Enceladus (Breathy)"},
	{ID: "Iapetus", Name: "Iapetus (Clear)"},
	{ID: "Umbriel", Name: "Umbriel (Easy-going)"},
	{ID: "Algieba", Name: "Algieba (Smooth)"},
	{ID: "Despina", Name: "Despina (Smooth)"},
	{ID: "Erinome", Name: "Erinome (Clear)"},
	{ID: "Algenib", Name: "Algenib (Gravelly)"},
	{ID: "Rasalgethi", Name: "Rasalgethi (Informational)"},
	{ID: "Laomedeia", Name: "Laomedeia (Upbeat)"},
	{ID: "Achernar", Name: "Achernar (Soft)"},
	{ID: "Alnilam", Name: "Alnilam (Firm)"},
	{ID: "Schedar", Name: "Schedar (Even)"},
	{ID: "Gacrux", Name: "Gacrux (Mature)"},
	{ID: "Pulcherrima", Name: "Pulcherrima (Forward)"},
	{ID: "Achird", Name: "Achird (Friendly)"},
	{ID: "Zubenelgenubi", Name: "Zubenelgenubi (Casual)"},
	{ID: "Vindemiatrix", Name: "Vindemiatrix (Gentle)"},
	{ID: "Sadachbia", Name: "Sadachbia (Lively)"},
	{ID: "Sadaltager", Name: "Sadaltager (Knowledgeable)"},
	{ID: "Sulafat", Name: "Sulafat (Warm)"},
}

// isRealtimeModel matches the reference's own test: the token "realtime"
// anywhere in the model name, with underscores read as hyphens.
func isRealtimeModel(model string) bool {
	return strings.Contains(strings.ReplaceAll(strings.ToLower(model), "_", "-"), "realtime")
}

func isGeminiModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "gemini")
}

// voiceCatalogueFor resolves the voice set for one provider type and model.
//
// The bool is false for a type this build knows no catalogue for. It is NOT an
// error: see the file doc.
func voiceCatalogueFor(providerType, model string) ([]providerVoice, bool) {
	switch normaliseVoiceProviderType(providerType) {
	case "open_ai", "azure_open_ai", "open_ai_azure", "ai_dial", "vllm", "ollama":
		// Every OpenAI-dialect path, including the two proxies and the two
		// self-hosted servers: they accept whatever voice the model behind
		// them accepts, and the OpenAI set is the only one the dialect names.
		if isRealtimeModel(model) {
			return append(append([]providerVoice{}, openAITTSVoices...), openAIRealtimeExtraVoices...), true
		}
		return append([]providerVoice{}, openAITTSVoices...), true
	case "vertex_ai":
		// Checked before the generic Vertex answer, exactly as the reference
		// dispatches: a Gemini model on a Vertex credential takes Gemini names.
		if isGeminiModel(model) {
			return append([]providerVoice{}, geminiTTSVoices...), true
		}
		return append([]providerVoice{}, vertexTTSVoices...), true
	default:
		return nil, false
	}
}

// normaliseVoiceProviderType folds the type the platform stores onto the keys
// above. It is trim-and-lowercase only: a type this build does not know must
// fall through to "no catalogue" rather than be guessed at by prefix.
func normaliseVoiceProviderType(providerType string) string {
	return strings.ToLower(strings.TrimSpace(providerType))
}

// ListProviderVoices answers with the voices a TTS model can be asked for.
//
// It dials nothing, writes nothing and bills nothing — see the file doc for why
// that is the correct implementation today rather than a stub.
func (h *Handler) ListProviderVoices(w http.ResponseWriter, r *http.Request) {
	if !verifySignature(r.Header, h.identitySecret) {
		writeError(w, http.StatusForbidden, "permission_error", "invalid identity signature", "")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, listProviderVoicesMaxBody)
	var req listProviderVoicesRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	voices, known := voiceCatalogueFor(req.Type, req.Model)
	if !known {
		// success:true with an empty list. The caller keeps its own default
		// voice, which is the correct behaviour for a provider that publishes
		// no catalogue, and `reason` says so rather than leaving the caller to
		// guess between "none" and "broken".
		writeJSON(w, http.StatusOK, listProviderVoicesResponse{
			Success: true,
			Reason:  checkConnectionReasonUnsupported,
			Voices:  []providerVoice{},
		})
		return
	}

	writeJSON(w, http.StatusOK, listProviderVoicesResponse{
		Success: true,
		Reason:  checkConnectionReasonOK,
		Voices:  voices,
	})
}
