package mcpregistry

// Unit cover for the transport details the PostgreSQL acceptance tests cannot
// reach.
//
// The acceptance test in `internal/api/v2/mcp` drives a fake MCP server that
// answers `application/json`. The specification lets a server answer the same
// POST with an event stream instead, and several widely used servers do. That
// branch has its own framing, its own "skip frames that answer something else"
// rule, and its own end conditions, so it is exercised here directly rather
// than left to the first real server that uses it.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func itoa(value int) string { return strconv.Itoa(value) }

// eventStreamServer answers initialize and tools/list as SSE frames.
func eventStreamServer(t *testing.T, toolsFrame string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if err := decodeJSON(r, &request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if request.Method == "initialize" {
			writeFrame(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`)
			return
		}
		// A notification the client did not ask for arrives first. It must be
		// skipped, not mistaken for the answer and not treated as a failure.
		writeFrame(w, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`)
		writeFrame(w, toolsFrame)
	}))
	t.Cleanup(server.Close)
	return server
}

// writeFrame emits one SSE frame. Every line of the payload carries its own
// `data:` prefix, which is what the SSE rules require and what a real MCP
// server sends. A fixture that prefixes only the first line is malformed, and
// it would test the parser against input no server produces.
func writeFrame(w http.ResponseWriter, payload string) {
	_, _ = w.Write([]byte("event: message\n"))
	for _, line := range strings.Split(payload, "\n") {
		_, _ = w.Write([]byte("data: " + line + "\n"))
	}
	_, _ = w.Write([]byte("\n"))
}

func decodeJSON(r *http.Request, into any) error {
	return json.NewDecoder(r.Body).Decode(into)
}

// The core claim for this branch: an event-stream answer yields the same tools
// a JSON answer would.
func TestDiscoverReadsToolsFromAnEventStreamAnswer(t *testing.T) {
	server := eventStreamServer(t, `{"jsonrpc":"2.0","id":2,"result":{"tools":[
		{"name":"create_page","description":"Create a page",
		 "inputSchema":{"type":"object","properties":{"title":{"type":"string"}}}},
		{"name":"search","description":"Search pages","inputSchema":{"type":"object"}}]}}`)

	tools, err := NewDiscoverer(http.DefaultClient.Do).
		Discover(context.Background(), server.URL, nil)
	if err != nil {
		t.Fatalf("discover over an event stream: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("tools = %+v, want the two the server published", tools)
	}
	if tools[0].Name != "create_page" || tools[0].Description != "Create a page" {
		t.Fatalf("first tool = %+v, want create_page", tools[0])
	}
	properties, _ := tools[0].InputSchema["properties"].(map[string]any)
	if properties["title"] == nil {
		t.Fatalf("inputSchema lost properties.title: %v", tools[0].InputSchema)
	}
	if tools[1].Name != "search" {
		t.Fatalf("second tool = %+v, want search", tools[1])
	}
}

// A refusal the server states must surface as an error, not as an empty tool
// list. An empty list would replace a working registration with nothing.
func TestDiscoverReportsAStatedRefusal(t *testing.T) {
	server := eventStreamServer(t,
		`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"tools are not available"}}`)

	tools, err := NewDiscoverer(http.DefaultClient.Do).
		Discover(context.Background(), server.URL, nil)
	if err == nil {
		t.Fatalf("a stated refusal returned tools = %+v and no error", tools)
	}
	if !strings.Contains(err.Error(), "tools are not available") {
		t.Fatalf("error = %v, want the server's stated message", err)
	}
}

// The caller's headers must reach the server. A pre-built MCP toolkit
// authenticates with them, so dropping them would turn every authenticated
// server into a failed discovery.
func TestDiscoverSendsTheCallerHeadersAndTheProtocolHeaders(t *testing.T) {
	var authorization, accept, contentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if err := decodeJSON(r, &request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Method == "tools/list" {
			authorization = r.Header.Get("Authorization")
			accept = r.Header.Get("Accept")
			contentType = r.Header.Get("Content-Type")
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*request.ID) + `,"result":{"tools":[]}}`))
	}))
	t.Cleanup(server.Close)

	_, err := NewDiscoverer(http.DefaultClient.Do).Discover(
		context.Background(), server.URL, map[string]string{"Authorization": "Bearer secret-token"})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if authorization != "Bearer secret-token" {
		t.Fatalf("Authorization = %q, want the caller's header", authorization)
	}
	// Accept must name both encodings, or a server that prefers an event
	// stream has no way to say so.
	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		t.Fatalf("Accept = %q, want both encodings", accept)
	}
	if contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
}

// A caller header must not be able to overwrite the protocol headers, or a
// stored toolkit configuration could break the exchange.
func TestCallerHeadersCannotOverwriteTheProtocolHeaders(t *testing.T) {
	var accept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if err := decodeJSON(r, &request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.Method == "tools/list" {
			accept = r.Header.Get("Accept")
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + itoa(*request.ID) + `,"result":{"tools":[]}}`))
	}))
	t.Cleanup(server.Close)

	if _, err := NewDiscoverer(http.DefaultClient.Do).Discover(
		context.Background(), server.URL, map[string]string{"Accept": "text/plain"}); err != nil {
		t.Fatalf("discover: %v", err)
	}
	if strings.Contains(accept, "text/plain") {
		t.Fatalf("Accept = %q, want the protocol value to win", accept)
	}
}

// A server that answers a status outside 2xx is a failed discovery.
func TestDiscoverTreatsANonSuccessStatusAsAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	if _, err := NewDiscoverer(http.DefaultClient.Do).
		Discover(context.Background(), server.URL, nil); err == nil {
		t.Fatal("a 500 from the MCP server was accepted as a discovery")
	}
}

// #6688: a 405/410 on a `/sse` URL is a retired HTTP+SSE endpoint. The error
// names it and suggests the `/mcp` replacement; the same status on any other
// path stays a plain failed discovery.
func TestDiscoverNamesARetiredSSEEndpoint(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusGone} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		t.Cleanup(server.Close)

		_, err := NewDiscoverer(http.DefaultClient.Do).Discover(context.Background(), server.URL+"/v1/sse?x=1", nil)
		var retired *RetiredSSEEndpoint
		if !errors.As(err, &retired) {
			t.Fatalf("status %d on /sse: want RetiredSSEEndpoint, got %v", status, err)
		}
		if retired.Suggested != server.URL+"/v1/mcp" || retired.Status != status {
			t.Fatalf("suggested %q status %d", retired.Suggested, retired.Status)
		}
		for _, want := range []string{server.URL + "/v1/sse", server.URL + "/v1/mcp", "HTTP+SSE"} {
			if !strings.Contains(retired.Message(), want) {
				t.Fatalf("message %q lacks %q", retired.Message(), want)
			}
		}

		_, err = NewDiscoverer(http.DefaultClient.Do).Discover(context.Background(), server.URL+"/mcp", nil)
		if err == nil || errors.As(err, &retired) {
			t.Fatalf("status %d on /mcp must be a plain failure, got %v", status, err)
		}
	}
}
