package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/browsersessions"
)

// The view route is mounted only when composed, and the group's middleware
// (audit, tracing, compression, no-store) leaves the response writer
// hijackable: a WebSocket upgrade through the whole router completes.
func TestRouterMountsTheBrowserViewAndKeepsItUpgradable(t *testing.T) {
	t.Parallel()
	upgraded := make(chan struct{}, 1)
	view := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		upgraded <- struct{}{}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	router := NewRouter(RouterConfig{BrowserSessionView: view})
	if !hasRoute(routePatterns(t, router), http.MethodGet, browsersessions.ViewPath) {
		t.Fatalf("%s is not registered", browsersessions.ViewPath)
	}
	if hasRoute(routePatterns(t, NewRouter(RouterConfig{})), http.MethodGet, browsersessions.ViewPath) {
		t.Fatalf("%s is registered with no relay composed", browsersessions.ViewPath)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v2/browser_sessions/elitea-br-000000000000000000000000/view"
	conn, response, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Accept-Encoding": []string{"gzip"}},
	})
	if err != nil {
		t.Fatalf("upgrade through the router: %v (response %v)", err, response)
	}
	defer func() { _ = conn.CloseNow() }()
	select {
	case <-upgraded:
	case <-ctx.Done():
		t.Fatal("the handler never upgraded")
	}
}
