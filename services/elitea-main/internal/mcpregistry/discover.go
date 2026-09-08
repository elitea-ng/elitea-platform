package mcpregistry

// The MCP client half: ask a remote MCP server which tools it publishes.
//
// This is the streamable-HTTP transport from the MCP specification, which is
// plain HTTP with a JSON-RPC body. It is NOT socket.io, and it adds no server
// of any kind to this service: every exchange here is an outbound request that
// this process starts and finishes.
//
// pylon reaches the same servers through
// `indexer_worker/methods/indexer_mcp_sync_tools.py`, which imports the Python
// SDK and calls `discover_mcp_tools`. The tool descriptors it returns carry
// exactly three fields — `name`, `description`, `inputSchema` — which is what
// this reader keeps.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// protocolVersion is the MCP revision this client announces. It is the newest
// of the three the server half of this service accepts (see
// `internal/api/v2/mcp/server.go`), so the two halves agree on one revision.
const protocolVersion = "2025-06-18"

// maxResponseBytes bounds what a remote server can make this process hold. A
// tool listing is a few kilobytes; a server that sends more than this is not
// answering the question that was asked.
const maxResponseBytes = 4 << 20

// maxToolPages bounds the pagination loop. A server that keeps returning a
// cursor must not hold this request open forever.
const maxToolPages = 20

// Doer sends one prepared request. The caller supplies it, so this package
// never chooses the HTTP client, the redirect policy or the address rules: the
// existing MCP proxy path in `internal/api/v2/eliteacore` already validates the
// endpoint and refuses a redirect that leaves its origin, and passing its
// sender in reuses that guard rather than writing a second one.
type Doer func(*http.Request) (*http.Response, error)

// Discoverer reads the tool list of a remote MCP server.
type Discoverer struct {
	send Doer
}

func NewDiscoverer(send Doer) *Discoverer { return &Discoverer{send: send} }

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Discover performs the MCP handshake and returns the server's tools.
//
// The handshake is not optional. A server may refuse `tools/list` before
// `initialize`, and a server that keeps state gives out its session id in the
// `Mcp-Session-Id` response header of the initialize reply. Skipping the
// handshake works against some servers and fails against others, which is the
// worst of the three outcomes.
func (d *Discoverer) Discover(
	ctx context.Context, endpoint string, headers map[string]string,
) ([]Tool, error) {
	if d == nil || d.send == nil {
		return nil, fmt.Errorf("mcpregistry: no request sender configured")
	}

	sessionID, err := d.initialize(ctx, endpoint, headers)
	if err != nil {
		return nil, err
	}
	// The specification requires this notification after a successful
	// initialize. It carries no id and expects no reply, so its outcome is not
	// checked: a server that ignores it still answers tools/list.
	d.notifyInitialized(ctx, endpoint, headers, sessionID)

	tools := make([]Tool, 0)
	cursor := ""
	seen := make(map[string]struct{})
	for page := 0; page < maxToolPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := d.call(ctx, endpoint, headers, sessionID, 2+page, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var listing struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(result, &listing); err != nil {
			return nil, fmt.Errorf("mcpregistry: tools/list result is not a listing: %w", err)
		}
		for _, entry := range listing.Tools {
			name := strings.TrimSpace(entry.Name)
			if name == "" {
				continue
			}
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			seen[name] = struct{}{}
			schema := entry.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, Tool{Name: name, Description: entry.Description, InputSchema: schema})
		}
		if listing.NextCursor == "" || listing.NextCursor == cursor {
			break
		}
		cursor = listing.NextCursor
	}
	return tools, nil
}

func (d *Discoverer) initialize(
	ctx context.Context, endpoint string, headers map[string]string,
) (string, error) {
	body, err := json.Marshal(jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "elitea-main", "version": protocolVersion},
		},
	})
	if err != nil {
		return "", err
	}
	response, err := d.post(ctx, endpoint, headers, "", body)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()

	sessionID := response.Header.Get("Mcp-Session-Id")
	if _, err := readRPCResult(response, 1); err != nil {
		return "", err
	}
	return sessionID, nil
}

func (d *Discoverer) notifyInitialized(
	ctx context.Context, endpoint string, headers map[string]string, sessionID string,
) {
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	if err != nil {
		return
	}
	response, err := d.post(ctx, endpoint, headers, sessionID, body)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes))
	_ = response.Body.Close()
}

func (d *Discoverer) call(
	ctx context.Context, endpoint string, headers map[string]string,
	sessionID string, id int, method string, params any,
) (json.RawMessage, error) {
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	response, err := d.post(ctx, endpoint, headers, sessionID, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	return readRPCResult(response, id)
}

func (d *Discoverer) post(
	ctx context.Context, endpoint string, headers map[string]string, sessionID string, body []byte,
) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Caller headers first, so the three below cannot be overwritten by a
	// stored configuration. Accept must name both encodings: the specification
	// lets a server answer a POST with either a JSON body or an event stream.
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("MCP-Protocol-Version", protocolVersion)
	if sessionID != "" {
		request.Header.Set("Mcp-Session-Id", sessionID)
	}
	return d.send(request)
}

// readRPCResult extracts the result of the request with the given id from a
// response that may be a JSON body or an event stream.
func readRPCResult(response *http.Response, id int) (json.RawMessage, error) {
	if response.StatusCode == http.StatusUnauthorized {
		return nil, authorizationRequired(response.Header.Values("WWW-Authenticate"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("mcpregistry: MCP server answered %d", response.StatusCode)
	}
	limited := io.LimitReader(response.Body, maxResponseBytes)

	if strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return readEventStreamResult(limited, id)
	}

	payload, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	return decodeRPCResult(payload, id)
}

// readEventStreamResult walks an SSE body and returns the first `data:` frame
// that carries the answer to the given id.
//
// Frames for other ids are skipped rather than treated as an error: a server
// may push notifications on the same stream, and one of them arriving first
// must not fail the read.
func readEventStreamResult(body io.Reader, id int) (json.RawMessage, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxResponseBytes)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if value, isData := strings.CutPrefix(line, "data:"); isData {
			// The SSE rules: every line of a payload carries its own `data:`,
			// the lines join with a newline, and exactly ONE leading space is
			// removed. A line WITHOUT the prefix is not a continuation — it is
			// another field, and it is ignored, which is what the branch below
			// does.
			//
			// For a JSON payload this join is equivalent to concatenating the
			// lines: whitespace between JSON tokens is insignificant, and a raw
			// newline inside a string is illegal, so no valid payload can tell
			// the two apart. The rule is followed anyway, because a reader that
			// matches the specification is one less thing to re-derive.
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(value, " "))
			continue
		}
		if strings.TrimSpace(line) != "" {
			continue
		}
		if data.Len() == 0 {
			continue
		}
		frame := data.String()
		data.Reset()
		if result, err := decodeRPCResult([]byte(frame), id); err == nil {
			return result, nil
		} else if isRPCError(err) {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if data.Len() > 0 {
		return decodeRPCResult([]byte(data.String()), id)
	}
	return nil, fmt.Errorf("mcpregistry: MCP event stream carried no answer to request %d", id)
}

// rpcError marks a refusal the server stated, as opposed to a frame that simply
// did not answer this request. The two must not be confused: the first ends the
// read, the second continues it.
type rpcError struct{ message string }

func (e rpcError) Error() string { return e.message }

func isRPCError(err error) bool {
	var stated rpcError
	return errors.As(err, &stated)
}

func decodeRPCResult(payload []byte, id int) (json.RawMessage, error) {
	var response jsonRPCResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, fmt.Errorf("mcpregistry: MCP server did not answer with JSON-RPC: %w", err)
	}
	if response.ID == nil || *response.ID != id {
		return nil, fmt.Errorf("mcpregistry: frame does not answer request %d", id)
	}
	if response.Error != nil {
		return nil, rpcError{message: fmt.Sprintf(
			"mcpregistry: MCP server refused %d: %s", response.Error.Code, response.Error.Message)}
	}
	if len(response.Result) == 0 {
		return nil, fmt.Errorf("mcpregistry: MCP server answered request %d with no result", id)
	}
	return response.Result, nil
}
