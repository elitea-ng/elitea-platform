package webedge

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The edge is now the ONLY place a cluster decides which process answers a
// path (#1359 P3): the chart's Ingress or HTTPRoute is one `/` rule to
// elitea-main. These cases are the browser paths the old split route carried
// as separate rules, and each one must land where it did before.
func TestEdgeRoutesEveryBrowserFamily(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Answered-By", "web")
		_, _ = io.WriteString(w, r.URL.Path)
	}))
	t.Cleanup(web.Close)
	upstream, err := ParseUpstream(web.URL)
	if err != nil {
		t.Fatal(err)
	}
	main := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Answered-By", "main")
	})
	edge := httptest.NewServer(Middleware(upstream)(main))
	t.Cleanup(edge.Close)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	cases := []struct {
		path string
		want string
	}{
		// The SPA and the embedded docs.
		{"/app", "web"},
		{"/app/", "web"},
		{"/app/1/chat", "web"},
		{"/app/assets/index.js", "web"},
		{"/docs", "web"},
		{"/docs/deployment/from-scratch", "web"},
		// The MCP server inside the SPA's namespace: the carve-out that once
		// needed a RegularExpression match at the cluster edge.
		{"/app/1/mcp", "main"},
		{"/app/42/mcp/", "main"},
		{"/app/42/mcp/sse", "main"},
		// Not a numeric project id, so it is the SPA's deep link.
		{"/app/abc/mcp", "web"},
		{"/app/1/mcpx", "web"},
		// elitea-main's file servers under /app.
		{"/app/application_icon/1/a.png", "main"},
		{"/app/application_tool_icon/1/a.png", "main"},
		// Everything elitea-main serves from the root.
		{"/api/v2/projects", "main"},
		{"/llm/v1/chat/completions", "main"},
		{"/healthz", "main"},
		{"/auth/login", "main"},
		{"/icons/1/a.png", "main"},
		{"/admin/app", "main"},
	}
	for _, testCase := range cases {
		response, err := client.Get(edge.URL + testCase.path)
		if err != nil {
			t.Fatalf("GET %s: %v", testCase.path, err)
		}
		_ = response.Body.Close()
		if got := response.Header.Get("X-Answered-By"); got != testCase.want {
			t.Errorf("GET %s answered by %q, want %q (status %d)", testCase.path, got, testCase.want, response.StatusCode)
		}
	}

	// `/` is the edge's own redirect, answered by neither process.
	response, err := client.Get(edge.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusFound || response.Header.Get("Location") != "/app/" {
		t.Errorf("GET / = %d Location %q, want 302 /app/", response.StatusCode, response.Header.Get("Location"))
	}
	if got := response.Header.Get("X-Answered-By"); got != "" {
		t.Errorf("GET / was answered by %q; the edge redirects it itself", got)
	}

	// /readyz and /startupz are refused to a non-loopback caller. httptest
	// listens on loopback, so a peer outside it is simulated on the handler.
	for _, path := range NotPublished {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "10.1.2.3:40000"
		Middleware(upstream)(main).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound || recorder.Header().Get("X-Answered-By") != "" {
			t.Errorf("GET %s from a pod-network peer = %d by %q; the edge must refuse it", path, recorder.Code, recorder.Header().Get("X-Answered-By"))
		}
	}
}
