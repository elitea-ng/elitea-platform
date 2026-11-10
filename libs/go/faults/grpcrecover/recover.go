// Package grpcrecover turns a panic in a gRPC handler into one fault line
// (internal.panic, with a bounded stack) and an Internal status that holds
// only the sentence and the code, never the panic value or the stack (C2).
package grpcrecover

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/elitea-ng/elitea-platform/libs/go/faults"
)

// Status is what a caller gets after a panic.
func Status() error {
	return status.Error(codes.Internal, fmt.Sprintf("%s (%s)", faults.InternalPanic.Sentence(), faults.InternalPanic))
}

// UnaryServerInterceptor recovers panics in unary handlers.
func UnaryServerInterceptor(logger *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				faults.Log(ctx, logger, slog.LevelError, faults.Panic(recovered, slog.String("grpc_method", info.FullMethod)))
				resp, err = nil, Status()
			}
		}()
		return handler(ctx, req)
	}
}

// StreamServerInterceptor recovers panics in stream handlers.
func StreamServerInterceptor(logger *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				faults.Log(stream.Context(), logger, slog.LevelError, faults.Panic(recovered, slog.String("grpc_method", info.FullMethod)))
				err = Status()
			}
		}()
		return handler(srv, stream)
	}
}
