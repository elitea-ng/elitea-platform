package material

// The invoke handler a rewriting facade mounts in place of the plain forward.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy"
	"github.com/go-chi/chi/v5"
)

// Rewriter turns a reference-carrying body into a material-carrying one, and
// reports the grant it minted so a failed hop can revoke it.
type Rewriter func(ctx context.Context, body io.Reader, projectID, userID int64) ([]byte, Grant, error)

// Forwarder is the facade's hop, as providerhost/proxy spells it.
type Forwarder func(w http.ResponseWriter, r *http.Request, providerPath, projectID, userID string)

// Served describes one invocation a facade forwarded, for an Observer.
//
// Request is what the CLIENT sent, before the rewrite; Response is a bounded
// capture of what the provider answered. Either can be empty — a body larger
// than the limit is reported as absent rather than as a prefix, because every
// consumer of these parses JSON and half a document is not a shorter one.
type Served struct {
	ProjectID   int64
	UserID      int64
	ToolkitName string
	ToolName    string
	Request     []byte
	Response    []byte
	Status      int
}

// Observer is told about one served invocation.
type Observer func(context.Context, Served)

// DefaultObserveLimit bounds both captured bodies when a facade names no
// limit of its own. An invoke request is a question and a settings block, and
// an invoke response is an acknowledgement carrying an id; 256 KiB is far
// above either and far below anything worth streaming.
const DefaultObserveLimit = 256 << 10

// Invocation is one facade's rewriting invoke route.
type Invocation struct {
	// Provider names the app in log lines and in the two refusals below.
	Provider string
	Rewrite  Rewriter
	Forward  Forwarder
	// Path is the provider SPI path this request forwards to, built per
	// request because it carries the toolkit and tool names.
	Path   func(*http.Request) string
	Minter Minter
	// Status maps a rewrite failure to a status a caller can act on.
	Status func(error) (int, string)
	Logger *slog.Logger
	// RewriteFor, when set, CHOOSES the rewrite from the toolkit and tool
	// names in the path, and Rewrite is then the fallback for a pair it
	// does not recognise.
	//
	// It exists because one provider's toolkits do not all carry the same
	// references: DeepWiki's `Wikis` names a code toolkit, its `wikis_query`
	// names a Wikis toolkit, and its `wiki_query` names nothing at all — it
	// reads a bucket. One rewrite for all three would have to make every
	// reference optional, and "optional" is how a body that names no
	// credential gets forwarded to a tool that needs one.
	RewriteFor func(toolkitName, toolName string) Rewriter
	// Observe, when set, is told about every invocation this handler served —
	// after the hop, with the client's ORIGINAL body and a bounded capture of
	// the provider's answer.
	//
	// THE ORIGINAL BODY AND NOT THE REWRITTEN ONE. The rewrite is what puts
	// credentials and a minted bearer into the payload; an observer that
	// recorded that would persist secrets on behalf of a feature that only
	// wanted the question the user typed.
	//
	// It runs on the request's own goroutine, after the response has been
	// written, so a slow observer delays nothing the caller is waiting for
	// but does hold the connection's handler. Observers here are expected to
	// be one bounded database write.
	Observe Observer
	// ObserveLimit bounds the captured response. Zero uses DefaultObserveLimit.
	ObserveLimit int
	// Tools, when set, are the only tool names this handler rewrites for.
	// Every other tool forwards plainly — rewriting a body that names no
	// credential would mint a callback grant for work that asks for none.
	// Empty rewrites every tool, which is DeepWiki's shape.
	Tools []string
}

// Handler is Serve with the tool filter applied.
func (in Invocation) Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(in.Tools) > 0 && !allowed(in.Tools, chi.URLParam(r, "tool_name")) {
			in.Forward(w, r, in.Path(r),
				chi.URLParam(r, "project_id"), facade.UserID(r))
			return
		}
		in.Serve(w, r)
	}
}

// Serve rewrites the body, forwards it, and revokes the minted bearer when the
// provider did not accept the invocation.
//
// REVOCATION IS THE POINT OF CAPTURING THE STATUS. A refused invoke leaves a
// live, project-bound credential behind for nothing: the work it was minted
// for will never run, and it stays valid until its TTL. It expires either way,
// so this is a narrowing rather than a guarantee — but it narrows the window
// from hours to nothing for the case that fails fastest and most often, a
// malformed or refused request.
func (in Invocation) Serve(w http.ResponseWriter, r *http.Request) {
	logger := in.Logger
	if logger == nil {
		logger = slog.Default()
	}
	projectID, ok := PathProjectID(r)
	if !ok {
		WriteError(w, http.StatusBadRequest, "The project id is not valid.")
		return
	}
	userID := OwnerID(r)
	if userID <= 0 {
		// A machine principal with no owning user cannot be given a callback
		// bearer: the token row must belong to somebody, and inventing an
		// owner would put a credential under a user who did not ask for it.
		WriteError(w, http.StatusForbidden,
			fmt.Sprintf("Starting a %s invocation requires a user identity.", in.Provider))
		return
	}

	rewrite := in.Rewrite
	if in.RewriteFor != nil {
		if chosen := in.RewriteFor(chi.URLParam(r, "toolkit_name"), chi.URLParam(r, "tool_name")); chosen != nil {
			rewrite = chosen
		}
	}
	if rewrite == nil {
		// A handler with no rewrite at all would forward a body carrying
		// references the provider cannot read, having already passed
		// authentication and permissions — the point at which a defect
		// stops looking like one.
		WriteError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("%s cannot prepare this invocation.", in.Provider))
		return
	}

	// The client's body is buffered BEFORE the rewrite reads it, because the
	// rewrite consumes it and what an observer wants is what the user sent.
	// A body over the limit is not observed at all (see Served); the rewrite
	// still gets every byte, because the buffer is put back in front of the
	// rest of the stream rather than replacing it.
	var original []byte
	body := io.Reader(r.Body)
	if in.Observe != nil {
		original, body = peek(r.Body, in.limit())
	}

	rewritten, grant, err := rewrite(r.Context(), body, projectID, userID)
	if err != nil {
		Revoke(r.Context(), in.Minter, logger, userID, grant.UUID)
		status, message := in.Status(err)
		logger.Warn(in.Provider+" invoke rejected",
			"project", projectID, "status", status, "error", err)
		WriteError(w, status, message)
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	r.Header.Set("Content-Type", "application/json")
	// A stale Content-Length from the original body would be forwarded
	// alongside the new one; the header and the field must agree.
	r.Header.Del("Content-Length")

	// The status is observed through the proxy's own ModifyResponse hook, not
	// by wrapping the ResponseWriter. A wrapper has to forward Flush or a
	// streaming response stops streaming, and it puts a Write on a
	// caller-influenced response body, which CodeQL reads as a reflected-XSS
	// sink (go/reflected-xss).
	outcome := &proxy.Outcome{}
	if in.Observe != nil {
		outcome.CaptureLimit = in.limit()
	}
	in.Forward(w, r.WithContext(proxy.WithOutcome(r.Context(), outcome)),
		in.Path(r), strconv.FormatInt(projectID, 10), strconv.FormatInt(userID, 10))

	// Zero means ModifyResponse never ran, which is a transport failure — the
	// provider was never reached, so the invocation certainly did not start.
	if outcome.Status >= 400 || outcome.Status == 0 {
		Revoke(r.Context(), in.Minter, logger, userID, grant.UUID)
	}

	if in.Observe != nil {
		response := outcome.Body
		if outcome.Truncated {
			response = nil
		}
		// WithoutCancel: the response has already been written, so the
		// request context may be cancelled the instant the client has its
		// bytes — and an observer that records the turn must not lose it
		// because the browser was quick.
		in.Observe(context.WithoutCancel(r.Context()), Served{
			ProjectID:   projectID,
			UserID:      userID,
			ToolkitName: chi.URLParam(r, "toolkit_name"),
			ToolName:    chi.URLParam(r, "tool_name"),
			Request:     original,
			Response:    response,
			Status:      outcome.Status,
		})
	}
}

func (in Invocation) limit() int {
	if in.ObserveLimit > 0 {
		return in.ObserveLimit
	}
	return DefaultObserveLimit
}

// peek buffers up to limit bytes and returns them alongside a reader that
// still yields the WHOLE stream, prefix included. A stream longer than the
// limit reports no prefix at all — see Served.
func peek(body io.Reader, limit int) ([]byte, io.Reader) {
	if body == nil {
		return nil, http.NoBody
	}
	buffered, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		// The read failed part-way. The rewrite must see the same failure
		// rather than a body that silently ends early.
		return nil, io.MultiReader(bytes.NewReader(buffered), failingReader{err})
	}
	if len(buffered) <= limit {
		return buffered, bytes.NewReader(buffered)
	}
	return nil, io.MultiReader(bytes.NewReader(buffered), body)
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

// PathProjectID reads and validates the {project_id} segment.
//
// THE UPPER BOUND IS NOT COSMETIC, and facade.ValidProjectID carries it. The
// id is narrowed to int32 downstream to read a row (the columns are Postgres
// `integer`), and in Go that narrowing is a silent truncation: without the
// bound `4294967301` truncates to `5`, so a caller could name an out-of-range
// project and have a facade resolve project 5's stored credentials — and push
// them to the provider. CodeQL found the conversion
// (go/incorrect-integer-conversion); the bound belongs at the only parse in
// this request path rather than at each narrowing downstream, and NarrowRowID
// refuses again at the narrowing itself for a caller that does not come
// through here.
func PathProjectID(r *http.Request) (int64, bool) {
	raw := chi.URLParam(r, "project_id")
	if !facade.ValidProjectID(raw) {
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil
}

// OwnerID reads the database user a callback token can belong to.
//
// RuntimePrincipalFromContext, not UserFromContext: the former requires the
// server-derived provenance marker that only the authentication middleware
// sets, so a context carrying a user placed there by some other path yields
// nothing rather than an identity a facade would then sign.
func OwnerID(r *http.Request) int64 {
	principal, ok := auth.RuntimePrincipalFromContext(r.Context())
	if !ok {
		return 0
	}
	owner, ok := principal.OwningUserID()
	if !ok {
		return 0
	}
	return owner
}

// WriteError answers with the facades' JSON error shape.
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + quoteJSON(message) + `}`))
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
