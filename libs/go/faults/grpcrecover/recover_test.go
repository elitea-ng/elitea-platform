package grpcrecover

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// C2: the status holds the sentence and the code only.
func TestAPanicIsAnInternalStatusWithoutItsValueOrStack(t *testing.T) {
	var logs bytes.Buffer
	interceptor := UnaryServerInterceptor(slog.New(slog.NewJSONHandler(&logs, nil)))
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/elitea.runtime.v1.Control/RenewLease"},
		func(context.Context, any) (any, error) { panic("secret-value token=abc") })
	st, _ := status.FromError(err)
	if st.Code() != codes.Internal || !strings.Contains(st.Message(), "internal.panic") {
		t.Fatalf("status = %v %q", st.Code(), st.Message())
	}
	for _, leak := range []string{"secret-value", "goroutine", ".go:"} {
		if strings.Contains(st.Message(), leak) {
			t.Fatalf("the status carries %q", leak)
		}
	}
	if strings.Contains(logs.String(), "secret-value") || !strings.Contains(logs.String(), "RenewLease") {
		t.Fatalf("log = %q", logs.String())
	}
}

type stream struct{ grpc.ServerStream }

func (stream) Context() context.Context { return context.Background() }

func TestAStreamPanicIsRecoveredTheSameWay(t *testing.T) {
	interceptor := StreamServerInterceptor(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	err := interceptor(nil, stream{}, &grpc.StreamServerInfo{FullMethod: "/elitea.runtime.v1.Output/Publish"},
		func(any, grpc.ServerStream) error { panic(42) })
	if st, _ := status.FromError(err); st.Code() != codes.Internal || strings.Contains(st.Message(), "42") {
		t.Fatalf("status = %v %q", st.Code(), st.Message())
	}
}
