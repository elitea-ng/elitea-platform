package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	tracer          trace.Tracer
	requestDuration metric.Float64Histogram
	requestsTotal   metric.Int64Counter
	activeRequests  metric.Int64UpDownCounter
)

func init() {
	tracer = otel.Tracer("elitea-main")
	meter := otel.Meter("elitea-main")

	requestDuration, _ = meter.Float64Histogram(
		"http.server.request.duration",
		metric.WithUnit("s"),
		metric.WithDescription("HTTP request duration in seconds"),
	)
	requestsTotal, _ = meter.Int64Counter(
		"http.server.requests_total",
		metric.WithDescription("Total HTTP requests"),
	)
	activeRequests, _ = meter.Int64UpDownCounter(
		"http.server.active_requests",
		metric.WithDescription("Number of active HTTP requests"),
	)
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

// Unwrap preserves optional streaming and deadline interfaces for
// http.ResponseController (SSE, HTTP/2 flush, and per-write deadlines).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush makes the recorder an http.Flusher too. A wrapper inside it that
// flushes by asserting http.Flusher on the writer it holds (chi's Compress
// middleware does) would otherwise find none here, and an event stream's
// frames would wait in the server's buffer (the project event stream did).
func (r *statusRecorder) Flush() {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(body []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.ResponseWriter.Write(body)
}

const unmatchedRoutePattern = "unmatched"

func matchedRoutePattern(r *http.Request) string {
	routeContext := chi.RouteContext(r.Context())
	if routeContext == nil {
		return unmatchedRoutePattern
	}
	pattern := routeContext.RoutePattern()
	if pattern == "" {
		return unmatchedRoutePattern
	}
	return pattern
}

func normalizedHTTPMethod(method string) string {
	switch method {
	case http.MethodConnect,
		http.MethodDelete,
		http.MethodGet,
		http.MethodHead,
		http.MethodOptions,
		http.MethodPatch,
		http.MethodPost,
		http.MethodPut,
		http.MethodTrace:
		return method
	default:
		return "_OTHER"
	}
}

func OtelMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The raw target may contain user, project, token, and execution IDs. Start
		// with a bounded name and replace it with the registered chi pattern only
		// after routing completes.
		method := normalizedHTTPMethod(r.Method)
		ctx, span := tracer.Start(r.Context(), method+" request")
		defer span.End()

		activeRequests.Add(ctx, 1)
		defer activeRequests.Add(ctx, -1)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		request := r.WithContext(ctx)

		next.ServeHTTP(rec, request)

		duration := time.Since(start).Seconds()
		route := matchedRoutePattern(request)
		attrs := []attribute.KeyValue{
			attribute.String("http.method", method),
			attribute.String("http.route", route),
			attribute.String("http.status_code", strconv.Itoa(rec.status)),
		}

		requestDuration.Record(ctx, duration, metric.WithAttributes(attrs...))
		requestsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))

		span.SetName(method + " " + route)
		span.SetAttributes(attrs...)
	})
}
