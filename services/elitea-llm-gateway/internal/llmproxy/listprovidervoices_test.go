package llmproxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// listprovidervoices_test.go covers POST /llm/v1/list_provider_voices
// (issue 323).
//
// The route dials nothing, so the questions this file asks are not the
// listing route's ("did it dial when the gate said no"). They are about the
// TABLE: does the answer differ where the providers differ, and does an
// unknown provider come back as an empty list rather than as an error a caller
// would report as a broken voice picker.

// stubVoiceProvider is the "provider" this route has: a type string and a
// model name. There is no HTTP endpoint to stand in for, which is the finding
// the route records — see listprovidervoices.go's doc.
type stubVoiceProvider struct {
	providerType string
	model        string
}

func doListProviderVoices(t *testing.T, h *Handler, stub stubVoiceProvider) listProviderVoicesResponse {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"type": stub.providerType, "model": stub.model})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/list_provider_voices", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ListProviderVoices(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (this route never refuses a known-shaped request)", rec.Code)
	}
	var resp listProviderVoicesResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func voiceIDs(voices []providerVoice) []string {
	ids := make([]string, 0, len(voices))
	for _, voice := range voices {
		ids = append(ids, voice.ID)
	}
	return ids
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestListProviderVoices_OpenAIDialect — the nine OpenAI publishes, for every
// provider that speaks the dialect.
func TestListProviderVoices_OpenAIDialect(t *testing.T) {
	h := &Handler{}
	for _, providerType := range []string{"open_ai", "azure_open_ai", "open_ai_azure", "ai_dial", "vllm", "ollama"} {
		t.Run(providerType, func(t *testing.T) {
			resp := doListProviderVoices(t, h, stubVoiceProvider{providerType, "tts-1"})
			if !resp.Success {
				t.Fatalf("%s reported failure: %+v", providerType, resp)
			}
			if len(resp.Voices) != 9 {
				t.Fatalf("%s returned %d voices, want the published nine: %v",
					providerType, len(resp.Voices), voiceIDs(resp.Voices))
			}
			if !contains(voiceIDs(resp.Voices), "alloy") {
				t.Errorf("%s omits `alloy`: %v", providerType, voiceIDs(resp.Voices))
			}
			// Every voice carries a display name, because the picker shows one.
			for _, voice := range resp.Voices {
				if voice.ID == "" || voice.Name == "" {
					t.Fatalf("%s returned an incomplete voice %+v", providerType, voice)
				}
			}
		})
	}
}

// TestListProviderVoices_RealtimeAddsTwo — a realtime model exposes `ballad`
// and `verse` on top. A build that ignored the model name would return nine
// here and an operator could never select either.
func TestListProviderVoices_RealtimeAddsTwo(t *testing.T) {
	h := &Handler{}
	resp := doListProviderVoices(t, h, stubVoiceProvider{"open_ai", "gpt-4o-realtime-preview"})
	ids := voiceIDs(resp.Voices)
	if len(ids) != 11 {
		t.Fatalf("a realtime model returned %d voices, want 11: %v", len(ids), ids)
	}
	for _, want := range []string{"ballad", "verse"} {
		if !contains(ids, want) {
			t.Errorf("a realtime model omits %q: %v", want, ids)
		}
	}

	// The underscore form is the same model. The reference normalises it, and
	// a build that did not would silently drop the two extra voices.
	underscored := doListProviderVoices(t, h, stubVoiceProvider{"open_ai", "gpt_4o_realtime_preview"})
	if len(underscored.Voices) != 11 {
		t.Fatalf("the underscore form returned %d voices, want 11", len(underscored.Voices))
	}

	// …and a non-realtime model must NOT gain them.
	unary := doListProviderVoices(t, h, stubVoiceProvider{"open_ai", "gpt-4o-mini-tts"})
	if contains(voiceIDs(unary.Voices), "ballad") {
		t.Error("a unary model was offered a realtime-only voice")
	}
}

// TestListProviderVoices_VertexOmitsTheThreeGoogleRejects is the case a
// "return the OpenAI nine everywhere" implementation gets wrong.
//
// The classic Vertex path maps six of the nine. `ash`, `coral` and `sage` are
// passed through literally and answered with 400 by the Google API, so
// offering them puts three broken options in the picker.
func TestListProviderVoices_VertexOmitsTheThreeGoogleRejects(t *testing.T) {
	resp := doListProviderVoices(t, &Handler{}, stubVoiceProvider{"vertex_ai", "tts-1"})
	ids := voiceIDs(resp.Voices)
	if len(ids) != 6 {
		t.Fatalf("vertex_ai returned %d voices, want the six that map: %v", len(ids), ids)
	}
	for _, rejected := range []string{"ash", "coral", "sage"} {
		if contains(ids, rejected) {
			t.Errorf("vertex_ai offers %q, which the Google API rejects: %v", rejected, ids)
		}
	}
}

// TestListProviderVoices_GeminiUsesNativeNames — nothing translates an OpenAI
// voice name on the Gemini code path, so the native names are the only ones
// that work. Dispatch is on the MODEL, under the same vertex_ai credential.
func TestListProviderVoices_GeminiUsesNativeNames(t *testing.T) {
	resp := doListProviderVoices(t, &Handler{}, stubVoiceProvider{"vertex_ai", "gemini-2.5-flash-preview-tts"})
	ids := voiceIDs(resp.Voices)
	if len(ids) != 30 {
		t.Fatalf("a gemini model returned %d voices, want the 30 native ones", len(ids))
	}
	if !contains(ids, "Zephyr") {
		t.Errorf("the gemini set omits `Zephyr`: %v", ids)
	}
	if contains(ids, "alloy") {
		t.Error("a gemini model was offered an OpenAI voice name, which that path rejects")
	}
}

// TestListProviderVoices_UnknownProviderIsAnEmptyListNotAFailure is the whole
// reason this route exists instead of the 501 it replaces.
//
// A provider that publishes no catalogue is a normal state: the caller keeps
// its own default voice. Reporting it as a failure would put an error in front
// of a user whose TTS works perfectly.
func TestListProviderVoices_UnknownProviderIsAnEmptyListNotAFailure(t *testing.T) {
	resp := doListProviderVoices(t, &Handler{}, stubVoiceProvider{"elevenlabs", "eleven_multilingual_v2"})
	if !resp.Success {
		t.Fatalf("an unknown provider reported failure: %+v", resp)
	}
	if len(resp.Voices) != 0 {
		t.Fatalf("an unknown provider returned voices: %v", voiceIDs(resp.Voices))
	}
	if resp.Reason != checkConnectionReasonUnsupported {
		t.Errorf("reason = %q, want %q so a caller can tell `none` from `broken`",
			resp.Reason, checkConnectionReasonUnsupported)
	}
}

// TestListProviderVoices_VoicesIsNeverNull — a missing field reads as "not
// implemented" to a client, which is exactly the state this route removes.
func TestListProviderVoices_VoicesIsNeverNull(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"type": "nothing-at-all", "model": ""})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/llm/v1/list_provider_voices", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	(&Handler{}).ListProviderVoices(rec, req)
	if body := rec.Body.String(); !bytes.Contains([]byte(body), []byte(`"voices":[]`)) {
		t.Fatalf("the empty answer does not carry an empty array: %s", body)
	}
}

// TestListProviderVoices_TypeIsNormalisedButNotGuessed — trim and lowercase
// only. A type this build does not know must fall through to "no catalogue"
// rather than be matched by prefix onto a set it does not accept.
func TestListProviderVoices_TypeIsNormalisedButNotGuessed(t *testing.T) {
	h := &Handler{}
	if resp := doListProviderVoices(t, h, stubVoiceProvider{"  Open_AI ", "tts-1"}); len(resp.Voices) != 9 {
		t.Fatalf("a padded, mixed-case type resolved to %d voices, want 9", len(resp.Voices))
	}
	if resp := doListProviderVoices(t, h, stubVoiceProvider{"open_ai_compatible_thing", "tts-1"}); len(resp.Voices) != 0 {
		t.Fatalf("an unknown type beginning with a known one resolved to %d voices, want 0",
			len(resp.Voices))
	}
}

// TestListProviderVoices_CatalogueIsNotAliased — the handler must not hand a
// caller a slice backed by the package-level table, or one request's append
// would rewrite the table for every later one.
func TestListProviderVoices_CatalogueIsNotAliased(t *testing.T) {
	first, _ := voiceCatalogueFor("open_ai", "tts-1")
	first[0] = providerVoice{ID: "tampered", Name: "Tampered"}
	second, _ := voiceCatalogueFor("open_ai", "tts-1")
	if second[0].ID != "alloy" {
		t.Fatalf("the catalogue was mutated through a returned slice: got %q", second[0].ID)
	}
}
