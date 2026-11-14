// Package agentmemoryembed embeds an agent's memory text through the LLM
// gateway (issue 1657): the project's default embedding model, called as
// POST /llm/v1/embeddings over the same gRPC hop every other elitea-main
// caller of the gateway uses (llmproxy.Gateway). The gateway meters, budgets
// and audits the call like any other, under the project's identity.
//
// The model is the project's default embedding model (the project's own, else
// the platform's, else the first catalogue model: the rule index runs use,
// #2042). The answer is cached per project for a few minutes, so a recall costs
// one embedding call, not a catalogue read as well. A project with no embedding
// model gets ErrNoEmbeddingModel, which the callers treat like any outage.
package agentmemoryembed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentmemoryvector"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
)

// ErrNoEmbeddingModel reports a project with no embedding model to call.
var ErrNoEmbeddingModel = errors.New("agent memory: the project has no embedding model")

// modelCacheTTL is how long a project's default model is kept.
const modelCacheTTL = 5 * time.Minute

// maxResponseBytes bounds the embeddings answer: 32 vectors of a large model.
const maxResponseBytes = 16 << 20

// Catalog reads the model catalogue (*configurationapp.CurrentModelCatalogService).
type Catalog interface {
	Get(context.Context, configurationapp.CurrentModelCatalogQuery) (configurationapp.CurrentModelCatalogResponse, error)
}

// Embedder is the gateway-backed agentmemoryvector.Embedder.
type Embedder struct {
	gateway         llmproxy.Gateway
	catalog         Catalog
	publicProjectID int32

	mutex  sync.Mutex
	models map[int64]cachedModel
	now    func() time.Time
}

type cachedModel struct {
	name    string
	expires time.Time
}

var _ agentmemoryvector.Embedder = (*Embedder)(nil)

// New builds the embedder.
func New(gateway llmproxy.Gateway, catalog Catalog, publicProjectID int32) (*Embedder, error) {
	if gateway == nil || catalog == nil || publicProjectID <= 0 {
		return nil, errors.New("agent memory embedder needs the gateway, the model catalogue and the public project")
	}
	return &Embedder{
		gateway: gateway, catalog: catalog, publicProjectID: publicProjectID,
		models: map[int64]cachedModel{}, now: time.Now,
	}, nil
}

func (e *Embedder) model(ctx context.Context, projectID int64) (string, error) {
	e.mutex.Lock()
	cached, ok := e.models[projectID]
	e.mutex.Unlock()
	if ok && e.now().Before(cached.expires) {
		return cached.name, nil
	}
	if projectID <= 0 || projectID > 1<<31-1 {
		return "", ErrNoEmbeddingModel
	}
	catalog, err := e.catalog.Get(ctx, configurationapp.CurrentModelCatalogQuery{
		Section:         configurationapp.CurrentModelSectionEmbedding,
		ProjectID:       int32(projectID),
		PublicProjectID: e.publicProjectID,
		IncludeShared:   true,
	})
	if err != nil {
		return "", fmt.Errorf("read the embedding model catalogue: %w", err)
	}
	if catalog.DefaultModelName == nil || *catalog.DefaultModelName == "" {
		return "", ErrNoEmbeddingModel
	}
	e.mutex.Lock()
	if len(e.models) > 4096 {
		clear(e.models)
	}
	e.models[projectID] = cachedModel{name: *catalog.DefaultModelName, expires: e.now().Add(modelCacheTTL)}
	e.mutex.Unlock()
	return *catalog.DefaultModelName, nil
}

type embeddingsRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed embeds inputs, one vector each, in order.
func (e *Embedder) Embed(ctx context.Context, projectID int64, inputs []string) (agentmemoryvector.Embedding, error) {
	if len(inputs) == 0 {
		return agentmemoryvector.Embedding{}, errors.New("agent memory: nothing to embed")
	}
	model, err := e.model(ctx, projectID)
	if err != nil {
		return agentmemoryvector.Embedding{}, err
	}
	body, err := json.Marshal(embeddingsRequest{Model: model, Input: inputs})
	if err != nil {
		return agentmemoryvector.Embedding{}, fmt.Errorf("encode the embeddings request: %w", err)
	}
	response, err := e.gateway.Embeddings(ctx, &llmproxy.Request{
		Header:   http.Header{"Content-Type": {"application/json"}},
		Body:     bytes.NewReader(body),
		Identity: llmproxy.Identity{ProjectID: fmt.Sprint(projectID)},
	})
	if err != nil {
		return agentmemoryvector.Embedding{}, fmt.Errorf("call the gateway: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		// The gateway's body is not echoed: it can carry provider text.
		return agentmemoryvector.Embedding{}, fmt.Errorf("the gateway answered %d", response.StatusCode)
	}
	var decoded embeddingsResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&decoded); err != nil {
		return agentmemoryvector.Embedding{}, fmt.Errorf("decode the embeddings answer: %w", err)
	}
	if len(decoded.Data) != len(inputs) {
		return agentmemoryvector.Embedding{}, fmt.Errorf("the gateway returned %d vectors for %d inputs", len(decoded.Data), len(inputs))
	}
	// An answer numbers its items by `index`; one that leaves the field out
	// numbers them all 0, and then its own order is the order.
	numbered := false
	for _, item := range decoded.Data {
		if item.Index != 0 {
			numbered = true
		}
	}
	vectors := make([][]float32, len(inputs))
	for position, item := range decoded.Data {
		index := position
		if numbered {
			index = item.Index
		}
		if index < 0 || index >= len(inputs) || vectors[index] != nil || len(item.Embedding) == 0 {
			return agentmemoryvector.Embedding{}, errors.New("the gateway's embeddings answer is unusable")
		}
		vectors[index] = item.Embedding
	}
	return agentmemoryvector.Embedding{Model: model, Vectors: vectors}, nil
}
