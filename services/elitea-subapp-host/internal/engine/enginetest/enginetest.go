// Package enginetest serves a fake engine sidecar — any
// elitea.engine.v1.EngineServiceServer — on a real Unix socket, for the
// tests of the host's engine client and of the runners built on it.
package enginetest

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"

	enginev1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/engine/v1"
)

// Serve runs server on a fresh Unix socket until the test ends and returns
// the socket's path. macOS caps a socket path at 104 bytes, so it lives in a
// short directory under /tmp rather than t.TempDir.
func Serve(t testing.TB, server enginev1.EngineServiceServer) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "eng")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "e.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	served := grpc.NewServer(grpc.MaxRecvMsgSize(128 << 20))
	enginev1.RegisterEngineServiceServer(served, server)
	go func() { _ = served.Serve(listener) }()
	t.Cleanup(served.Stop)
	return socket
}

// Event is one event from the JSON document the NDJSON protocol carried for
// it — `{"thinking": …}`, `{"token": …}`, `{"result": {…}}` or
// `{"error": {message, error_type, error_category[, code]}}` — so a test
// scripts a stream the way it always read.
func Event(line string) (*enginev1.InvokeEvent, error) {
	var document struct {
		Thinking *string         `json:"thinking"`
		Token    *string         `json:"token"`
		Result   json.RawMessage `json:"result"`
		Error    *struct {
			Message       string `json:"message"`
			ErrorType     string `json:"error_type"`
			ErrorCategory string `json:"error_category"`
			Code          string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &document); err != nil {
		return nil, fmt.Errorf("%q is not a line: %w", line, err)
	}
	switch {
	case document.Thinking != nil:
		return &enginev1.InvokeEvent{Event: &enginev1.InvokeEvent_Thinking{Thinking: *document.Thinking}}, nil
	case document.Token != nil:
		return &enginev1.InvokeEvent{Event: &enginev1.InvokeEvent_Token{Token: *document.Token}}, nil
	case document.Error != nil:
		return &enginev1.InvokeEvent{Event: &enginev1.InvokeEvent_Error{Error: &enginev1.EngineError{
			Message: document.Error.Message, ErrorType: document.Error.ErrorType,
			ErrorCategory: document.Error.ErrorCategory, Code: document.Error.Code,
		}}}, nil
	case document.Result != nil:
		return &enginev1.InvokeEvent{Event: &enginev1.InvokeEvent_ResultJson{ResultJson: document.Result}}, nil
	}
	return nil, fmt.Errorf("%q is none of thinking, token, result or error", line)
}
