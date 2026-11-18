package main

// Semantic recall for an agent's own memory (ADR-0050 decision 1b, issue
// 1657): the composition of the embedding, the vector copy and the reconciler.
//
// WHAT RUNS. Every entry change writes a journal row in its own transaction
// (shared/0248, trigger tenant/0248). A reconciler in each elitea-main replica
// works those rows: it embeds an active entry through the LLM gateway and
// writes its point into elitea-vector (source agent_memory), or removes the
// point of an entry that is archived, pending or gone. Recall ranks the agent's
// rows by that similarity and falls back to the keyword ranking whenever the
// embedding or the vector store cannot answer.
//
// WHAT IT NEEDS, AND WHAT HAPPENS WITHOUT IT. All of: the elitea-vector client
// (ELITEA_VECTOR_GRPC_TARGET), the LLM gateway (LLM_GATEWAY_URL), the model
// catalogue, and APPLICATION_SECRET_KEY (the key of the platform token that
// elitea-vector verifies through ELITEA_VECTOR_INTROSPECTION_CLIENTS). With one
// of them missing the feature is off, one line says which, and recall is the
// keyword ranking it always was. ELITEA_AGENT_MEMORY_VECTORS_ENABLED=false
// turns it off on purpose.

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentmemoryvector"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentmemory"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/agentmemoryembed"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/vectorclient"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/llmproxy"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/runtimecomposition"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/transport/runtimegrpc/vectorintrospection"
	"github.com/jackc/pgx/v5/pgxpool"
)

// agentMemoryVectorsEnv switches the feature (default on).
const agentMemoryVectorsEnv = "ELITEA_AGENT_MEMORY_VECTORS_ENABLED"

// agentMemoryVectors is what the composition root hands on: the platform token
// the introspection service verifies, and the ranker recall consults. Either
// may be nil.
type agentMemoryVectors struct {
	Tokens *vectorintrospection.PlatformTokens
	Ranker agentmemory.Ranker
}

// agentMemoryVectorsEnabled reads the switch. An unparsable value is a boot
// error, so a typo never passes as "on".
func agentMemoryVectorsEnabled(lookup func(string) (string, bool)) (bool, error) {
	raw, _ := lookup(agentMemoryVectorsEnv)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s=%q is not a boolean (true/false)", agentMemoryVectorsEnv, raw)
	}
	return enabled, nil
}

// composeAgentMemoryVectors builds the pieces and starts the reconciler. It
// returns the zero value, never an error, when a requirement is missing: the
// feature is an addition, and an absent one is the keyword recall.
func composeAgentMemoryVectors(
	ctx context.Context,
	logger *slog.Logger,
	lookup func(string) (string, bool),
	pool *pgxpool.Pool,
	vectors *vectorclient.Client,
	gateway llmproxy.Gateway,
	configurations *runtimecomposition.CurrentConfigurationsRuntime,
	publicProjectID int32,
) (agentMemoryVectors, error) {
	enabled, err := agentMemoryVectorsEnabled(lookup)
	if err != nil {
		return agentMemoryVectors{}, err
	}
	if !enabled {
		logger.Info("agent memory vectors are off; recall ranks by keyword", "env", agentMemoryVectorsEnv)
		return agentMemoryVectors{}, nil
	}
	secret, _ := lookup("APPLICATION_SECRET_KEY")
	tokens, tokensErr := vectorintrospection.NewPlatformTokens(secret)
	switch {
	case pool == nil:
		logger.Info("agent memory vectors are off: no database; recall ranks by keyword")
		return agentMemoryVectors{}, nil
	case vectors == nil:
		logger.Info("agent memory vectors are off: no elitea-vector client; recall ranks by keyword", "set", vectorTargetEnv)
		return agentMemoryVectors{}, nil
	case gateway == nil || configurations == nil || publicProjectID <= 0:
		logger.Info("agent memory vectors are off: no LLM gateway or model catalogue; recall ranks by keyword")
		return agentMemoryVectors{}, nil
	case tokensErr != nil:
		logger.Info("agent memory vectors are off: no platform token key; recall ranks by keyword", "reason", tokensErr.Error())
		return agentMemoryVectors{}, nil
	}
	embedder, err := agentmemoryembed.New(gateway, configurations.ModelCatalog(), publicProjectID)
	if err != nil {
		return agentMemoryVectors{}, fmt.Errorf("compose the agent memory embedder: %w", err)
	}
	journal, err := dbrepos.NewAgentMemoryVectorJournal(pool)
	if err != nil {
		return agentMemoryVectors{}, fmt.Errorf("compose the agent memory vector journal: %w", err)
	}
	service, err := agentmemoryvector.New(journal, embedder, vectors, tokens, agentmemoryvector.Config{}, logger)
	if err != nil {
		return agentMemoryVectors{}, fmt.Errorf("compose the agent memory vectors: %w", err)
	}
	go service.Run(ctx)
	logger.Info("agent memory vectors are on: entries are embedded through the gateway and recall ranks by similarity")
	return agentMemoryVectors{Tokens: tokens, Ranker: service}, nil
}
