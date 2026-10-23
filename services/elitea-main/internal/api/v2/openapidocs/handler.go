// Package openapidocs serves the current API spec and a minimal docs UI over
// HTTP — the legacy shared plugin's openapi/swagger-ui routes had no Go
// counterpart at all (issue #251 item 3). The UI is a small, fully
// self-contained HTML/JS page (no swagger-ui/Stoplight bundle, no CDN
// fetch): it renders the same JSON this package converts server-side from
// api/openapi/v2.yaml, keeping the strict-CSP, no-external-network posture
// from #177's discussion without vendoring a third-party asset bundle.
package openapidocs

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/api/openapi"
	"gopkg.in/yaml.v3"
)

type Handler struct {
	specYAML []byte

	once     sync.Once
	specJSON []byte
	specErr  error
}

func NewHandler() *Handler {
	return &Handler{specYAML: openapi.SpecYAML}
}

// Spec serves the raw hand-maintained spec byte-for-byte.
func (h *Handler) Spec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.specYAML)
}

// SpecJSON serves the spec converted to JSON for the docs UI to consume
// without a client-side YAML parser.
func (h *Handler) SpecJSON(w http.ResponseWriter, _ *http.Request) {
	h.once.Do(func() {
		var document any
		if err := yaml.Unmarshal(h.specYAML, &document); err != nil {
			h.specErr = err
			return
		}
		h.specJSON, h.specErr = json.Marshal(convertYAMLMapKeys(document))
	})
	if h.specErr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"failed to load OpenAPI spec"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.specJSON)
}

// UI serves a minimal, self-contained docs page — no external stylesheet,
// script, font, or image request of any kind.
func (h *Handler) UI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(docsPageHTML))
}

// convertYAMLMapKeys rewrites map[interface{}]interface{} to
// map[string]interface{} so encoding/json can marshal it (json.Marshal
// rejects the former with an unsupported-type error). Verified this is not
// dead code: a trivial YAML doc decodes straight to map[string]interface{}
// under yaml.v3, but the real api/openapi/v2.yaml does not — some part of
// the actual spec still produces map[interface{}]interface{} nodes, and
// removing this caused SpecJSON to 500 against the real embedded spec.
func convertYAMLMapKeys(value any) any {
	switch v := value.(type) {
	case map[string]any:
		converted := make(map[string]any, len(v))
		for key, item := range v {
			converted[key] = convertYAMLMapKeys(item)
		}
		return converted
	case map[any]any:
		converted := make(map[string]any, len(v))
		for key, item := range v {
			if keyString, ok := key.(string); ok {
				converted[keyString] = convertYAMLMapKeys(item)
			}
		}
		return converted
	case []any:
		converted := make([]any, len(v))
		for i, item := range v {
			converted[i] = convertYAMLMapKeys(item)
		}
		return converted
	default:
		return v
	}
}

const docsPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Elitea API Reference</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; margin: 0; color: #1a1a1a; }
  header { padding: 1rem 1.5rem; border-bottom: 1px solid #ddd; }
  header h1 { font-size: 1.1rem; margin: 0; }
  main { display: flex; }
  nav { width: 320px; height: calc(100vh - 3.5rem); overflow-y: auto; border-right: 1px solid #ddd; padding: 0.5rem; box-sizing: border-box; }
  nav button { display: block; width: 100%; text-align: left; padding: 0.35rem 0.5rem; border: none; background: none; cursor: pointer; font-size: 0.85rem; border-radius: 4px; }
  nav button:hover { background: #f0f0f0; }
  nav .method { display: inline-block; width: 3.2rem; font-weight: 600; font-size: 0.7rem; }
  section#detail { flex: 1; padding: 1.5rem; max-width: 900px; }
  code, pre { background: #f5f5f5; padding: 0.15rem 0.35rem; border-radius: 4px; }
  pre { padding: 0.75rem; overflow-x: auto; }
  .op-id { color: #666; font-size: 0.85rem; }
</style>
</head>
<body>
<header><h1>Elitea API Reference</h1></header>
<main>
  <nav id="nav">Loading spec&hellip;</nav>
  <section id="detail"><p>Select an operation from the list.</p></section>
</main>
<script>
(function () {
  "use strict";
  fetch("/api/openapi.json").then(function (r) { return r.json(); }).then(render).catch(function (err) {
    document.getElementById("nav").textContent = "Failed to load spec: " + err;
  });

  function render(spec) {
    var nav = document.getElementById("nav");
    nav.textContent = "";
    var paths = spec.paths || {};
    Object.keys(paths).sort().forEach(function (path) {
      var operations = paths[path];
      Object.keys(operations).forEach(function (method) {
        if (method === "parameters") { return; }
        var op = operations[method];
        var button = document.createElement("button");
        var methodSpan = document.createElement("span");
        methodSpan.className = "method";
        methodSpan.textContent = method.toUpperCase();
        button.appendChild(methodSpan);
        button.appendChild(document.createTextNode(path));
        button.addEventListener("click", function () { showDetail(method, path, op); });
        nav.appendChild(button);
      });
    });
  }

  function showDetail(method, path, op) {
    var detail = document.getElementById("detail");
    detail.textContent = "";
    var heading = document.createElement("h2");
    heading.textContent = method.toUpperCase() + " " + path;
    detail.appendChild(heading);
    if (op.operationId) {
      var opId = document.createElement("div");
      opId.className = "op-id";
      opId.textContent = op.operationId;
      detail.appendChild(opId);
    }
    if (op.summary) {
      var summary = document.createElement("p");
      summary.textContent = op.summary;
      detail.appendChild(summary);
    }
    if (op.description) {
      var description = document.createElement("pre");
      description.textContent = op.description;
      detail.appendChild(description);
    }
    var responsesHeading = document.createElement("h3");
    responsesHeading.textContent = "Responses";
    detail.appendChild(responsesHeading);
    var responses = document.createElement("pre");
    responses.textContent = JSON.stringify(op.responses || {}, null, 2);
    detail.appendChild(responses);
  }
})();
</script>
</body>
</html>
`
