package vectorclient

// An agent's own memory in elitea-vector (ADR-0050 decision 1b, issue 1657):
// source AGENT_MEMORY, one namespace per (project, agent), one document per
// entry (document_key the entry id), one point per document (chunk_id "0").
//
// TWO CALLERS, ONE CONNECTION. Delete is the administrator's: the platform's
// own certificate may remove points of any source, so removal needs no token
// and works whenever the store is up. Upsert and Search need a project-bound
// TOKEN that names the source; the caller passes the platform token
// (vectorintrospection.PlatformTokens) it minted, which this client sends as
// `authorization: Bearer <token>`.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	vectorv1 "github.com/elitea-ng/elitea-platform/libs/proto/gen/go/elitea/vector/v1"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentmemoryvector"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/providerhost/platformops"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
)

// agentMemoryNamespaceRoot is the UUIDv5 namespace of every agent's vector
// namespace: UUIDv5(NAMESPACE_URL, "https://elitea.ai/ns/agent-memory").
// Never change it: every stored vector is addressed under it.
var agentMemoryNamespaceRoot = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://elitea.ai/ns/agent-memory"))

// AgentMemoryNamespace is the elitea-vector namespace_id of one agent's
// memory: the canonical text of UUIDv5(root, "<projectID>/<applicationID>").
func AgentMemoryNamespace(projectID, applicationID int64) string {
	return uuid.NewSHA1(agentMemoryNamespaceRoot, fmt.Appendf(nil, "%d/%d", projectID, applicationID)).String()
}

// maxAgentMemoryBatch is elitea-vector's cap of points per batch.
const maxAgentMemoryBatch = 512

func bearer(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// UpsertAgentMemory writes the entries of one agent. Idempotent: an entry's
// point id is fixed by (project, source, namespace, entry id), so a repeat
// replaces the point.
func (c *Client) UpsertAgentMemory(ctx context.Context, token string, projectID, applicationID int64, space agentmemoryvector.Space, points []agentmemoryvector.Point) error {
	if c == nil {
		return fmt.Errorf("%w: no elitea-vector client", platformops.ErrConfig)
	}
	if token == "" || projectID <= 0 || applicationID <= 0 || len(points) == 0 || len(points) > maxAgentMemoryBatch {
		return fmt.Errorf("%w: an agent memory upsert needs a token, a project, an agent and 1 to %d points", platformops.ErrConfig, maxAgentMemoryBatch)
	}
	callCtx, cancel := context.WithTimeout(bearer(ctx, token), c.indexTimeout)
	defer cancel()
	stream, err := c.vectors.Upsert(callCtx)
	if err != nil {
		return fmt.Errorf("elitea-vector agent memory upsert (project %d): %w", projectID, err)
	}
	if err := stream.Send(&vectorv1.UpsertRequest{Message: &vectorv1.UpsertRequest_Header{Header: &vectorv1.UpsertHeader{
		ProjectId: projectID,
		Space:     &vectorv1.EmbeddingSpace{ModelSlug: space.ModelSlug, Dimension: space.Dimension},
		Namespace: &vectorv1.Namespace{
			Source:      vectorv1.Source_SOURCE_AGENT_MEMORY,
			NamespaceId: AgentMemoryNamespace(projectID, applicationID),
		},
	}}}); err != nil {
		return upsertStreamError(stream, err)
	}
	batch := make([]*vectorv1.Point, len(points))
	for i, point := range points {
		batch[i] = &vectorv1.Point{
			DocumentKey:     strconv.FormatInt(point.EntryID, 10),
			DocumentVersion: strconv.Itoa(point.Version),
			ChunkId:         "0",
			ChunkType:       "memory",
			Text:            point.Text,
			MetadataJson:    fmt.Sprintf(`{"entry_id":%d,"kind":%q}`, point.EntryID, point.Kind),
			Vector:          point.Vector,
		}
	}
	if err := stream.Send(&vectorv1.UpsertRequest{Message: &vectorv1.UpsertRequest_Batch{Batch: &vectorv1.UpsertBatch{Points: batch}}}); err != nil {
		return upsertStreamError(stream, err)
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		return fmt.Errorf("elitea-vector agent memory upsert (project %d): %w", projectID, err)
	}
	return nil
}

// upsertStreamError reads the stream's own status after a failed Send (a Send
// returns io.EOF when the server already ended the stream with one).
func upsertStreamError(stream vectorv1.VectorService_UpsertClient, sendErr error) error {
	if errors.Is(sendErr, io.EOF) {
		if _, err := stream.CloseAndRecv(); err != nil {
			return fmt.Errorf("elitea-vector agent memory upsert: %w", err)
		}
	}
	return fmt.Errorf("elitea-vector agent memory upsert: %w", sendErr)
}

// SearchAgentMemory ranks the entries of one agent against vector, highest
// cosine similarity first, at most limit.
func (c *Client) SearchAgentMemory(ctx context.Context, token string, projectID, applicationID int64, space agentmemoryvector.Space, vector []float32, limit uint32) ([]agentmemoryvector.Hit, error) {
	if c == nil {
		return nil, fmt.Errorf("%w: no elitea-vector client", platformops.ErrConfig)
	}
	if token == "" || projectID <= 0 || applicationID <= 0 || len(vector) == 0 || limit == 0 {
		return nil, fmt.Errorf("%w: an agent memory search needs a token, a project, an agent, a vector and a limit", platformops.ErrConfig)
	}
	callCtx, cancel := context.WithTimeout(bearer(ctx, token), c.indexTimeout)
	defer cancel()
	response, err := c.vectors.Search(callCtx, &vectorv1.SearchRequest{
		Scope: &vectorv1.Scope{
			ProjectId:    projectID,
			Source:       vectorv1.Source_SOURCE_AGENT_MEMORY,
			NamespaceIds: []string{AgentMemoryNamespace(projectID, applicationID)},
		},
		Space:  &vectorv1.EmbeddingSpace{ModelSlug: space.ModelSlug, Dimension: space.Dimension},
		Vector: vector,
		Limit:  limit,
	})
	if err != nil {
		return nil, fmt.Errorf("elitea-vector agent memory search (project %d): %w", projectID, err)
	}
	hits := make([]agentmemoryvector.Hit, 0, len(response.GetPoints()))
	for _, point := range response.GetPoints() {
		id, parseErr := strconv.ParseInt(point.GetDocumentKey(), 10, 64)
		if parseErr != nil || id <= 0 {
			continue
		}
		hits = append(hits, agentmemoryvector.Hit{EntryID: id, Score: point.GetScore()})
	}
	return hits, nil
}

// DeleteAgentMemoryEntries removes the points of some entries of one agent, in
// every collection, as the administrator. Idempotent: an entry with no point is
// a clean success. The errors are platformops's classes.
func (c *Client) DeleteAgentMemoryEntries(ctx context.Context, projectID, applicationID int64, entryIDs []int64) error {
	if c == nil {
		return fmt.Errorf("%w: no elitea-vector client", platformops.ErrConfig)
	}
	if projectID <= 0 || projectID > 1<<31-1 || applicationID <= 0 || len(entryIDs) == 0 {
		return fmt.Errorf("%w: project %d, agent %d and at least one entry are required", platformops.ErrConfig, projectID, applicationID)
	}
	keys := make([]string, len(entryIDs))
	for i, id := range entryIDs {
		keys[i] = strconv.FormatInt(id, 10)
	}
	callCtx, cancel := context.WithTimeout(ctx, c.indexTimeout)
	defer cancel()
	response, err := c.vectors.Delete(callCtx, &vectorv1.DeleteRequest{
		ProjectId: projectID,
		Namespace: &vectorv1.Namespace{
			Source:      vectorv1.Source_SOURCE_AGENT_MEMORY,
			NamespaceId: AgentMemoryNamespace(projectID, applicationID),
		},
		Selector: &vectorv1.DeleteRequest_Documents{Documents: &vectorv1.DocumentKeys{DocumentKeys: keys}},
	})
	if err != nil {
		if ctxErr := callerEnded(ctx); ctxErr != nil {
			return ctxErr
		}
		return c.classifyMemoryDelete("delete_agent_memory", projectID, err)
	}
	c.logger.Debug("agent memory vectors deleted", "project_id", projectID, "application_id", applicationID,
		"entries", len(entryIDs), "collections", response.GetCollections())
	return nil
}

var _ agentmemoryvector.Store = (*Client)(nil)
