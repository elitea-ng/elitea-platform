package agentmemoryembed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentmemoryvector"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
)

type fakeGateway struct {
	llmproxy.Gateway
	status  int
	body    string
	err     error
	request embeddingsRequest
	project string
	calls   int
}

func (g *fakeGateway) Embeddings(_ context.Context, request *llmproxy.Request) (*llmproxy.Response, error) {
	g.calls++
	g.project = request.Identity.ProjectID
	raw, _ := io.ReadAll(request.Body)
	_ = json.Unmarshal(raw, &g.request)
	if g.err != nil {
		return nil, g.err
	}
	return &llmproxy.Response{StatusCode: g.status, Body: io.NopCloser(strings.NewReader(g.body))}, nil
}

type fakeCatalog struct {
	name  *string
	err   error
	calls int
	query configurationapp.CurrentModelCatalogQuery
}

func (c *fakeCatalog) Get(_ context.Context, query configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error) {
	c.calls++
	c.query = query
	return configurationapp.CurrentModelCatalogResponse{DefaultModelName: c.name}, c.err
}

func ptr(value string) *string { return &value }

func TestEmbedCallsTheProjectsDefaultModelAndOrdersTheVectors(t *testing.T) {
	gateway := &fakeGateway{status: http.StatusOK, body: `{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`}
	catalog := &fakeCatalog{name: ptr("embed-small")}
	embedder, err := New(gateway, catalog, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := embedder.Embed(context.Background(), 7, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "embed-small" || len(got.Vectors) != 2 || got.Vectors[0][0] != 1 || got.Vectors[1][1] != 1 {
		t.Fatalf("embedding %+v", got)
	}
	if gateway.request.Model != "embed-small" || len(gateway.request.Input) != 2 || gateway.project != "7" {
		t.Fatalf("request %+v project %q", gateway.request, gateway.project)
	}
	if catalog.query.ProjectID != 7 || catalog.query.PublicProjectID != 1 || catalog.query.Section != configurationapp.CurrentModelSectionEmbedding {
		t.Fatalf("catalogue query %+v", catalog.query)
	}
	// The model is cached: a second call does not read the catalogue again.
	if _, err := embedder.Embed(context.Background(), 7, []string{"a", "b"}); err != nil || catalog.calls != 1 {
		t.Fatalf("catalogue calls %d, err %v", catalog.calls, err)
	}
	// ...until the cache expires.
	embedder.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := embedder.Embed(context.Background(), 7, []string{"a", "b"}); err != nil || catalog.calls != 2 {
		t.Fatalf("catalogue calls %d, err %v", catalog.calls, err)
	}
}

func TestEmbedFailsClosedOnEveryOutage(t *testing.T) {
	for name, build := range map[string]func() (*fakeGateway, *fakeCatalog){
		"no model":       func() (*fakeGateway, *fakeCatalog) { return &fakeGateway{}, &fakeCatalog{} },
		"empty model":    func() (*fakeGateway, *fakeCatalog) { return &fakeGateway{}, &fakeCatalog{name: ptr("")} },
		"catalogue down": func() (*fakeGateway, *fakeCatalog) { return &fakeGateway{}, &fakeCatalog{err: errors.New("down")} },
		"gateway down": func() (*fakeGateway, *fakeCatalog) {
			return &fakeGateway{err: errors.New("down")}, &fakeCatalog{name: ptr("m")}
		},
		"gateway refuses": func() (*fakeGateway, *fakeCatalog) { return &fakeGateway{status: 402}, &fakeCatalog{name: ptr("m")} },
		"bad json": func() (*fakeGateway, *fakeCatalog) {
			return &fakeGateway{status: 200, body: "{"}, &fakeCatalog{name: ptr("m")}
		},
		"too few vectors": func() (*fakeGateway, *fakeCatalog) {
			return &fakeGateway{status: 200, body: `{"data":[]}`}, &fakeCatalog{name: ptr("m")}
		},
		"an empty vector": func() (*fakeGateway, *fakeCatalog) {
			return &fakeGateway{status: 200, body: `{"data":[{"index":0,"embedding":[]}]}`}, &fakeCatalog{name: ptr("m")}
		},
		"a repeated index": func() (*fakeGateway, *fakeCatalog) {
			return &fakeGateway{status: 200, body: `{"data":[{"index":1,"embedding":[1]},{"index":1,"embedding":[1]}]}`}, &fakeCatalog{name: ptr("m")}
		},
	} {
		gateway, catalog := build()
		embedder, err := New(gateway, catalog, 1)
		if err != nil {
			t.Fatal(err)
		}
		inputs := []string{"a"}
		if name == "a repeated index" {
			inputs = []string{"a", "b"}
		}
		if got, err := embedder.Embed(context.Background(), 7, inputs); err == nil {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestNewRefusesMissingDependencies(t *testing.T) {
	if _, err := New(nil, &fakeCatalog{}, 1); err == nil {
		t.Fatal("nil gateway")
	}
	if _, err := New(&fakeGateway{}, nil, 1); err == nil {
		t.Fatal("nil catalogue")
	}
	if _, err := New(&fakeGateway{}, &fakeCatalog{}, 0); err == nil {
		t.Fatal("no public project")
	}
}

func TestTheGatewaysRefusalIsProjectScopedButItsOutageIsNot(t *testing.T) {
	for status, scoped := range map[int]bool{400: true, 402: true, 403: true, 404: true, 429: false, 500: false, 503: false} {
		embedder, err := New(&fakeGateway{status: status}, &fakeCatalog{name: ptr("m")}, 1)
		if err != nil {
			t.Fatal(err)
		}
		_, err = embedder.Embed(context.Background(), 7, []string{"a"})
		if err == nil || errors.Is(err, agentmemoryvector.ErrProjectScoped) != scoped {
			t.Errorf("status %d: scoped=%v err=%v", status, scoped, err)
		}
	}
	embedder, err := New(&fakeGateway{}, &fakeCatalog{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := embedder.Embed(context.Background(), 7, []string{"a"}); !errors.Is(err, agentmemoryvector.ErrProjectScoped) {
		t.Fatalf("no model: %v", err)
	}
}
