package runtimecomposition

import (
	"context"
	"errors"
	indexing "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	toolkitexecution "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
)

// currentActorToolkitReader applies the folder access overlay before settings resolution.
// The underlying read does not require MCP exposure or tool selection.
type currentActorToolkitReader struct {
	reader toolkitexecution.CurrentMCPToolkitReader
}

func newCurrentActorToolkitReader(reader indexing.CurrentToolkitReader) (indexing.CurrentToolkitReader, error) {
	scoped, ok := reader.(toolkitexecution.CurrentMCPToolkitReader)
	if !ok {
		return nil, errors.New("actor-scoped toolkit reader is required")
	}
	return currentActorToolkitReader{reader: scoped}, nil
}
func (r currentActorToolkitReader) GetCurrentToolkit(ctx context.Context, projectID, actorID, toolkitID int32) (indexing.CurrentToolkitSnapshot, bool, error) {
	toolkit, found, err := r.reader.GetCurrentMCPToolkit(ctx, projectID, actorID, toolkitID)
	if err != nil || !found {
		return indexing.CurrentToolkitSnapshot{}, found, err
	}
	return indexing.CurrentToolkitSnapshot{ID: toolkit.ID, Type: toolkit.Type, Name: toolkit.Name, Settings: toolkit.Settings}, true, nil
}
