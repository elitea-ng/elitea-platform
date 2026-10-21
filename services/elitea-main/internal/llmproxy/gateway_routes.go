package llmproxy

// gateway_routes.go — which Gateway method serves an /llm request. It mirrors
// the gateway router (elitea-llm-gateway internal/api/router.go), including
// the 404 and 405 answers for a request no method serves.

import (
	"context"
	"net/http"
	"strings"
)

// gatewayRPC names a Gateway method.
type gatewayRPC int

const (
	rpcNone gatewayRPC = iota
	rpcChatCompletions
	rpcCompletions
	rpcMessages
	rpcCountTokens
	rpcResponses
	rpcEmbeddings
	rpcImageGenerate
	rpcImageEdit
	rpcSpeech
	rpcTranscribe
	rpcListModels
	rpcGetModel
	rpcCheckConnection
	rpcListProviderModels
	rpcListProviderVoices
	rpcRealtime
)

const modelsPrefix = "/llm/v1/models/"

var postRoutes = map[string]gatewayRPC{
	"/llm/v1/chat/completions":      rpcChatCompletions,
	"/llm/v1/completions":           rpcCompletions,
	"/llm/v1/messages":              rpcMessages,
	"/llm/v1/messages/count_tokens": rpcCountTokens,
	"/llm/v1/responses":             rpcResponses,
	"/llm/v1/embeddings":            rpcEmbeddings,
	"/llm/v1/images/generations":    rpcImageGenerate,
	"/llm/v1/images/edits":          rpcImageEdit,
	"/llm/v1/images/variations":     rpcImageEdit,
	"/llm/v1/audio/speech":          rpcSpeech,
	"/llm/v1/audio/transcriptions":  rpcTranscribe,
	"/llm/v1/audio/translations":    rpcTranscribe,
	"/llm/v1/check_connection":      rpcCheckConnection,
	"/llm/v1/list_provider_models":  rpcListProviderModels,
	"/llm/v1/list_provider_voices":  rpcListProviderVoices,
	"/llm/v1/realtime":              rpcRealtime,
}

// routeOf returns the method that serves (method, path), or rpcNone.
func routeOf(method, path string) gatewayRPC {
	switch method {
	case http.MethodGet:
		switch {
		case path == "/llm/v1/models":
			return rpcListModels
		case strings.HasPrefix(path, modelsPrefix) && len(path) > len(modelsPrefix):
			return rpcGetModel
		case path == "/llm/v1/realtime":
			return rpcRealtime
		}
	case http.MethodPost:
		return postRoutes[path]
	}
	return rpcNone
}

// call returns g's method for rpc. Realtime is a session, not a call: it
// returns nil for it, as for rpcNone.
func (rpc gatewayRPC) call(g Gateway) func(context.Context, *Request) (*Response, error) {
	switch rpc {
	case rpcChatCompletions:
		return g.ChatCompletions
	case rpcCompletions:
		return g.Completions
	case rpcMessages:
		return g.Messages
	case rpcCountTokens:
		return g.CountTokens
	case rpcResponses:
		return g.Responses
	case rpcEmbeddings:
		return g.Embeddings
	case rpcImageGenerate:
		return g.ImageGenerate
	case rpcImageEdit:
		return g.ImageEdit
	case rpcSpeech:
		return g.Speech
	case rpcTranscribe:
		return g.Transcribe
	case rpcListModels:
		return g.ListModels
	case rpcGetModel:
		return g.GetModel
	case rpcCheckConnection:
		return g.CheckConnection
	case rpcListProviderModels:
		return g.ListProviderModels
	case rpcListProviderVoices:
		return g.ListProviderVoices
	}
	return nil
}

// Do dispatches req by its method and path, as the gateway's router does.
func (c *GatewayClient) Do(ctx context.Context, req *Request) (*Response, error) {
	call := routeOf(req.Method, req.Path).call(c)
	if call == nil {
		return nil, ErrNoGatewayRoute
	}
	return call(ctx, req)
}

// writeGatewayRouteMiss answers a request no Gateway method serves, byte for
// byte as the gateway router's NotFound, MethodNotAllowed and MessagesSubPath
// handlers answer it (gateway internal/llmproxy/handler.go, httpio.go).
func writeGatewayRouteMiss(w http.ResponseWriter, r *http.Request) {
	status, message := http.StatusNotFound, "unknown route"
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/llm/v1/messages/"):
		message = "unknown messages sub-path"
	case routeOf(http.MethodGet, p) != rpcNone || routeOf(http.MethodPost, p) != rpcNone ||
		strings.HasPrefix(p, "/llm/v1/messages/"):
		status, message = http.StatusMethodNotAllowed, "method not allowed"
	}
	w.Header().Set("Server", "Centry")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":{"message":"` + message + `","type":"invalid_request_error"}}` + "\n"))
}
