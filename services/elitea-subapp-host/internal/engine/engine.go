// Package engine is the host's client for an engine sidecar (ADR-0023 H2).
//
// An application whose engine is not Go runs it as a sidecar next to this
// host and reaches it over a Unix socket the two containers share:
// elitea.engine.v1.EngineService (libs/proto/elitea/engine/v1), gRPC over
// the socket (#1359, which replaced the NDJSON-over-HTTP protocol). No TLS:
// the socket directory is the only way in (the pod-local Unix-socket
// exception, AGENTS.md). The host keeps the SPI, admission, the parameter
// merge, composition and upload; the sidecar runs one tool at a time:
//
//	Invoke(invocation_id, tool, arguments_json)
//	  → stream: thinking and token events interleaved, then result | error
//	Stop(invocation_id)  requests a cooperative stop
//	Health()             status, runner, active, tools
//
// Progress events become the invocation's thinking events as they arrive and
// token events become its answer events, IN THE ORDER THEY ARRIVE: they share
// one event list, so a poll shows the answer growing between the steps that
// produced it. Two kinds rather than one because the two are different things
// and a reader must not have to guess which (issue #701; the envelope is
// conformance/provider/fixtures/deepwiki/stream/token_events.json).
//
// The host's own stop checkpoint is watched while the stream is open, and a
// stop is forwarded to the sidecar — which terminates the engine's worker
// subprocess — before this side gives up on the stream.
//
// Which tools a sidecar serves, and how its result dict is composed, stay
// with the application; the Label a client is built with names the engine in
// the messages a caller reads.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	enginev1 "github.com/EliteaAI/elitea-platform/libs/proto/gen/go/elitea/engine/v1"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/spi"
)

const (
	// MaxStreamLineBytes is the largest event the host reads from a sidecar
	// stream, as JSON. A result carries every artifact inline, so it bounds
	// what one tool can return: the Inventory engine's export_graph returns
	// the whole graph.json, JSON-escaped, in artifacts[].data. The DeepWiki
	// engine's MAX_RESULT_LINE (src/runner/native.rs) is pinned to it by a
	// test that reads this file.
	MaxStreamLineBytes = 64 << 20
	// MaxExportDocumentBytes mirrors the Inventory engine's
	// MAX_EXPORT_DOCUMENT_BYTES (services/elitea-inventory-engine/src/lib.rs):
	// the largest escaped graph.json its export_graph returns. Above it the
	// engine refuses with a ValueError that names its export-graph command.
	// TestExportBoundMirrorsTheEngine reads the Rust constant and pins this
	// value to it, and pins MaxStreamLineBytes above it with headroom
	// (streamLineOverhead) for the result's envelope and the export summary.
	MaxExportDocumentBytes = 60 << 20
	// streamLineOverhead is the headroom MaxStreamLineBytes keeps above
	// MaxExportDocumentBytes.
	streamLineOverhead = 1 << 20
	// eventFraming is what one gRPC message adds around the largest event:
	// the field tags and lengths of InvokeEvent.
	eventFraming = 64
	// MaxInvokeBytes is the largest Invoke the host sends: the Inventory
	// engine's MAX_INVOKE_BYTES (import_graph carries a graph document of up
	// to 64 MiB, JSON-escaped once in the arguments).
	MaxInvokeBytes = 96 << 20
	// healthTimeout bounds one Health call: the list is read in the
	// background, and an engine that does not answer must not stall a caller.
	healthTimeout = 3 * time.Second
	// stopTimeout bounds the Stop a cancelled invocation sends.
	stopTimeout = 5 * time.Second
)

// ErrUnknownTool is what errors.Is finds in an error when the engine answered
// that it does not serve the tool asked for.
var ErrUnknownTool = errors.New("the engine does not serve this tool")

type unknownToolError struct{ error }

func (e unknownToolError) Unwrap() error      { return e.error }
func (unknownToolError) Is(target error) bool { return target == ErrUnknownTool }

// Client speaks elitea.engine.v1 over a Unix socket.
type Client struct {
	Socket string
	// Label names the engine in the messages a caller reads ("The DeepWiki
	// engine is not reachable at …"). Empty is "The engine".
	Label      string
	StopPeriod time.Duration

	rpc enginev1.EngineServiceClient
	// dialErr is a target the gRPC client could not even parse; every call
	// reports it.
	dialErr error
	caps    capabilities
}

// NewClient builds the client; nothing is dialled until the first call, and a
// failed dial is retried on the next. The connection is shared by every call
// (HTTP/2 streams), and a generation can run for an hour: no call here has a
// deadline the caller did not set.
func NewClient(socket, label string) *Client {
	client := &Client{Socket: socket, Label: label, StopPeriod: 250 * time.Millisecond}
	conn, err := grpc.NewClient("unix:"+socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(MaxStreamLineBytes+eventFraming),
			grpc.MaxCallSendMsgSize(MaxInvokeBytes),
		),
	)
	if err != nil {
		client.dialErr = err
		return client
	}
	client.rpc = enginev1.NewEngineServiceClient(conn)
	return client
}

// subject is how the engine is named in a message.
func (c *Client) subject() string {
	if c.Label == "" {
		return "The engine"
	}
	return "The " + c.Label + " engine"
}

// Invoke runs one tool and streams its progress into the invocation. It
// returns the engine's result dict — success or not; the caller maps a
// failed result — or an error for a transport failure, a refusal the
// sidecar reported, or a stop.
func (c *Client) Invoke(ctx context.Context, tool string, arguments map[string]any, tc *spi.Context) (map[string]any, error) {
	if c.dialErr != nil {
		return nil, spi.Failf(spi.KindRuntime, "%s is not reachable at %s: %v", c.subject(), c.Socket, c.dialErr)
	}
	body, err := json.Marshal(arguments)
	if err != nil {
		return nil, spi.Failf(spi.KindRuntime, "encode the engine request: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.rpc.Invoke(ctx, &enginev1.InvokeRequest{InvocationId: tc.InvocationID(), Tool: tool, ArgumentsJson: body})
	if err != nil {
		return nil, c.refused(err, false)
	}

	// The stop watcher: the host's checkpoint is the only place a stop is
	// visible; the sidecar must hear about it to kill the worker.
	stopped := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		ticker := time.NewTicker(c.StopPeriod)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if tc.Checkpoint() != nil {
					c.stop(tc.InvocationID())
					close(stopped)
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-watcherDone }()

	received := false
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			select {
			case <-stopped:
				return nil, spi.ErrCancelled
			default:
			}
			if tc.Checkpoint() != nil {
				return nil, spi.ErrCancelled
			}
			return nil, c.refused(err, received)
		}
		received = true
		switch next := event.GetEvent().(type) {
		case *enginev1.InvokeEvent_Thinking:
			if err := tc.Thinking(ctx, next.Thinking); err != nil {
				return nil, err
			}
		case *enginev1.InvokeEvent_Token:
			if err := tc.Token(ctx, next.Token); err != nil {
				return nil, err
			}
		case *enginev1.InvokeEvent_Error:
			failure := fmt.Errorf("%s", next.Error.GetMessage())
			if next.Error.GetErrorType() == "KeyError" && strings.HasPrefix(next.Error.GetMessage(), "Unknown tool:") {
				failure = unknownToolError{failure}
			}
			if code := next.Error.GetCode(); code != "" {
				return nil, spi.NewCodedFailure(KindOf(next.Error.GetErrorType()), code, failure)
			}
			return nil, spi.NewFailure(KindOf(next.Error.GetErrorType()), failure)
		case *enginev1.InvokeEvent_ResultJson:
			var result map[string]any
			if err := json.Unmarshal(next.ResultJson, &result); err != nil || result == nil {
				return nil, spi.Failf(spi.KindRuntime, "%s sent a result this host cannot read: %.120s", c.subject(), next.ResultJson)
			}
			return result, nil
		}
	}
	select {
	case <-stopped:
		return nil, spi.ErrCancelled
	default:
	}
	if tc.Checkpoint() != nil {
		return nil, spi.ErrCancelled
	}
	return nil, spi.Failf(spi.KindRuntime, "%s closed the stream without a result", c.subject())
}

// refused is the error for a call that failed: a refusal the sidecar
// reported, a sidecar that cannot be reached, a result over the limit, or a
// stream that broke after it had started (streaming).
func (c *Client) refused(err error, streaming bool) error {
	st, _ := status.FromError(err)
	switch {
	case st.Code() == codes.InvalidArgument && strings.HasPrefix(st.Message(), "Unknown tool:"):
		// An engine of an older release does not serve this tool (a
		// rolling deploy: the host is newer than the engine).
		return spi.NewFailure(spi.KindRuntime, unknownToolError{fmt.Errorf("%s refused the invocation: %s", c.subject(), st.Message())})
	case st.Code() == codes.ResourceExhausted && strings.Contains(st.Message(), "received message larger than max"):
		return spi.Failf(spi.KindRuntime, "%s sent a result over this host's %d MiB limit", c.subject(), MaxStreamLineBytes>>20)
	case streaming:
		return spi.Failf(spi.KindRuntime, "%s's stream ended in error: %v", c.subject(), st.Message())
	case st.Code() == codes.Unavailable:
		return spi.Failf(spi.KindRuntime, "%s is not reachable at %s: %v", c.subject(), c.Socket, st.Message())
	default:
		return spi.Failf(spi.KindRuntime, "%s refused the invocation: %s %s", c.subject(), st.Code(), st.Message())
	}
}

// stop asks the sidecar to stop one invocation. Best effort: the stream is
// cancelled either way, and the sidecar terminates its worker on its own
// checkpoint.
func (c *Client) stop(invocationID string) {
	ctx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	_, _ = c.rpc.Stop(ctx, &enginev1.StopRequest{InvocationId: invocationID})
}

// KindOf maps the sidecar's error_type — the Python exception class name —
// onto the host's kinds; the classifier does the rest.
func KindOf(errorType string) spi.Kind {
	switch errorType {
	case "FileNotFoundError":
		return spi.KindNotFound
	case "ValueError":
		return spi.KindValue
	case "MemoryError":
		return spi.KindMemory
	case "KeyError":
		return spi.KindKey
	case "RuntimeError", "":
		return spi.KindRuntime
	default:
		return spi.KindGeneric
	}
}
