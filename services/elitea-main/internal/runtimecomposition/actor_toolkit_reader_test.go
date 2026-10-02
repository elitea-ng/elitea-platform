package runtimecomposition

import (
	"context"
	indexing "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	toolkitexecution "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/toolkitexecution"
	"testing"
)

type actorReaderStub struct {
	project, actor, toolkit int32
	visible                 bool
}

func (s *actorReaderStub) GetCurrentToolkit(context.Context, int32, int32, int32) (indexing.CurrentToolkitSnapshot, bool, error) {
	panic("project-only read used")
}
func (s *actorReaderStub) GetCurrentMCPToolkit(_ context.Context, project, actor, toolkit int32) (toolkitexecution.CurrentMCPToolkitSnapshot, bool, error) {
	s.project = project
	s.actor = actor
	s.toolkit = toolkit
	return toolkitexecution.CurrentMCPToolkitSnapshot{ID: toolkit, Type: "github", Name: "saved", Settings: map[string]any{}}, s.visible, nil
}
func TestActorToolkitReaderUsesFolderScopedRead(t *testing.T) {
	for _, visible := range []bool{true, false} {
		stub := &actorReaderStub{visible: visible}
		reader, err := newCurrentActorToolkitReader(stub)
		if err != nil {
			t.Fatal(err)
		}
		result, found, err := reader.GetCurrentToolkit(context.Background(), 7, 42, 19)
		if err != nil || found != visible || stub.project != 7 || stub.actor != 42 || stub.toolkit != 19 {
			t.Fatal("actor scope changed")
		}
		if visible && result.ID != 19 {
			t.Fatal("saved identity changed")
		}
	}
}
