package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	defaultReplayBatchSize = 100
	maxSSEEventBytes       = 64 * 1024
	// An authorization decision covers at most one already-fetched replay
	// batch. That batch is written under this deadline. A full batch is
	// reauthorized immediately before the next fetch; a partial batch enters
	// the two-second waiter before reauthorization. The revocation exposure is
	// therefore bounded by the already-authorized batch and its write deadline,
	// not by the idle polling interval alone.
	defaultSSEWriteTimeout         = 10 * time.Second
	defaultSSEAuthorizationTimeout = 2 * time.Second
)

var (
	ErrExecutionEventsForbidden = errors.New("execution events are forbidden")
	ErrInvalidEventStream       = errors.New("invalid durable execution event stream")
	errSSEAuthorizationBusy     = errors.New("execution event authorization capacity is busy")
)

type DurableEvent struct {
	Cursor uint64
	Type   string
	Data   json.RawMessage
}

type EventRepository interface {
	Replay(ctx context.Context, projectID, executionID string, afterCursor uint64, limit int) ([]DurableEvent, error)
}

// ReplayWaiter is only a bounded wakeup/heartbeat mechanism. A wake carries no
// payload and is never authoritative; the handler always replays from the
// durable repository after it returns.
type ReplayWaiter interface {
	Wait(ctx context.Context, projectID, executionID string, afterCursor uint64) (heartbeat bool, err error)
}

type EventAuthorizer interface {
	AuthorizeExecutionEvents(ctx context.Context, projectID, executionID string) error
}

type EventHandler struct {
	authorizer             EventAuthorizer
	repository             EventRepository
	waiter                 ReplayWaiter
	batchSize              int
	writeTimeout           time.Duration
	authorizationTimeout   time.Duration
	authorizationAdmission *sseAuthorizationGate
	admission              *sseAdmissionGate
}

func NewEventHandler(authorizer EventAuthorizer, repository EventRepository, waiter ReplayWaiter) (*EventHandler, error) {
	return NewEventHandlerWithReplayCapacity(
		authorizer,
		repository,
		waiter,
		defaultMaxSSEAuthorizations,
	)
}

// NewEventHandlerWithReplayCapacity binds authorization concurrency to the
// database capacity that serves both execution-policy lookup and durable
// replay. Stream lifetime limits remain the built-in defaults.
func NewEventHandlerWithReplayCapacity(
	authorizer EventAuthorizer,
	repository EventRepository,
	waiter ReplayWaiter,
	replayCapacity int,
) (*EventHandler, error) {
	return NewEventHandlerWithStreamLimits(
		authorizer,
		repository,
		waiter,
		replayCapacity,
		DefaultSSEStreamLimits(),
	)
}

// NewEventHandlerWithStreamLimits binds authorization concurrency to the
// database capacity that serves both execution-policy lookup and durable
// replay, and takes the stream lifetime profile explicitly. The zero profile
// keeps the built-in defaults.
func NewEventHandlerWithStreamLimits(
	authorizer EventAuthorizer,
	repository EventRepository,
	waiter ReplayWaiter,
	replayCapacity int,
	limits SSEStreamLimits,
) (*EventHandler, error) {
	if authorizer == nil || repository == nil || waiter == nil {
		return nil, errors.New("event authorizer, repository and waiter are required")
	}
	if limits == (SSEStreamLimits{}) {
		limits = DefaultSSEStreamLimits()
	}
	admission := newSSEAdmissionGate(
		limits.MaxStreams,
		limits.MaxPerPrincipal,
		limits.MaxPerProject,
	)
	if admission == nil {
		return nil, errors.New("event stream admission profile is invalid")
	}
	authorizationAdmission := newSSEAuthorizationGate(
		replayCapacity,
		defaultMaxSSEAuthorizationsPerPrincipal,
	)
	if authorizationAdmission == nil {
		return nil, errors.New("event stream authorization profile is invalid")
	}
	return &EventHandler{
		authorizer:             authorizer,
		repository:             repository,
		waiter:                 waiter,
		batchSize:              defaultReplayBatchSize,
		writeTimeout:           defaultSSEWriteTimeout,
		authorizationTimeout:   defaultSSEAuthorizationTimeout,
		authorizationAdmission: authorizationAdmission,
		admission:              admission,
	}, nil
}

func (h *EventHandler) Stream(w http.ResponseWriter, r *http.Request) {
	projectID := chi.URLParam(r, "projectID")
	executionID := chi.URLParam(r, "executionID")
	if projectID == "" || executionID == "" {
		http.Error(w, "project and execution are required", http.StatusBadRequest)
		return
	}
	principalID, ok := sseOwningPrincipalID(r.Context())
	if !ok {
		http.Error(w, "runtime authentication required", http.StatusUnauthorized)
		return
	}
	if err := h.authorizeInitial(r.Context(), principalID, projectID, executionID); err != nil {
		if errors.Is(err, ErrExecutionEventsForbidden) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if errors.Is(err, errSSEAuthorizationBusy) {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "event authorization capacity is busy", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "authorization failed", http.StatusInternalServerError)
		return
	}

	cursor, err := requestedCursor(r)
	if err != nil {
		http.Error(w, "invalid event cursor", http.StatusBadRequest)
		return
	}
	release, admitted := h.admission.acquire(principalID, projectID)
	if !admitted {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "too many active event streams", http.StatusTooManyRequests)
		return
	}
	defer release()

	initial, err := h.repository.Replay(r.Context(), projectID, executionID, cursor, h.batchSize)
	if err != nil {
		http.Error(w, "event replay failed", http.StatusInternalServerError)
		return
	}

	stream, err := newBoundedSSEWriter(w, h.writeTimeout)
	if err != nil {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	if err := stream.writeAndFlush(func() error {
		w.WriteHeader(http.StatusOK)
		if len(initial) == 0 {
			_, err := fmt.Fprint(w, ": connected\n\n")
			return err
		}
		return nil
	}); err != nil {
		return
	}

	events := initial
	for {
		if len(events) > 0 {
			if err := stream.writeAndFlush(func() error {
				for _, event := range events {
					if event.Cursor <= cursor || validateDurableEvent(event) != nil {
						return ErrInvalidEventStream
					}
					if err := writeDurableEvent(w, event); err != nil {
						return err
					}
					cursor = event.Cursor
				}
				return nil
			}); err != nil {
				return
			}
		}

		if len(events) == h.batchSize {
			if err := h.reauthorize(r.Context(), principalID, projectID, executionID); err != nil {
				return
			}
			events, err = h.repository.Replay(r.Context(), projectID, executionID, cursor, h.batchSize)
			if err != nil {
				return
			}
			continue
		}

		heartbeat, err := h.waiter.Wait(r.Context(), projectID, executionID, cursor)
		if err != nil {
			return
		}
		// Project suspension or role revocation closes the stream before another
		// repository fetch. The waiter bounds idle detection to two seconds; the
		// already-authorized write exposure is documented with the write timeout.
		if err := h.reauthorize(r.Context(), principalID, projectID, executionID); err != nil {
			return
		}
		if heartbeat {
			if err := stream.writeAndFlush(func() error {
				_, writeErr := fmt.Fprint(w, ": heartbeat\n\n")
				return writeErr
			}); err != nil {
				return
			}
		}
		events, err = h.repository.Replay(r.Context(), projectID, executionID, cursor, h.batchSize)
		if err != nil {
			return
		}
	}
}

func (h *EventHandler) authorizeInitial(
	ctx context.Context,
	principalID,
	projectID,
	executionID string,
) error {
	if h == nil || h.authorizer == nil || h.authorizationAdmission == nil ||
		h.authorizationTimeout <= 0 || ctx == nil || principalID == "" {
		return errors.New("event stream authorization is unavailable")
	}
	release, admitted := h.authorizationAdmission.acquire(principalID)
	if !admitted {
		return errSSEAuthorizationBusy
	}
	defer release()

	authorizationContext, cancel := context.WithTimeout(ctx, h.authorizationTimeout)
	defer cancel()
	return h.authorizer.AuthorizeExecutionEvents(authorizationContext, projectID, executionID)
}

func (h *EventHandler) reauthorize(
	ctx context.Context,
	principalID,
	projectID,
	executionID string,
) error {
	if h == nil || h.authorizer == nil || h.authorizationAdmission == nil ||
		h.authorizationTimeout <= 0 || ctx == nil || principalID == "" {
		return errors.New("event stream authorization is unavailable")
	}
	authorizationContext, cancel := context.WithTimeout(ctx, h.authorizationTimeout)
	defer cancel()
	release, err := h.authorizationAdmission.acquireContext(
		authorizationContext,
		principalID,
	)
	if err != nil {
		return err
	}
	defer release()
	return h.authorizer.AuthorizeExecutionEvents(authorizationContext, projectID, executionID)
}

type boundedSSEWriter struct {
	controller *http.ResponseController
	timeout    time.Duration
}

func newBoundedSSEWriter(response http.ResponseWriter, timeout time.Duration) (*boundedSSEWriter, error) {
	if response == nil || timeout <= 0 {
		return nil, errors.New("bounded SSE writer configuration is invalid")
	}
	if !supportsResponseFlush(response) {
		return nil, errors.New("SSE response does not support flushing")
	}
	controller := http.NewResponseController(response)
	// Clear the public server's request-wide WriteTimeout before streaming.
	// Every actual write below installs its own short deadline and clears it
	// after the flush, so an idle but healthy SSE connection remains long-lived.
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear inherited SSE write deadline: %w", err)
	}
	return &boundedSSEWriter{controller: controller, timeout: timeout}, nil
}

func supportsResponseFlush(response http.ResponseWriter) bool {
	for depth := 0; depth < 16 && response != nil; depth++ {
		if _, ok := response.(interface{ FlushError() error }); ok {
			return true
		}
		if _, ok := response.(http.Flusher); ok {
			return true
		}
		unwrapper, ok := response.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		next := unwrapper.Unwrap()
		if next == response {
			return false
		}
		response = next
	}
	return false
}

func (w *boundedSSEWriter) writeAndFlush(write func() error) error {
	if write == nil {
		return errors.New("SSE write is required")
	}
	if err := w.controller.SetWriteDeadline(time.Now().Add(w.timeout)); err != nil {
		return err
	}
	writeErr := write()
	flushErr := w.controller.Flush()
	clearErr := w.controller.SetWriteDeadline(time.Time{})
	return errors.Join(writeErr, flushErr, clearErr)
}

func requestedCursor(r *http.Request) (uint64, error) {
	header := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	query := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if header != "" && query != "" && header != query {
		return 0, errors.New("conflicting event cursors")
	}
	value := header
	if value == "" {
		value = query
	}
	if value == "" {
		return 0, nil
	}
	return strconv.ParseUint(value, 10, 64)
}

func validateDurableEvent(event DurableEvent) error {
	if event.Cursor == 0 || event.Type == "" || strings.ContainsAny(event.Type, "\r\n") || len(event.Data) == 0 || len(event.Data) > maxSSEEventBytes || !json.Valid(event.Data) {
		return ErrInvalidEventStream
	}
	return nil
}

func writeDurableEvent(w http.ResponseWriter, event DurableEvent) error {
	data := []byte(event.Data)
	if !isCompactSingleLineJSON(data) {
		var compact bytes.Buffer
		if err := json.Compact(&compact, data); err != nil {
			return err
		}
		data = compact.Bytes()
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: ", event.Cursor, event.Type); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := fmt.Fprint(w, "\n\n")
	return err
}

func isCompactSingleLineJSON(data []byte) bool {
	inString := false
	escaped := false
	for _, value := range data {
		if inString {
			switch {
			case escaped:
				escaped = false
			case value == '\\':
				escaped = true
			case value == '"':
				inString = false
			}
			continue
		}
		switch value {
		case '"':
			inString = true
		case ' ', '\t', '\n', '\r':
			return false
		}
	}
	return json.Valid(data)
}
