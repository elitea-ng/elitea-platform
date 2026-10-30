package runtimecomposition

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
)

type childVersionSourceStub struct {
	record storage.CurrentApplicationVersionRecord
	err    error
}

func (s childVersionSourceStub) ReadCurrentApplicationVersion(context.Context, int64, int64, int64) (storage.CurrentApplicationVersionRecord, error) {
	return s.record, s.err
}

func TestPipelineApprovalChildrenReadsTheSavedVersion(t *testing.T) {
	saved := json.RawMessage(`{"agent_type":"pipeline"}`)
	children := pipelineApprovalChildren{versions: childVersionSourceStub{record: storage.CurrentApplicationVersionRecord{ApplicationID: 3, VersionID: 4, VersionDetails: saved}}}
	if got, err := children.ReadPipelineChildVersion(t.Context(), 7, 3, 4); err != nil || string(got) != string(saved) {
		t.Fatalf("got %s, %v", got, err)
	}
	if _, err := children.ReadPipelineChildVersion(t.Context(), 7, 3, 5); !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("a mismatched row: %v", err)
	}
	missing := pipelineApprovalChildren{versions: childVersionSourceStub{err: storage.ErrContentNotFound}}
	if _, err := missing.ReadPipelineChildVersion(t.Context(), 7, 3, 4); !errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("a missing child: %v", err)
	}
	down := errors.New("database down")
	failing := pipelineApprovalChildren{versions: childVersionSourceStub{err: down}}
	if _, err := failing.ReadPipelineChildVersion(t.Context(), 7, 3, 4); !errors.Is(err, down) || errors.Is(err, agentexecutionapp.ErrUnsupportedCurrentAgentStart) {
		t.Fatalf("a read fault: %v", err)
	}
}
