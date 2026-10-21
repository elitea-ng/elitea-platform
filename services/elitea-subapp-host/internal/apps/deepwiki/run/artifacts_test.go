package run_test

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/apps/deepwiki/run"
	"github.com/EliteaAI/elitea-platform/services/elitea-subapp-host/internal/artifacts"
)

// The upload is the platform's artifact route, bearer-authenticated, the
// object in the `file` part, overwrite=true — and a refusal carries the
// platform's own text so the in-band message says why.
func TestTheHTTPClientUploadsTheWayThePythonClientDid(t *testing.T) {
	type seen struct{ method, path, query, auth, filename, body, partType string }
	var got seen
	status := http.StatusCreated
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// elitea-main parses the part's Content-Disposition itself, because
		// r.FormFile would strip "a/b/c.md" to "c.md" and a wiki key is a path.
		reader, err := r.MultipartReader()
		if err != nil {
			t.Errorf("not multipart: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := reader.NextPart()
		if err != nil || part.FormName() != "file" {
			t.Errorf("no file part: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		data, _ := io.ReadAll(part)
		got = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), params["filename"], string(data), part.Header.Get("Content-Type")}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"key":"` + params["filename"] + `"}`))
	}))
	defer server.Close()

	settings := run.ExtractArtifactSettings(map[string]any{"api_base": server.URL + "/llm/v1", "api_key": "minted", "organization": "90200"})
	if settings.BaseURL != server.URL || settings.ProjectID != "90200" {
		t.Fatalf("%+v", settings)
	}
	client, err := run.NewHTTPArtifactClient(settings, artifacts.CallbackTLS{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Upload(context.Background(), "Wiki-Artifacts", "acme--x--main/wiki_pages/a/b.md", []byte("# b")); err != nil {
		t.Fatal(err)
	}
	if got.method != http.MethodPost || got.path != "/api/v2/artifacts/objects/90200/wiki-artifacts" || got.query != "overwrite=true" ||
		got.auth != "Bearer minted" || got.filename != "acme--x--main/wiki_pages/a/b.md" || got.body != "# b" {
		t.Fatalf("%+v", got)
	}
	// No Content-Type on the part: elitea-main types the object by its
	// extension, and a manifest labelled octet-stream is invisible to the
	// wiki browser.
	if got.partType != "" {
		t.Fatalf("the part carried Content-Type %q", got.partType)
	}
	status = http.StatusForbidden
	err = client.Upload(context.Background(), "wiki-artifacts", "k", nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Not authorized to upload artifact (HTTP 403)") {
		t.Fatalf("%v", err)
	}
	status = http.StatusBadGateway
	err = client.Upload(context.Background(), "wiki-artifacts", "k", nil)
	if err == nil || !strings.HasPrefix(err.Error(), "Failed to upload artifact: HTTP 502") {
		t.Fatalf("%v", err)
	}
	for _, s := range []run.ArtifactSettings{{APIKey: "k"}, {BaseURL: "http://x"}} {
		if _, err := run.NewHTTPArtifactClient(s, artifacts.CallbackTLS{}); err == nil {
			t.Fatalf("%+v was accepted", s)
		}
	}
	// The LLM suffix is stripped in every legacy spelling.
	for base, want := range map[string]string{"http://x/llm/v1": "http://x", "http://x/llm/api/v2/": "http://x", "http://x/llm": "http://x", "http://x/api": "http://x/api"} {
		if got := run.ExtractArtifactSettings(map[string]any{"openai_api_base": base, "openai_api_key": "k", "project_id": 7.0}); got.BaseURL != want || got.ProjectID != "7" {
			t.Errorf("%s → %+v", base, got)
		}
	}
}
