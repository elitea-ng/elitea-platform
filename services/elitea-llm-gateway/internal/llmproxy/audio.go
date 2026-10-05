// audio.go — the /llm/v1/audio surface: text-to-speech and speech-to-text
// (issue #323).
//
// WHY THIS FILE EXISTS. The retired LiteLLM proxy served /v1/audio/speech and
// /v1/audio/transcriptions, and pylon-indexer's voice paths call them directly
// (indexer_tts.py, indexer_asr_whisper.py). Nothing replaced those two routes
// when LiteLLM was removed, so the indexer kept a LiteLLM process of its own to
// answer them. That process is a SECOND LLM data plane: it applies no budget,
// bills nothing, and resolves credentials from a registry the platform no
// longer writes. These two routes exist so it can be deleted.
//
// The two routes follow the same order every /llm route follows, and the order
// is a correctness rule, not a style: decode, then mapModel, then checkBudget,
// then dispatch, then updateUsage. mapModel must run first so the provider
// never sees a caller-authored model title and so the cost tables are keyed by
// the provider's own model name (CLAUDE.md, modelmap.go).
//
// TWO DELIBERATE LIMITS, both stated here rather than discovered later:
//
//  1. Neither route streams. The speech route answers one complete audio body.
//     The pylon TTS client reads the response with `iter_content`, so a unary
//     body still arrives as a stream of chunks to it; what it loses is
//     first-byte latency, not audio. A streaming speech route needs the same
//     detached-drain billing machinery the chat stream has, and that is a
//     larger change than this one.
//  2. Unknown request fields are dropped, exactly as they are on every other
//     JSON route here (see decodeJSON). bifrost carries them in ExtraParams,
//     which only its fasthttp integrations layer populates, and this gateway
//     decodes its own bodies. For TTS this drops the ElevenLabs
//     `previous_text` / `next_text` prosody hints. `instructions`, `voice`,
//     `speed` and `response_format` are first-class fields and DO travel.
package llmproxy

import (
	"context"
	"expvar"
	"math"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/maximhq/bifrost/core/providers/openai"
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/EliteaAI/elitea-platform/services/elitea-llm-gateway/internal/cost"
)

// MetricAudioUnpriced counts audio responses the gateway could not price.
//
// An audio provider reports its usage in whichever denomination it sells in:
// tokens, a duration in seconds, or a character count. The gateway prices all
// three (issue #323).
//
// The SECONDS and CHARACTERS bases are catalog-only: audioCost refuses a rate
// that did not come from gateway_models, so those two can only ever bill a real
// price or nothing. A response on those bases stays unpriced when the provider
// reports NO usage at all, when the duration it reports is not a usable number,
// or when the catalog holds no rate for the units it did report. Such a response
// is dispatched, delivered and billed as zero, and counted here.
//
// The TOKEN basis is NOT catalog-only, and this counter does NOT see it. It
// falls back to the pylon default table exactly like every other route on this
// gateway — longstanding, disclosed behaviour (internal/cost/cost.go: "The token
// basis never fails"). So an audio response that reports TOKENS can bill a
// figure the gateway invented. That is a different defect from this one —
// priced wrongly, rather than not priced at all — and it gets its own counter,
// MetricAudioDefaultPriced, because an alarm on one must not stay silent about
// the other.
//
// That is a real gap in the money path, so it gets a number an operator can
// alarm on rather than a silent zero. Inventing a per-second price would be
// worse: it would put a made-up figure onto the authoritative counter, and the
// counter is what the budget gate reads back.
const MetricAudioUnpriced = "gateway_audio_unpriced_total"

// MetricAudioDefaultPriced counts audio requests billed on a TOKEN price that
// did not come from the catalog.
//
// gpt-4o-transcribe reports tokens and publishes a token price upstream; a
// deployment whose catalog has not synced still bills, from the pylon default
// table or from the ultimate 1.0/3.0 USD-per-1M fallback. The amount is non-zero
// and plausible, so nothing else on this path reports it: MetricAudioUnpriced
// cannot fire because a price WAS produced, and MetricAudioNonTokenBasis cannot
// fire because the basis IS tokens.
//
// That silence is the whole reason this counter exists. "We could not price it"
// and "we priced it with a number we made up" are different operator problems,
// and only the second one reaches an invoice.
const MetricAudioDefaultPriced = "gateway_audio_default_priced_total"

// MetricAudioNonTokenBasis counts requests a NON-token rate paid for.
//
// Every other route on this gateway bills tokens, so until the audio routes
// existed the basis was never in question. Now it is, and the wrong basis is
// silent: a per-second rate applied to a millisecond count bills 1000x, and a
// per-second rate applied where a token rate was meant bills a plausible wrong
// number. This counter is how an operator sees that the seconds and characters
// paths are live at all, and how many requests they carry.
const MetricAudioNonTokenBasis = "gateway_audio_non_token_priced_total"

// MetricAudioRefusedUnpricedModel counts audio requests refused BEFORE
// dispatch, because the project has a budget and the catalog carries no rate
// the model's response could be billed on.
//
// "Bill zero and count it" (MetricAudioUnpriced) was the answer while one
// audio request was one bounded call from the indexer. The browser voice
// client now calls these routes for every utterance and every sentence, with
// the member's session, so a project with a budget and an unpriced audio model
// had no ceiling on that spend at all. A project without a budget keeps the
// old answer: it has no ceiling to protect.
const MetricAudioRefusedUnpricedModel = "gateway_audio_refused_unpriced_model_total"

var (
	audioRefusedUnpricedModel = expvar.NewInt(MetricAudioRefusedUnpricedModel)
	audioUnpriced             = expvar.NewInt(MetricAudioUnpriced)
	audioNonTokenBasis        = expvar.NewInt(MetricAudioNonTokenBasis)
	audioDefaultPriced        = expvar.NewInt(MetricAudioDefaultPriced)
)

// AudioMetricNames returns the names of this file's counters, in a fixed order,
// for the composition root's /metrics allowlist. It mirrors
// ModelMapMetricNames: a counter this package publishes reaches the scrape
// surface through ONE named path, never a name copied into a second file.
//
// An expvar variable that is not listed here has NO route on this process's
// mux: expvar registers /debug/vars on http.DefaultServeMux, which this gateway
// never serves (CLAUDE.md, issue #465). Add a counter here when you publish it.
func AudioMetricNames() []string {
	return []string{MetricAudioUnpriced, MetricAudioNonTokenBasis, MetricAudioDefaultPriced, MetricAudioRefusedUnpricedModel}
}

// maxBillableAudioSeconds bounds a provider-reported duration at 24 hours.
//
// The bound is not about overflow, which math/big already handles. It is about
// a garbage field. A duration is the only number on this path that arrives as a
// float, and a provider that reports 1e18 seconds — or a bug that reads a
// millisecond field as seconds — would bill a project into a hard budget block
// on one request. No real speech request is a day long, so a value past this
// bound is not a long request; it is a broken field, and the honest answer is
// UNPRICED.
const maxBillableAudioSeconds = 24 * 60 * 60

// secondsToMillis converts a provider-reported duration to integer
// milliseconds. It is THE boundary between the fractional second count the
// provider reports and the int64 the money path requires, and it is crossed
// exactly ONCE per request.
//
// THE TWO CALL SITES DO NOT AGREE ON THE TYPE, and the doc here used to name
// only one of them. TranscriptionUsage.Seconds is a *float64, so a transcription
// really does arrive as a float (523.5). SpeechUsage.AudioSeconds is an INT,
// widened at that call site: a generated clip shorter than one second reports 0
// there, so it never reaches this function at all — speechUnits requires
// AudioSeconds > 0 before it selects the seconds basis, and moves on to the
// character count of the text instead. That is deliberate: bifrost gives the
// speech route no sub-second figure to bill, and inventing one is not an option.
//
// ok is false for NaN, for either infinity, for a non-positive value, for
// anything past maxBillableAudioSeconds, and for a duration that ROUNDS TO ZERO
// milliseconds. The first four each reach the money path as a wrong number
// rather than as an error: NaN and Inf convert to an unspecified int64, and a
// negative duration would credit the project.
//
// The zero-millisecond case is an invariant and not a rounding detail: a
// NON-EMPTY basis must imply a POSITIVE quantity in that basis. Returning
// (0, true) for a 0.4 ms duration handed the caller a seconds basis with
// all-zero Units; updateUsageUnits then re-derives the basis from those zeros,
// reads tokens, and NEITHER audio counter moves — the request is neither billed
// nor counted as unpriced. A duration under half a millisecond is UNPRICED, and
// unpriced is a number an operator can see.
func secondsToMillis(sec float64) (int64, bool) {
	if math.IsNaN(sec) || math.IsInf(sec, 0) {
		return 0, false
	}
	if sec <= 0 || sec > maxBillableAudioSeconds {
		return 0, false
	}
	millis := int64(math.Round(sec * 1000))
	if millis <= 0 {
		return 0, false
	}
	return millis, true
}

// speechPriceProbes and transcriptionPriceProbes are the non-token units each
// route's responses can be billed on (speechUnits, transcriptionUnits). A probe
// that prices to a non-empty basis means the catalog carries that rate.
var (
	speechPriceProbes        = []cost.Units{{InputChars: 1000}, {OutputMillis: 1000}}
	transcriptionPriceProbes = []cost.Units{{InputMillis: 1000}}
)

// checkAudioAdmission is checkBudget for the audio routes, plus one rule: a
// project WITH a budget is refused a model the catalog cannot price.
//
// Such a request would be dispatched, delivered and billed zero
// (MetricAudioUnpriced), and a zero never moves the accumulator the budget gate
// reads. Every member of the project could then send unlimited speech and
// transcription traffic to a paid provider and never reach the budget. The
// refusal is a 501 `audio_unpriced`, not a 402: the budget is not exhausted, a
// piece of model configuration (the catalog rate) is missing.
//
// The probe accepts a CATALOG token rate or a catalog rate for one of the
// route's audio bases, like realtimePricedModel. A default-table token price
// does not count: it is a guess, and an audio model that reports seconds or
// characters is not billed by it anyway.
func (h *Handler) checkAudioAdmission(w http.ResponseWriter, ctx context.Context, provider, model string, probes []cost.Units) bool {
	v := h.admissionVerdict(ctx, model)
	if !v.allow {
		if v.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(int64(v.retryAfter/time.Second)+1, 10))
		}
		writeBudgetRefusal(w, v)
		return false
	}
	if !v.budgeted || h.audioModelPriced(ctx, provider, model, probes) {
		return true
	}
	audioRefusedUnpricedModel.Add(1)
	h.logger.WarnContext(ctx, "audio: the project has a budget and the catalog carries no rate for this model; refusing the request",
		"provider", provider, "model", model, "metric", MetricAudioRefusedUnpricedModel)
	writeError(w, http.StatusNotImplemented, "invalid_request_error",
		"the model `"+model+"` has no audio price in the catalogue, and the project has a budget; the request was refused", "audio_unpriced")
	return false
}

// audioModelPriced reports whether the catalog carries a rate this model's
// audio response can be billed on.
func (h *Handler) audioModelPriced(ctx context.Context, provider, model string, probes []cost.Units) bool {
	bp := h.budget()
	if bp.calc == nil {
		return true
	}
	if bp.calc.CostUnits(ctx, provider, model, cost.Units{InputTokens: 1, OutputTokens: 1}).FromCatalog() {
		return true
	}
	for _, u := range probes {
		if bp.calc.CostUnits(ctx, provider, model, u).Basis != "" {
			return true
		}
	}
	return false
}

// Speech handles POST /llm/v1/audio/speech (text-to-speech).
//
// The success body is the raw audio the provider returned, NOT JSON. That is
// the OpenAI contract and the one the pylon TTS client reads. Errors stay
// OpenAI-shaped JSON like every other route (spec §2.5).
func (h *Handler) Speech(w http.ResponseWriter, r *http.Request) {
	var req openai.OpenAISpeechRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	// stream_format decodes, and OpenAISpeechRequest.IsStreamingRequested() reads
	// it — but this route is unary. Answering a caller that asked for SSE with a
	// complete audio body is a silent contract break: it looks like success and
	// the caller's frame loop never fires. Refuse it by name instead.
	if req.IsStreamingRequested() {
		writeError(w, http.StatusBadRequest, "invalid_request_error",
			"`stream_format` is not supported on this route; /llm/v1/audio/speech answers one complete audio body",
			"streaming_unsupported")
		return
	}
	ctx, ok := h.newContext(w, r)
	if !ok {
		return
	}
	bifReq := req.ToBifrostSpeechRequest(ctx)

	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		return
	}
	provider, model := providerModelFromSpeechReq(bifReq)
	format := applyDefaultSpeechFormat(bifReq, provider)
	if !h.checkAudioAdmission(w, ctx, provider, model, speechPriceProbes) {
		return
	}

	resp, bErr := h.router.SpeechRequest(ctx, bifReq)
	if bErr != nil {
		h.writeOpenAIError(w, bErr)
		return
	}
	if resp == nil {
		writeError(w, http.StatusBadGateway, "api_error",
			"the speech provider returned no audio", "upstream_empty_response")
		return
	}

	writeAudio(w, resp.Audio, speechContentType(format, provider))

	units, basis := speechUnits(resp)
	if basis == "" {
		// UNREACHABLE THROUGH BIFROST, kept for a router that is not bifrost.
		// bifrost backfills Usage.InputChars on every speech response from the
		// input it forwarded, and refuses an empty input earlier, so a real
		// speech response always carries a billable quantity. The transcription
		// route's copy of this branch is a different matter: nothing backfills
		// there, and a provider that reports no usage really does land on it.
		audioUnpriced.Add(1)
		h.logger.WarnContext(ctx, "audio: the speech response carries no usable usage; the request bills zero",
			"provider", provider, "model", model, "metric", MetricAudioUnpriced)
		return
	}
	h.updateUsageUnits(ctx, surfaceAudio, provider, model, units, identityProjectFromCtx(ctx), identityUserFromCtx(ctx))
}

// Transcription handles POST /llm/v1/audio/transcriptions and
// POST /llm/v1/audio/translations (speech-to-text), both multipart.
//
// The two OpenAI routes share one bifrost request type. They are mounted
// separately so a caller that posts to /translations gets an answer instead of
// a 404; the request reaches the provider under the model the project
// configured, and the provider decides what the model does with it.
// maxTranscriptionUpload is the largest transcription body this route accepts.
//
// maxMultipartMemory is NOT a ceiling: it is the point at which ParseMultipartForm
// stops buffering in memory and starts spilling parts to TEMPORARY FILES. Without
// this, an unbounded upload is accepted, written to the pod's disk, and only then
// refused by the provider — so the cost of a hostile or merely wrong caller is
// paid before anything looks at the request.
//
// 25 MiB is OpenAI's own documented per-file transcription limit, so a body this
// route accepts is one the provider will accept too. As audio it is generous:
// 16-bit 16 kHz mono PCM runs 32,000 B/s, which is about thirteen minutes.
const maxTranscriptionUpload int64 = 25 << 20

func (h *Handler) Transcription(w http.ResponseWriter, r *http.Request) {
	// The identity check runs BEFORE the body is parsed. newContext reads
	// headers only, so the order is free, and an audio upload is the largest
	// body this gateway accepts: buffering 32 MiB for a request whose identity
	// signature does not verify is work done for a caller that gets a 403.
	//
	// The image multipart routes still parse first. Copy THIS order into a new
	// route, not that one.
	ctx, ok := h.newContext(w, r)
	if !ok {
		return
	}
	// The ceiling goes on BEFORE the parse, so an oversize body is refused while
	// it is still on the wire rather than after it reaches the disk.
	r.Body = http.MaxBytesReader(w, r.Body, maxTranscriptionUpload)
	form, ok := parseMultipart(w, r)
	if !ok {
		return
	}
	req, err := buildTranscriptionRequest(form)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "")
		return
	}
	bifReq := req.ToBifrostTranscriptionRequest(ctx)
	// ToBifrostTranscriptionRequest copies File but not Filename, and the
	// provider needs the extension to know the container format. Carry it.
	if bifReq.Input != nil {
		bifReq.Input.Filename = req.Filename
	}

	if !h.mapModel(w, ctx, &bifReq.Provider, &bifReq.Model) {
		return
	}
	provider, model := providerModelFromTranscriptionReq(bifReq)
	if !h.checkAudioAdmission(w, ctx, provider, model, transcriptionPriceProbes) {
		return
	}

	resp, bErr := h.router.TranscriptionRequest(ctx, bifReq)
	if bErr != nil {
		h.writeOpenAIError(w, bErr)
		return
	}
	if resp == nil {
		writeError(w, http.StatusBadGateway, "api_error",
			"the transcription provider returned no result", "upstream_empty_response")
		return
	}

	writeTranscription(w, resp)

	units, basis := transcriptionUnits(resp)
	if basis == "" {
		audioUnpriced.Add(1)
		h.logger.WarnContext(ctx, "audio: the transcription response carries no usable usage; the request bills zero",
			"provider", provider, "model", model, "metric", MetricAudioUnpriced)
		return
	}
	h.updateUsageUnits(ctx, surfaceAudio, provider, model, units, identityProjectFromCtx(ctx), identityUserFromCtx(ctx))
}

// buildTranscriptionRequest constructs an OpenAITranscriptionRequest from a
// parsed multipart form, mirroring the field contract of the upstream fasthttp
// parser. `file` and `model` are required; every other field is copied only
// when present.
func buildTranscriptionRequest(form *multipart.Form) (*openai.OpenAITranscriptionRequest, error) {
	req := &openai.OpenAITranscriptionRequest{}

	model, err := requiredValue(form, "model")
	if err != nil {
		return nil, err
	}
	req.Model = model

	headers := form.File["file"]
	if len(headers) == 0 {
		return nil, errRequired("file")
	}
	data, err := readFileHeader(headers[0])
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errRequired("file")
	}
	req.File = data
	req.Filename = headers[0].Filename

	if v, ok := stringValue(form, "language"); ok {
		req.Language = &v
	}
	if v, ok := stringValue(form, "prompt"); ok {
		req.Prompt = &v
	}
	if v, ok := stringValue(form, "response_format"); ok {
		req.ResponseFormat = &v
	}
	if v, ok := stringValue(form, "file_format"); ok {
		req.Format = &v
	}
	if v, ok := stringValue(form, "temperature"); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, wrapInvalid("temperature")
		}
		req.Temperature = &f
	}
	// Both spellings reach the same field: OpenAI's own clients send the
	// repeated-key form `timestamp_granularities[]`, and hand-built callers
	// send the bare name.
	if g := form.Value["timestamp_granularities[]"]; len(g) > 0 {
		req.TimestampGranularities = g
	} else if g := form.Value["timestamp_granularities"]; len(g) > 0 {
		req.TimestampGranularities = g
	}
	if inc := form.Value["include[]"]; len(inc) > 0 {
		req.Include = inc
	} else if inc := form.Value["include"]; len(inc) > 0 {
		req.Include = inc
	}
	if fb := form.Value["fallbacks"]; len(fb) > 0 {
		req.Fallbacks = fb
	}
	return req, nil
}

// speechAudioContentTypes maps an OpenAI speech `response_format` onto the MIME
// type of the body the provider returns.
//
// "pcm" is headerless 16-bit little-endian mono, which has no registered MIME
// type of its own; audio/L16 is the closest standard name.
//
// THE SAMPLE RATE IS NOT IN THIS TABLE, and that is the point. The rate is a
// property of the PROVIDER, not of the format the caller asked for: OpenAI and
// Azure serve pcm at 24 kHz, and bifrost maps the same "pcm" onto ElevenLabs'
// pcm_44100 (core providers/elevenlabs/utils.go). This table used to answer
// `rate=24000` for every one of them, so an ElevenLabs response was labelled at
// 24 kHz while carrying 44.1 kHz audio — a caller that trusted the header played
// it ~1.84x too slow, with no error anywhere.
//
// bifrost's BifrostSpeechResponse carries no served-format field, so the gateway
// cannot read the real rate back. It therefore states the rate only for the
// providers whose pcm rate is part of their contract, and says nothing for the
// rest. Saying nothing is recoverable; saying the wrong number is not.
var speechAudioContentTypes = map[string]string{
	"mp3":  "audio/mpeg",
	"opus": "audio/opus",
	"aac":  "audio/aac",
	"flac": "audio/flac",
	"wav":  "audio/wav",
	"pcm":  "audio/L16; channels=1",
}

// pcmRate24kProviders are the providers whose "pcm" response format is defined
// as 24 kHz. Add one only on the provider's documented contract, never on a
// guess: a wrong rate here is inaudible to every test and obvious to every user.
var pcmRate24kProviders = map[string]bool{
	"openai": true,
	"azure":  true,
}

// defaultSpeechFormats are the providers whose answer for an ABSENT
// response_format is not mp3, mapped to the format the gateway asks for
// instead.
//
// Gemini returns raw s16le 24 kHz PCM when the format is empty: bifrost core
// (providers/gemini/speech.go) wraps it in a WAV header only when the format is
// "wav", although its own validation message says an empty format "defaults to
// wav". The gateway then labelled the headerless PCM audio/mpeg, and every
// caller that decodes by container (the browser's decodeAudioData) failed on
// every Gemini TTS model. Asking for "wav" gives a self-describing container
// that the label states correctly. Gemini accepts only "wav" or "".
var defaultSpeechFormats = map[string]string{
	"gemini": "wav",
}

// applyDefaultSpeechFormat sets the provider's default response_format on a
// request that names none, and returns the format the answer is in. An
// explicit format is never changed.
func applyDefaultSpeechFormat(req *schemas.BifrostSpeechRequest, provider string) string {
	if req.Params == nil {
		req.Params = &schemas.SpeechParameters{}
	}
	if req.Params.ResponseFormat == "" {
		req.Params.ResponseFormat = defaultSpeechFormats[strings.ToLower(provider)]
	}
	return req.Params.ResponseFormat
}

// speechContentType resolves the response Content-Type for a speech request.
// An absent format is mp3, which is the OpenAI default. An unrecognised format
// is served as opaque bytes rather than mislabelled.
func speechContentType(format, provider string) string {
	if format == "" {
		return speechAudioContentTypes["mp3"]
	}
	ct, ok := speechAudioContentTypes[format]
	if !ok {
		return "application/octet-stream"
	}
	// The rate is added only where it is a fact. See speechAudioContentTypes.
	if format == "pcm" && pcmRate24kProviders[strings.ToLower(provider)] {
		return "audio/L16; rate=24000; channels=1"
	}
	return ct
}

// transcriptionTextFormats are the transcription response formats whose body is
// plain text, not JSON. bifrost records the format the provider actually served
// on the response, and the body for these is the transcript itself.
var transcriptionTextFormats = map[string]bool{
	"text": true,
	"srt":  true,
	"vtt":  true,
}

// writeAudio writes a raw audio body with the response-header hygiene every
// other write on this surface applies.
func writeAudio(w http.ResponseWriter, audio []byte, contentType string) {
	finish(w.Header())
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(audio)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(audio)
}

// writeTranscription writes a transcription response in the shape the caller's
// response_format asked for: plain text for text/srt/vtt, JSON otherwise.
func writeTranscription(w http.ResponseWriter, resp *schemas.BifrostTranscriptionResponse) {
	if resp.ResponseFormat != nil && transcriptionTextFormats[*resp.ResponseFormat] {
		finish(w.Header())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(resp.Text))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// providerModelFromSpeechReq extracts provider and model from a
// BifrostSpeechRequest.
func providerModelFromSpeechReq(req *schemas.BifrostSpeechRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	return string(req.Provider), req.Model
}

// providerModelFromTranscriptionReq extracts provider and model from a
// BifrostTranscriptionRequest.
func providerModelFromTranscriptionReq(req *schemas.BifrostTranscriptionRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	return string(req.Provider), req.Model
}

// transcriptionUsageTypeDuration is the ONE TranscriptionUsage.Type value that
// changes how a response is billed: it says the Seconds field carries the
// quantity and any token field on the same object is describing the audio, not
// selling it. bifrost's other value is "tokens", and the field is a plain
// string that no schema guarantees — a provider adapter may leave it empty.
//
// The gate is written against "duration" and not against "tokens" on purpose
// (issue #323 review). Requiring Type == "tokens" before billing token counts
// made a provider that reports counts and leaves Type empty bill ZERO and count
// as UNPRICED. The baseline this route replaced billed on the counts alone, so
// that reading was a revenue-losing change of behaviour, not a stricter rule.
const transcriptionUsageTypeDuration = "duration"

// speechUnits selects the ONE basis a speech response is billed on, and returns
// the quantity for it. An empty basis means UNPRICED.
//
// THE PRECEDENCE IS FIXED, AND IT NEVER FALLS THROUGH:
//
//  1. Non-zero provider token counts  → tokens.
//  2. Usage.AudioSeconds > 0          → seconds (the audio the model GENERATED,
//     so it pays the OUTPUT per-second rate).
//  3. Usage.InputChars > 0            → characters (the text sent IN, so it pays
//     the INPUT per-character rate).
//  4. otherwise                       → unpriced.
//
// It never sums two of them. gpt-4o-mini-tts publishes BOTH a token price and a
// per-second price upstream, so one response can carry both quantities; billing
// both charges the project twice for one request.
//
// A step that matches but whose rate the catalog does not carry ends the
// selection: it does not slide down to the next basis. CostUnits returns an
// unpriced Cost, and the caller counts it. That is the conservative answer —
// falling through would let a response that reports a duration be billed on a
// character rate that was never meant to price it.
//
// Step 2 is a whole-second test because SpeechUsage.AudioSeconds is an INT. A
// generated clip under one second reports 0 there and the selection moves to
// step 3. bifrost hands this route no sub-second figure, and the honest answer
// is to bill the quantity that WAS reported rather than to invent a duration.
//
// WHERE InputChars COMES FROM, said plainly because it is not the same kind of
// number as the other two. NO PROVIDER REPORTS IT. bifrost's
// BifrostSpeechResponse.BackfillParams runs on every speech response and sets
// Usage.InputChars = utf8.RuneCountInString(request.Input.Input): the rune count
// of the text THIS GATEWAY forwarded. On the real router it is therefore never
// absent and never zero, because bifrost refuses an empty input before it calls
// a provider.
//
// It still bills, and this is why that is legitimate rather than an estimate. A
// character-billed TTS provider charges for the input text it was sent. The
// quantity of the sale is the text, and this is a count of that exact text, so
// it is the billable quantity itself and not a guess at one. Contrast
// BifrostTranscriptionResponse.Duration, which transcriptionUnits refuses forty
// lines below: bifrost DERIVES that from word timestamps, so it is an
// observation of something the provider never stated and never agreed to sell.
// The rule is not "the gateway computed it, so it must not bill"; it is "bill
// the quantity the sale is priced on, never an inference about it".
//
// Two consequences follow, both handled where they land rather than left to be
// discovered:
//
//   - Against the real router steps 1 to 3 always match, so step 4 is
//     unreachable through bifrost. It is kept as a guard for a router that is
//     not bifrost — this package's own fake is one — and NOT as a defence
//     against provider behaviour, which cannot produce it.
//   - A speech model whose only character rate is the OUTPUT one is UNPRICED:
//     nothing on this path produces cost.Units.OutputChars. See that field.
func speechUnits(resp *schemas.BifrostSpeechResponse) (cost.Units, string) {
	if resp == nil || resp.Usage == nil {
		return cost.Units{}, ""
	}
	usage := resp.Usage
	in, out := int64(usage.InputTokens), int64(usage.OutputTokens)
	switch {
	case in > 0 || out > 0:
		return cost.Units{InputTokens: in, OutputTokens: out}, cost.BasisTokens
	case usage.AudioSeconds > 0:
		millis, ok := secondsToMillis(float64(usage.AudioSeconds))
		if !ok {
			return cost.Units{}, ""
		}
		return cost.Units{OutputMillis: millis}, cost.BasisSeconds
	case usage.InputChars > 0:
		return cost.Units{InputChars: int64(usage.InputChars)}, cost.BasisCharacters
	default:
		return cost.Units{}, ""
	}
}

// transcriptionUnits selects the ONE basis a transcription response is billed
// on. An empty basis means UNPRICED.
//
// THE PRECEDENCE IS FIXED:
//
//  1. A non-zero token count, unless Usage.Type says "duration" → tokens.
//  2. Usage.Seconds present and usable → seconds (the audio sent IN, so it pays
//     the INPUT per-second rate).
//  3. otherwise → unpriced.
//
// Step 1 reads the Type only to EXCLUDE a duration-billed response. bifrost
// writes "duration" when the Seconds field carries the sale, and a provider
// that also fills a token field on such a response is describing the audio. Any
// other Type — "tokens", or the empty string a provider adapter may leave
// behind — bills the counts the provider reported, because a reported count is
// what the token rate prices.
//
// BifrostTranscriptionResponse.Duration is NOT read here, and must not be. For
// several providers bifrost derives that field from word timestamps, so it is
// an OBSERVED ESTIMATE of how long the audio was, not usage the provider
// reported. Billing on it charges a project for a number the provider never
// sent and never agreed to. Usage.Seconds is the reported figure; when it is
// absent the honest answer is UNPRICED.
func transcriptionUnits(resp *schemas.BifrostTranscriptionResponse) (cost.Units, string) {
	if resp == nil || resp.Usage == nil {
		return cost.Units{}, ""
	}
	usage := resp.Usage
	var in, out int64
	if usage.InputTokens != nil {
		in = int64(*usage.InputTokens)
	}
	if usage.OutputTokens != nil {
		out = int64(*usage.OutputTokens)
	}
	if usage.Type != transcriptionUsageTypeDuration && (in > 0 || out > 0) {
		return cost.Units{InputTokens: in, OutputTokens: out}, cost.BasisTokens
	}
	if usage.Seconds != nil {
		if millis, ok := secondsToMillis(*usage.Seconds); ok {
			return cost.Units{InputMillis: millis}, cost.BasisSeconds
		}
	}
	return cost.Units{}, ""
}
