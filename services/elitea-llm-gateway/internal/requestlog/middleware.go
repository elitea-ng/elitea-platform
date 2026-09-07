package requestlog

// The HTTP middleware that guarantees no request goes unrecorded.
//
// # Why it is middleware and not a call in each handler
//
// The /llm surface has seventeen routes across five dialects, plus NotFound and
// MethodNotAllowed. Recording from inside each handler means a list that has to
// stay complete, and this codebase has met that failure repeatedly: a section
// missing from `addressableModelSections` makes a configured model answer 404,
// a pair missing from a section list makes a row invisible. A log with the same
// shape of hole is worse than most, because its gaps look like absence of
// traffic.
//
// So the recording happens once, around everything. A route added tomorrow is
// logged without anyone remembering to log it, and the requests that never
// reach a handler at all — a 404, a 405, a rejection before dispatch — are
// recorded too. Those are often the interesting ones.
//
// # What the middleware knows, and what the handler adds
//
// The middleware sees the transport: method, route pattern, status, duration,
// and the forwarded identity headers. It cannot see the model, because that is
// in the request body and parsing the body here would mean buffering every
// request to read one field.
//
// So the handler ENRICHES. `FromContext(ctx).SetModel(...)` attaches what only
// the handler knows, and the middleware emits the finished record. Enrichment
// is best-effort by design: a request that fails before a model resolves has no
// model to report, and an empty model on a failed request is a true statement
// about it.

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"
)

// Identity headers, mirrored from internal/llmproxy. They are read rather than
// verified here: verification happens on the request path, and a record of a
// request whose signature failed is a record worth keeping — with its identity
// taken as claimed, which is why a refused request's project_id is only ever
// used for filtering and never for authorization.
const (
	headerProjectID = "X-Elitea-Project-Id"
	headerUserID    = "X-Elitea-User-Id"
	// headerExecutionID is the runtime execution the request was made from. It
	// is read here rather than enriched by a handler for the same reason the
	// other two are: it is on the transport, so it is known for a request that
	// 404s or is refused before any handler runs — and those are exactly the
	// agent runs an operator is looking for when a run produced nothing.
	//
	// Like the two above it is taken AS CLAIMED. The signature that covers it
	// (v2, llmproxy/identity.go) is checked on the request path; a record of a
	// request whose signature failed is still worth keeping, with its identity
	// unverified, which is why nothing read from here is ever an authorization
	// input.
	headerExecutionID = "X-Elitea-Execution-Id"
)

type contextKey struct{}

// Enrichment is what a handler can add to the record in flight. Every method is
// safe on a nil receiver, so a handler never needs to check whether logging is
// on.
//
// The mutex is not decoration. A streamed request settles its usage from the
// stream drain, and that drain can outlive the request on a detached goroutine
// (stream_drain.go). A write from there races the middleware's read, and it
// arrives after the row is already gone to the writer. `sealed` makes that late
// write a deterministic no-op instead of a race: the row states what the
// gateway knew when it answered.
type Enrichment struct {
	mu         sync.Mutex
	sealed     bool
	provider   string
	model      string
	streaming  bool
	errorCode  string
	promptToks int64
	outputToks int64
}

// FromContext returns the enrichment handle for this request, or nil when the
// request is not being logged.
func FromContext(ctx context.Context) *Enrichment {
	enrichment, _ := ctx.Value(contextKey{}).(*Enrichment)
	return enrichment
}

// SetModel records which provider and model served the request.
func (e *Enrichment) SetModel(provider, model string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return
	}
	e.provider, e.model = provider, model
}

// SetStreaming marks the response as streamed.
func (e *Enrichment) SetStreaming(streaming bool) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return
	}
	e.streaming = streaming
}

// SetError records the gateway's CLASSIFICATION of a failure.
//
// A code, never a message. There is deliberately no way to attach an upstream
// error string: provider errors quote the offending fragment of the request
// back, and the request is user content.
func (e *Enrichment) SetError(code string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return
	}
	e.errorCode = code
}

// SetTokens records the usage the response reported.
func (e *Enrichment) SetTokens(prompt, completion int64) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return
	}
	e.promptToks, e.outputToks = prompt, completion
}

// snapshot is the sealed value of an Enrichment. It is a separate type because
// Enrichment holds a mutex, and a mutex must not be copied.
type snapshot struct {
	provider   string
	model      string
	streaming  bool
	errorCode  string
	promptToks int64
	outputToks int64
}

// seal takes the final values and closes the handle to further writes.
//
// The middleware calls it once, from the defer that emits the row. A write
// after it is dropped, because there is no row left to write it to.
func (e *Enrichment) seal() snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sealed = true
	return snapshot{
		provider:   e.provider,
		model:      e.model,
		streaming:  e.streaming,
		errorCode:  e.errorCode,
		promptToks: e.promptToks,
		outputToks: e.outputToks,
	}
}

// ErrorCodeSetter is how the gateway's error writer attaches its own
// classification to the record without every call site knowing about the log.
//
// `writeError` receives the wrapped ResponseWriter and type-asserts to this,
// so one change there covers all thirty-odd refusal sites — and when the log is
// off the assertion simply fails and nothing happens.
type ErrorCodeSetter interface{ SetErrorCode(code string) }

// statusRecorder captures the status code and whether anything was written.
type statusRecorder struct {
	http.ResponseWriter
	status    int
	written   bool
	errorCode string
}

// SetErrorCode records the gateway's error TYPE. First write wins: the first
// refusal is the one that decided the response, and a later write (an error
// while writing an error) would relabel it.
func (w *statusRecorder) SetErrorCode(code string) {
	if w.errorCode == "" {
		w.errorCode = code
	}
}

func (w *statusRecorder) WriteHeader(status int) {
	if !w.written {
		w.status = status
		w.written = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.written {
		// An implicit 200: the handler wrote a body without calling
		// WriteHeader. Recording 0 here would make every successful buffered
		// response look like a request that never answered.
		w.status = http.StatusOK
		w.written = true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets the http.ResponseController machinery reach the real writer.
func (w *statusRecorder) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Flush forwards to the wrapped writer.
//
// WITHOUT THIS, EVERY STREAMED REQUEST FAILS. `beginStream` asserts
// `w.(http.Flusher)` DIRECTLY — not through http.NewResponseController — and
// answers 500 "streaming unsupported" when the assertion fails. A wrapper that
// embeds http.ResponseWriter promotes only that interface's methods, so it does
// NOT satisfy Flusher, and inserting this middleware without forwarding Flush
// would have broken chat on every deployment: the product's most visible path,
// failing with an error about a capability the real writer has.
//
// Unwrap alone does not fix it. Unwrap is what ResponseController follows; a
// direct type assertion never consults it.
func (w *statusRecorder) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack forwards to the wrapped writer, for the realtime WebSocket route.
//
// `websocket.Accept` hijacks the connection, and it reaches for http.Hijacker
// on the writer it is handed. The same argument as Flush: a wrapper that hides
// it turns every realtime session into a failed upgrade.
//
// It returns the wrapped writer's error when the underlying writer cannot be
// hijacked, rather than a nil connection with a nil error — a caller that got
// both would panic on first use.
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	// A hijacked connection means the status this middleware recorded is the
	// last one net/http knows about. 101 is what the upgrade answers, and
	// recording it here keeps a realtime session from being logged as a 200
	// that never streamed.
	if !w.written {
		w.status = http.StatusSwitchingProtocols
		w.written = true
	}
	return hijacker.Hijack()
}

// Middleware records one row per request.
//
// A nil recorder returns the handler unwrapped, so a deployment with no log
// pays nothing — not even the wrapper's allocation.
func Middleware(recorder *Recorder) func(http.Handler) http.Handler {
	if recorder == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := recorder.now()
			enrichment := &Enrichment{}
			recording := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

			request := r.WithContext(context.WithValue(r.Context(), contextKey{}, enrichment))
			// The record is emitted from a defer so a handler that panics is
			// still logged. The panic then continues to the recovery
			// middleware; swallowing it here would turn a crash into a
			// silently-empty response.
			defer func() {
				final := enrichment.seal()
				recorder.Record(Record{
					OccurredAt:  started,
					ProjectID:   r.Header.Get(headerProjectID),
					UserID:      r.Header.Get(headerUserID),
					Route:       routePattern(request),
					Method:      r.Method,
					Status:      recording.status,
					Duration:    recorder.now().Sub(started),
					Provider:    final.provider,
					Model:       final.model,
					Streaming:   final.streaming,
					ErrorCode:   errorCodeFor(&final, recording),
					PromptToks:  final.promptToks,
					OutputToks:  final.outputToks,
					ExecutionID: r.Header.Get(headerExecutionID),
				})
			}()

			next.ServeHTTP(recording, request)
		})
	}
}

// routePattern returns the chi route PATTERN, never the raw URL.
//
// The pattern is bounded — there are seventeen of them — while a raw path is
// not, and a raw path carries the query string. Both matter: unbounded values
// make the column useless for grouping, and a query string is somewhere a
// caller can put a secret.
//
// A request that matched no route has no pattern; it is reported as the literal
// "(unmatched)" rather than as an empty string, so a 404 flood is legible as
// one thing instead of as a column of blanks.
func routePattern(r *http.Request) string {
	if routeCtx := chi.RouteContext(r.Context()); routeCtx != nil {
		if pattern := routeCtx.RoutePattern(); pattern != "" {
			return pattern
		}
	}
	return "(unmatched)"
}

// errorCodeFor resolves the failure classification, in precedence order.
//
//  1. What the HANDLER set explicitly, which is the most specific.
//  2. What the gateway's error writer captured — the OpenAI-shaped error type
//     it returned to the caller.
//  3. Nothing on a 2xx/3xx, and a status-derived bucket otherwise, so a failure
//     that took neither path above is still legible as a failure rather than
//     appearing successful with an empty code.
//
// (3) matters: a request that fails inside net/http, or one a future route
// refuses without going through writeError, would otherwise be a 500 with no
// classification, and a log that shows failures with no reason is only half the
// answer.
func errorCodeFor(enrichment *snapshot, recording *statusRecorder) string {
	if enrichment.errorCode != "" {
		return enrichment.errorCode
	}
	if recording.errorCode != "" {
		return recording.errorCode
	}
	if recording.status < 400 {
		return ""
	}
	if recording.status < 500 {
		return "client_error"
	}
	return "server_error"
}
