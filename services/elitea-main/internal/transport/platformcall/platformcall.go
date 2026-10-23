// Package platformcall runs one platform-listener RPC as an in-process HTTP
// request against elitea-main's public handler (#1359 U5).
//
// The artifact and internal-tool services on the platform gRPC listener do
// not re-implement the routes they replace. Each RPC builds the request the
// HTTP caller used to send — a fixed route, the caller's bearer from the
// `authorization` metadata — and hands it to the public handler, so
// authentication, the project's permission resolution, the per-bucket access
// lists and every other gate run as one implementation for the browser and
// the workloads alike. Nothing crosses a socket between the RPC and the
// handler. elitea.llm.v1.ModelService (internal/transport/modelgrpc) does the
// same for /llm.
package platformcall

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/grpcauth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/transport/modelgrpc"
)

// NewRequest builds the in-process request one RPC makes: method on path
// (decoded; it is routed as is, never re-parsed), with query, body and the
// call's bearer. A missing or malformed bearer is not refused here: the
// handler's authentication answers it (401), so the refusal reads as the
// HTTP caller saw it.
func NewRequest(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, "/", body)
	if err != nil {
		return nil, status.Error(codes.Internal, "the platform request could not be built")
	}
	request.URL = &url.URL{Path: path, RawQuery: query.Encode()}
	request.RequestURI = request.URL.RequestURI()
	request.Host = "elitea-main"
	request.Proto, request.ProtoMajor, request.ProtoMinor = "HTTP/2.0", 2, 0
	if address, ok := peer.FromContext(ctx); ok && address.Addr != nil {
		request.RemoteAddr = address.Addr.String()
	} else {
		request.RemoteAddr = net.JoinHostPort("127.0.0.1", "0")
	}
	if token, err := grpcauth.BearerToken(ctx); err == nil {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request, nil
}

// Serve runs handler and reports whether it aborted the response (a panic,
// http.ErrAbortHandler included). A panic other than the abort sentinel is
// logged.
func Serve(handler http.Handler, writer http.ResponseWriter, request *http.Request, logger *slog.Logger) (aborted bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recovered != http.ErrAbortHandler { //nolint:errorlint // the sentinel is panicked by value
				if logger == nil {
					logger = slog.Default()
				}
				logger.Error("platformcall: the handler panicked", "path", request.URL.Path, "panic", fmt.Sprint(recovered))
			}
			aborted = true
		}
	}()
	handler.ServeHTTP(writer, request)
	return false
}

// ContextStatus is the status of a call whose context ended, or nil.
func ContextStatus(ctx context.Context) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	return nil
}

// CodeForHTTPStatus maps a refusal's HTTP status to its gRPC code: the
// mapping elitea.llm.v1.ModelRefusal documents, shared by every platform
// service so a caller classifies them alike.
func CodeForHTTPStatus(httpStatus int) codes.Code { return modelgrpc.CodeForHTTPStatus(httpStatus) }

// Refuse is the status of a refused call, with refusal encoded under
// trailer. The trailer is set on ctx's call; a refusal that does not encode
// still ends the call with the mapped code.
func Refuse(ctx context.Context, trailer string, httpStatus int, refusal proto.Message, what string) error {
	if encoded, err := proto.Marshal(refusal); err == nil {
		// grpc.SetTrailer reaches a stream's trailer through its context too.
		_ = grpc.SetTrailer(ctx, metadata.Pairs(trailer, string(encoded)))
	}
	return status.Errorf(CodeForHTTPStatus(httpStatus), "the %s was refused (HTTP %d)", what, httpStatus)
}

// HeaderSafe reports whether value can stand as one header value: the
// ModelService rule (modelgrpc.HeaderSafe), so the two cannot drift.
func HeaderSafe(value string) bool { return modelgrpc.HeaderSafe(value) }

// Buffer is the http.ResponseWriter of an RPC whose answer is read whole. A
// success body beyond limit marks the answer too large (the handler's write
// fails); a refusal body is kept up to limit and the rest dropped.
type Buffer struct {
	header   http.Header
	code     int
	body     []byte
	limit    int
	tooLarge bool
}

// NewBuffer is a Buffer keeping at most limit body bytes.
func NewBuffer(limit int) *Buffer { return &Buffer{header: http.Header{}, limit: limit} }

func (b *Buffer) Header() http.Header { return b.header }

func (b *Buffer) WriteHeader(code int) {
	if b.code != 0 || (code >= 100 && code <= 199) {
		return
	}
	b.code = code
}

func (b *Buffer) Write(data []byte) (int, error) {
	if b.code == 0 {
		b.WriteHeader(http.StatusOK)
	}
	room := b.limit - len(b.body)
	if len(data) > room {
		if Success(b.code) {
			b.tooLarge = true
			return 0, fmt.Errorf("platformcall: the answer exceeds %d bytes", b.limit)
		}
		b.body = append(b.body, data[:max(room, 0)]...)
		return len(data), nil
	}
	b.body = append(b.body, data...)
	return len(data), nil
}

// Flush is a no-op: the answer is read whole.
func (b *Buffer) Flush() {}

// Status is the answered status (200 when the handler wrote nothing).
func (b *Buffer) Status() int {
	if b.code == 0 {
		return http.StatusOK
	}
	return b.code
}

// Body is the kept body.
func (b *Buffer) Body() []byte { return b.body }

// TooLarge reports a success body beyond the limit.
func (b *Buffer) TooLarge() bool { return b.tooLarge }

// Success reports a 2xx status.
func Success(code int) bool { return code >= 200 && code <= 299 }
